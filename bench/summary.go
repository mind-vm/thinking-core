package bench

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// WriteSummary renders the Markdown report: one table per model with a
// row per agent, then a per-task breakdown. Medians, not means — one
// baseline run that pages through everything twice would otherwise
// dominate the token column and hide the typical case. Every run counts
// toward the medians, failed ones included: a run that burned 80k tokens
// and answered wrong still cost 80k tokens.
func WriteSummary(w io.Writer, title string, results []Result) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", title)
	if len(results) == 0 {
		b.WriteString("No results.\n")
		_, err := io.WriteString(w, b.String())
		return err
	}
	fmt.Fprintf(&b, "Seed %d · %d runs. Tokens are as reported by the endpoint's usage fields. Medians over all runs, failed ones included.\n\n", results[0].Seed, len(results))

	models := uniq(results, func(r Result) string { return r.Model })
	agents := uniq(results, func(r Result) string { return r.Agent })
	tasks := uniq(results, func(r Result) string { return r.Task })

	for _, m := range models {
		fmt.Fprintf(&b, "## %s\n\n", m)
		b.WriteString("| agent | correct | accuracy | median prompt tok | median completion tok | median turns | median tool calls | median wall s | errors |\n")
		b.WriteString("|---|---:|---:|---:|---:|---:|---:|---:|---:|\n")
		for _, a := range agents {
			rs := filter(results, func(r Result) bool { return r.Model == m && r.Agent == a })
			if len(rs) == 0 {
				continue
			}
			s := stats(rs)
			fmt.Fprintf(&b, "| %s | %d/%d | %.0f%% | %d | %d | %s | %s | %.1f | %d |\n",
				a, s.correct, s.n, 100*float64(s.correct)/float64(s.n), int(s.prompt), int(s.completion), num(s.turns), num(s.tools), s.wall/1000, s.errors)
		}
		b.WriteString("\n### Per task\n\n")
		b.WriteString("| task | agent | correct | median prompt tok | median turns |\n")
		b.WriteString("|---|---|---:|---:|---:|\n")
		for _, t := range tasks {
			for _, a := range agents {
				rs := filter(results, func(r Result) bool { return r.Model == m && r.Agent == a && r.Task == t })
				if len(rs) == 0 {
					continue
				}
				s := stats(rs)
				fmt.Fprintf(&b, "| %s | %s | %d/%d | %d | %s |\n", t, a, s.correct, s.n, int(s.prompt), num(s.turns))
			}
		}
		b.WriteString("\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}

type summaryStats struct {
	n, correct, errors                     int
	prompt, completion, turns, tools, wall float64
}

func stats(rs []Result) summaryStats {
	s := summaryStats{n: len(rs)}
	var p, c, t, tc, w []float64
	for _, r := range rs {
		if r.Correct {
			s.correct++
		}
		if r.Error != "" {
			s.errors++
		}
		p = append(p, float64(r.PromptTokens))
		c = append(c, float64(r.CompletionTokens))
		t = append(t, float64(r.Turns))
		tc = append(tc, float64(r.ToolCalls))
		w = append(w, float64(r.WallMS))
	}
	s.prompt, s.completion, s.turns, s.tools, s.wall = median(p), median(c), median(t), median(tc), median(w)
	return s
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sort.Float64s(xs)
	n := len(xs)
	if n%2 == 1 {
		return xs[n/2]
	}
	return (xs[n/2-1] + xs[n/2]) / 2
}

// num prints a median that is a whole number without a decimal point.
func num(f float64) string {
	if f == float64(int(f)) {
		return fmt.Sprint(int(f))
	}
	return fmt.Sprintf("%.1f", f)
}

func uniq(rs []Result, key func(Result) string) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range rs {
		if k := key(r); !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

func filter(rs []Result, keep func(Result) bool) []Result {
	var out []Result
	for _, r := range rs {
		if keep(r) {
			out = append(out, r)
		}
	}
	return out
}
