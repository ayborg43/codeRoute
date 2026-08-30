package gateway

import (
	"errors"
	"strings"
	"testing"

	"github.com/coderouter/coderouter/internal/config"
	"github.com/coderouter/coderouter/internal/provider"
	"github.com/coderouter/coderouter/internal/routing"
)

// testGateway builds a gateway for planning tests; plan() never touches the DB.
func testGateway(t *testing.T, mode, objective string) *Gateway {
	t.Helper()

	cfg := config.Load()
	cfg.RoutingMode = mode
	cfg.RoutingObjective = objective
	cfg.RouterModel = "auto"
	gw, err := New(nil, Options{Config: cfg})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return gw
}

// allKeys pretends every configured provider has a key, so planning tests see
// the full candidate chain rather than an empty one.
func allKeys(g *Gateway) map[string]string {
	keys := map[string]string{}
	for _, name := range g.ProviderNames() {
		keys[name] = "test-key"
	}
	return keys
}

func req(model, prompt string) *provider.ChatRequest {
	return &provider.ChatRequest{
		Model:    model,
		Messages: []provider.Message{{Role: "user", Content: provider.Content(prompt)}},
	}
}

func TestPlanHonoursAnExplicitModel(t *testing.T) {
	g := testGateway(t, "auto", "balanced")

	cands, task := g.plan(req("claude-3-5-sonnet-20241022", "refactor this function"), allKeys(g))

	if len(cands) == 0 || cands[0].model != "claude-3-5-sonnet-20241022" {
		t.Fatalf("explicit model was not honoured: %+v", cands)
	}
	if cands[0].provider != "anthropic" {
		t.Errorf("provider = %q, want anthropic", cands[0].provider)
	}
	// The task is still detected, for usage analytics.
	if task != routing.TaskCodeGeneration {
		t.Errorf("task = %q, want code_generation", task)
	}
}

func TestPlanRoutesSmartlyForTheRouterModel(t *testing.T) {
	g := testGateway(t, "auto", "cost")

	cands, _ := g.plan(req("auto", "chat with me"), allKeys(g))

	if len(cands) == 0 {
		t.Fatal("smart routing produced no candidates")
	}
	if cands[0].model == "auto" {
		t.Fatal("the router sentinel leaked through as a real model")
	}

	// Cheapest-first for a conversation task.
	ranked := g.catalog.Rank(routing.TaskConversation, routing.ObjectiveCost)
	if cands[0].model != ranked[0].Model {
		t.Errorf("primary = %q, want cheapest %q", cands[0].model, ranked[0].Model)
	}
}

func TestPlanUsesSmartRoutingWhenNoModelGiven(t *testing.T) {
	g := testGateway(t, "auto", "latency")

	cands, _ := g.plan(req("", "hello there"), allKeys(g))

	if len(cands) == 0 || cands[0].model == "" {
		t.Fatalf("no candidates for an empty model: %+v", cands)
	}
	ranked := g.catalog.Rank(routing.TaskConversation, routing.ObjectiveLatency)
	if cands[0].model != ranked[0].Model {
		t.Errorf("primary = %q, want fastest %q", cands[0].model, ranked[0].Model)
	}
}

func TestAlwaysModeOverridesTheClientsModel(t *testing.T) {
	g := testGateway(t, "always", "cost")

	cands, _ := g.plan(req("gpt-4o", "write me a story"), allKeys(g))

	ranked := g.catalog.Rank(routing.TaskCreative, routing.ObjectiveCost)
	if cands[0].model != ranked[0].Model {
		t.Errorf("primary = %q, want %q; always mode must override", cands[0].model, ranked[0].Model)
	}
}

func TestOffModeFallsBackToStaticChain(t *testing.T) {
	g := testGateway(t, "off", "balanced")

	cands, _ := g.plan(req("", "anything"), allKeys(g))

	if cands[0].model != g.cfg.DefaultModel {
		t.Errorf("primary = %q, want default %q", cands[0].model, g.cfg.DefaultModel)
	}
}

func TestCandidateChainHasOneEntryPerProvider(t *testing.T) {
	g := testGateway(t, "always", "balanced")

	cands, _ := g.plan(req("", "implement a parser"), allKeys(g))

	seen := make(map[string]bool)
	for _, c := range cands {
		if seen[c.provider] {
			t.Fatalf("provider %q appears twice; failover would retry a failed provider", c.provider)
		}
		seen[c.provider] = true
	}
	if len(cands) < 2 {
		t.Errorf("chain has %d entries; failover needs at least two providers", len(cands))
	}
}

// keysFor pretends only the named providers hold a key.
func keysFor(names ...string) map[string]string {
	keys := map[string]string{}
	for _, n := range names {
		keys[n] = "test-key"
	}
	return keys
}

// The curated catalogue covers three vendors. A deployment keyed on any of the
// other fourteen must still be able to ask for automatic routing — otherwise
// "auto" is unroutable for most of the providers the gateway ships.
func TestAutoFallsBackToDiscoveredModels(t *testing.T) {
	g := testGateway(t, "auto", "balanced")

	g.catalog.SetDiscovered("groq", []provider.DiscoveredModel{
		{Provider: "groq", Model: "llama-3.3-70b", InputCostPer1M: 0.6, OutputCostPer1M: 0.8, PriceKnown: true},
		{Provider: "groq", Model: "llama-3.1-8b", InputCostPer1M: 0.05, OutputCostPer1M: 0.08, PriceKnown: true},
	})

	cands, _ := g.plan(req("auto", "hello"), keysFor("groq"))
	if len(cands) == 0 {
		t.Fatal("auto produced no candidates for a provider outside the curated catalogue")
	}
	if cands[0].provider != "groq" {
		t.Errorf("primary = %+v, want groq", cands[0])
	}
	// Price is the only axis discovered models offer, so the cheaper one wins.
	if cands[0].model != "llama-3.1-8b" {
		t.Errorf("primary model = %q, want the cheaper llama-3.1-8b", cands[0].model)
	}
}

// The curated catalogue still wins where its providers are keyed: those entries
// are task-aware and latency-aware, which discovered ones are not.
func TestAutoPrefersTheCuratedCatalogue(t *testing.T) {
	g := testGateway(t, "auto", "cost")

	g.catalog.SetDiscovered("groq", []provider.DiscoveredModel{
		{Provider: "groq", Model: "llama-3.1-8b", InputCostPer1M: 0.001, OutputCostPer1M: 0.001, PriceKnown: true},
	})

	cands, _ := g.plan(req("auto", "chat with me"), keysFor("openai", "groq"))
	if len(cands) == 0 {
		t.Fatal("no candidates")
	}
	if cands[0].provider == "groq" {
		t.Errorf("a discovered model displaced the curated catalogue: %+v", cands[0])
	}
}

// An unstated price is not a zero one: a genuinely cheap model must outrank an
// unpriced one, while the unpriced one is kept as a fallback rather than
// discarded.
func TestDiscoveredFallbackRanksUnpricedFairly(t *testing.T) {
	g := testGateway(t, "auto", "balanced")
	g.cfg.AttemptsPerProvider = 1

	g.catalog.SetDiscovered("a", []provider.DiscoveredModel{
		{Provider: "a", Model: "mystery"},
	})
	g.catalog.SetDiscovered("b", []provider.DiscoveredModel{
		{Provider: "b", Model: "cheap", InputCostPer1M: 0.2, OutputCostPer1M: 0.2, PriceKnown: true},
	})

	cands, _ := g.plan(req("auto", "hello"), keysFor("a", "b"))
	if len(cands) != 2 {
		t.Fatalf("got %d candidates, want one per provider", len(cands))
	}
	if cands[0].model != "cheap" {
		t.Errorf("primary = %q, want the known-cheap model first", cands[0].model)
	}
	if cands[1].model != "mystery" {
		t.Errorf("the unpriced model was not kept as a fallback: %+v", cands)
	}
}

// Failover needs more than one provider keyed; this documents that the chain
// length tracks the number of usable providers.
func TestFailoverChainSpansProviders(t *testing.T) {
	g := testGateway(t, "auto", "balanced")

	if got := len(g.filterToKeyed(g.rankedCandidates("conversation", sentinelIntent{}), keysFor("openai"))); got != 1 {
		t.Errorf("one keyed provider gave %d candidates, want 1 — there is nothing to fail over to", got)
	}
	if got := len(g.filterToKeyed(g.rankedCandidates("conversation", sentinelIntent{}), keysFor("openai", "anthropic", "google"))); got < 2 {
		t.Errorf("three keyed providers gave %d candidates; failover needs at least two", got)
	}
}

func TestSummarizeFailuresListsEveryProvider(t *testing.T) {
	err := summarizeFailures([]attemptFailure{
		{candidate{provider: "xkiro", model: "deepseek/deepseek-chat-v3.1"}, "xkiro returned 401: invalid key"},
		{candidate{provider: "bai", model: "claude-fable-5"}, "bai returned 403: Deposit required"},
	})

	msg := err.Error()
	// Reporting only the last failure hid the first one entirely, however many
	// times the request was retried.
	for _, want := range []string{"xkiro", "deepseek/deepseek-chat-v3.1", "invalid key",
		"bai", "claude-fable-5", "Deposit required", "all 2 providers failed"} {
		if !strings.Contains(msg, want) {
			t.Errorf("summary is missing %q:\n%s", want, msg)
		}
	}
}

func TestSummarizeFailuresStaysReadableForOne(t *testing.T) {
	err := summarizeFailures([]attemptFailure{
		{candidate{provider: "groq", model: "llama"}, "groq returned 429: slow down"},
	})
	if got := err.Error(); strings.Contains(got, "•") || !strings.Contains(got, "groq (llama)") {
		t.Errorf("single failure reads badly: %s", got)
	}

	if err := summarizeFailures(nil); !errors.Is(err, ErrNoProvider) {
		t.Errorf("no attempts = %v, want ErrNoProvider", err)
	}
}

// One verbose upstream body must not bury the other providers' reasons.
func TestSummarizeFailuresTruncatesLongBodies(t *testing.T) {
	long := strings.Repeat("x", 2000)
	err := summarizeFailures([]attemptFailure{
		{candidate{provider: "a", model: "m"}, long},
		{candidate{provider: "b", model: "n"}, "short reason"},
	})

	msg := err.Error()
	if len(msg) > 900 {
		t.Errorf("summary is %d chars; one long body swamped it", len(msg))
	}
	if !strings.Contains(msg, "short reason") {
		t.Error("the second provider's reason was lost")
	}
}

// A model the account may not use is skipped next time, so automatic routing
// stops burning a round trip on it every request.
func TestAccessDeniedModelsAreBenched(t *testing.T) {
	g := testGateway(t, "auto", "balanced")
	c := candidate{provider: "bai", model: "claude-fable-5"}

	if g.benched(c) {
		t.Fatal("a fresh model is already benched")
	}

	g.benchIfAccessDenied(c, errors.New(`bai returned 403: {"message":"Deposit required"}`))
	if !g.benched(c) {
		t.Error("a 403 did not bench the model")
	}

	// A different model at the same provider is unaffected: an account is
	// typically entitled to some of an aggregator's models but not all.
	if g.benched(candidate{provider: "bai", model: "something-else"}) {
		t.Error("benching one model took out the whole provider")
	}
}

func TestTransientFailuresAreNotBenched(t *testing.T) {
	g := testGateway(t, "auto", "balanced")
	c := candidate{provider: "groq", model: "llama"}

	for _, transient := range []string{
		"groq returned 429: rate limited",
		"groq returned 500: internal error",
		"groq returned 503: overloaded",
		"context deadline exceeded",
	} {
		g.benchIfAccessDenied(c, errors.New(transient))
		if g.benched(c) {
			t.Errorf("%q benched a model that would have recovered", transient)
			g.ClearBench()
		}
	}
}

func TestClearBenchRestoresEverything(t *testing.T) {
	g := testGateway(t, "auto", "balanced")
	c := candidate{provider: "bai", model: "premium"}

	g.benchIfAccessDenied(c, errors.New("bai returned 403: nope"))
	g.ClearBench()

	if g.benched(c) {
		t.Error("ClearBench left a model sidelined; a new key must get a clean slate")
	}
}

// A 502 tells most clients to retry. When every provider refused on
// entitlement, retrying is pointless and the message has to say so.
func TestSummarizeFailuresFlagsPermanentRefusals(t *testing.T) {
	permanent := summarizeFailures([]attemptFailure{
		{candidate{provider: "bai", model: "m"}, "bai returned 403: Deposit required"},
		{candidate{provider: "xkiro", model: "n"}, "xkiro returned 401: invalid key"},
	}).Error()
	if !strings.Contains(permanent, "retrying will not help") {
		t.Errorf("permanent failure invites a pointless retry:\n%s", permanent)
	}

	// One transient failure among them means a retry might work after all.
	mixed := summarizeFailures([]attemptFailure{
		{candidate{provider: "bai", model: "m"}, "bai returned 403: Deposit required"},
		{candidate{provider: "groq", model: "n"}, "groq returned 503: overloaded"},
	}).Error()
	if strings.Contains(mixed, "retrying will not help") {
		t.Errorf("a transient failure was reported as permanent:\n%s", mixed)
	}
}

// Being out of money or out of daily allowance is not the same as going too
// fast. The first is not fixed by an immediate retry; the second is.
func TestExhaustionIsBenchedButRateLimitingIsNot(t *testing.T) {
	permanent := []string{
		`bai returned 403: {"message":"Deposit required to unlock premium models"}`,
		`teamorouter returned 400: {"type":"insufficient_balance"}`,
		`teamorouter returned 400: {"message":"钱包余额不足"}`,
		`xkiro returned 429: {"message":"You've reached today's free-model token quota"}`,
		`openai returned 429: {"message":"You exceeded your current quota, check your billing"}`,
	}
	for _, msg := range permanent {
		if !isAccessDenied(errors.New(msg)) {
			t.Errorf("not treated as exhaustion, so it will be retried every request:\n  %s", msg)
		}
	}

	transient := []string{
		`groq returned 429: {"message":"Rate limit reached for model llama, try again in 20s"}`,
		`groq returned 429: {"message":"Too many requests, please slow down"}`,
		`openai returned 503: {"message":"The engine is overloaded"}`,
		`openai returned 500: internal error`,
		"context deadline exceeded",
	}
	for _, msg := range transient {
		if isAccessDenied(errors.New(msg)) {
			t.Errorf("a recoverable failure was benched:\n  %s", msg)
		}
	}
}

// When a provider's cheapest model is out of allowance, routing must move to
// that provider's next model — not drop the provider from the chain.
func TestBenchedModelPromotesTheNextOneAtTheSameProvider(t *testing.T) {
	g := testGateway(t, "auto", "balanced")

	g.catalog.SetDiscovered("xkiro", []provider.DiscoveredModel{
		{Provider: "xkiro", Model: "free-one", InputCostPer1M: 0, OutputCostPer1M: 0, PriceKnown: true},
		{Provider: "xkiro", Model: "paid-one", InputCostPer1M: 1, OutputCostPer1M: 2, PriceKnown: true},
	})
	keys := keysFor("xkiro")

	cands, _ := g.plan(req("auto", "hello"), keys)
	if len(cands) == 0 || cands[0].model != "free-one" {
		t.Fatalf("expected the free model first, got %+v", cands)
	}

	// The free allowance runs out.
	g.benchIfAccessDenied(cands[0], errors.New(`xkiro returned 429: reached today's free-model token quota`))

	cands, _ = g.plan(req("auto", "hello"), keys)
	if len(cands) == 0 {
		t.Fatalf("provider dropped out of the chain entirely: %+v", cands)
	}
	if cands[0].model != "paid-one" {
		t.Errorf("primary = %q, want the provider's next model", cands[0].model)
	}
	for _, c := range cands {
		if c.model == "free-one" {
			t.Errorf("the benched model is still in the chain: %+v", cands)
		}
	}
}

// The same promotion applies to free-only routing across providers.
func TestFreeRoutingStepsOverBenchedModels(t *testing.T) {
	g := testGateway(t, "auto", "balanced")

	g.catalog.SetDiscovered("a", []provider.DiscoveredModel{
		{Provider: "a", Model: "spent", PriceKnown: true},
		{Provider: "a", Model: "spare", PriceKnown: true},
	})

	cands, _ := g.plan(req("auto:free", "hello"), keysFor("a"))
	if len(cands) == 0 {
		t.Fatalf("got %+v", cands)
	}
	first := cands[0]

	// A plain entitlement refusal concerns one model, so the provider's other
	// free models stay in play.
	g.benchIfAccessDenied(first, errors.New("a returned 403: not entitled"))
	cands, _ = g.plan(req("auto:free", "hello"), keysFor("a"))

	if len(cands) == 0 {
		t.Fatalf("free routing lost the provider: %+v", cands)
	}
	for _, c := range cands {
		if c.model == first.model {
			t.Errorf("free routing kept offering the benched model: %+v", cands)
		}
	}
}

// A free allowance is a shared pool. One "free quota spent" must set aside the
// provider's whole free list, or routing walks it one failed request at a time.
func TestSpentFreeAllowanceBenchesTheWholeFreePool(t *testing.T) {
	g := testGateway(t, "auto", "balanced")

	g.catalog.SetDiscovered("xkiro", []provider.DiscoveredModel{
		{Provider: "xkiro", Model: "free-a", PriceKnown: true},
		{Provider: "xkiro", Model: "free-b", PriceKnown: true},
		{Provider: "xkiro", Model: "free-c", PriceKnown: true},
		{Provider: "xkiro", Model: "paid", InputCostPer1M: 1, OutputCostPer1M: 2, PriceKnown: true},
	})

	failed := candidate{provider: "xkiro", model: "free-a"}
	g.benchIfAccessDenied(failed, errors.New(
		`xkiro returned 429: {"message":"You've reached today's free-model token quota"}`))

	for _, m := range []string{"free-a", "free-b", "free-c"} {
		if !g.benched(candidate{provider: "xkiro", model: m}) {
			t.Errorf("%s is still offered after the free allowance was reported spent", m)
		}
	}
	// The paid model is a different budget and must survive.
	if g.benched(candidate{provider: "xkiro", model: "paid"}) {
		t.Error("the paid model was benched too; that is a separate allowance")
	}

	// So the very next request reaches the paid model rather than free-b.
	cands, _ := g.plan(req("auto", "hello"), keysFor("xkiro"))
	if len(cands) != 1 || cands[0].model != "paid" {
		t.Errorf("next attempt = %+v, want the paid model", cands)
	}
}

// A paid model refused for entitlement must not drag the free pool down.
func TestBenchingAPaidModelLeavesFreeOnesAlone(t *testing.T) {
	g := testGateway(t, "auto", "balanced")

	g.catalog.SetDiscovered("bai", []provider.DiscoveredModel{
		{Provider: "bai", Model: "premium", InputCostPer1M: 9, OutputCostPer1M: 9, PriceKnown: true},
		{Provider: "bai", Model: "gratis", PriceKnown: true},
	})

	g.benchIfAccessDenied(candidate{provider: "bai", model: "premium"},
		errors.New(`bai returned 403: Deposit required`))

	if g.benched(candidate{provider: "bai", model: "gratis"}) {
		t.Error("a premium refusal benched the free model as well")
	}
}

// The curated catalogue is hand-written and vendors retire models. Once a
// provider has been discovered, an entry it no longer lists must leave the
// chain — otherwise every request spends an attempt earning a 404.
func TestRetiredCuratedModelsLeaveTheChain(t *testing.T) {
	g := testGateway(t, "auto", "balanced")

	// Google no longer serves gemini-1.5-flash; it serves something newer.
	g.catalog.SetDiscovered("google", []provider.DiscoveredModel{
		{Provider: "google", Model: "gemini-9.9-flash"},
	})

	cands, _ := g.plan(req("auto", "chat with me"), keysFor("google"))
	for _, c := range cands {
		if c.model == "gemini-1.5-flash" || c.model == "gemini-1.5-pro" {
			t.Errorf("a retired model is still in the chain: %+v", c)
		}
	}
	if len(cands) == 0 {
		t.Fatal("dropping the retired entries left nothing; it should fall through to discovery")
	}
	if cands[0].model != "gemini-9.9-flash" {
		t.Errorf("primary = %q, want the model the provider actually lists", cands[0].model)
	}
}

// A provider that has not been discovered yet keeps its curated entries: no
// information is not the same as a negative.
func TestUndiscoveredProvidersKeepCuratedEntries(t *testing.T) {
	g := testGateway(t, "auto", "balanced")

	cands, _ := g.plan(req("auto", "chat with me"), keysFor("openai"))
	if len(cands) == 0 {
		t.Fatal("an undiscovered provider lost its curated models")
	}
	if cands[0].provider != "openai" {
		t.Errorf("primary = %+v", cands[0])
	}
}

// Benching every curated candidate must fall through to discovery rather than
// leaving the request with nothing to try.
func TestBenchingAllCuratedFallsThroughToDiscovery(t *testing.T) {
	g := testGateway(t, "auto", "balanced")

	g.catalog.SetDiscovered("google", []provider.DiscoveredModel{
		{Provider: "google", Model: "gemini-1.5-flash"}, // still listed, so curated stands
		{Provider: "google", Model: "gemini-fresh"},
	})
	keys := keysFor("google")

	cands, _ := g.plan(req("auto", "chat with me"), keys)
	if len(cands) == 0 {
		t.Fatal("no candidates to begin with")
	}
	for _, c := range cands {
		g.benchIfAccessDenied(c, errors.New(c.provider+" returned 403: nope"))
	}

	cands, _ = g.plan(req("auto", "chat with me"), keys)
	if len(cands) == 0 {
		t.Fatal("every curated candidate was benched and routing gave up instead of using discovery")
	}
	if cands[0].model != "gemini-fresh" {
		t.Errorf("fell through to %q, want the un-benched discovered model", cands[0].model)
	}
}

// A model that cannot do chat is set aside, but an ordinary bad request must
// not bench a perfectly good model — in automatic routing the same body goes
// to every candidate, so that would take out the whole chain.
func TestWrongKindOfModelIsBenchedButBadRequestsAreNot(t *testing.T) {
	capability := []string{
		`google returned 400: {"message":"This model only supports Interactions API."}`,
		`openai returned 400: {"message":"This model does not support chat completions"}`,
		`x returned 400: {"message":"not supported for this endpoint"}`,
	}
	for _, msg := range capability {
		if !isAccessDenied(errors.New(msg)) {
			t.Errorf("a non-chat model was not set aside:\n  %s", msg)
		}
	}

	ordinary := []string{
		`openai returned 400: {"message":"'messages' must contain at least one item"}`,
		`openai returned 400: {"message":"Invalid value for 'temperature'"}`,
		`groq returned 400: {"message":"max_tokens is too large"}`,
	}
	for _, msg := range ordinary {
		if isAccessDenied(errors.New(msg)) {
			t.Errorf("a malformed request benched a working model:\n  %s", msg)
		}
	}
}

// The circuit breaker is about provider health. A provider whose model list
// contains entries this account cannot use is not itself unhealthy, and
// counting those would take the whole provider out over its own catalogue.
func TestEntitlementFailuresDoNotTripTheBreaker(t *testing.T) {
	g := testGateway(t, "auto", "balanced")

	for i := 0; i < 10; i++ {
		if !isAccessDenied(errors.New("google returned 404: model not available")) {
			t.Fatal("precondition: a 404 should count as access denied")
		}
	}
	// Ten entitlement failures, none recorded against the provider.
	if !g.allow("google") {
		t.Error("the provider was put in cooldown by model-level refusals")
	}

	// Genuine provider trouble still trips it.
	for i := 0; i < g.cfg.BreakerTrip; i++ {
		g.recordFailure("google")
	}
	if g.allow("google") {
		t.Error("repeated genuine failures did not trip the breaker")
	}
}

// A provider's list mixes chat models with speech, image and research
// endpoints. "auto" should reach for a general one first.
func TestGeneralModelsSortAheadOfSpecialisedOnes(t *testing.T) {
	g := testGateway(t, "auto", "balanced")

	g.catalog.SetDiscovered("google", []provider.DiscoveredModel{
		{Provider: "google", Model: "deep-research-max-preview"},
		{Provider: "google", Model: "gemini-2.5-flash-preview-tts"},
		{Provider: "google", Model: "gemini-2.5-computer-use-preview"},
		{Provider: "google", Model: "gemini-2.5-pro"},
	})

	cands, _ := g.plan(req("auto", "hello"), keysFor("google"))
	if len(cands) == 0 {
		t.Fatal("no candidates")
	}
	if cands[0].model != "gemini-2.5-pro" {
		t.Errorf("primary = %q, want the general-purpose model", cands[0].model)
	}
}

func TestSpecialisedDetection(t *testing.T) {
	for _, m := range []string{
		"gemini-2.5-flash-preview-tts", "deep-research-pro", "gpt-4o-realtime",
		"text-embedding-3-small", "whisper-large-v3", "gemini-2.5-computer-use",
		"llama-guard-4", "gemini-live-2.5-flash",
	} {
		if !specialised(m) {
			t.Errorf("%q was not recognised as specialised", m)
		}
	}
	for _, m := range []string{
		"gemini-2.5-pro", "gpt-4o-mini", "claude-haiku-4.5",
		"llama-3.3-70b-versatile", "deepseek-chat-v3.1",
	} {
		if specialised(m) {
			t.Errorf("%q was wrongly treated as specialised", m)
		}
	}
}

// Naming a model is an instruction. If the account cannot use it, the caller
// should see the provider's own error, not a stale note from a past attempt.
func TestNamedModelsBypassTheBench(t *testing.T) {
	g := testGateway(t, "auto", "balanced")
	g.catalog.SetDiscovered("openrouter", []provider.DiscoveredModel{
		{Provider: "openrouter", Model: "deepseek/deepseek-chat"},
	})

	pinned := candidate{provider: "openrouter", model: "deepseek/deepseek-chat"}
	g.benchIfAccessDenied(pinned, errors.New("openrouter returned 403: nope"))

	cands, _ := g.plan(req("openrouter:deepseek/deepseek-chat", "hi"), keysFor("openrouter"))
	if len(cands) == 0 || cands[0] != pinned {
		t.Fatalf("a benched model that was explicitly named was not attempted: %+v", cands)
	}
}

// The fallbacks appended after the caller's model are the gateway's own
// choice, so a retired one must not occupy a slot in the chain.
func TestStaleFallbacksAreScreenedOut(t *testing.T) {
	g := testGateway(t, "off", "balanced")
	g.cfg.FallbackModels = []string{"gemini-1.5-flash"}

	// Google no longer lists gemini-1.5-flash.
	g.catalog.SetDiscovered("google", []provider.DiscoveredModel{
		{Provider: "google", Model: "gemini-3-flash-preview"},
	})
	g.catalog.SetDiscovered("openrouter", []provider.DiscoveredModel{
		{Provider: "openrouter", Model: "some/model"},
	})

	cands, _ := g.plan(req("some/model", "hi"), keysFor("openrouter", "google"))
	for _, c := range cands {
		if c.model == "gemini-1.5-flash" {
			t.Errorf("a retired fallback stayed in the chain: %+v", cands)
		}
	}
	if len(cands) == 0 || cands[0].model != "some/model" {
		t.Errorf("the caller's own model is not first: %+v", cands)
	}
}

// The free-only switch takes effect on the next request, without a restart.
func TestFreeOnlyTogglesAtRuntime(t *testing.T) {
	g := testGateway(t, "auto", "balanced")
	g.catalog.SetDiscovered("p", []provider.DiscoveredModel{
		{Provider: "p", Model: "gratis", PriceKnown: true},
		{Provider: "p", Model: "paid", InputCostPer1M: 1, OutputCostPer1M: 1, PriceKnown: true},
	})
	keys := keysFor("p")

	if g.FreeOnly() {
		t.Fatal("free-only is on by default")
	}
	if g.freeOnly("gpt-4o") {
		t.Error("a plain request was treated as free-only")
	}

	g.SetFreeOnly(true)
	if !g.FreeOnly() || !g.freeOnly("gpt-4o") {
		t.Fatal("the toggle did not take effect")
	}

	// Every candidate must now be a zero-priced model.
	cands, _ := g.plan(req("gpt-4o", "hello"), keys)
	for _, c := range cands {
		if c.model != "gratis" {
			t.Errorf("free-only routed to %q", c.model)
		}
	}

	g.SetFreeOnly(false)
	if g.FreeOnly() {
		t.Error("turning it off did not take effect")
	}
}

// auto:free works regardless of the deployment-wide setting.
func TestFreeSentinelIgnoresTheToggle(t *testing.T) {
	g := testGateway(t, "auto", "balanced")

	if !g.freeOnly("auto:free") {
		t.Error("auto:free was not treated as free-only while the toggle is off")
	}
	g.SetFreeOnly(true)
	if !g.freeOnly("auto:free") {
		t.Error("auto:free stopped working with the toggle on")
	}
}

// Free-only mode substitutes a free model for one that would have cost money.
// It must not substitute for a named model that is already free.
func TestFreeOnlyHonoursANamedFreeModel(t *testing.T) {
	g := testGateway(t, "off", "balanced")
	g.catalog.SetDiscovered("p", []provider.DiscoveredModel{
		{Provider: "p", Model: "cheapest-free", PriceKnown: true},
		{Provider: "p", Model: "another-free", PriceKnown: true},
		{Provider: "p", Model: "costs-money", InputCostPer1M: 3, OutputCostPer1M: 6, PriceKnown: true},
	})
	keys := keysFor("p")
	g.SetFreeOnly(true)

	// A free model the caller named is used as asked.
	cands, _ := g.plan(req("another-free", "hi"), keys)
	if len(cands) == 0 || cands[0].model != "another-free" {
		t.Errorf("a named free model was replaced: %+v", cands)
	}

	// A priced one is substituted rather than allowed to spend.
	cands, _ = g.plan(req("costs-money", "hi"), keys)
	if len(cands) == 0 {
		t.Fatal("no substitute offered")
	}
	for _, c := range cands {
		if c.model == "costs-money" {
			t.Errorf("free-only let a priced model through: %+v", cands)
		}
	}
}

// With the toggle off, the same named model is used as asked.
func TestPricedModelsPassWhenFreeOnlyIsOff(t *testing.T) {
	g := testGateway(t, "off", "balanced")
	g.catalog.SetDiscovered("p", []provider.DiscoveredModel{
		{Provider: "p", Model: "costs-money", InputCostPer1M: 3, OutputCostPer1M: 6, PriceKnown: true},
	})

	cands, _ := g.plan(req("costs-money", "hi"), keysFor("p"))
	if len(cands) == 0 || cands[0].model != "costs-money" {
		t.Errorf("got %+v, want the model as asked", cands)
	}
}

// auto:code and auto:chat say what the request is for, which is more reliable
// than reading it out of the prompt.
func TestSentinelsSelectTheTask(t *testing.T) {
	cases := map[string]routing.TaskType{
		"auto:code": routing.TaskCodeGeneration,
		"auto:chat": routing.TaskConversation,
	}
	for sentinel, want := range cases {
		if got := intentFor(sentinel).task; got != want {
			t.Errorf("%s → task %q, want %q", sentinel, got, want)
		}
		if !intentFor(sentinel).known {
			t.Errorf("%s was not recognised as a routing alias", sentinel)
		}
	}

	// The axis sentinels choose an objective rather than a task.
	if got := intentFor("auto:fast").objective; got != routing.ObjectiveLatency {
		t.Errorf("auto:fast → %q", got)
	}
	if got := intentFor("auto:cheap").objective; got != routing.ObjectiveCost {
		t.Errorf("auto:cheap → %q", got)
	}
	if !intentFor("auto:free").freeOnly {
		t.Error("auto:free did not ask for free-only routing")
	}

	// A real model name is not a sentinel.
	if intentFor("gpt-4o-mini").known {
		t.Error("a model name was mistaken for a routing alias")
	}
}

// The whole point: asking for code gets a coding model, asking for chat gets a
// general one, from the same pool.
func TestCodeAndChatSentinelsPickDifferentModels(t *testing.T) {
	g := testGateway(t, "auto", "balanced")
	g.cfg.AttemptsPerProvider = 1

	g.catalog.SetDiscovered("p", []provider.DiscoveredModel{
		{Provider: "p", Model: "qwen-coder-32b", InputCostPer1M: 1, OutputCostPer1M: 1, PriceKnown: true},
		{Provider: "p", Model: "general-chat-70b", InputCostPer1M: 1, OutputCostPer1M: 1, PriceKnown: true},
	})
	keys := keysFor("p")

	code, task := g.plan(req("auto:code", "hello"), keys)
	if task != routing.TaskCodeGeneration {
		t.Errorf("auto:code detected task %q", task)
	}
	if len(code) == 0 || code[0].model != "qwen-coder-32b" {
		t.Errorf("auto:code picked %+v, want the coding model", code)
	}

	chat, task := g.plan(req("auto:chat", "hello"), keys)
	if task != routing.TaskConversation {
		t.Errorf("auto:chat detected task %q", task)
	}
	if len(chat) == 0 || chat[0].model != "general-chat-70b" {
		t.Errorf("auto:chat picked %+v, want the general model", chat)
	}
}

// Routing must prefer a model that works over one that keeps failing, even
// when the failing one is cheaper and nominally better suited.
func TestRoutingLearnsFromFailures(t *testing.T) {
	g := testGateway(t, "auto", "cost")
	g.cfg.AttemptsPerProvider = 1

	g.catalog.SetDiscovered("a", []provider.DiscoveredModel{
		{Provider: "a", Model: "cheap-coder", PriceKnown: true},
	})
	g.catalog.SetDiscovered("b", []provider.DiscoveredModel{
		{Provider: "b", Model: "dearer-general", InputCostPer1M: 2, OutputCostPer1M: 4, PriceKnown: true},
	})
	keys := keysFor("a", "b")

	// With nothing observed, the free coding model leads for a coding task.
	cands, _ := g.plan(req("auto:code", "hi"), keys)
	if len(cands) == 0 || cands[0].model != "cheap-coder" {
		t.Fatalf("before evidence, chain = %+v", cands)
	}

	// It then fails consistently.
	g.catalog.ObserveReliability(map[string]routing.Reliability{
		routing.ObservationKey("a", "cheap-coder"):    {Attempts: 20, Successes: 1},
		routing.ObservationKey("b", "dearer-general"): {Attempts: 20, Successes: 20},
	})

	cands, _ = g.plan(req("auto:code", "hi"), keys)
	if len(cands) == 0 || cands[0].model != "dearer-general" {
		t.Errorf("after 19 failures the chain still leads with %+v", cands)
	}
}

// Marking models for a task turns inference into an instruction: only the
// marked ones serve that task.
func TestTaggedModelsAreTheOnlyOnesUsedForTheirTask(t *testing.T) {
	g := testGateway(t, "auto", "balanced")
	g.cfg.AttemptsPerProvider = 3

	g.catalog.SetDiscovered("p", []provider.DiscoveredModel{
		{Provider: "p", Model: "chosen-coder", InputCostPer1M: 5, OutputCostPer1M: 5, PriceKnown: true},
		{Provider: "p", Model: "cheap-general", PriceKnown: true},
		{Provider: "p", Model: "another-coder", InputCostPer1M: 9, OutputCostPer1M: 9, PriceKnown: true},
	})
	keys := keysFor("p")

	// Untagged: the cheap model leads, as before.
	cands, _ := g.plan(req("auto:code", "hi"), keys)
	if len(cands) == 0 || cands[0].model != "cheap-general" {
		t.Fatalf("before tagging, chain = %+v", cands)
	}

	// The operator marks one model for coding, despite it being dearer.
	g.catalog.SetTags(map[string][]routing.TaskType{
		routing.TagKey("p", "chosen-coder"): {routing.TaskCodeGeneration},
	})

	cands, _ = g.plan(req("auto:code", "hi"), keys)
	if len(cands) != 1 {
		t.Fatalf("chain = %+v, want only the marked model", cands)
	}
	if cands[0].model != "chosen-coder" {
		t.Errorf("chain leads with %q, want the marked model", cands[0].model)
	}
}

// Marking models for coding must not restrict chat, which has no marks.
func TestTagsOnlyRestrictTheirOwnTask(t *testing.T) {
	g := testGateway(t, "auto", "balanced")

	g.catalog.SetDiscovered("p", []provider.DiscoveredModel{
		{Provider: "p", Model: "the-coder", InputCostPer1M: 5, OutputCostPer1M: 5, PriceKnown: true},
		{Provider: "p", Model: "a-chatter", PriceKnown: true},
	})
	g.catalog.SetTags(map[string][]routing.TaskType{
		routing.TagKey("p", "the-coder"): {routing.TaskCodeGeneration},
	})
	keys := keysFor("p")

	code, _ := g.plan(req("auto:code", "hi"), keys)
	if len(code) != 1 || code[0].model != "the-coder" {
		t.Errorf("auto:code = %+v, want only the marked model", code)
	}

	// Chat has no marks, so every model remains available to it.
	chat, _ := g.plan(req("auto:chat", "hi"), keys)
	if len(chat) < 2 {
		t.Errorf("auto:chat = %+v; marking models for coding should not restrict chat", chat)
	}
}

// A model marked for one task is deliberately not used for another: the
// operator considered it and chose otherwise.
func TestTaggingForOneTaskExcludesItFromOthersOnlyWhenThatTaskIsTagged(t *testing.T) {
	c := routing.NewCatalog()
	p := routing.ModelProfile{Provider: "p", Model: "specialist"}

	c.SetTags(map[string][]routing.TaskType{
		routing.TagKey("p", "specialist"): {routing.TaskCodeGeneration},
	})

	if got := c.CatalogSuitabilityFor(p, routing.TaskCodeGeneration); got != routing.SuitabilityStrong {
		t.Errorf("marked model rated %v for its own task", got)
	}
	if got := c.CatalogSuitabilityFor(p, routing.TaskConversation); got != routing.SuitabilityPoor {
		t.Errorf("marked model rated %v for a task it was not marked for", got)
	}
}

// Naming a model directly still works, marked or not: a tag governs automatic
// routing, not what a caller may ask for.
func TestTagsDoNotBlockNamedModels(t *testing.T) {
	g := testGateway(t, "off", "balanced")

	g.catalog.SetDiscovered("p", []provider.DiscoveredModel{
		{Provider: "p", Model: "untagged", PriceKnown: true},
		{Provider: "p", Model: "tagged", PriceKnown: true},
	})
	g.catalog.SetTags(map[string][]routing.TaskType{
		routing.TagKey("p", "tagged"): {routing.TaskCodeGeneration},
	})

	cands, _ := g.plan(req("untagged", "hi"), keysFor("p"))
	if len(cands) == 0 || cands[0].model != "untagged" {
		t.Errorf("a named untagged model was not honoured: %+v", cands)
	}
}

// When every marked model is unusable, the error must say so — a stale mark
// list is a likely cause and an easy fix.
func TestUnroutableExplainsStaleTags(t *testing.T) {
	g := testGateway(t, "auto", "balanced")

	// A real provider name, so unroutable can see it holds a key.
	g.catalog.SetDiscovered("openai", []provider.DiscoveredModel{
		{Provider: "openai", Model: "available", PriceKnown: true},
	})
	// Marked for coding, but the provider no longer lists it.
	g.catalog.SetTags(map[string][]routing.TaskType{
		routing.TagKey("openai", "withdrawn"): {routing.TaskCodeGeneration},
	})

	err := g.unroutable(req("auto:code", "hi"), map[string]string{"openai": "k"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "marked for") {
		t.Errorf("error does not mention the marks as a cause:\n%s", err)
	}
}
