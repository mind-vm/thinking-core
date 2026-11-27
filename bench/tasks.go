package bench

import (
	"fmt"
	"sort"
	"strings"
)

// Task is one question with an exact answer. Expected is computed from
// the fixture data by plain Go — the reference solution — so the answer
// changes with the seed and cannot be memorised, and checking it needs no
// judge model whose own mistakes would land in the accuracy column.
type Task struct {
	ID string
	// Shape names what the task exercises, for the summary and README.
	Shape    string
	Prompt   string
	Expected map[string]any
}

// answerFormat is appended to every prompt so both agents are held to the
// same output contract and the checker can parse either.
func answerFormat(shape string) string {
	return "\n\nReply with the final answer as a JSON object of exactly this form, and nothing else: " + shape
}

// Tasks builds the task list for one fixture world. Order is stable; IDs
// are what the -tasks flag selects.
func Tasks(d *Data) []Task {
	return []Task{
		taskLookup(d),
		taskOverdueByCountry(d),
		taskProSignups(d),
		taskTopEnterpriseH1(d),
		taskManagerMarch(d),
		taskTopFeatureMarch(d),
		taskFreeAPIPowerUsers(d),
		taskOverdueEnterpriseUsage(d),
	}
}

// TaskByID returns the task with id, for tests and the -tasks flag.
func TaskByID(tasks []Task, id string) (Task, bool) {
	for _, t := range tasks {
		if t.ID == id {
			return t, true
		}
	}
	return Task{}, false
}

// taskLookup is the honest control: one tool call answers it, nothing is
// joined, and code-as-action can only add overhead. A harness that shows
// thinking-core winning here too would be measuring something else.
func taskLookup(d *Data) Task {
	c, _ := d.Customer("C0042")
	return Task{
		ID:       "lookup",
		Shape:    "single tool call, no join",
		Prompt:   "Which plan is customer C0042 on, and in which country is it based?" + answerFormat(`{"plan": "<plan>", "country": "<country code>"}`),
		Expected: map[string]any{"plan": c.Plan, "country": c.Country},
	}
}

func taskOverdueByCountry(d *Data) Task {
	in := map[string]bool{}
	for _, c := range d.Customers {
		if c.Country == "DE" {
			in[c.ID] = true
		}
	}
	n, total := 0, 0
	for _, i := range d.Invoices {
		if i.Status == "overdue" && in[i.CustomerID] {
			n++
			total += i.AmountCents
		}
	}
	return Task{
		ID:       "overdue_de",
		Shape:    "join customers×invoices, filter, sum",
		Prompt:   "What is the total amount of overdue invoices belonging to customers based in Germany (DE), and how many such invoices are there?" + answerFormat(`{"invoice_count": <integer>, "total_cents": <integer>}`),
		Expected: map[string]any{"invoice_count": n, "total_cents": total},
	}
}

// taskProSignups cannot be answered from one page: list_customers has no
// signup filter and the pro plan spans several pages of 50.
func taskProSignups(d *Data) Task {
	n := 0
	for _, c := range d.Customers {
		if c.Plan == "pro" && strings.HasPrefix(c.SignupDate, "2025-") {
			n++
		}
	}
	return Task{
		ID:       "pro_signups_2025",
		Shape:    "pagination, filter, count",
		Prompt:   "How many customers on the pro plan signed up during the year 2025?" + answerFormat(`{"count": <integer>}`),
		Expected: map[string]any{"count": n},
	}
}

func taskTopEnterpriseH1(d *Data) Task {
	ent := map[string]bool{}
	for _, c := range d.Customers {
		if c.Plan == "enterprise" {
			ent[c.ID] = true
		}
	}
	sum := map[string]int{}
	for _, i := range d.Invoices {
		if ent[i.CustomerID] && i.Status == "paid" && i.Month <= "2026-06" {
			sum[i.CustomerID] += i.AmountCents
		}
	}
	top := topN(sum, 5)
	rows := make([]any, 0, len(top))
	for _, kv := range top {
		rows = append(rows, map[string]any{"id": kv.key, "paid_cents": kv.val})
	}
	return Task{
		ID:    "top_enterprise_h1",
		Shape: "join, group-by, top-N over ~1k invoices",
		Prompt: "Which 5 enterprise-plan customers paid the most in the first half of 2026 (invoices with status paid, months 2026-01 through 2026-06)? " +
			"Order them by paid total, highest first; break ties by customer id ascending." +
			answerFormat(`{"top": [{"id": "<customer id>", "paid_cents": <integer>}, ...5 entries]}`),
		Expected: map[string]any{"top": rows},
	}
}

// taskManagerMarch needs every customer (8 pages) to map invoices to
// account managers, then a group-by.
func taskManagerMarch(d *Data) Task {
	sum := map[string]int{}
	for _, i := range d.Invoices {
		if i.Month == "2026-03" && i.Status == "paid" {
			c, _ := d.Customer(i.CustomerID)
			sum[c.AccountManager] += i.AmountCents
		}
	}
	top := topN(sum, 1)[0]
	return Task{
		ID:    "manager_march",
		Shape: "pagination ×8, join, group-by, argmax",
		Prompt: "Whose customers paid the most in March 2026? For each account manager, sum the paid invoices of month 2026-03 of the customers they manage; give the account manager with the largest total and that total." +
			answerFormat(`{"account_manager": "<name>", "paid_cents": <integer>}`),
		Expected: map[string]any{"account_manager": top.key, "paid_cents": top.val},
	}
}

// taskTopFeatureMarch has the large intermediate result: about 1,400
// usage events for one month, of which the answer is two values.
func taskTopFeatureMarch(d *Data) Task {
	sum := map[string]int{}
	for _, e := range d.Usage {
		if strings.HasPrefix(e.Day, "2026-03-") {
			sum[e.Feature] += e.Units
		}
	}
	top := topN(sum, 1)[0]
	return Task{
		ID:       "top_feature_march",
		Shape:    "large intermediate result (~1.4k events), aggregate",
		Prompt:   "Which product feature had the highest total usage units in March 2026, across all customers, and what was that total?" + answerFormat(`{"feature": "<feature>", "units": <integer>}`),
		Expected: map[string]any{"feature": top.key, "units": top.val},
	}
}

func taskFreeAPIPowerUsers(d *Data) Task {
	sum := map[string]int{}
	for _, e := range d.Usage {
		c, _ := d.Customer(e.CustomerID)
		if c.Plan == "free" && e.Feature == "api" && e.Day >= "2026-04-01" && e.Day <= "2026-06-30" {
			sum[e.CustomerID] += e.Units
		}
	}
	top := topN(sum, 3)
	rows := make([]any, 0, len(top))
	for _, kv := range top {
		rows = append(rows, map[string]any{"id": kv.key, "units": kv.val})
	}
	return Task{
		ID:    "free_api_q2",
		Shape: "join customers×usage, filter, top-N",
		Prompt: "Which 3 free-plan customers used the api feature the most in Q2 2026 (2026-04-01 to 2026-06-30), by total units? " +
			"Order them by units, highest first; break ties by customer id ascending." +
			answerFormat(`{"top": [{"id": "<customer id>", "units": <integer>}, ...3 entries]}`),
		Expected: map[string]any{"top": rows},
	}
}

// taskOverdueEnterpriseUsage joins all three sources.
func taskOverdueEnterpriseUsage(d *Data) Task {
	overdue := map[string]int{}
	for _, i := range d.Invoices {
		c, _ := d.Customer(i.CustomerID)
		if c.Plan == "enterprise" && i.Status == "overdue" {
			overdue[i.CustomerID]++
		}
	}
	units := map[string]int{}
	for id := range overdue {
		units[id] = 0
	}
	for _, e := range d.Usage {
		if _, ok := overdue[e.CustomerID]; ok && e.Day <= "2026-03-31" {
			units[e.CustomerID] += e.Units
		}
	}
	top := topN(units, 1)[0]
	return Task{
		ID:    "overdue_enterprise_usage",
		Shape: "three-source join (CRM, billing, usage), argmax",
		Prompt: "Among enterprise-plan customers that have at least one overdue invoice, which one had the most total usage units in Q1 2026 (2026-01-01 to 2026-03-31)? " +
			"Give its id, its Q1 usage units, and how many overdue invoices it has. Break ties by customer id ascending." +
			answerFormat(`{"id": "<customer id>", "units": <integer>, "overdue_invoices": <integer>}`),
		Expected: map[string]any{"id": top.key, "units": top.val, "overdue_invoices": overdue[top.key]},
	}
}

type kv struct {
	key string
	val int
}

// topN orders by value descending, then key ascending — the tie-break
// every prompt states, so a correct answer is unique.
func topN(m map[string]int, n int) []kv {
	out := make([]kv, 0, len(m))
	for k, v := range m {
		out = append(out, kv{k, v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].val != out[j].val {
			return out[i].val > out[j].val
		}
		return out[i].key < out[j].key
	})
	if len(out) > n {
		out = out[:n]
	}
	if len(out) == 0 {
		panic(fmt.Sprintf("bench: empty top-%d; the fixture sizes no longer support this task", n))
	}
	return out
}
