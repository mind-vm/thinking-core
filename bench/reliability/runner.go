package reliability

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
)

// Job is one run to do.
type Job struct {
	Arm   string
	Model Model
	Task  Task
	Rep   int
}

func (j Job) key() string { return fmt.Sprintf("%s|%s|%s|%d", j.Arm, j.Model.Name, j.Task.ID, j.Rep) }

// Plan is every combination of arms, models, tasks and repetitions, in a
// shuffled order: a host that slows down in the afternoon, or a budget that
// runs out, then hits every arm alike instead of the last ones listed.
func Plan(arms []string, models []Model, tasks []Task, reps int) []Job {
	var jobs []Job
	for rep := 1; rep <= reps; rep++ {
		for _, m := range models {
			for _, a := range arms {
				for _, t := range tasks {
					jobs = append(jobs, Job{Arm: a, Model: m, Task: t, Rep: rep})
				}
			}
		}
	}
	rng := rand.New(rand.NewPCG(20251127, 1))
	rng.Shuffle(len(jobs), func(i, j int) { jobs[i], jobs[j] = jobs[j], jobs[i] })
	return jobs
}

type Runner struct {
	Chat     *Chat
	Parallel int
	// PerModel bounds concurrent runs on one model, so one slow host does
	// not take every worker.
	PerModel  int
	BudgetUSD float64
	Out       string // results.jsonl; runs already in it are skipped
	Log       func(format string, args ...any)
}

// Run does the jobs not already in r.Out and appends each result as it
// lands, so an interrupted run resumes where it stopped.
func (r *Runner) Run(ctx context.Context, jobs []Job) error {
	done, spent, err := readDone(r.Out)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(r.Out, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	tdir := filepath.Join(filepath.Dir(r.Out), "transcripts")
	if err := os.MkdirAll(tdir, 0o755); err != nil {
		return err
	}

	var cost atomic.Int64 // micro-dollars
	cost.Store(int64(spent * 1e6))
	var mu sync.Mutex
	sems := map[string]chan struct{}{}
	for _, j := range jobs {
		if _, ok := sems[j.Model.Name]; !ok {
			sems[j.Model.Name] = make(chan struct{}, max(1, r.PerModel))
		}
	}
	queue := make(chan Job)
	var wg sync.WaitGroup
	var n atomic.Int64
	todo := 0
	for _, j := range jobs {
		if !done[j.key()] {
			todo++
		}
	}
	r.logf("%d runs to do (%d already done, $%.4f spent)", todo, len(jobs)-todo, spent)
	for range max(1, r.Parallel) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range queue {
				sem := sems[j.Model.Name]
				sem <- struct{}{}
				res := r.one(ctx, j)
				<-sem
				total := float64(cost.Add(int64(res.CostUSD*1e6))) / 1e6
				mu.Lock()
				line, _ := json.Marshal(res)
				_, _ = f.Write(append(line, '\n'))
				if t, err := json.Marshal(res.Transcript); err == nil {
					_ = os.WriteFile(filepath.Join(tdir, fmt.Sprintf("%s_%s_%s_%d.json", res.Arm, res.Model, res.Task, res.Rep)), t, 0o644)
				}
				mu.Unlock()
				k := n.Add(1)
				mark := "pass"
				if !res.Pass {
					mark = "FAIL " + res.Failure
				}
				r.logf("[%d/%d] $%.3f %s %s %s #%d: %s (%d turns)", k, todo, total, res.Arm, res.Model, res.Task, res.Rep, mark, res.Turns)
			}
		}()
	}
	for _, j := range jobs {
		if done[j.key()] {
			continue
		}
		if c := float64(cost.Load()) / 1e6; r.BudgetUSD > 0 && c >= r.BudgetUSD {
			r.logf("budget of $%.2f reached at $%.4f; stopping", r.BudgetUSD, c)
			break
		}
		select {
		case queue <- j:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(queue)
	wg.Wait()
	return ctx.Err()
}

func (r *Runner) one(ctx context.Context, j Job) Result {
	var res Result
	switch j.Arm {
	case ArmTools:
		res = RunTools(ctx, r.Chat, j.Model, j.Task)
	case ArmLoop:
		res = RunLoop(ctx, r.Chat.BaseURL, r.Chat.APIKey, j.Model, j.Task)
	default:
		res = RunCode(ctx, r.Chat, j.Model, j.Arm, j.Task)
	}
	res.Rep = j.Rep
	// A run that timed out before a single reply arrived never reached the
	// arm: the host hung. That is infrastructure, like a refused call.
	if res.Status == "timeout" && res.Turns == 0 {
		res.Status, res.Failure = "llm_error", ""
		if res.Error == "" {
			res.Error = "no reply within the run time"
		}
	}
	return res
}

func (r *Runner) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log(format, args...)
	}
}

func readDone(path string) (map[string]bool, float64, error) {
	done := map[string]bool{}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return done, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	spent := 0.0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var res Result
		if json.Unmarshal(sc.Bytes(), &res) != nil {
			continue
		}
		done[fmt.Sprintf("%s|%s|%s|%d", res.Arm, res.Model, res.Task, res.Rep)] = true
		spent += res.CostUSD
	}
	return done, spent, sc.Err()
}

// ReadResults loads results.jsonl.
func ReadResults(path string) ([]Result, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Result
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var res Result
		if err := json.Unmarshal(sc.Bytes(), &res); err == nil {
			out = append(out, res)
		}
	}
	return out, sc.Err()
}
