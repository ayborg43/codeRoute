package gateway

import (
	"sort"
	"sync"
	"time"

	"github.com/coderouter/coderouter/internal/routing"
)

// This file answers one question the usage log answers badly: which model is
// answering right now. The log is written after a call finishes, so a request
// that is still running — or one that took thirty seconds and is the reason
// someone opened the dashboard — is invisible while it matters most. Routing
// also picks a different model per request, so "the model in use" is not
// something an operator can read off the configuration either.

// LiveCall is one upstream attempt that has not finished yet.
type LiveCall struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`

	// Requested is what the caller asked for, which is a sentinel like "auto"
	// whenever routing chose the model. Showing both is the point: it says
	// that a choice was made, and what it landed on.
	Requested string `json:"requested"`

	Task      string    `json:"task"`
	Streaming bool      `json:"streaming"`
	StartedAt time.Time `json:"started_at"`
	ElapsedMs int64     `json:"elapsed_ms"`
}

// LastCall is the most recent attempt to finish, kept so the view still names
// a model on an idle gateway rather than going blank between requests.
type LastCall struct {
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	Requested string `json:"requested"`
	Task      string `json:"task"`

	Status    string `json:"status"`
	CacheHit  bool   `json:"cache_hit"`
	LatencyMs int64  `json:"latency_ms"`

	FinishedAt time.Time `json:"finished_at"`
	AgoMs      int64     `json:"ago_ms"`

	// Failure is the upstream's own words, present only on a failed attempt.
	// A failover chain overwrites it with the step that succeeded, so what is
	// reported here is what the caller actually got.
	Failure string `json:"failure,omitempty"`
}

// LiveStatus is the whole of what the status view reads.
type LiveStatus struct {
	Active []LiveCall `json:"active"`
	Last   *LastCall  `json:"last,omitempty"`
}

// liveState is in-memory and process-local: it is a view of what this process
// is doing, and survives neither a restart nor a second replica. That is the
// right trade for a status light — persisting it would cost a write on the
// hot path of every request to say something only true for a moment.
type liveState struct {
	mu       sync.Mutex
	seq      int64
	inFlight map[int64]LiveCall
	last     *LastCall
}

func newLiveState() *liveState {
	return &liveState{inFlight: make(map[int64]LiveCall)}
}

// begin records an attempt as in flight and returns the id that ends it.
func (l *liveState) begin(c candidate, requested string, task routing.TaskType, streaming bool) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.seq++
	l.inFlight[l.seq] = LiveCall{
		Provider:  c.provider,
		Model:     c.model,
		Requested: requested,
		Task:      string(task),
		Streaming: streaming,
		StartedAt: time.Now(),
	}
	return l.seq
}

// end clears an in-flight attempt and keeps it as the last one to finish.
func (l *liveState) end(id int64, status, failure string, latency time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	call, ok := l.inFlight[id]
	if !ok {
		return
	}
	delete(l.inFlight, id)

	l.last = &LastCall{
		Provider:   call.Provider,
		Model:      call.Model,
		Requested:  call.Requested,
		Task:       call.Task,
		Status:     status,
		LatencyMs:  latency.Milliseconds(),
		FinishedAt: time.Now(),
		Failure:    failure,
	}
}

// servedFromCache records a hit as the last thing to answer. Without it the
// view would keep naming the model from an earlier request, implying an
// upstream call that never happened.
func (l *liveState) servedFromCache(requested, model string, task routing.TaskType, latency time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.last = &LastCall{
		Provider:   cacheProvider,
		Model:      model,
		Requested:  requested,
		Task:       string(task),
		Status:     "success",
		CacheHit:   true,
		LatencyMs:  latency.Milliseconds(),
		FinishedAt: time.Now(),
	}
}

// snapshot copies the state out, stamping the elapsed times against one clock
// reading so the numbers in a single view agree with each other.
func (l *liveState) snapshot() LiveStatus {
	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	status := LiveStatus{Active: make([]LiveCall, 0, len(l.inFlight))}
	for _, c := range l.inFlight {
		c.ElapsedMs = now.Sub(c.StartedAt).Milliseconds()
		status.Active = append(status.Active, c)
	}
	// Longest-running first: with several in flight, the one that has been
	// waiting the longest is the one worth looking at.
	sort.Slice(status.Active, func(i, j int) bool {
		return status.Active[i].StartedAt.Before(status.Active[j].StartedAt)
	})

	if l.last != nil {
		last := *l.last
		last.AgoMs = now.Sub(last.FinishedAt).Milliseconds()
		status.Last = &last
	}
	return status
}

// Live reports which models this process is calling now, and which one
// answered last.
func (g *Gateway) Live() LiveStatus { return g.live.snapshot() }
