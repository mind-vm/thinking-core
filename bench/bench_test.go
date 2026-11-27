package bench

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestGenerateIsDeterministic(t *testing.T) {
	a, b := Generate(7), Generate(7)
	if !reflect.DeepEqual(a.Customers, b.Customers) || !reflect.DeepEqual(a.Invoices, b.Invoices) || !reflect.DeepEqual(a.Usage, b.Usage) {
		t.Fatal("same seed produced different fixture data")
	}
	ta, tb := Tasks(a), Tasks(b)
	if !reflect.DeepEqual(ta, tb) {
		t.Fatal("same seed produced different tasks or answers")
	}
	if reflect.DeepEqual(Generate(8).Invoices, a.Invoices) {
		t.Fatal("a different seed produced the same invoices")
	}
}

// TestSeedOneIsPinned guards against an innocent-looking change to the
// generator (a new field drawn from the RNG, a reordered loop) silently
// changing every answer, which would make results from before and after
// it incomparable without anyone noticing.
func TestSeedOneIsPinned(t *testing.T) {
	d := Generate(1)
	t.Logf("seed 1: %d customers, %d invoices, %d usage events", len(d.Customers), len(d.Invoices), len(d.Usage))
	if len(d.Customers) != numCustomers || len(d.Usage) != numUsage {
		t.Fatalf("sizes: %d customers, %d usage events", len(d.Customers), len(d.Usage))
	}
	got := map[string]string{}
	for _, task := range Tasks(d) {
		got[task.ID] = brief(task.Expected)
	}
	want := map[string]string{
		"lookup":     `{"country":"FR","plan":"free"}`,
		"overdue_de": `{"invoice_count":29,"total_cents":2671898}`,
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("seed 1 %s = %s, want %s — if the generator changed on purpose, update this and say so in the README", id, got[id], w)
		}
	}
}

func TestTaskShapes(t *testing.T) {
	d := Generate(1)
	tasks := Tasks(d)
	if len(tasks) < 6 || len(tasks) > 10 {
		t.Fatalf("%d tasks, want 6–10", len(tasks))
	}
	seen := map[string]bool{}
	for _, task := range tasks {
		if seen[task.ID] {
			t.Errorf("duplicate task id %s", task.ID)
		}
		seen[task.ID] = true
		if !strings.Contains(task.Prompt, "JSON object") {
			t.Errorf("%s: prompt does not state the answer format", task.ID)
		}
		// The expected value, serialised, must pass its own checker.
		if ok, why := task.Check(brief0(task.Expected)); !ok {
			t.Errorf("%s: expected answer fails its own check: %s", task.ID, why)
		}
	}
	// The pagination tasks really need more than one page.
	pro, _ := listCustomers(d, args{"plan": "pro"})
	if pages := pro.(map[string]any)["total_pages"].(int); pages < 2 {
		t.Errorf("pro customers fit on %d page; pro_signups_2025 no longer exercises pagination", pages)
	}
}

// TestReferenceAgreesWithTools answers two tasks the way an agent must —
// through the tools, page by page — and compares with the reference
// solution, which reads the slices directly. A disagreement means a tool
// filters or paginates differently from what the task assumes, and every
// agent would be marked wrong for being right.
func TestReferenceAgreesWithTools(t *testing.T) {
	d := Generate(3)
	tools := map[string]Tool{}
	for _, tl := range Tools() {
		tools[tl.Name] = tl
	}
	all := func(name, key string, a map[string]any) []any {
		var rows []any
		for page := 1; ; page++ {
			a["page"] = float64(page)
			v, err := tools[name].Call(d, a)
			if err != nil {
				t.Fatal(err)
			}
			m := v.(map[string]any)
			rows = append(rows, m[key].([]any)...)
			if float64(page) >= m["total_pages"].(float64) {
				return rows
			}
		}
	}

	de := map[string]bool{}
	for _, c := range all("list_customers", "customers", map[string]any{"country": "DE"}) {
		de[c.(map[string]any)["id"].(string)] = true
	}
	n, sum := 0, 0
	for _, in := range all("list_invoices", "invoices", map[string]any{"status": "overdue"}) {
		m := in.(map[string]any)
		if de[m["customer_id"].(string)] {
			n++
			sum += int(m["amount_cents"].(float64))
		}
	}
	task, _ := TaskByID(Tasks(d), "overdue_de")
	if ok, why := task.Check(brief0(map[string]any{"invoice_count": n, "total_cents": sum})); !ok {
		t.Errorf("overdue_de via tools disagrees with the reference: %s", why)
	}

	count := 0
	for _, c := range all("list_customers", "customers", map[string]any{"plan": "pro"}) {
		if strings.HasPrefix(c.(map[string]any)["signup_date"].(string), "2025-") {
			count++
		}
	}
	task, _ = TaskByID(Tasks(d), "pro_signups_2025")
	if ok, why := task.Check(brief0(map[string]any{"count": count})); !ok {
		t.Errorf("pro_signups_2025 via tools disagrees with the reference: %s", why)
	}
}

func TestToolErrors(t *testing.T) {
	d := Generate(1)
	tools := Tools()
	if _, err := tools[0].Call(d, map[string]any{"date": "2026"}); err == nil || !strings.Contains(err.Error(), "accepted: country, page, plan") {
		t.Errorf("unknown argument: %v", err)
	}
	if _, err := tools[1].Call(d, map[string]any{"id": "nope"}); err == nil {
		t.Error("unknown customer id did not fail")
	}
	if _, err := tools[2].Call(d, map[string]any{"page": 0.5}); err == nil {
		t.Error("fractional page did not fail")
	}
}

func TestCheck(t *testing.T) {
	task := Task{Expected: map[string]any{
		"name": "Bilal Khan",
		"n":    42,
		"top":  []any{map[string]any{"id": "C1", "u": 3}, map[string]any{"id": "C2", "u": 2}},
	}}
	cases := []struct {
		name, answer string
		ok           bool
	}{
		{"exact", `{"name":"Bilal Khan","n":42,"top":[{"id":"C1","u":3},{"id":"C2","u":2}]}`, true},
		{"fenced after prose", "Here you go:\n```json\n{\"name\":\"bilal khan \",\"n\":42.0,\"top\":[{\"id\":\"C1\",\"u\":3},{\"id\":\"C2\",\"u\":2}]}\n```", true},
		{"prose around", `The answer is {"name":"Bilal Khan","n":"42","extra":1,"top":[{"id":"C1","u":3},{"id":"C2","u":2}]} — done.`, true},
		{"wrong number", `{"name":"Bilal Khan","n":41,"top":[{"id":"C1","u":3},{"id":"C2","u":2}]}`, false},
		{"wrong order", `{"name":"Bilal Khan","n":42,"top":[{"id":"C2","u":2},{"id":"C1","u":3}]}`, false},
		{"too few", `{"name":"Bilal Khan","n":42,"top":[{"id":"C1","u":3}]}`, false},
		{"missing key", `{"n":42,"top":[{"id":"C1","u":3},{"id":"C2","u":2}]}`, false},
		{"not json", `Bilal Khan, 42`, false},
		{"empty", ``, false},
	}
	for _, c := range cases {
		ok, why := task.Check(c.answer)
		if ok != c.ok {
			t.Errorf("%s: ok=%v (%s), want %v", c.name, ok, why, c.ok)
		}
		if !ok && why == "" {
			t.Errorf("%s: a failed check gave no reason", c.name)
		}
	}
}

func TestSummary(t *testing.T) {
	rs := []Result{
		{Task: "a", Agent: "x", Model: "m", Seed: 1, Correct: true, Turns: 2, PromptTokens: 100, WallMS: 1000},
		{Task: "a", Agent: "x", Model: "m", Seed: 1, Correct: false, Turns: 4, PromptTokens: 300, WallMS: 3000, Error: "boom"},
		{Task: "a", Agent: "y", Model: "m", Seed: 1, Correct: true, Turns: 5, PromptTokens: 900, WallMS: 2000},
	}
	var b strings.Builder
	if err := WriteSummary(&b, "T", rs); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"# T", "## m",
		"| x | 1/2 | 50% | 200 | 0 | 3 | 0 | 2.0 | 1 |",
		"| y | 1/1 | 100% | 900 | 0 | 5 | 0 | 2.0 | 0 |",
		"| a | x | 1/2 | 200 | 3 |",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary lacks %q:\n%s", want, out)
		}
	}
}

// brief0 serialises an answer in full, for feeding it back to Check.
func brief0(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
