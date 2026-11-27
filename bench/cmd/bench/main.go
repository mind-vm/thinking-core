// Command bench runs the code-as-action vs tool-calling comparison
// against an OpenAI-wire endpoint and writes results.jsonl and
// summary.md to -out. See bench/README.md.
//
//	export OPENAI_API_KEY=...
//	go run ./bench/cmd/bench -base-url https://openrouter.ai/api/v1 \
//	    -models openai/gpt-4o-mini -reps 3 -out bench-results/run1
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/mind-vm/thinking-core/bench"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		os.Exit(1)
	}
}

func run() error {
	baseURL := flag.String("base-url", envOr("OPENAI_BASE_URL", "https://api.openai.com/v1"), "OpenAI-wire base URL (default $OPENAI_BASE_URL)")
	models := flag.String("models", os.Getenv("OPENAI_CHAT_MODEL"), "comma-separated model names, e.g. gpt-4o-mini,gpt-4.1 (default $OPENAI_CHAT_MODEL)")
	reps := flag.Int("reps", 3, "repetitions per (task, agent, model)")
	seed := flag.Uint64("seed", 1, "fixture seed; the same seed gives the same data and answers")
	tasks := flag.String("tasks", "", "comma-separated task ids (default all; see -list)")
	agents := flag.String("agents", "thinking-core,tool-calling", "comma-separated agents to run")
	out := flag.String("out", "", "results directory (required unless -list)")
	maxTurns := flag.Int("max-turns", 20, "LLM round-trip cap per run, for both agents")
	timeout := flag.Duration("timeout", 10*time.Minute, "wall-time cap per run")
	parallel := flag.Int("parallel", 1, "runs in flight at once")
	list := flag.Bool("list", false, "print the tasks and their expected answers for -seed, then exit")
	flag.Parse()

	data := bench.Generate(*seed)
	all := bench.Tasks(data)
	if *list {
		for _, t := range all {
			fmt.Printf("%-26s %-52s %v\n", t.ID, t.Shape, t.Expected)
		}
		return nil
	}

	selected := all
	if *tasks != "" {
		selected = nil
		for _, id := range split(*tasks) {
			t, ok := bench.TaskByID(all, id)
			if !ok {
				return fmt.Errorf("unknown task %q (see -list)", id)
			}
			selected = append(selected, t)
		}
	}
	if *out == "" {
		return fmt.Errorf("-out is required")
	}
	modelList := split(*models)
	if len(modelList) == 0 {
		return fmt.Errorf("-models is required")
	}
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		return fmt.Errorf("OPENAI_API_KEY is not set (for a local server that needs none, any value works)")
	}

	ep := bench.Endpoint{BaseURL: *baseURL, APIKey: key, MaxTurns: *maxTurns, RunTimeout: *timeout}
	var agentList []bench.Agent
	for _, name := range split(*agents) {
		switch name {
		case "thinking-core":
			agentList = append(agentList, bench.CodeAction{Endpoint: ep})
		case "tool-calling":
			agentList = append(agentList, bench.ToolCalling{Endpoint: ep})
		default:
			return fmt.Errorf("unknown agent %q (thinking-core, tool-calling)", name)
		}
	}

	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	// Results are appended as they complete, so an interrupted run keeps
	// what it paid for.
	jl, err := os.Create(filepath.Join(*out, "results.jsonl"))
	if err != nil {
		return err
	}
	defer func() { _ = jl.Close() }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	total := len(selected) * len(agentList) * len(modelList) * max(*reps, 1)
	done := 0
	plan := bench.Plan{
		Data: data, Tasks: selected, Agents: agentList, Models: modelList, Reps: *reps, Parallel: *parallel,
		OnResult: func(r bench.Result) {
			done++
			_ = bench.WriteJSONL(jl, []bench.Result{r})
			mark := "ok   "
			if !r.Correct {
				mark = "WRONG"
			}
			fmt.Fprintf(os.Stderr, "[%d/%d] %s %-24s %-13s %-20s rep %d  %2d turns %7d prompt tok  %s\n",
				done, total, mark, r.Task, r.Agent, r.Model, r.Rep, r.Turns, r.PromptTokens, r.Error)
		},
	}
	results := plan.Run(ctx)

	f, err := os.Create(filepath.Join(*out, "summary.md"))
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if err := bench.WriteSummary(f, "thinking-core bench: "+*baseURL, results); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s and %s\n", jl.Name(), f.Name())
	return ctx.Err()
}

func split(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
