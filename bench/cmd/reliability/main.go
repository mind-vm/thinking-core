// Command reliability runs step 1 of the tool-use comparison: ten small
// tasks, each probing one property of the mechanics, under six ways of
// letting the model act (see package reliability), on cheap models.
//
//	export EDENAI_API_KEY=... EDENAI_BASE_URL=https://api.eu.edenai.run/v3/
//	go run ./cmd/reliability run -reps 10 -budget 5 -out results/01/results.jsonl
//	go run ./cmd/reliability report -in results/01/results.jsonl
//
// run resumes: runs already in -out are skipped, so an interrupted or
// budget-capped run continues where it stopped.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"

	"github.com/mind-vm/thinking-core/bench/reliability"
)

// models are the ones under test, EU-hosted with zero data retention, at
// Eden AI's list price on 2026-11-27 (USD per million tokens).
var models = []reliability.Model{
	{Name: "deepseek-v4.1-flash", ID: "vertex/deepseek-ai/deepseek-v4.1-flash-maas@eu", InPerM: 0.33, OutPerM: 1.32},
	{Name: "glm-5.3-flash", ID: "mistral/zai-glm-5-3-flash@eu", InPerM: 0.17, OutPerM: 0.55},
	{Name: "qwen3.6-35b-a3b", ID: "flexai/Qwen3.6-35B-A3B-FP8@eu", InPerM: 0.10, OutPerM: 0.95},
	{Name: "gpt-oss-20b", ID: "flexai/gpt-oss-20b@eu", InPerM: 0.02, OutPerM: 0.09, ReasoningEffort: "low"},
	{Name: "devstral-small", ID: "mistral/devstral-small-latest@eu", InPerM: 0.11, OutPerM: 0.33},
	{Name: "gemini-3.5-flash-lite", ID: "vertex/gemini-3.5-flash-lite@eu", InPerM: 0.33, OutPerM: 2.75},
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "run":
		run(os.Args[2:])
	case "report":
		report(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: reliability run|report [flags]")
	os.Exit(2)
}

func run(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	out := fs.String("out", "results/01/results.jsonl", "results file; runs already in it are skipped")
	reps := fs.Int("reps", 10, "repetitions of every arm × model × task")
	budget := fs.Float64("budget", 5, "stop starting runs once this many USD are spent (0: no cap)")
	parallel := fs.Int("parallel", 12, "runs at once")
	perModel := fs.Int("per-model", 4, "runs at once on one model")
	armsFlag := fs.String("arms", strings.Join(reliability.AllArms, ","), "arms to run")
	modelsFlag := fs.String("models", "", "models to run, by short name (default all)")
	tasksFlag := fs.String("tasks", "", "tasks to run, by id (default all)")
	_ = fs.Parse(args)

	key, base := os.Getenv("EDENAI_API_KEY"), os.Getenv("EDENAI_BASE_URL")
	if key == "" || base == "" {
		log.Fatal("reliability: EDENAI_API_KEY and EDENAI_BASE_URL must be set")
	}
	ms := pick(models, *modelsFlag, func(m reliability.Model) string { return m.Name })
	ts := pick(reliability.Tasks(), *tasksFlag, func(t reliability.Task) string { return t.ID })
	arms := strings.Split(*armsFlag, ",")
	for _, a := range arms {
		if !slices.Contains(reliability.AllArms, a) {
			log.Fatalf("reliability: unknown arm %q (have %s)", a, strings.Join(reliability.AllArms, ", "))
		}
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	r := &reliability.Runner{
		Chat:      &reliability.Chat{BaseURL: base, APIKey: key},
		Parallel:  *parallel,
		PerModel:  *perModel,
		BudgetUSD: *budget,
		Out:       *out,
		Log:       log.Printf,
	}
	if err := r.Run(ctx, reliability.Plan(arms, ms, ts, *reps)); err != nil {
		log.Fatal(err)
	}
}

func report(args []string) {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	in := fs.String("in", "results/01/results.jsonl", "results file")
	_ = fs.Parse(args)
	rs, err := reliability.ReadResults(*in)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print(reliability.Report(rs, models))
}

func pick[T any](all []T, names string, name func(T) string) []T {
	if names == "" {
		return all
	}
	var out []T
	for _, n := range strings.Split(names, ",") {
		i := slices.IndexFunc(all, func(t T) bool { return name(t) == n })
		if i < 0 {
			log.Fatalf("reliability: unknown name %q", n)
		}
		out = append(out, all[i])
	}
	return out
}
