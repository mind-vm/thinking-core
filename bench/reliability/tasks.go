package reliability

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Task is one probe of the mechanics. Messages holds the user turns of one
// session: most tasks have one, the follow-up task two. Check sees what the
// run left behind and says whether it passed and, when not, why.
type Task struct {
	ID        string
	Probes    string // the one property this task tests, for the report
	Messages  []string
	Functions []Function
	Check     func(o *Outcome) (bool, string)
}

// Outcome is what a run left behind, whatever the arm.
type Outcome struct {
	Answers []string // final answer per user message; "" when none came
	Calls   []Call
	Tickets []Ticket
}

func (o *Outcome) last() string {
	if len(o.Answers) == 0 {
		return ""
	}
	return o.Answers[len(o.Answers)-1]
}

func (o *Outcome) called(name string) int {
	n := 0
	for _, c := range o.Calls {
		if c.Name == name && c.Error == "" {
			n++
		}
	}
	return n
}

var core = []Function{fnFindCustomer, fnListOrders, fnGetOrder, fnGetStock, fnCreateTicket}

func Tasks() []Task {
	hoff := findCustomer("Hoffmann GmbH")
	weber := findCustomer("Weber KG")
	sum := func(os []orderSummary) float64 {
		t := 0
		for _, o := range os {
			t += o.AmountCents
		}
		return float64(t) / 100
	}
	openN := len(ordersOf(hoff.ID, "open", ""))
	paidAll := sum(ordersOf(hoff.ID, "paid", ""))
	march, may := sum(ordersOf(hoff.ID, "paid", "2025-03")), sum(ordersOf(hoff.ID, "paid", "2025-05"))
	histN, histSum := 0, 0
	for _, o := range orders {
		if o.Status == "paid" && strings.HasPrefix(o.Date, "2025-03-") {
			histN++
			histSum += o.AmountCents
		}
	}

	return []Task{
		{
			ID: "01-lookup", Probes: "one call, then finish",
			Messages:  []string{"In which city is the customer Hoffmann GmbH based? Answer with the city only."},
			Functions: core,
			Check: func(o *Outcome) (bool, string) {
				return contains(o.last(), hoff.City), "city " + hoff.City
			},
		},
		{
			ID: "02-chain", Probes: "a call that needs the previous call's result",
			Messages:  []string{"How many open orders does Hoffmann GmbH have? Answer with the number only."},
			Functions: core,
			Check: func(o *Outcome) (bool, string) {
				return hasNumber(o.last(), float64(openN), 0), fmt.Sprint(openN, " open orders")
			},
		},
		{
			ID: "03-sum", Probes: "computing over a list instead of guessing",
			Messages:  []string{"What is the total of all paid orders of Hoffmann GmbH, in euros, to the cent?"},
			Functions: core,
			Check: func(o *Outcome) (bool, string) {
				return hasNumber(o.last(), paidAll, 0.005), fmt.Sprintf("%.2f EUR", paidAll)
			},
		},
		{
			ID: "04-carry", Probes: "data kept across turns, read once",
			Messages: []string{"First load the shop's order history with loadOrderHistory(); it may be called only once. " +
				"Then get the question about that history with getQuestion() and answer it."},
			Functions: []Function{fnLoadHistory, fnGetQuestion},
			Check: func(o *Outcome) (bool, string) {
				want := fmt.Sprintf("%d orders, %.2f EUR", histN, float64(histSum)/100)
				return hasNumber(o.last(), float64(histN), 0) && hasNumber(o.last(), float64(histSum)/100, 0.005), want
			},
		},
		{
			ID: "05-recover", Probes: "a failing call retried, the run goes on",
			Messages:  []string{"How many units of the product LMP-200 are in stock? Answer with the number only."},
			Functions: core,
			Check: func(o *Outcome) (bool, string) {
				return hasNumber(o.last(), float64(stock["LMP-200"]), 0), fmt.Sprint(stock["LMP-200"], " units")
			},
		},
		{
			ID: "06-args", Probes: "an object argument with an enum and an array",
			Messages: []string{"Open a high-priority support ticket for Hoffmann GmbH with the subject \"Invoice 2025-03 missing\" " +
				"and the tags billing and urgent. Answer with the ticket id."},
			Functions: core,
			Check: func(o *Outcome) (bool, string) {
				want := "one ticket C-007/high/\"Invoice 2025-03 missing\"/[billing urgent], its id in the answer"
				if len(o.Tickets) != 1 {
					return false, want
				}
				t := o.Tickets[0]
				tags := slices.Clone(t.Tags)
				slices.Sort(tags)
				ok := t.CustomerID == hoff.ID && t.Priority == "high" && t.Subject == "Invoice 2025-03 missing" &&
					slices.Equal(tags, []string{"billing", "urgent"}) && contains(o.last(), t.ID)
				return ok, want
			},
		},
		{
			ID: "07-followup", Probes: "a second question in the same session",
			Messages: []string{
				"What was the total of the paid orders of Hoffmann GmbH in March 2025, in euros?",
				"And in May 2025?",
			},
			Functions: core,
			Check: func(o *Outcome) (bool, string) {
				want := fmt.Sprintf("%.2f then %.2f EUR", march, may)
				return len(o.Answers) == 2 && hasNumber(o.Answers[0], march, 0.005) && hasNumber(o.Answers[1], may, 0.005), want
			},
		},
		{
			ID: "08-notool", Probes: "answering without touching the API",
			Messages:  []string{"What is 17 times 23? Answer with the number only."},
			Functions: core,
			Check: func(o *Outcome) (bool, string) {
				return hasNumber(o.last(), 391, 0) && len(o.Calls) == 0, "391, no calls"
			},
		},
		{
			ID: "09-select", Probes: "the right one of 20 functions",
			Messages:  []string{"Where is order O-1042 right now, and when will it arrive? Answer with the status and the date."},
			Functions: append([]Function{fnShipping, fnFindCustomer, fnListOrders, fnGetOrder, fnGetStock}, decoys()...),
			Check: func(o *Outcome) (bool, string) {
				a := strings.ToLower(normalizeText(o.last()))
				status := strings.Contains(a, "in_transit") || strings.Contains(a, "in transit") || strings.Contains(a, "unterwegs")
				date := strings.Contains(a, "2025-06-14") || strings.Contains(a, "14.06.2025") || strings.Contains(a, "june 14") || strings.Contains(a, "14 june")
				return status && date && o.called("getShippingStatus") > 0, "in_transit, 2025-06-14"
			},
		},
		{
			ID: "10-once", Probes: "an effect done exactly once, then stop",
			Messages:  []string{"Create a normal-priority support ticket for Weber KG with the subject \"Address change\" and tell me its id."},
			Functions: core,
			Check: func(o *Outcome) (bool, string) {
				ok := len(o.Tickets) == 1 && o.Tickets[0].CustomerID == weber.ID && o.Tickets[0].Priority == "normal" && contains(o.last(), o.Tickets[0].ID)
				return ok, "exactly one ticket for C-012, its id in the answer"
			},
		},
	}
}

func contains(answer, want string) bool {
	return strings.Contains(strings.ToLower(normalizeText(answer)), strings.ToLower(want))
}

var (
	dashes   = strings.NewReplacer("\u2010", "-", "\u2011", "-", "\u2012", "-", "\u2013", "-", "\u2014", "-", "\u2212", "-")
	digitGap = regexp.MustCompile(`(\d)[\x{00a0}\x{202f}\x{2009} ](\d{3})\b`)
)

// normalizeText undoes typography a model adds and a reader would not
// notice: non-breaking hyphens in dates, thin spaces grouping digits.
func normalizeText(s string) string {
	s = dashes.Replace(s)
	for {
		t := digitGap.ReplaceAllString(s, "$1$2")
		if t == s {
			break
		}
		s = t
	}
	return strings.NewReplacer("\u00a0", " ", "\u202f", " ", "\u2009", " ").Replace(s)
}

var numberToken = regexp.MustCompile(`\d[\d.,']*\d|\d`)

// hasNumber reports whether answer states want, within tol, in any of the
// ways a model writes numbers: 1234.5, 1,234.50, 1.234,50 or 1'234.50.
func hasNumber(answer string, want, tol float64) bool {
	for _, tok := range numberToken.FindAllString(normalizeText(answer), -1) {
		for _, v := range readings(tok) {
			if math.Abs(v-want) <= tol+1e-9 {
				return true
			}
		}
	}
	return false
}

// readings is every value a number token can mean: with "." or "," as
// decimal separator, and the other as grouping.
func readings(tok string) []float64 {
	tok = strings.ReplaceAll(tok, "'", "")
	var out []float64
	for _, dec := range []string{".", ","} {
		group := map[string]string{".": ",", ",": "."}[dec]
		s := strings.ReplaceAll(tok, group, "")
		if strings.Count(s, dec) > 1 {
			continue
		}
		s = strings.Replace(s, dec, ".", 1)
		if v, err := strconv.ParseFloat(s, 64); err == nil {
			out = append(out, v)
		}
	}
	return out
}
