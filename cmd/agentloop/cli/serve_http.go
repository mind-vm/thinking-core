package cli

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/mind-vm/thinking-core/agentloop"
	"github.com/mind-vm/thinking-core/agentloopsql"
	"github.com/mind-vm/thinking-core/agentrun"
)

// serve_http.go is the HTTP face of `agentloop serve`: runs started over POST,
// watched over Server-Sent Events, answered over another POST.
//
// What it adds to agentrun is the one thing examples/blocks does not need: a
// run that outlives the request that started it. The example streams from the
// POST itself, so a dropped connection is a dead run. A backend in another
// language wants the opposite — start, then watch, possibly twice, possibly
// late — which means each run owns an event buffer and every stream is a
// reader of it.

const (
	// maxRunEvents bounds one run's replay buffer. A run emits an event or two
	// per turn plus response chunks, so this is far above any run that fits
	// in a context window; the bound exists so a runaway script cannot grow
	// the process without limit. Past it, only the terminal events are kept:
	// a late subscriber then sees the start, a `warning`, and the end, and a
	// live one still sees everything through its own connection's pace.
	maxRunEvents = 10_000

	// runRetention is how long a finished run stays addressable. Its answer
	// is in the session store forever; this only bounds the in-memory
	// buffer a late GET /events replays from.
	runRetention = time.Hour

	maxRequestBytes = 4 << 20
)

// tenantID is the one tenant a token stands for. The broker compares tenants
// so a reply from another one finds no run; with one token there is one
// tenant, and the comparison still guards the day a token table appears.
var tenantID = uuid.Nil

// serveDeps is what the server needs, as values, so tests can supply a loop
// over a scripted LLM without going through config and environment.
type serveDeps struct {
	loop   agentloop.Loop
	store  *agentloopsql.Store
	broker *agentrun.Broker
	token  string // empty: no auth (loopback only; the caller checked)
	// ctx is the parent of every run. Cancelling it is how shutdown stops
	// runs that no request is attached to.
	ctx context.Context
}

type serveServer struct {
	serveDeps
	wg sync.WaitGroup

	mu       sync.Mutex
	runs     map[string]*serveRun
	bySessID map[string]*serveRun // live runs only; one at a time per session
}

func newServeServer(d serveDeps) *serveServer {
	return &serveServer{serveDeps: d, runs: map[string]*serveRun{}, bySessID: map[string]*serveRun{}}
}

// wait blocks until every run goroutine has returned.
func (s *serveServer) wait() { s.wg.Wait() }

func (s *serveServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /v1/runs", s.createRun)
	mux.HandleFunc("GET /v1/runs/{id}", s.getRun)
	mux.HandleFunc("GET /v1/runs/{id}/events", s.runEvents)
	mux.HandleFunc("POST /v1/runs/{id}/answer", s.answerRun)
	mux.HandleFunc("GET /v1/sessions", s.listSessions)
	mux.HandleFunc("DELETE /v1/sessions/{id}", s.deleteSession)
	return s.auth(mux)
}

// auth guards everything under /v1/. /healthz stays open so a load balancer
// or systemd watchdog needs no secret.
func (s *serveServer) auth(next http.Handler) http.Handler {
	if s.token == "" {
		return next
	}
	// Compared as digests so the comparison is constant-time and the length
	// of the real token does not leak through ConstantTimeCompare's early
	// exit on unequal lengths.
	want := sha256.Sum256([]byte(s.token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		got, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		sum := sha256.Sum256([]byte(got))
		if subtle.ConstantTimeCompare(sum[:], want[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="agentloop"`)
			writeError(w, http.StatusUnauthorized, "missing or wrong bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// serveRun is one run and everything a late reader needs of it.
type serveRun struct {
	id, sessionID string

	mu        sync.Mutex
	events    []agentrun.Event
	truncated bool
	sawError  bool
	// changed is closed and replaced on every append: a reader snapshots it
	// under mu, then waits on it without holding the lock. A sync.Cond would
	// do the same but cannot be selected against a request's cancellation.
	changed chan struct{}

	finished   bool
	finishedAt time.Time
	status     string // running | completed | max_iterations | canceled | error
	answer     string
	errMsg     string
	summary    *agentrun.Summary
}

func newServeRun(id, sessionID string) *serveRun {
	return &serveRun{id: id, sessionID: sessionID, status: "running", changed: make(chan struct{})}
}

// emit buffers one event for readers, past the bound only terminal ones.
func (r *serveRun) emit(e agentrun.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.appendLocked(e)
}

func (r *serveRun) appendLocked(e agentrun.Event) {
	terminal := e.Type == "done" || e.Type == "error"
	if e.Type == "error" {
		r.sawError = true
	}
	if len(r.events) >= maxRunEvents && !terminal {
		if !r.truncated {
			r.truncated = true
			r.events = append(r.events, agentrun.Event{Type: "warning", Content: fmt.Sprintf("this run emitted more than %d events; later ones are not replayed to subscribers that connect afterwards", maxRunEvents)})
			r.signalLocked()
		}
		return
	}
	r.events = append(r.events, e)
	r.signalLocked()
}

func (r *serveRun) signalLocked() {
	close(r.changed)
	r.changed = make(chan struct{})
}

// finish records the outcome and appends the single terminal `done`.
//
// The loop emits its own `done`, which the OnEvent bridge drops: its summary
// is the same numbers, but a run that fails before the loop starts never emits
// one, and a stream that sometimes ends in two and sometimes in none is worse
// than one source of truth. Everything happens under one lock so a reader that
// sees finished also sees the done.
func (r *serveRun) finish(res agentloop.RunResult, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.status = res.Status
	switch {
	case errors.Is(err, context.Canceled):
		r.status = "canceled"
	case r.status == "":
		r.status = "error"
	}
	if err != nil && r.status == "completed" {
		r.status = "error"
	}
	if err != nil {
		r.errMsg = err.Error()
		// The loop reports most failures as an `error` event of its own; a
		// second one would show the client the same failure twice.
		if !r.sawError {
			r.appendLocked(agentrun.Event{Type: "error", Content: r.errMsg})
		}
	}
	if r.status == "completed" {
		r.answer = res.FinalText
	}
	sum := agentrun.SummaryOf(res)
	sum.SessionID = r.sessionID
	r.summary = &sum
	r.appendLocked(agentrun.Event{Type: "done", Summary: &sum})
	r.finished = true
	r.finishedAt = time.Now()
	r.signalLocked()
}

// runView is the JSON shape of GET /v1/runs/{id} and of ?wait=true.
type runView struct {
	RunID     string            `json:"runId"`
	SessionID string            `json:"sessionId"`
	Status    string            `json:"status"`
	Answer    string            `json:"answer,omitempty"`
	Error     string            `json:"error,omitempty"`
	Summary   *agentrun.Summary `json:"summary,omitempty"`
}

func (r *serveRun) view() runView {
	r.mu.Lock()
	defer r.mu.Unlock()
	return runView{RunID: r.id, SessionID: r.sessionID, Status: r.status, Answer: r.answer, Error: r.errMsg, Summary: r.summary}
}

// waitDone blocks until the run has finished, or ctx ends first (false).
func (r *serveRun) waitDone(ctx context.Context) bool {
	for {
		r.mu.Lock()
		fin, ch := r.finished, r.changed
		r.mu.Unlock()
		if fin {
			return true
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return false
		}
	}
}

type createRunRequest struct {
	SessionID string `json:"sessionId"`
	Message   string `json:"message"`
	Context   string `json:"context"`
}

func (s *serveServer) createRun(w http.ResponseWriter, r *http.Request) {
	var req createRunRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || strings.TrimSpace(req.Message) == "" {
		writeError(w, http.StatusBadRequest, `want {"message": "...", "sessionId"?: "<uuid>", "context"?: "..."}`)
		return
	}
	wait := false
	if v := r.URL.Query().Get("wait"); v != "" {
		var err error
		if wait, err = strconv.ParseBool(v); err != nil {
			writeError(w, http.StatusBadRequest, "wait must be true or false")
			return
		}
	}
	// The broker keys prompts by uuid, so a session id has to be one. Saying so
	// here beats a run that starts and then cannot ask a question.
	if req.SessionID == "" {
		req.SessionID = uuid.NewString()
	} else if _, err := uuid.Parse(req.SessionID); err != nil {
		writeError(w, http.StatusBadRequest, "sessionId must be a UUID (omit it to start a new session)")
		return
	}

	run, err := s.startRun(req)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if !wait {
		writeJSON(w, http.StatusAccepted, map[string]string{"runId": run.id, "sessionId": run.sessionID})
		return
	}
	// A client that hangs up stops waiting, not running: the run is already
	// recorded in the session, and cancelling work the caller may poll for
	// later is the more surprising of the two behaviours.
	if !run.waitDone(r.Context()) {
		return
	}
	writeJSON(w, http.StatusOK, run.view())
}

// startRun registers the run and launches it. The broker registration is made
// before the goroutine starts: an ask() in the first script must find it.
func (s *serveServer) startRun(req createRunRequest) (*serveRun, error) {
	s.mu.Lock()
	if _, busy := s.bySessID[req.SessionID]; busy {
		s.mu.Unlock()
		// One run per session at a time: two would interleave steps into
		// one history and fight over the broker's single prompt slot.
		return nil, errors.New("this session already has a run in progress")
	}
	s.reapLocked()
	run := newServeRun(uuid.NewString(), req.SessionID)
	s.runs[run.id] = run
	s.bySessID[run.sessionID] = run
	s.mu.Unlock()

	sid := uuid.MustParse(run.sessionID)
	// end is called from two places — the goroutine below and the shutdown
	// watcher — and the broker's func closes a channel, so it runs once.
	end := sync.OnceFunc(s.broker.Begin(sid, tenantID, run.emit))

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		// ask() waits on the broker, not on the run's context, so without this
		// a shutdown would sit out the broker's whole AskTimeout for a prompt
		// nobody is going to answer. Ending the registration fails the ask()
		// at once, and the run unwinds through its ordinary error path.
		stop := context.AfterFunc(s.ctx, end)
		defer stop()
		res, err := s.loop.Run(s.ctx, agentloop.RunRequest{
			SessionID: run.sessionID,
			Scope:     agentloop.Scope{WorkspaceID: "serve"},
			Message:   req.Message,
			Context:   req.Context,
			OnEvent: func(e agentloop.RunEvent) {
				if e.Type == "done" {
					return // finish emits the one terminal event
				}
				run.emit(agentrun.FromRunEvent(e))
			},
		})
		// Unregister before announcing the end: a client that sees `done` and
		// immediately sends the next message for this session must not meet
		// a 409 for a run that has finished.
		end()
		s.mu.Lock()
		delete(s.bySessID, run.sessionID)
		s.mu.Unlock()
		run.finish(res, err)
	}()
	return run, nil
}

// reapLocked forgets finished runs past retention. Called on each new run, so
// a server that is idle holds a bounded amount and one that is busy trims
// itself without a background goroutine to stop at shutdown.
func (s *serveServer) reapLocked() {
	cutoff := time.Now().Add(-runRetention)
	for id, r := range s.runs {
		r.mu.Lock()
		old := r.finished && r.finishedAt.Before(cutoff)
		r.mu.Unlock()
		if old {
			delete(s.runs, id)
		}
	}
}

func (s *serveServer) lookup(id string) *serveRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runs[id]
}

func (s *serveServer) getRun(w http.ResponseWriter, r *http.Request) {
	run := s.lookup(r.PathValue("id"))
	if run == nil {
		writeError(w, http.StatusNotFound, "no such run (finished runs are forgotten after "+runRetention.String()+")")
		return
	}
	writeJSON(w, http.StatusOK, run.view())
}

// runEvents replays the run's buffer from its first event, then follows it
// until the terminal `done`. The same code serves a client that connects
// before the first event and one that connects after the last: it is never
// "catch up, then switch to live", which is where events go missing.
func (s *serveServer) runEvents(w http.ResponseWriter, r *http.Request) {
	run := s.lookup(r.PathValue("id"))
	if run == nil {
		writeError(w, http.StatusNotFound, "no such run")
		return
	}
	stream := agentrun.Open(w)
	if stream == nil {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	sent := 0
	for {
		run.mu.Lock()
		batch := append([]agentrun.Event(nil), run.events[sent:]...)
		fin, ch := run.finished, run.changed
		run.mu.Unlock()

		for _, e := range batch {
			stream.Send(e)
		}
		sent += len(batch)
		if fin && len(batch) == 0 {
			return
		}
		if len(batch) > 0 {
			continue // more may have arrived while writing
		}
		select {
		case <-ch:
		case <-r.Context().Done():
			return
		}
	}
}

type answerRequest struct {
	RequestID string `json:"requestId"`
	Value     any    `json:"value"`
}

func (s *serveServer) answerRun(w http.ResponseWriter, r *http.Request) {
	run := s.lookup(r.PathValue("id"))
	if run == nil {
		writeError(w, http.StatusNotFound, "no such run")
		return
	}
	var req answerRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.RequestID == "" {
		writeError(w, http.StatusBadRequest, `want {"requestId": "...", "value": ...}`)
		return
	}
	// The value goes through as decoded JSON; ask() checks it against what
	// was asked before the script sees it.
	if err := s.broker.Reply(uuid.MustParse(run.sessionID), tenantID, req.RequestID, req.Value); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type sessionView struct {
	ID        string    `json:"id"`
	Model     string    `json:"model,omitempty"`
	Status    string    `json:"status"`
	Steps     int32     `json:"steps"`
	Tokens    tokenView `json:"tokens"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	Opening   string    `json:"opening,omitempty"`
}

type tokenView struct {
	Prompt     int32 `json:"prompt"`
	Completion int32 `json:"completion"`
}

func (s *serveServer) listSessions(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "limit must be a non-negative integer (0 for all)")
			return
		}
		limit = n
	}
	rows, err := s.store.List(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Initialised, not nil: an empty list must reach a client as [] and not null.
	out := make([]sessionView, 0, len(rows))
	for _, row := range rows {
		out = append(out, sessionView{
			ID: row.ID, Model: row.Model, Status: row.Status, Steps: row.Steps,
			Tokens:    tokenView{Prompt: row.Tokens.Prompt, Completion: row.Tokens.Completion},
			CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, Opening: row.Opening,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

func (s *serveServer) deleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	_, busy := s.bySessID[id]
	s.mu.Unlock()
	if busy {
		// Deleting under a live run would have it append steps to a session
		// that no longer exists.
		writeError(w, http.StatusConflict, "this session has a run in progress")
		return
	}
	switch err := s.store.Delete(r.Context(), id); {
	case errors.Is(err, agentloopsql.ErrNoSession):
		writeError(w, http.StatusNotFound, "no such session")
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
