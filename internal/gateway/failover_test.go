package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/coderouter/coderouter/internal/config"
	"github.com/coderouter/coderouter/internal/provider"
	"github.com/coderouter/coderouter/internal/routing"
)

// upstream is a stand-in provider whose behaviour each test dictates.
type upstream struct {
	server *httptest.Server
	calls  atomic.Int64
}

// newUpstream serves /chat/completions with the given status. A 200 returns a
// completion naming the upstream, so a test can tell which one answered.
func newUpstream(t *testing.T, name string, status int, body string) *upstream {
	t.Helper()

	u := &upstream{}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		u.calls.Add(1)

		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "x", "object": "chat.completion", "model": name,
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "stop",
				"message": map[string]string{"role": "assistant", "content": "answered by " + name},
			}},
			"usage": map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	}))
	t.Cleanup(u.server.Close)
	return u
}

// failoverGateway wires a gateway to the given stand-in upstreams, one model
// each, all holding keys.
func failoverGateway(t *testing.T, ups map[string]*upstream) (*Gateway, map[string]string) {
	t.Helper()

	cfg := config.Load()
	cfg.RoutingMode = "always"

	var specs []provider.Spec
	keys := map[string]string{}
	catalog := routing.NewCatalog()

	for name, u := range ups {
		specs = append(specs, provider.Spec{
			Name: name, BaseURL: u.server.URL + "/v1", Kind: provider.KindOpenAI,
		})
		keys[name] = "test-key"
		catalog.SetDiscovered(name, []provider.DiscoveredModel{
			{Provider: name, Model: name + "-model", PriceKnown: true},
		})
	}
	cfg.Providers = specs

	g, err := New(nil, Options{Config: cfg, Catalog: catalog})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return g, keys
}

// chatRequest is a plain non-streaming request.
func chatRequest() *provider.ChatRequest {
	return &provider.ChatRequest{
		Model:    "auto",
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
	}
}

// routeWith runs the real routing loop against the stand-ins. The usage log is
// skipped by passing a nil key, which is the "unattributed caller" path.
func routeWith(t *testing.T, g *Gateway, keys map[string]string) (*provider.ChatResponse, error) {
	t.Helper()

	req := chatRequest()
	cands, task := g.plan(req, keys)
	if len(cands) == 0 {
		return nil, errors.New("no candidates")
	}
	return g.attemptChain(context.Background(), req, cands, keys, nil, task, nil)
}

// THE QUESTION THIS FILE ANSWERS: when the model a request lands on is
// unavailable, does the caller see an error, or does routing continue?
//
// It continues. An error reaches the caller only once every candidate has been
// tried and failed.
func TestUnavailableModelFallsThroughToTheNextProvider(t *testing.T) {
	dead := newUpstream(t, "dead", http.StatusNotFound, `{"error":{"message":"model not found"}}`)
	alive := newUpstream(t, "alive", http.StatusOK, "")

	g, keys := failoverGateway(t, map[string]*upstream{"dead": dead, "alive": alive})

	resp, err := routeWith(t, g, keys)
	if err != nil {
		t.Fatalf("routing gave up instead of trying the next provider: %v", err)
	}
	if got := completionText(resp); !strings.Contains(got, "alive") {
		t.Errorf("answered by the wrong upstream: %q", got)
	}
	if dead.calls.Load() != 1 {
		t.Errorf("the failing provider was called %d times, want exactly 1", dead.calls.Load())
	}
	if alive.calls.Load() != 1 {
		t.Errorf("the working provider was called %d times, want 1", alive.calls.Load())
	}
}

// The caller sees an error only when nothing is left to try, and it names
// every attempt rather than just the last.
func TestErrorOnlyAfterEveryProviderFails(t *testing.T) {
	a := newUpstream(t, "a", http.StatusNotFound, `{"error":{"message":"gone"}}`)
	b := newUpstream(t, "b", http.StatusInternalServerError, `{"error":{"message":"boom"}}`)

	g, keys := failoverGateway(t, map[string]*upstream{"a": a, "b": b})

	_, err := routeWith(t, g, keys)
	if err == nil {
		t.Fatal("every provider failed but no error was returned")
	}
	for _, want := range []string{"a", "b", "gone", "boom"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%s", want, err)
		}
	}
	if a.calls.Load() != 1 || b.calls.Load() != 1 {
		t.Errorf("attempts: a=%d b=%d, want one each", a.calls.Load(), b.calls.Load())
	}
}

// Transient upstream trouble is retried on the next provider just the same.
func TestServerErrorsAlsoFailOver(t *testing.T) {
	broken := newUpstream(t, "broken", http.StatusBadGateway, `{"error":{"message":"upstream down"}}`)
	fine := newUpstream(t, "fine", http.StatusOK, "")

	g, keys := failoverGateway(t, map[string]*upstream{"broken": broken, "fine": fine})

	resp, err := routeWith(t, g, keys)
	if err != nil {
		t.Fatalf("a 502 was not failed over: %v", err)
	}
	if got := completionText(resp); !strings.Contains(got, "fine") {
		t.Errorf("answered by %q", got)
	}
}

// Once a stream has begun the answer is committed: switching providers would
// splice two different completions together, so the error surfaces instead.
func TestStreamingCannotFailOverOnceBytesAreSent(t *testing.T) {
	partial := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// One good chunk, then the connection dies mid-stream.
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"par\"}}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler)
	}))
	t.Cleanup(partial.Close)

	rescue := newUpstream(t, "rescue", http.StatusOK, "")

	cfg := config.Load()
	cfg.RoutingMode = "always"
	cfg.Providers = []provider.Spec{
		{Name: "partial", BaseURL: partial.URL + "/v1", Kind: provider.KindOpenAI},
		{Name: "rescue", BaseURL: rescue.server.URL + "/v1", Kind: provider.KindOpenAI},
	}
	catalog := routing.NewCatalog()
	catalog.SetDiscovered("partial", []provider.DiscoveredModel{{Provider: "partial", Model: "m", PriceKnown: true}})
	catalog.SetDiscovered("rescue", []provider.DiscoveredModel{{Provider: "rescue", Model: "m2", PriceKnown: true, InputCostPer1M: 1, OutputCostPer1M: 1}})

	g, err := New(nil, Options{Config: cfg, Catalog: catalog})
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]string{"partial": "k", "rescue": "k"}

	var got strings.Builder
	req := chatRequest()
	req.Stream = true
	cands, _ := g.plan(req, keys)

	_, err = g.attemptChain(context.Background(), req, cands, keys, nil, "conversation",
		func(c *provider.ChatResponse) error {
			for _, ch := range c.Choices {
				if ch.Delta != nil {
					got.WriteString(ch.Delta.Content)
				}
			}
			return nil
		})

	if err == nil {
		t.Fatal("a stream that died mid-flight was silently rescued; the caller would receive two spliced answers")
	}
	if !strings.Contains(got.String(), "par") {
		t.Errorf("the partial chunk never reached the caller: %q", got.String())
	}
	if rescue.calls.Load() != 0 {
		t.Errorf("the second provider was called %d times after bytes were already sent", rescue.calls.Load())
	}
}

// A provider's model list routinely contains entries a given account cannot
// use. Trying only one of them per request made the first request likelier to
// fail than it needed to be — the chain now gives each provider more than one
// chance, within one request.
func TestOneRequestTriesSeveralModelsPerProvider(t *testing.T) {
	g := testGateway(t, "auto", "balanced")
	g.cfg.AttemptsPerProvider = 2

	g.catalog.SetDiscovered("p", []provider.DiscoveredModel{
		{Provider: "p", Model: "first", PriceKnown: true},
		{Provider: "p", Model: "second", PriceKnown: true},
		{Provider: "p", Model: "third", PriceKnown: true},
	})

	cands, _ := g.plan(req("auto", "hello"), keysFor("p"))
	if len(cands) != 2 {
		t.Fatalf("chain is %d deep, want 2 attempts at the one provider: %+v", len(cands), cands)
	}
	if cands[0].model != "first" || cands[1].model != "second" {
		t.Errorf("chain = %+v, want the provider's first two models", cands)
	}
}

// The chain alternates vendors before returning to one: a provider that is
// entirely down must not consume two slots before another is tried.
func TestChainInterleavesProvidersBeforeRetryingOne(t *testing.T) {
	g := testGateway(t, "auto", "balanced")
	g.cfg.AttemptsPerProvider = 2

	g.catalog.SetDiscovered("a", []provider.DiscoveredModel{
		{Provider: "a", Model: "a1", InputCostPer1M: 1, OutputCostPer1M: 1, PriceKnown: true},
		{Provider: "a", Model: "a2", InputCostPer1M: 2, OutputCostPer1M: 2, PriceKnown: true},
	})
	g.catalog.SetDiscovered("b", []provider.DiscoveredModel{
		{Provider: "b", Model: "b1", InputCostPer1M: 3, OutputCostPer1M: 3, PriceKnown: true},
		{Provider: "b", Model: "b2", InputCostPer1M: 4, OutputCostPer1M: 4, PriceKnown: true},
	})

	cands, _ := g.plan(req("auto", "hello"), keysFor("a", "b"))

	var order []string
	for _, c := range cands {
		order = append(order, c.provider)
	}
	if len(order) != 4 {
		t.Fatalf("chain = %v, want four attempts", order)
	}
	if order[0] == order[1] {
		t.Errorf("chain retries the same provider before trying another: %v", order)
	}
	if order[0] != order[2] || order[1] != order[3] {
		t.Errorf("chain is not interleaved: %v", order)
	}
}

// A deployment with many providers must not turn one request into dozens of
// upstream calls.
func TestChainIsCappedOverall(t *testing.T) {
	g := testGateway(t, "auto", "balanced")
	g.cfg.AttemptsPerProvider = 3
	g.cfg.MaxAttempts = 5

	keys := map[string]string{}
	for _, name := range []string{"a", "b", "c", "d"} {
		var models []provider.DiscoveredModel
		for i := 0; i < 3; i++ {
			models = append(models, provider.DiscoveredModel{
				Provider: name, Model: fmt.Sprintf("%s%d", name, i), PriceKnown: true,
			})
		}
		g.catalog.SetDiscovered(name, models)
		keys[name] = "k"
	}

	cands, _ := g.plan(req("auto", "hello"), keys)
	if len(cands) != 5 {
		t.Errorf("chain is %d long, want the cap of 5", len(cands))
	}
}

// End to end: a provider whose first model is unavailable is retried on its
// second within the same request, so the caller never sees the failure.
func TestSecondModelAtTheSameProviderRescuesTheRequest(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		calls.Add(1)

		// Only the second model works for this account.
		if body.Model != "good" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"model not available to your account"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "x", "object": "chat.completion", "model": "good",
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "stop",
				"message": map[string]string{"role": "assistant", "content": "rescued"},
			}},
		})
	}))
	t.Cleanup(srv.Close)

	cfg := config.Load()
	cfg.RoutingMode = "always"
	cfg.AttemptsPerProvider = 2
	cfg.Providers = []provider.Spec{{Name: "solo", BaseURL: srv.URL + "/v1", Kind: provider.KindOpenAI}}

	catalog := routing.NewCatalog()
	catalog.SetDiscovered("solo", []provider.DiscoveredModel{
		{Provider: "solo", Model: "retired", PriceKnown: true},
		{Provider: "solo", Model: "good", InputCostPer1M: 1, OutputCostPer1M: 1, PriceKnown: true},
	})

	g, err := New(nil, Options{Config: cfg, Catalog: catalog})
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]string{"solo": "k"}

	resp, err := routeWith(t, g, keys)
	if err != nil {
		t.Fatalf("one bad model at the only provider failed the whole request: %v", err)
	}
	if got := completionText(resp); got != "rescued" {
		t.Errorf("content = %q", got)
	}
	if calls.Load() != 2 {
		t.Errorf("made %d upstream calls, want 2 (the retired model then the good one)", calls.Load())
	}
}

// The attempt cap bounds retries; it must never stop a provider being tried at
// all. A deployment with more providers than the cap was silently never
// reaching the ones at the end of the list.
func TestEveryProviderIsTriedEvenBeyondTheCap(t *testing.T) {
	g := testGateway(t, "auto", "balanced")
	g.cfg.AttemptsPerProvider = 2
	g.cfg.MaxAttempts = 8

	keys := map[string]string{}
	names := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k"}
	for _, n := range names {
		g.catalog.SetDiscovered(n, []provider.DiscoveredModel{
			{Provider: n, Model: n + "-1", PriceKnown: true},
			{Provider: n, Model: n + "-2", PriceKnown: true},
		})
		keys[n] = "k"
	}

	cands, _ := g.plan(chatRequest(), keys)

	reached := map[string]bool{}
	for _, c := range cands {
		reached[c.provider] = true
	}
	for _, n := range names {
		if !reached[n] {
			t.Errorf("provider %q was never tried; the cap excluded it entirely", n)
		}
	}
	// Eleven providers means at least eleven attempts, cap of eight or not.
	if len(cands) < len(names) {
		t.Errorf("chain is %d long for %d providers", len(cands), len(names))
	}
}

// With fewer providers than the cap it still bounds retries as before.
func TestCapStillBoundsRetriesWhenProvidersAreFew(t *testing.T) {
	g := testGateway(t, "auto", "balanced")
	g.cfg.AttemptsPerProvider = 3
	g.cfg.MaxAttempts = 4

	keys := map[string]string{}
	for _, n := range []string{"a", "b"} {
		var models []provider.DiscoveredModel
		for i := 0; i < 3; i++ {
			models = append(models, provider.DiscoveredModel{
				Provider: n, Model: fmt.Sprintf("%s-%d", n, i), PriceKnown: true,
			})
		}
		g.catalog.SetDiscovered(n, models)
		keys[n] = "k"
	}

	if got := len(g.mustPlan(t, keys)); got != 4 {
		t.Errorf("chain is %d long, want the cap of 4", got)
	}
}

// mustPlan is a small helper for the assertion above.
func (g *Gateway) mustPlan(t *testing.T, keys map[string]string) []candidate {
	t.Helper()
	cands, _ := g.plan(chatRequest(), keys)
	return cands
}

// A probe result is evidence about this account, so a confirmed model should
// be reached before one nothing is known about — even a cheaper one.
func TestConfirmedModelsAreTriedFirst(t *testing.T) {
	g := testGateway(t, "auto", "cost")
	g.cfg.AttemptsPerProvider = 3

	g.catalog.SetDiscovered("p", []provider.DiscoveredModel{
		{Provider: "p", Model: "cheap-unknown", PriceKnown: true},
		{Provider: "p", Model: "dearer-confirmed", InputCostPer1M: 4, OutputCostPer1M: 4, PriceKnown: true},
	})

	// Before any probe, price decides.
	cands, _ := g.plan(chatRequest(), keysFor("p"))
	if len(cands) == 0 || cands[0].model != "cheap-unknown" {
		t.Fatalf("before probing, chain = %+v", cands)
	}

	g.catalog.SetConfirmed(map[string]bool{
		routing.TagKey("p", "dearer-confirmed"): true,
	})

	cands, _ = g.plan(chatRequest(), keysFor("p"))
	if len(cands) == 0 || cands[0].model != "dearer-confirmed" {
		t.Errorf("chain leads with %+v; a confirmed model should come first", cands)
	}
	// The unconfirmed one is kept as a fallback, not discarded: not probed is
	// not the same as broken.
	if len(cands) < 2 {
		t.Error("the unprobed model was dropped rather than kept as a fallback")
	}
}

// Until a sweep has run there is nothing to prefer, and treating everything as
// unconfirmed must not change the order.
func TestUnprobedDeploymentsAreUnaffected(t *testing.T) {
	c := routing.NewCatalog()
	pool := []routing.ModelProfile{
		{Provider: "a", Model: "one"}, {Provider: "b", Model: "two"},
	}

	got := c.PreferConfirmed(pool)
	if len(got) != 2 || got[0].Model != "one" {
		t.Errorf("an unprobed catalogue was reordered: %+v", got)
	}
}

// A probe that succeeds clears an earlier sidelining: entitlements change, and
// a model that has just answered is working whatever happened an hour ago.
func TestASuccessfulProbeClearsTheBench(t *testing.T) {
	g := testGateway(t, "auto", "balanced")
	c := candidate{provider: "p", model: "recovered"}

	g.benchIfAccessDenied(c, errors.New("p returned 403: no credit"))
	if !g.benched(c) {
		t.Fatal("precondition: the model should be benched")
	}

	g.unbench(c)
	if g.benched(c) {
		t.Error("a model shown to work is still sidelined")
	}
}

// The sweep is bounded: probing every model at every provider would cost real
// money and burn the free allowances it exists to protect.
func TestProbeTargetsAreBounded(t *testing.T) {
	g := testGateway(t, "auto", "balanced")
	g.cfg.ProbeModelsPerProvider = 2

	keys := map[string]string{}
	for _, name := range []string{"a", "b", "c"} {
		var models []provider.DiscoveredModel
		for i := 0; i < 50; i++ {
			models = append(models, provider.DiscoveredModel{
				Provider: name, Model: fmt.Sprintf("%s-%02d", name, i), PriceKnown: true,
			})
		}
		g.catalog.SetDiscovered(name, models)
		keys[name] = "k"
	}

	targets := g.probeTargets(keys)
	if len(targets) != 6 {
		t.Errorf("probing %d models across 3 providers, want 2 each", len(targets))
	}

	counts := map[string]int{}
	for _, c := range targets {
		counts[c.provider]++
	}
	for name, n := range counts {
		if n != 2 {
			t.Errorf("provider %s got %d probes", name, n)
		}
	}
}

// Marked models are probed whatever their ranking: routing is restricted to
// them, so their health matters more than anything else's.
func TestMarkedModelsAreAlwaysProbed(t *testing.T) {
	g := testGateway(t, "auto", "balanced")
	g.cfg.ProbeModelsPerProvider = 1

	var models []provider.DiscoveredModel
	for i := 0; i < 20; i++ {
		models = append(models, provider.DiscoveredModel{
			Provider: "p", Model: fmt.Sprintf("m-%02d", i), PriceKnown: true,
		})
	}
	g.catalog.SetDiscovered("p", models)
	// Mark one buried deep in the list.
	g.catalog.SetTags(map[string][]routing.TaskType{
		routing.TagKey("p", "m-17"): {routing.TaskCodeGeneration},
	})

	var found bool
	for _, c := range g.probeTargets(keysFor("p")) {
		if c.model == "m-17" {
			found = true
		}
	}
	if !found {
		t.Error("a marked model was not probed, though routing is restricted to it")
	}
}

// A provider with no key is never probed — there is nothing to probe with.
func TestUnkeyedProvidersAreNotProbed(t *testing.T) {
	g := testGateway(t, "auto", "balanced")

	g.catalog.SetDiscovered("keyed", []provider.DiscoveredModel{{Provider: "keyed", Model: "m"}})
	g.catalog.SetDiscovered("unkeyed", []provider.DiscoveredModel{{Provider: "unkeyed", Model: "m"}})

	for _, c := range g.probeTargets(keysFor("keyed")) {
		if c.provider == "unkeyed" {
			t.Error("probed a provider with no key")
		}
	}
}

// An empty chain is exactly when an explanation matters most. Returning a bare
// empty list left an operator to guess between "no keys", "marks too narrow"
// and "everything is sidelined".
func TestPlanExplainsAnEmptyChain(t *testing.T) {
	g := testGateway(t, "auto", "balanced")

	// Marked for chat, but the marked model has been sidelined.
	g.catalog.SetDiscovered("openai", []provider.DiscoveredModel{
		{Provider: "openai", Model: "only-one", PriceKnown: true},
	})
	g.catalog.SetTags(map[string][]routing.TaskType{
		routing.TagKey("openai", "only-one"): {routing.TaskConversation},
	})
	g.benchIfAccessDenied(candidate{provider: "openai", model: "only-one"},
		errors.New("openai returned 403: not entitled"))

	// unroutable is what Plan reports as the reason; exercised directly so the
	// test needs no database behind it.
	keys := map[string]string{"openai": "k"}
	if cands, _ := g.plan(req("auto:chat", "hi"), keys); len(cands) != 0 {
		t.Fatalf("expected an empty chain, got %+v", cands)
	}

	reason := g.unroutable(req("auto:chat", "hi"), keys).Error()
	if !strings.Contains(reason, "marked") {
		t.Errorf("the explanation does not point at the marks:\n%s", reason)
	}
}

// A chain that is fine carries no reason, so the field means something.
func TestPlanOnAKeylessGatewayExplainsItself(t *testing.T) {
	g := testGateway(t, "auto", "balanced")
	g.catalog.SetDiscovered("openai", []provider.DiscoveredModel{
		{Provider: "openai", Model: "works", PriceKnown: true},
	})

	// With no database there are no stored keys, so Plan reports an empty
	// chain and says why rather than dereferencing nil.
	steps, _, reason, err := g.Plan("auto")
	if err != nil {
		t.Fatalf("Plan on a keyless gateway errored: %v", err)
	}
	if len(steps) != 0 {
		t.Errorf("a gateway with no keys produced a chain: %+v", steps)
	}
	if reason == "" {
		t.Error("an empty chain came back with no explanation")
	}
}
