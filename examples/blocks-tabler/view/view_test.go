package view

import (
	"context"
	"encoding/json"
	"html"
	"regexp"
	"strings"
	"testing"

	"github.com/mind-vm/thinking-core/sandbox"
)

func render(t *testing.T, b Block) string {
	t.Helper()
	out, err := Render(context.Background(), BlockView(b))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// chartConfig pulls the ECharts option out of a rendered chart block —
// the thing ECharts will actually draw, which is what has to be right.
func chartConfig(t *testing.T, out string) map[string]any {
	t.Helper()
	m := regexp.MustCompile(`data-tabler-chart="([^"]*)"`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no chart in %s", out)
	}
	var cfg struct {
		Option map[string]any `json:"option"`
	}
	if err := json.Unmarshal([]byte(html.UnescapeString(m[1])), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Option == nil {
		t.Fatalf("chart carries no option: %s", m[1])
	}
	return cfg.Option
}

// at walks a decoded JSON value by map keys and list indexes.
func at(t *testing.T, v any, path ...any) any {
	t.Helper()
	for _, k := range path {
		switch k := k.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				t.Fatalf("%v: not an object at %q", path, k)
			}
			v = m[k]
		case int:
			l, ok := v.([]any)
			if !ok || k >= len(l) {
				t.Fatalf("%v: no element %d", path, k)
			}
			v = l[k]
		}
	}
	return v
}

func payload(t *testing.T, js string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(js), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// A fragment must not carry the chart start-up script: the page loaded it
// once, and a copy per streamed chart is kilobytes of dead text.
func TestFragmentOmitsChartScript(t *testing.T) {
	out := render(t, Block{Type: "chart", DOMID: "b", ChartID: "c1", Payload: payload(t, `{"variant":"bar","x":"m","series":["v"],"data":[{"m":"Jan","v":1}]}`)})
	if strings.Contains(out, "<script") {
		t.Fatalf("a chart fragment carries a script:\n%s", out)
	}
	if !strings.Contains(out, `id="chart-c1"`) {
		t.Fatalf("chart element not named by ChartID:\n%s", out)
	}
}

func TestBarChartSeriesAndCategories(t *testing.T) {
	cfg := chartConfig(t, render(t, Block{Type: "chart", ChartID: "c", Payload: payload(t,
		`{"variant":"bar","x":"month","series":["revenue","cost"],"data":[{"month":"Jan","revenue":10,"cost":"4"},{"month":"Feb","revenue":12.5,"cost":5}]}`)}))
	cats := at(t, cfg, "xAxis", 0, "data").([]any)
	if len(cats) != 2 || cats[0] != "Jan" {
		t.Fatalf("categories = %v", cats)
	}
	series := cfg["series"].([]any)
	if len(series) != 2 || at(t, series, 0, "type") != "bar" {
		t.Fatalf("series = %v", series)
	}
	// A numeric string is a number: CSV parsed in a script stays text.
	if d := at(t, series, 1, "data").([]any); d[0] != 4.0 || d[1] != 5.0 {
		t.Fatalf("cost data = %v", d)
	}
}

// Lines are straight: a spline would draw values between the points that
// no row holds.
func TestLineIsStraight(t *testing.T) {
	cfg := chartConfig(t, render(t, Block{Type: "chart", ChartID: "c", Payload: payload(t,
		`{"variant":"line","x":"m","series":["v"],"data":[{"m":"Jan","v":1},{"m":"Feb","v":3}]}`)}))
	if s := at(t, cfg, "series", 0).(map[string]any); s["type"] != "line" || s["smooth"] != nil {
		t.Fatalf("serie = %v", s)
	}
}

func TestDonutIsOneSlicePerRow(t *testing.T) {
	cfg := chartConfig(t, render(t, Block{Type: "chart", ChartID: "c", Payload: payload(t,
		`{"variant":"donut","x":"product","series":["revenue"],"data":[{"product":"A","revenue":3},{"product":"B","revenue":7}]}`)}))
	pie := at(t, cfg, "series", 0).(map[string]any)
	// A donut is a pie with an inner radius.
	if pie["type"] != "pie" || at(t, pie, "radius", 0) == "0%" {
		t.Fatalf("serie = %v", pie)
	}
	slices := pie["data"].([]any)
	if len(slices) != 2 || at(t, slices, 1, "name") != "B" || at(t, slices, 1, "value") != 7.0 {
		t.Fatalf("slices = %v", slices)
	}
}

func TestCompositeTypesEachSerie(t *testing.T) {
	cfg := chartConfig(t, render(t, Block{Type: "chart", ChartID: "c", Payload: payload(t,
		`{"variant":"composite","x":"m","series":["orders","revenue"],"types":{"revenue":"line"},"data":[{"m":"Jan","orders":3,"revenue":10}]}`)}))
	if at(t, cfg, "series", 0, "type") != "bar" || at(t, cfg, "series", 1, "type") != "line" {
		t.Fatalf("series = %v", cfg["series"])
	}
}

func TestScatterDropsPointsWithoutCoordinates(t *testing.T) {
	cfg := chartConfig(t, render(t, Block{Type: "chart", ChartID: "c", Payload: payload(t,
		`{"variant":"scatter","x":"a","series":["b"],"data":[{"a":1,"b":2},{"a":"n/a","b":3},{"a":4,"b":5}]}`)}))
	pts := at(t, cfg, "series", 0, "data").([]any)
	if len(pts) != 2 {
		t.Fatalf("points = %v, want the two with both coordinates", pts)
	}
	// x is a value, not an index into categories that do not exist.
	if typ := at(t, cfg, "xAxis", 0, "type"); typ != "value" {
		t.Fatalf("x axis type = %v", typ)
	}
}

func TestHeatmapIsWeeksByWeekday(t *testing.T) {
	// 2026-01-05 is a Monday, 2026-01-14 a Wednesday of the next week.
	cfg := chartConfig(t, render(t, Block{Type: "chart", ChartID: "c", Payload: payload(t,
		`{"variant":"heatmap","x":"day","series":["n"],"data":[{"day":"2026-01-05","n":2},{"day":"2026-01-14","n":5}]}`)}))
	days := at(t, cfg, "yAxis", 0, "data").([]any)
	// The y axis runs bottom up, so Monday last is Monday on top.
	if len(days) != 7 || days[0] != "Sun" || days[6] != "Mon" {
		t.Fatalf("days = %v, want one per weekday, Monday last", days)
	}
	if weeks := at(t, cfg, "xAxis", 0, "data").([]any); len(weeks) != 2 || weeks[1] != "2026-01-12" {
		t.Fatalf("weeks = %v", weeks)
	}
	// Cells are [week, weekday, value]; a day without a row is "-".
	cells := map[[2]float64]any{}
	for _, c := range at(t, cfg, "series", 0, "data").([]any) {
		c := c.([]any)
		cells[[2]float64{c[0].(float64), c[1].(float64)}] = c[2]
	}
	mon, wed := 6.0, 4.0
	if cells[[2]float64{0, mon}] != 2.0 || cells[[2]float64{1, mon}] != "-" {
		t.Fatalf("Mon = %v, %v", cells[[2]float64{0, mon}], cells[[2]float64{1, mon}])
	}
	if cells[[2]float64{1, wed}] != 5.0 {
		t.Fatalf("Wed of week 2 = %v", cells[[2]float64{1, wed}])
	}
}

// An unknown variant still draws its numbers.
func TestUnknownVariantDrawsBars(t *testing.T) {
	cfg := chartConfig(t, render(t, Block{Type: "chart", ChartID: "c", Payload: payload(t,
		`{"variant":"treemapish","x":"k","series":["v"],"data":[{"k":"a","v":1}]}`)}))
	if typ := at(t, cfg, "series", 0, "type"); typ != "bar" {
		t.Fatalf("type = %v", typ)
	}
}

func TestTableCellsAndTruncation(t *testing.T) {
	out := render(t, Block{Type: "table", DOMID: "block-t", Payload: payload(t,
		`{"title":"Overdue","columns":["order","amount"],"rows":[{"order":"A-1","amount":1240.5},{"order":"<b>x</b>","amount":3}],"truncated":true,"totalRows":812}`)})
	for _, want := range []string{`id="block-t"`, "Overdue", "1240.5", "&lt;b&gt;x&lt;/b&gt;", "Showing 2 of 812 rows.", `class="text-end font-monospace"`} {
		if !strings.Contains(out, want) {
			t.Errorf("table lacks %q:\n%s", want, out)
		}
	}
}

func TestNumericStringsAlignAsNumbers(t *testing.T) {
	rs := rows(payload(t, `{"rows":[{"a":"2210.00","b":"West"},{"a":"75.25","b":"North"}]}`), "rows")
	if !numeric(rs, "a") || numeric(rs, "b") {
		t.Fatalf("numeric(a)=%v numeric(b)=%v", numeric(rs, "a"), numeric(rs, "b"))
	}
}

func TestStatsShowsUnitTrendAndHint(t *testing.T) {
	out := render(t, Block{Type: "stats", Payload: payload(t,
		`{"items":[{"label":"Revenue","value":"48,200","unit":"EUR","delta":0.12,"hint":"vs. H2 2025"},{"label":"Orders","value":"24","delta":-0.05}]}`)})
	for _, want := range []string{"Revenue", "48,200 EUR", "12", "ti-arrow-up", "ti-arrow-down", "vs. H2 2025", "col-lg-6"} {
		if !strings.Contains(out, want) {
			t.Errorf("stats lack %q:\n%s", want, out)
		}
	}
}

// The model's markdown reaches the page unescaped, so the renderer must be
// the one that removes what a page must not run.
func TestMarkdownDropsRawHTMLAndScriptLinks(t *testing.T) {
	out := render(t, Block{Type: "markdown", Payload: payload(t,
		`{"markdown":"**bold** <script>alert(1)</script> [x](javascript:alert(1))\n\n| a | b |\n|---|---|\n| 1 | 2 |"}`)})
	if strings.Contains(out, "<script>") || strings.Contains(out, "javascript:") {
		t.Fatalf("markdown let active content through:\n%s", out)
	}
	if !strings.Contains(out, "<strong>bold</strong>") || !strings.Contains(out, "<table>") {
		t.Fatalf("markdown not rendered:\n%s", out)
	}
}

func TestUnknownBlockShowsItsJSON(t *testing.T) {
	out := render(t, Block{Type: "processMap", Payload: payload(t, `{"nodes":[1,2]}`)})
	if !strings.Contains(out, "Unrecognised block: processMap") || !strings.Contains(out, "&#34;nodes&#34;") {
		t.Fatalf("unknown block:\n%s", out)
	}
}

func TestRevisedBadge(t *testing.T) {
	if out := render(t, Block{Type: "markdown", Revised: true, Payload: payload(t, `{"markdown":"x"}`)}); !strings.Contains(out, "revised") {
		t.Fatalf("no revised badge:\n%s", out)
	}
}

// goja hands the pack int64s and typed slices; the renderer sees JSON's
// shapes whichever arrived.
func TestNormalizeFlattensGojaShapes(t *testing.T) {
	p := Normalize(map[string]any{"rows": []map[string]any{{"n": int64(3)}}, "columns": []string{"n"}})
	rs := rows(p, "rows")
	if len(rs) != 1 || rs[0]["n"] != 3.0 || len(strs(p, "columns")) != 1 {
		t.Fatalf("normalised = %#v", p)
	}
}

func TestAskFromInProcessQuestions(t *testing.T) {
	r, err := AskFromEvent("Pick", map[string]any{
		"requestId": "r1", "inputType": "questions",
		"questions": []AskQuestion{{ID: "period", Header: "Period", Question: "Which?", Options: []sandbox.QuestionOption{{Label: "Q1"}, {Label: "Q2"}}, AllowOther: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := Render(context.Background(), Ask(r))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`data-ask="r1"`, `data-question="period"`, `value="Q2"`, `data-other="true"`, `name="other-period"`, "Pick"} {
		if !strings.Contains(out, want) {
			t.Errorf("ask form lacks %q:\n%s", want, out)
		}
	}
	ans, _ := Render(context.Background(), Answered(r, map[string]any{"period": "Q2"}))
	if !strings.Contains(ans, "Period:") || !strings.Contains(ans, "Q2") || !strings.Contains(ans, `id="ask-r1"`) {
		t.Fatalf("answered:\n%s", ans)
	}
}
