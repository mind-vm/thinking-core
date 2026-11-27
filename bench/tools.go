package bench

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Tool is one fixture backend operation, defined once and handed to both
// agents: the baseline as an OpenAI function tool, thinking-core as an
// MCP-style tool behind ext.MCPPack. Same name, same one-line
// description, same JSON Schema, same handler — the only thing that
// differs between the two sides is how the model reaches it, which is
// the variable being measured.
//
// Description is one line on purpose: ext.MCPPack puts only the first
// line in the TypeScript declaration, and the baseline would otherwise
// be told more about the return shape than thinking-core is.
type Tool struct {
	Name        string
	Description string
	Schema      map[string]any
	call        func(d *Data, a args) (any, error)
}

// Call runs the tool and returns its result as JSON-shaped values
// (map[string]any, []any, float64, string), never Go structs: goja maps a
// struct by Go field name, not json tag (AGENTS.md), so a struct would
// show the sandbox different keys than the baseline reads.
func (t Tool) Call(d *Data, raw map[string]any) (any, error) {
	a := args(raw)
	if err := a.only(t.Schema); err != nil {
		return nil, fmt.Errorf("%s: %w", t.Name, err)
	}
	v, err := t.call(d, a)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", t.Name, err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	err = json.Unmarshal(b, &out)
	return out, err
}

// Tools is the fixed tool set every task is answered with. Pagination
// limits are deliberately small enough that two tasks cannot be answered
// from page 1; query_usage is deliberately unpaginated, because "the
// analytics API returns all matching events" is common and is where a
// large intermediate result comes from.
func Tools() []Tool {
	return []Tool{
		{
			Name:        "list_customers",
			Description: "List CRM customers, optionally filtered by country and plan, 50 per page. Returns {customers: {id, name, country, plan, signup_date, account_manager}[], page, total_pages, total}.",
			Schema: schema(`{"type":"object","properties":{
				"country":{"type":"string","description":"ISO country code, e.g. \"DE\""},
				"plan":{"type":"string","enum":["free","pro","enterprise"]},
				"page":{"type":"integer","description":"1-based page number, default 1"}}}`),
			call: listCustomers,
		},
		{
			Name:        "get_customer",
			Description: "Get one CRM customer by id. Returns {id, name, country, plan, signup_date, account_manager}.",
			Schema: schema(`{"type":"object","properties":{
				"id":{"type":"string","description":"customer id, e.g. \"C0042\""}},"required":["id"]}`),
			call: getCustomer,
		},
		{
			Name:        "list_invoices",
			Description: "List billing invoices for 2026, optionally filtered by customer_id, status and month, 200 per page. Returns {invoices: {id, customer_id, month, amount_cents, status}[], page, total_pages, total}.",
			Schema: schema(`{"type":"object","properties":{
				"customer_id":{"type":"string"},
				"status":{"type":"string","enum":["paid","open","overdue"]},
				"month":{"type":"string","description":"YYYY-MM, e.g. \"2026-03\""},
				"page":{"type":"integer","description":"1-based page number, default 1"}}}`),
			call: listInvoices,
		},
		{
			Name:        "query_usage",
			Description: "Query product usage events (2026-01-01 to 2026-06-30), optionally filtered by customer_id, feature and an inclusive day range; returns every match, unpaginated. Returns {events: {customer_id, day, feature, units}[], count}.",
			Schema: schema(`{"type":"object","properties":{
				"customer_id":{"type":"string"},
				"feature":{"type":"string","enum":["api","export","dashboard","sso","alerts"]},
				"from_day":{"type":"string","description":"YYYY-MM-DD, inclusive"},
				"to_day":{"type":"string","description":"YYYY-MM-DD, inclusive"}}}`),
			call: queryUsage,
		},
	}
}

const (
	customerPage = 50
	invoicePage  = 200
)

func listCustomers(d *Data, a args) (any, error) {
	country, err := a.str("country")
	if err != nil {
		return nil, err
	}
	plan, err := a.str("plan")
	if err != nil {
		return nil, err
	}
	page, err := a.page()
	if err != nil {
		return nil, err
	}
	match := make([]Customer, 0)
	for _, c := range d.Customers {
		if (country == "" || strings.EqualFold(c.Country, country)) && (plan == "" || c.Plan == plan) {
			match = append(match, c)
		}
	}
	items, pages := paginate(match, page, customerPage)
	return map[string]any{"customers": items, "page": page, "total_pages": pages, "total": len(match)}, nil
}

func getCustomer(d *Data, a args) (any, error) {
	id, err := a.str("id")
	if err != nil {
		return nil, err
	}
	c, ok := d.Customer(strings.ToUpper(strings.TrimSpace(id)))
	if !ok {
		return nil, fmt.Errorf("no customer with id %q (ids look like \"C0042\")", id)
	}
	return c, nil
}

func listInvoices(d *Data, a args) (any, error) {
	cust, err := a.str("customer_id")
	if err != nil {
		return nil, err
	}
	status, err := a.str("status")
	if err != nil {
		return nil, err
	}
	month, err := a.str("month")
	if err != nil {
		return nil, err
	}
	page, err := a.page()
	if err != nil {
		return nil, err
	}
	match := make([]Invoice, 0)
	for _, in := range d.Invoices {
		if (cust == "" || in.CustomerID == cust) && (status == "" || in.Status == status) && (month == "" || in.Month == month) {
			match = append(match, in)
		}
	}
	items, pages := paginate(match, page, invoicePage)
	return map[string]any{"invoices": items, "page": page, "total_pages": pages, "total": len(match)}, nil
}

func queryUsage(d *Data, a args) (any, error) {
	vals := map[string]string{}
	for _, k := range []string{"customer_id", "feature", "from_day", "to_day"} {
		v, err := a.str(k)
		if err != nil {
			return nil, err
		}
		vals[k] = v
	}
	match := make([]UsageEvent, 0)
	for _, e := range d.Usage {
		if (vals["customer_id"] == "" || e.CustomerID == vals["customer_id"]) &&
			(vals["feature"] == "" || e.Feature == vals["feature"]) &&
			(vals["from_day"] == "" || e.Day >= vals["from_day"]) &&
			(vals["to_day"] == "" || e.Day <= vals["to_day"]) {
			match = append(match, e)
		}
	}
	return map[string]any{"events": match, "count": len(match)}, nil
}

func paginate[T any](all []T, page, size int) ([]T, int) {
	pages := (len(all) + size - 1) / size
	if pages == 0 {
		pages = 1
	}
	lo := (page - 1) * size
	if lo >= len(all) {
		return make([]T, 0), pages
	}
	return all[lo:min(lo+size, len(all))], pages
}

func schema(s string) map[string]any {
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		panic(err)
	}
	return m
}

// args reads tool arguments as they arrive from either side: JSON
// decoding gives float64 for numbers, a goja export gives int64.
type args map[string]any

// only rejects an argument the schema does not declare. A silently
// ignored typo ("date" for "month") returns unfiltered data that looks
// like an answer; an error costs one turn and says what to do instead.
func (a args) only(schema map[string]any) error {
	props, _ := schema["properties"].(map[string]any)
	for k := range a {
		if _, ok := props[k]; !ok {
			names := make([]string, 0, len(props))
			for n := range props {
				names = append(names, n)
			}
			sort.Strings(names)
			return fmt.Errorf("unknown argument %q; accepted: %s", k, strings.Join(names, ", "))
		}
	}
	return nil
}

func (a args) str(k string) (string, error) {
	v, ok := a[k]
	if !ok || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("argument %q must be a string, got %T", k, v)
	}
	return strings.TrimSpace(s), nil
}

func (a args) page() (int, error) {
	v, ok := a["page"]
	if !ok || v == nil {
		return 1, nil
	}
	var n float64
	switch x := v.(type) {
	case float64:
		n = x
	case int64:
		n = float64(x)
	case int:
		n = float64(x)
	case string:
		f, err := strconv.ParseFloat(x, 64)
		if err != nil {
			return 0, fmt.Errorf("argument \"page\" must be an integer, got %q", x)
		}
		n = f
	default:
		return 0, fmt.Errorf("argument \"page\" must be an integer, got %T", v)
	}
	if n < 1 || n != math.Trunc(n) {
		return 0, fmt.Errorf("argument \"page\" must be an integer >= 1, got %v", v)
	}
	return int(n), nil
}
