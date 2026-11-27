package reliability

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
)

// Report renders results as the markdown tables of the write-up. Runs that
// ended in an infrastructure error (the gateway or host failing a call)
// say nothing about the arm, so they are counted apart and left out of
// every rate. A run that used up its time is the arm's failure: it was
// still working.
func Report(results []Result, models []Model) string {
	var b strings.Builder
	arms := present(results, func(r Result) string { return r.Arm }, AllArms)
	var modelOrder []string
	for _, m := range models {
		modelOrder = append(modelOrder, m.Name)
	}
	mods := present(results, func(r Result) string { return r.Model }, modelOrder)
	tasks := present(results, func(r Result) string { return r.Task }, nil)

	valid, infra := []Result{}, map[string]int{}
	for _, r := range results {
		if r.Status == "llm_error" {
			infra[r.Arm]++
			continue
		}
		valid = append(valid, r)
	}

	fmt.Fprintf(&b, "%d runs, %d of them infrastructure errors (left out below).\n\n", len(results), len(results)-len(valid))

	b.WriteString("### Pass rate by arm and model\n\n")
	b.WriteString("| arm | " + strings.Join(mods, " | ") + " | all models (95% CI) |\n")
	b.WriteString("|---" + strings.Repeat("|---:", len(mods)+1) + "|\n")
	for _, a := range arms {
		row := []string{a}
		for _, m := range mods {
			k, n := passes(valid, func(r Result) bool { return r.Arm == a && r.Model == m })
			row = append(row, pct(k, n))
		}
		k, n := passes(valid, func(r Result) bool { return r.Arm == a })
		lo, hi := wilson(k, n)
		row = append(row, fmt.Sprintf("**%s** (%.0f–%.0f)", pct(k, n), lo*100, hi*100))
		b.WriteString("| " + strings.Join(row, " | ") + " |\n")
	}

	b.WriteString("\n### Pass rate by task and arm (all models)\n\n")
	b.WriteString("| task | probes | " + strings.Join(arms, " | ") + " |\n")
	b.WriteString("|---|---" + strings.Repeat("|---:", len(arms)) + "|\n")
	probes := map[string]string{}
	for _, t := range Tasks() {
		probes[t.ID] = t.Probes
	}
	for _, t := range tasks {
		row := []string{t, probes[t]}
		for _, a := range arms {
			k, n := passes(valid, func(r Result) bool { return r.Arm == a && r.Task == t })
			row = append(row, pct(k, n))
		}
		b.WriteString("| " + strings.Join(row, " | ") + " |\n")
	}

	b.WriteString("\n### Paired against C-state (same model, task and repetition)\n\n")
	b.WriteString("Discordant pairs only: *C only* is pairs C passed and the other arm failed. p is McNemar's exact two-sided test.\n\n")
	b.WriteString("| arm | pairs | C only | arm only | p |\n|---|---:|---:|---:|---:|\n")
	base := index(valid, ArmState)
	for _, a := range arms {
		if a == ArmState {
			continue
		}
		pairs, conly, aonly := 0, 0, 0
		for k, r := range index(valid, a) {
			c, ok := base[k]
			if !ok {
				continue
			}
			pairs++
			switch {
			case c.Pass && !r.Pass:
				conly++
			case !c.Pass && r.Pass:
				aonly++
			}
		}
		fmt.Fprintf(&b, "| %s | %d | %d | %d | %s |\n", a, pairs, conly, aonly, pval(conly, aonly))
	}

	b.WriteString("\n### Why runs failed\n\n")
	classes := present(valid, func(r Result) string { return r.Failure }, nil)
	classes = slices.DeleteFunc(classes, func(s string) bool { return s == "" })
	b.WriteString("| failure | " + strings.Join(arms, " | ") + " |\n")
	b.WriteString("|---" + strings.Repeat("|---:", len(arms)) + "|\n")
	for _, c := range classes {
		row := []string{c}
		for _, a := range arms {
			n := 0
			for _, r := range valid {
				if r.Arm == a && r.Failure == c {
					n++
				}
			}
			row = append(row, fmt.Sprint(n))
		}
		b.WriteString("| " + strings.Join(row, " | ") + " |\n")
	}
	row := []string{"*infrastructure*"}
	for _, a := range arms {
		row = append(row, fmt.Sprint(infra[a]))
	}
	b.WriteString("| " + strings.Join(row, " | ") + " |\n")

	b.WriteString("\n### Cost and effort per run (means)\n\n")
	b.WriteString("| arm | turns | first reply valid (without 05) | script/tool errors | prompt tok | completion tok | $ per run | $ per pass |\n")
	b.WriteString("|---|---:|---:|---:|---:|---:|---:|---:|\n")
	for _, a := range arms {
		var rs []Result
		for _, r := range valid {
			if r.Arm == a {
				rs = append(rs, r)
			}
		}
		if len(rs) == 0 {
			continue
		}
		var turns, errs, pt, ct, cost float64
		fv, fvn, pass := 0, 0, 0
		for _, r := range rs {
			turns += float64(r.Turns)
			errs += float64(r.ScriptErrors)
			pt += float64(r.PromptTokens)
			ct += float64(r.CompletionTokens)
			cost += r.CostUSD
			// 05-recover fails its first call on purpose, so it would count
			// against every arm alike and say nothing.
			if r.Task != "05-recover" {
				fvn++
				if r.FirstValid {
					fv++
				}
			}
			if r.Pass {
				pass++
			}
		}
		n := float64(len(rs))
		perPass := "–"
		if pass > 0 {
			perPass = fmt.Sprintf("%.5f", cost/float64(pass))
		}
		fmt.Fprintf(&b, "| %s | %.1f | %s | %.2f | %.0f | %.0f | %.5f | %s |\n", a, turns/n, pct(fv, fvn), errs/n, pt/n, ct/n, cost/n, perPass)
	}
	return b.String()
}

func present(rs []Result, key func(Result) string, order []string) []string {
	seen := map[string]bool{}
	for _, r := range rs {
		seen[key(r)] = true
	}
	var out []string
	for _, o := range order {
		if seen[o] {
			out = append(out, o)
			delete(seen, o)
		}
	}
	var rest []string
	for k := range seen {
		rest = append(rest, k)
	}
	sort.Strings(rest)
	return append(out, rest...)
}

func passes(rs []Result, keep func(Result) bool) (k, n int) {
	for _, r := range rs {
		if keep(r) {
			n++
			if r.Pass {
				k++
			}
		}
	}
	return k, n
}

func pct(k, n int) string {
	if n == 0 {
		return "–"
	}
	return fmt.Sprintf("%.0f%% (%d/%d)", 100*float64(k)/float64(n), k, n)
}

func index(rs []Result, arm string) map[string]Result {
	out := map[string]Result{}
	for _, r := range rs {
		if r.Arm == arm {
			out[fmt.Sprintf("%s|%s|%d", r.Model, r.Task, r.Rep)] = r
		}
	}
	return out
}

// wilson is the 95% Wilson score interval for k successes in n, which
// stays sensible near 0% and 100%, where most cells of this bench sit.
func wilson(k, n int) (float64, float64) {
	if n == 0 {
		return 0, 0
	}
	const z = 1.96
	p := float64(k) / float64(n)
	nn := float64(n)
	den := 1 + z*z/nn
	c := p + z*z/(2*nn)
	h := z * math.Sqrt(p*(1-p)/nn+z*z/(4*nn*nn))
	return math.Max(0, (c-h)/den), math.Min(1, (c+h)/den)
}

// pval is McNemar's exact test: under no difference, each discordant pair
// is a fair coin, so the p-value is a two-sided binomial tail.
func pval(b, c int) string {
	n := b + c
	if n == 0 {
		return "–"
	}
	k := min(b, c)
	tail := 0.0
	for i := 0; i <= k; i++ {
		tail += math.Exp(lchoose(n, i) - float64(n)*math.Ln2)
	}
	p := math.Min(1, 2*tail)
	if p < 0.001 {
		return "<0.001"
	}
	return fmt.Sprintf("%.3f", p)
}

func lchoose(n, k int) float64 {
	a, _ := math.Lgamma(float64(n + 1))
	b, _ := math.Lgamma(float64(k + 1))
	c, _ := math.Lgamma(float64(n - k + 1))
	return a - b - c
}
