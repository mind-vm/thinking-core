package bench

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"time"
)

// Result is one (task, agent, model, repetition) measurement — one JSONL
// line.
type Result struct {
	Task             string `json:"task"`
	Agent            string `json:"agent"`
	Model            string `json:"model"`
	Rep              int    `json:"rep"`
	Seed             uint64 `json:"seed"`
	Correct          bool   `json:"correct"`
	CheckDetail      string `json:"check_detail,omitempty"`
	Turns            int    `json:"turns"`
	ToolCalls        int    `json:"tool_calls"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	WallMS           int64  `json:"wall_ms"`
	Error            string `json:"error,omitempty"`
	Answer           string `json:"answer,omitempty"`
}

// Plan is one benchmark run: every agent on every task for every model,
// Reps times.
type Plan struct {
	Data   *Data
	Tasks  []Task
	Agents []Agent
	Models []string
	Reps   int
	// Parallel is how many runs are in flight at once; <= 1 is serial.
	Parallel int
	// OnResult, when set, sees each result as it completes, in completion
	// order (the slice Run returns is in plan order).
	OnResult func(Result)
}

// Run executes the plan. Agents are interleaved within each repetition
// rather than run in blocks, so a provider that gets slower or flakier
// over the run affects both sides alike.
func (p Plan) Run(ctx context.Context) []Result {
	type job struct {
		i     int
		task  Task
		agent Agent
		model string
		rep   int
	}
	var jobs []job
	for _, m := range p.Models {
		for _, t := range p.Tasks {
			for r := 1; r <= max(p.Reps, 1); r++ {
				for _, a := range p.Agents {
					jobs = append(jobs, job{len(jobs), t, a, m, r})
				}
			}
		}
	}
	results := make([]Result, len(jobs))
	sem := make(chan struct{}, max(p.Parallel, 1))
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, j := range jobs {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			r := p.one(ctx, j.task, j.agent, j.model, j.rep)
			results[j.i] = r
			if p.OnResult != nil {
				mu.Lock()
				p.OnResult(r)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	out := results[:0]
	for _, r := range results {
		if r.Task != "" {
			out = append(out, r)
		}
	}
	return out
}

func (p Plan) one(ctx context.Context, t Task, a Agent, model string, rep int) Result {
	start := time.Now()
	o := a.Run(ctx, model, p.Data, t)
	r := Result{
		Task: t.ID, Agent: a.Name(), Model: model, Rep: rep, Seed: p.Data.Seed,
		Turns: o.Turns, ToolCalls: o.ToolCalls,
		PromptTokens: o.PromptTokens, CompletionTokens: o.CompletionTokens,
		WallMS: time.Since(start).Milliseconds(),
		Answer: clip(o.Final, 2000),
	}
	if o.Err != nil {
		r.Error = clip(o.Err.Error(), 500)
	}
	// A run that errored can still have answered (a turn cap hit after
	// answer, say); the answer is what is scored, the error is reported.
	r.Correct, r.CheckDetail = t.Check(o.Final)
	return r
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// WriteJSONL writes one result per line.
func WriteJSONL(w io.Writer, results []Result) error {
	enc := json.NewEncoder(w)
	for _, r := range results {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return nil
}
