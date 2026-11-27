package bench

import (
	"fmt"
	"math/rand/v2"
	"sort"
)

// Customer, Invoice and UsageEvent are the three fixture sources. They
// stand for a CRM, a billing system and a product-analytics store: three
// backends a real question has to join, which is the workload ADR-0003
// names as this library's niche. Money is integer cents so an exact
// answer is exactly checkable — a float total would make every checker a
// tolerance argument.
type Customer struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Country        string `json:"country"`
	Plan           string `json:"plan"`
	SignupDate     string `json:"signup_date"`
	AccountManager string `json:"account_manager"`
}

type Invoice struct {
	ID          string `json:"id"`
	CustomerID  string `json:"customer_id"`
	Month       string `json:"month"`
	AmountCents int    `json:"amount_cents"`
	Status      string `json:"status"`
}

type UsageEvent struct {
	CustomerID string `json:"customer_id"`
	Day        string `json:"day"`
	Feature    string `json:"feature"`
	Units      int    `json:"units"`
}

// Data is one generated world. Every slice is sorted by ID (usage by
// customer, then day) so a tool's output order — and therefore what a
// model sees on page 1 — is a function of the seed alone.
type Data struct {
	Seed      uint64
	Customers []Customer
	Invoices  []Invoice
	Usage     []UsageEvent

	byID map[string]Customer
}

var (
	countries = []string{"DE", "FR", "US", "GB", "NL", "ES", "IT", "SE", "PL", "CH"}
	managers  = []string{"Alice Brandt", "Bilal Khan", "Chen Wei", "Dana Novak", "Emil Sørensen", "Fatima Diallo", "Giulia Rossi", "Hiro Tanaka"}
	features  = []string{"api", "export", "dashboard", "sso", "alerts"}
	nameA     = []string{"Blue", "Iron", "North", "Quick", "Silver", "Bright", "Green", "Red", "Delta", "Nova", "Polar", "Urban"}
	nameB     = []string{"Logistics", "Labs", "Foods", "Systems", "Media", "Health", "Energy", "Retail", "Finance", "Robotics"}
	nameC     = []string{"GmbH", "Ltd", "Inc", "SA", "BV", "AB", "Srl", "AG"}
)

// Sizes are fixed rather than flags: a benchmark whose fixture size is a
// knob produces numbers nobody can compare. They put the per-task row
// counts in the 100–3,000 range the comparison is about — enough that
// echoing rows through the conversation costs real tokens, small enough
// that a baseline with a 128k window can still in principle finish.
const (
	numCustomers = 400
	numUsage     = 8000
)

// Generate builds the fixture world for seed. Same seed, same data, on
// every platform: math/rand/v2's PCG is specified, unlike the global
// source.
func Generate(seed uint64) *Data {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	d := &Data{Seed: seed, byID: map[string]Customer{}}

	for i := 1; i <= numCustomers; i++ {
		plan := "free"
		switch p := r.IntN(100); {
		case p < 15:
			plan = "enterprise"
		case p < 50:
			plan = "pro"
		}
		c := Customer{
			ID:             fmt.Sprintf("C%04d", i),
			Name:           fmt.Sprintf("%s %s %s", nameA[r.IntN(len(nameA))], nameB[r.IntN(len(nameB))], nameC[r.IntN(len(nameC))]),
			Country:        countries[r.IntN(len(countries))],
			Plan:           plan,
			SignupDate:     day(2023, r.IntN(3*365)),
			AccountManager: managers[r.IntN(len(managers))],
		}
		d.Customers = append(d.Customers, c)
		d.byID[c.ID] = c
	}

	inv := 0
	for _, c := range d.Customers {
		base := map[string]int{"free": 0, "pro": 4900, "enterprise": 49000}[c.Plan]
		for m := 1; m <= 12; m++ {
			if r.IntN(100) >= 70 {
				continue
			}
			inv++
			status := "paid"
			switch s := r.IntN(100); {
			case s < 8:
				status = "overdue"
			case s < 20:
				status = "open"
			}
			// Free customers still buy add-ons, so every plan has
			// invoices; the random part keeps totals from tying.
			d.Invoices = append(d.Invoices, Invoice{
				ID:          fmt.Sprintf("INV-%05d", inv),
				CustomerID:  c.ID,
				Month:       fmt.Sprintf("2026-%02d", m),
				AmountCents: base + 500 + r.IntN(150000),
				Status:      status,
			})
		}
	}

	for range numUsage {
		d.Usage = append(d.Usage, UsageEvent{
			CustomerID: d.Customers[r.IntN(len(d.Customers))].ID,
			Day:        day(2026, r.IntN(181)), // 2026-01-01 .. 2026-06-30
			Feature:    features[r.IntN(len(features))],
			Units:      1 + r.IntN(100),
		})
	}
	sort.SliceStable(d.Usage, func(i, j int) bool {
		a, b := d.Usage[i], d.Usage[j]
		if a.CustomerID != b.CustomerID {
			return a.CustomerID < b.CustomerID
		}
		return a.Day < b.Day
	})
	return d
}

// day renders the date offset days after Jan 1 of year, without
// time.Time so no time zone can ever enter the fixtures.
func day(year, offset int) string {
	months := []int{31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}
	for {
		leap := year%4 == 0 && (year%100 != 0 || year%400 == 0)
		n := 365
		if leap {
			n = 366
		}
		if offset < n {
			break
		}
		offset -= n
		year++
	}
	for m, n := range months {
		if m == 1 && year%4 == 0 && (year%100 != 0 || year%400 == 0) {
			n = 29
		}
		if offset < n {
			return fmt.Sprintf("%04d-%02d-%02d", year, m+1, offset+1)
		}
		offset -= n
	}
	panic("unreachable")
}

// Customer returns the customer with id.
func (d *Data) Customer(id string) (Customer, bool) {
	c, ok := d.byID[id]
	return c, ok
}
