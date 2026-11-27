package reliability

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
)

// The world is a small shop: customers, orders, stock, tickets, shipping.
// It is deterministic, so every task's expected answer is computed from the
// same data the agent reads, never written down by hand. Each run gets its
// own World, because tickets, the flaky stock service and the once-only
// history are per-run state.

type Customer struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	City string `json:"city"`
	Tier string `json:"tier"`
}

type Item struct {
	SKU        string `json:"sku"`
	Qty        int    `json:"qty"`
	PriceCents int    `json:"priceCents"`
}

type Order struct {
	ID          string `json:"id"`
	CustomerID  string `json:"customerId"`
	Date        string `json:"date"`
	Status      string `json:"status"`
	AmountCents int    `json:"amountCents"`
	Items       []Item `json:"items"`
}

type Ticket struct {
	ID         string   `json:"ticketId"`
	CustomerID string   `json:"customerId"`
	Priority   string   `json:"priority"`
	Subject    string   `json:"subject"`
	Tags       []string `json:"tags"`
}

// Call is one function call the agent made, whatever the arm: a JS call in
// the sandbox or a native tool call.
type Call struct {
	Name  string         `json:"name"`
	Args  map[string]any `json:"args,omitempty"`
	Error string         `json:"error,omitempty"`
}

var (
	customers = []Customer{
		{"C-001", "Albrecht & Söhne", "Hamburg", "silver"},
		{"C-002", "Bergmann AG", "München", "gold"},
		{"C-003", "Clausen Logistik", "Bremen", "bronze"},
		{"C-004", "Dietrich Bau", "Köln", "silver"},
		{"C-005", "Engel Design", "Berlin", "bronze"},
		{"C-006", "Fischer & Co", "Stuttgart", "gold"},
		{"C-007", "Hoffmann GmbH", "Leipzig", "gold"},
		{"C-008", "Krüger Elektro", "Dresden", "silver"},
		{"C-009", "Lange Medien", "Hannover", "bronze"},
		{"C-010", "Meyer Feinkost", "Nürnberg", "silver"},
		{"C-011", "Schulz Technik", "Frankfurt", "gold"},
		{"C-012", "Weber KG", "Düsseldorf", "bronze"},
	}
	skus   = []string{"LMP-200", "CHR-110", "DSK-450", "SHF-075", "CAB-020", "MON-270", "KBD-035", "MSE-015"}
	prices = map[string]int{"LMP-200": 4990, "CHR-110": 18900, "DSK-450": 45900, "SHF-075": 7500, "CAB-020": 1290, "MON-270": 27900, "KBD-035": 3490, "MSE-015": 1590}
	stock  = map[string]int{"LMP-200": 37, "CHR-110": 12, "DSK-450": 4, "SHF-075": 58, "CAB-020": 310, "MON-270": 9, "KBD-035": 75, "MSE-015": 120}
	orders = genOrders()
)

// genOrders builds 240 orders from January to June 2025 with a fixed
// linear congruential sequence: reproducible without a seed file, and
// irregular enough that no total can be guessed.
func genOrders() []Order {
	x := uint32(20251127)
	next := func(n int) int {
		x = x*1664525 + 1013904223
		return int((x >> 8) % uint32(n))
	}
	statuses := []string{"paid", "paid", "paid", "open", "cancelled"}
	var out []Order
	for i := 0; i < 240; i++ {
		c := customers[next(len(customers))]
		month := 1 + next(6)
		day := 1 + next(28)
		var items []Item
		total := 0
		for range 1 + next(3) {
			sku := skus[next(len(skus))]
			qty := 1 + next(4)
			items = append(items, Item{SKU: sku, Qty: qty, PriceCents: prices[sku]})
			total += qty * prices[sku]
		}
		out = append(out, Order{
			ID:          fmt.Sprintf("O-%d", 1001+i),
			CustomerID:  c.ID,
			Date:        fmt.Sprintf("2025-%02d-%02d", month, day),
			Status:      statuses[next(len(statuses))],
			AmountCents: total,
			Items:       items,
		})
	}
	return out
}

// World is one run's view of the shop.
type World struct {
	mu          sync.Mutex
	calls       []Call
	tickets     []Ticket
	stockFailed map[string]bool
	historyRead bool
	fns         map[string]Function
}

func NewWorld(fns []Function) *World {
	w := &World{stockFailed: map[string]bool{}, fns: map[string]Function{}}
	for _, f := range fns {
		w.fns[f.Name] = f
	}
	return w
}

// Invoke runs a function by name with named arguments, recording the call.
// Both the sandbox and the tool-calling driver come through here, so a call
// is a call whichever way the model made it.
func (w *World) Invoke(name string, args map[string]any) (any, error) {
	f, ok := w.fns[name]
	if !ok {
		err := fmt.Errorf("unknown function %s", name)
		w.record(Call{Name: name, Args: args, Error: err.Error()})
		return nil, err
	}
	res, err := f.Handler(w, args)
	c := Call{Name: name, Args: args}
	if err != nil {
		c.Error = err.Error()
	}
	w.record(c)
	if err != nil {
		return nil, err
	}
	// Plain maps and slices at the boundary: goja maps Go structs by field
	// name, not json tag, so a struct would surface as {CustomerID}.
	return plain(res), nil
}

func (w *World) record(c Call) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, c)
}

func (w *World) Calls() []Call {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.calls)
}

func (w *World) Tickets() []Ticket {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.tickets)
}

func plain(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}

// Param is one parameter of a Function. Kind is a JSON-schema type;
// Object holds the nested parameters of an object parameter.
type Param struct {
	Name     string
	Kind     string // "string", "number", "array", "object"
	Enum     []string
	Items    string // element type of an array
	Object   []Param
	Optional bool
	Doc      string
}

// Function is one capability, described once and offered two ways: as a
// TypeScript declaration to the code arms and as a JSON-schema tool to the
// tool-calling arm. Positional reports whether JS calls it with its
// parameters in order (findCustomer("x")) or with one object
// (createTicket({...})); tools always take an object.
type Function struct {
	Name       string
	Doc        string
	Params     []Param
	Returns    string // TypeScript return type
	Positional bool
	Handler    func(w *World, args map[string]any) (any, error)
}

func str(args map[string]any, k string) string {
	s, _ := args[k].(string)
	return strings.TrimSpace(s)
}

func findCustomer(name string) *Customer {
	n := strings.ToLower(strings.TrimSpace(name))
	for i := range customers {
		if strings.ToLower(customers[i].Name) == n {
			return &customers[i]
		}
	}
	for i := range customers {
		if n != "" && strings.Contains(strings.ToLower(customers[i].Name), n) {
			return &customers[i]
		}
	}
	return nil
}

func customerByID(id string) *Customer {
	for i := range customers {
		if customers[i].ID == id {
			return &customers[i]
		}
	}
	return nil
}

type orderSummary struct {
	ID          string `json:"id"`
	Date        string `json:"date"`
	Status      string `json:"status"`
	AmountCents int    `json:"amountCents"`
	Currency    string `json:"currency"`
}

func ordersOf(customerID, status, month string) []orderSummary {
	out := []orderSummary{}
	for _, o := range orders {
		if o.CustomerID != customerID || (status != "" && o.Status != status) || (month != "" && !strings.HasPrefix(o.Date, month+"-")) {
			continue
		}
		out = append(out, orderSummary{o.ID, o.Date, o.Status, o.AmountCents, "EUR"})
	}
	return out
}

var (
	fnFindCustomer = Function{
		Name: "findCustomer", Positional: true,
		Doc:     "Look up a customer by name. Returns null when there is no such customer.",
		Params:  []Param{{Name: "name", Kind: "string"}},
		Returns: "{ id: string; name: string; city: string; tier: \"bronze\" | \"silver\" | \"gold\" } | null",
		Handler: func(w *World, a map[string]any) (any, error) {
			c := findCustomer(str(a, "name"))
			if c == nil {
				return nil, nil
			}
			return c, nil
		},
	}
	fnListOrders = Function{
		Name:    "listOrders",
		Doc:     "A customer's orders, optionally filtered by status and by month. Amounts are in euro cents.",
		Params:  []Param{{Name: "customerId", Kind: "string"}, {Name: "status", Kind: "string", Enum: []string{"open", "paid", "cancelled"}, Optional: true}, {Name: "month", Kind: "string", Optional: true, Doc: "YYYY-MM"}},
		Returns: "{ id: string; date: string; status: \"open\" | \"paid\" | \"cancelled\"; amountCents: number; currency: \"EUR\" }[]",
		Handler: func(w *World, a map[string]any) (any, error) {
			id := str(a, "customerId")
			if customerByID(id) == nil {
				return nil, fmt.Errorf("listOrders: no customer with id %q; customerId is the id from findCustomer, e.g. \"C-001\"", id)
			}
			status, month := str(a, "status"), str(a, "month")
			if status != "" && !slices.Contains([]string{"open", "paid", "cancelled"}, status) {
				return nil, fmt.Errorf("listOrders: status must be one of open, paid, cancelled")
			}
			if month != "" && (len(month) != 7 || month[4] != '-') {
				return nil, fmt.Errorf("listOrders: month must be YYYY-MM, e.g. \"2025-03\"")
			}
			return ordersOf(id, status, month), nil
		},
	}
	fnGetOrder = Function{
		Name: "getOrder", Positional: true,
		Doc:     "One order with its items. Prices are in euro cents.",
		Params:  []Param{{Name: "orderId", Kind: "string"}},
		Returns: "{ id: string; customerId: string; date: string; status: string; amountCents: number; items: { sku: string; qty: number; priceCents: number }[] }",
		Handler: func(w *World, a map[string]any) (any, error) {
			id := str(a, "orderId")
			for _, o := range orders {
				if o.ID == id {
					return o, nil
				}
			}
			return nil, fmt.Errorf("getOrder: no order %q", id)
		},
	}
	fnGetStock = Function{
		Name: "getStock", Positional: true,
		Doc:     "Units of a product in stock.",
		Params:  []Param{{Name: "sku", Kind: "string"}},
		Returns: "{ sku: string; available: number }",
		Handler: func(w *World, a map[string]any) (any, error) {
			sku := str(a, "sku")
			n, ok := stock[sku]
			if !ok {
				return nil, fmt.Errorf("getStock: unknown sku %q", sku)
			}
			w.mu.Lock()
			failed := w.stockFailed[sku]
			w.stockFailed[sku] = true
			w.mu.Unlock()
			// The first call per sku fails the way a real backend does now
			// and then: retryable, and saying so.
			if !failed {
				return nil, fmt.Errorf("getStock: stock service unavailable (503), try again")
			}
			return map[string]any{"sku": sku, "available": n}, nil
		},
	}
	fnCreateTicket = Function{
		Name: "createTicket",
		Doc:  "Open a support ticket. Returns its id.",
		Params: []Param{
			{Name: "customerId", Kind: "string"},
			{Name: "priority", Kind: "string", Enum: []string{"low", "normal", "high"}},
			{Name: "subject", Kind: "string"},
			{Name: "tags", Kind: "array", Items: "string", Optional: true},
		},
		Returns: "{ ticketId: string }",
		Handler: func(w *World, a map[string]any) (any, error) {
			id := str(a, "customerId")
			if customerByID(id) == nil {
				return nil, fmt.Errorf("createTicket: no customer with id %q; customerId is the id from findCustomer, e.g. \"C-001\"", id)
			}
			prio := str(a, "priority")
			if !slices.Contains([]string{"low", "normal", "high"}, prio) {
				return nil, fmt.Errorf("createTicket: priority must be one of low, normal, high")
			}
			subject := str(a, "subject")
			if subject == "" {
				return nil, fmt.Errorf("createTicket: subject is required")
			}
			var tags []string
			switch t := a["tags"].(type) {
			case nil:
			case []any:
				for _, x := range t {
					s, ok := x.(string)
					if !ok {
						return nil, fmt.Errorf("createTicket: tags must be an array of strings")
					}
					tags = append(tags, s)
				}
			default:
				return nil, fmt.Errorf("createTicket: tags must be an array of strings")
			}
			w.mu.Lock()
			defer w.mu.Unlock()
			t := Ticket{ID: fmt.Sprintf("T-%d", 5001+len(w.tickets)), CustomerID: id, Priority: prio, Subject: subject, Tags: tags}
			w.tickets = append(w.tickets, t)
			return map[string]any{"ticketId": t.ID}, nil
		},
	}
	fnLoadHistory = Function{
		Name:    "loadOrderHistory",
		Doc:     "Every order of the shop, all customers. Slow and metered: it may be called only once per task, so keep what it returns.",
		Returns: "{ id: string; customerId: string; date: string; status: string; amountCents: number }[]",
		Handler: func(w *World, a map[string]any) (any, error) {
			w.mu.Lock()
			read := w.historyRead
			w.historyRead = true
			w.mu.Unlock()
			if read {
				return nil, fmt.Errorf("loadOrderHistory: quota exceeded, it may be called only once per task; use what the first call returned")
			}
			out := make([]map[string]any, 0, len(orders))
			for _, o := range orders {
				out = append(out, map[string]any{"id": o.ID, "customerId": o.CustomerID, "date": o.Date, "status": o.Status, "amountCents": o.AmountCents})
			}
			return out, nil
		},
	}
	fnGetQuestion = Function{
		Name:    "getQuestion",
		Doc:     "The question to answer about the order history.",
		Returns: "string",
		Handler: func(w *World, a map[string]any) (any, error) {
			return "How many paid orders were placed in March 2025, across all customers, and what is their total in euros?", nil
		},
	}
	fnShipping = Function{
		Name: "getShippingStatus", Positional: true,
		Doc:     "Where an order is on its way to the customer.",
		Params:  []Param{{Name: "orderId", Kind: "string"}},
		Returns: "{ orderId: string; carrier: string; status: \"preparing\" | \"in_transit\" | \"delivered\"; eta: string }",
		Handler: func(w *World, a map[string]any) (any, error) {
			id := str(a, "orderId")
			if id != "O-1042" {
				return map[string]any{"orderId": id, "carrier": "DPD", "status": "delivered", "eta": "2025-05-02"}, nil
			}
			return map[string]any{"orderId": id, "carrier": "DHL", "status": "in_transit", "eta": "2025-06-14"}, nil
		},
	}
)

// decoys are plausible functions of the same shop that no task needs. Task
// 9 offers them next to the one that answers it, so picking is part of the
// task; they work, so exploring them is not punished, only slow.
func decoys() []Function {
	mk := func(name, doc string, params []Param, ret string, positional bool, val any) Function {
		return Function{Name: name, Doc: doc, Params: params, Returns: ret, Positional: positional,
			Handler: func(*World, map[string]any) (any, error) { return val, nil }}
	}
	id := func(n string) []Param { return []Param{{Name: n, Kind: "string"}} }
	return []Function{
		mk("getInvoice", "One invoice.", id("invoiceId"), "{ id: string; orderId: string; amountCents: number; paid: boolean }", true, map[string]any{"id": "I-1", "orderId": "O-1001", "amountCents": 4990, "paid": true}),
		mk("listInvoices", "A customer's invoices.", id("customerId"), "{ id: string; orderId: string; amountCents: number }[]", true, []any{}),
		mk("getCustomerNotes", "Internal notes on a customer.", id("customerId"), "string[]", true, []any{}),
		mk("updateCustomer", "Change a customer's city or tier.", []Param{{Name: "customerId", Kind: "string"}, {Name: "city", Kind: "string", Optional: true}, {Name: "tier", Kind: "string", Optional: true}}, "{ ok: boolean }", false, map[string]any{"ok": true}),
		mk("listProducts", "All products.", nil, "{ sku: string; name: string }[]", true, []any{map[string]any{"sku": "LMP-200", "name": "Desk lamp"}}),
		mk("getProduct", "One product.", id("sku"), "{ sku: string; name: string; priceCents: number }", true, map[string]any{"sku": "LMP-200", "name": "Desk lamp", "priceCents": 4990}),
		mk("getRefund", "One refund.", id("refundId"), "{ id: string; orderId: string; amountCents: number }", true, nil),
		mk("listRefunds", "Refunds of an order.", id("orderId"), "{ id: string; amountCents: number }[]", true, []any{}),
		mk("getWarehouse", "One warehouse.", id("warehouseId"), "{ id: string; city: string }", true, map[string]any{"id": "W-1", "city": "Kassel"}),
		mk("listWarehouses", "All warehouses.", nil, "{ id: string; city: string }[]", true, []any{map[string]any{"id": "W-1", "city": "Kassel"}}),
		mk("getCarrier", "A shipping carrier's contact details.", id("carrier"), "{ name: string; phone: string }", true, map[string]any{"name": "DHL", "phone": "+49 228 1234"}),
		mk("listTickets", "A customer's support tickets.", id("customerId"), "{ ticketId: string; subject: string }[]", true, []any{}),
		mk("getTicket", "One support ticket.", id("ticketId"), "{ ticketId: string; subject: string; status: string }", true, nil),
		mk("closeTicket", "Close a support ticket.", id("ticketId"), "{ ok: boolean }", true, map[string]any{"ok": true}),
		mk("getPromotion", "A running promotion by code.", id("code"), "{ code: string; percent: number }", true, nil),
	}
}
