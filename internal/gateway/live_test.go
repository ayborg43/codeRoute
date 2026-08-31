package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/coderouter/coderouter/internal/provider"
)

// THE QUESTION THIS FILE ANSWERS: while a request is in flight, and after it
// finishes, can an operator find out which model served it? The usage log
// cannot say — it gains a row only once the call is over.

func TestLiveNamesTheModelThatAnswered(t *testing.T) {
	only := newUpstream(t, "only", http.StatusOK, "")
	g, keys := failoverGateway(t, map[string]*upstream{"only": only})

	if _, err := routeWith(t, g, keys); err != nil {
		t.Fatalf("routing failed: %v", err)
	}

	live := g.Live()
	if len(live.Active) != 0 {
		t.Errorf("a finished call is still reported as in flight: %+v", live.Active)
	}
	if live.Last == nil {
		t.Fatal("nothing was recorded as the last call")
	}
	if live.Last.Model != "only-model" || live.Last.Provider != "only" {
		t.Errorf("last = %s/%s, want only/only-model", live.Last.Provider, live.Last.Model)
	}
	// The caller asked for a sentinel; keeping both is what shows a choice was
	// made rather than a model being named outright.
	if live.Last.Requested != "auto" {
		t.Errorf("requested = %q, want auto", live.Last.Requested)
	}
	if live.Last.Status != "success" || live.Last.Failure != "" {
		t.Errorf("status = %q, failure = %q, want a clean success", live.Last.Status, live.Last.Failure)
	}
}

// A failover chain must report the step that actually answered. Reporting the
// last attempt outright would name the failure and hide the model the caller
// was really served by.
func TestLiveReportsTheStepThatSucceeded(t *testing.T) {
	dead := newUpstream(t, "dead", http.StatusNotFound, `{"error":{"message":"model not found"}}`)
	alive := newUpstream(t, "alive", http.StatusOK, "")

	g, keys := failoverGateway(t, map[string]*upstream{"dead": dead, "alive": alive})

	if _, err := routeWith(t, g, keys); err != nil {
		t.Fatalf("routing failed: %v", err)
	}

	last := g.Live().Last
	if last == nil {
		t.Fatal("nothing was recorded as the last call")
	}
	if last.Provider != "alive" || last.Status != "success" {
		t.Errorf("last = %s (%s), want the provider that answered", last.Provider, last.Status)
	}
}

// When everything fails there is still an answer to "what did it try", and the
// upstream's own words are worth keeping with it.
func TestLiveKeepsTheFailureWhenNothingAnswers(t *testing.T) {
	broken := newUpstream(t, "broken", http.StatusInternalServerError, `{"error":{"message":"boom"}}`)
	g, keys := failoverGateway(t, map[string]*upstream{"broken": broken})

	if _, err := routeWith(t, g, keys); err == nil {
		t.Fatal("the only provider failed but routing reported success")
	}

	last := g.Live().Last
	if last == nil {
		t.Fatal("a failed call left nothing to report")
	}
	if last.Status != "error" || last.Failure == "" {
		t.Errorf("status = %q, failure = %q, want an error with its reason",
			last.Status, last.Failure)
	}
	if len(g.Live().Active) != 0 {
		t.Error("a failed call is still reported as in flight")
	}
}

// The point of the whole file: a slow call is visible while it is still
// running, which is exactly when someone is looking.
func TestLiveShowsACallWhileItIsStillRunning(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})

	slow := &upstream{}
	slow.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","model":"slow",
			"choices":[{"index":0,"finish_reason":"stop",
			"message":{"role":"assistant","content":"eventually"}}]}`))
	}))
	defer slow.server.Close()

	g, keys := failoverGateway(t, map[string]*upstream{"slow": slow})

	done := make(chan error, 1)
	go func() {
		req := &provider.ChatRequest{
			Model:    "auto",
			Messages: []provider.Message{{Role: "user", Content: "hello"}},
		}
		cands, task := g.plan(req, keys)
		_, err := g.attemptChain(context.Background(), req, cands, keys, nil, task, nil)
		done <- err
	}()

	<-started

	live := g.Live()
	if len(live.Active) != 1 {
		t.Fatalf("in-flight calls = %d, want 1 while the upstream is still holding the request", len(live.Active))
	}
	if live.Active[0].Model != "slow-model" || live.Active[0].Provider != "slow" {
		t.Errorf("in flight = %s/%s, want slow/slow-model",
			live.Active[0].Provider, live.Active[0].Model)
	}
	if live.Active[0].StartedAt.IsZero() {
		t.Error("an in-flight call has no start time, so it cannot be shown as elapsed")
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("the slow call failed: %v", err)
	}

	// The in-flight record is cleared before the call returns, so by now it is
	// gone without any waiting.
	if got := len(g.Live().Active); got != 0 {
		t.Errorf("in-flight calls = %d after the request finished, want 0", got)
	}
}
