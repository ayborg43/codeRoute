package routing

import (
	"math"
	"testing"
)

func TestDetectTask(t *testing.T) {
	cases := map[string]TaskType{
		"refactor this function":     TaskCodeGeneration,
		"analyze these numbers":      TaskAnalysis,
		"write a poem about the sea": TaskCreative,
		"good morning":               TaskConversation,
	}
	for prompt, want := range cases {
		if got := DetectTask(prompt); got != want {
			t.Errorf("DetectTask(%q) = %q, want %q", prompt, got, want)
		}
	}
}

func TestBlacklistIsClearedByReplacement(t *testing.T) {
	c := NewCatalog()

	c.SetBlacklist(map[string]bool{TagKey("p", "banned"): true})
	if !c.Blacklisted("p", "banned") {
		t.Fatal("model was not recorded as blacklisted")
	}
	if c.Blacklisted("p", "other") {
		t.Error("an unrelated model was reported as blacklisted")
	}
	if got := c.BlacklistCount(); got != 1 {
		t.Errorf("BlacklistCount() = %d, want 1", got)
	}

	// A false entry must not linger as if it were true, and a wholesale
	// replacement with an empty map must clear everything — the dashboard
	// sends exactly what should now be blacklisted, not a delta.
	c.SetBlacklist(map[string]bool{TagKey("p", "banned"): false})
	if c.Blacklisted("p", "banned") {
		t.Error("a false entry left the model blacklisted")
	}

	c.SetBlacklist(map[string]bool{TagKey("p", "banned"): true})
	c.SetBlacklist(map[string]bool{})
	if c.Blacklisted("p", "banned") || c.BlacklistCount() != 0 {
		t.Error("replacing with an empty blacklist did not clear the previous one")
	}
}

func TestRankHonoursObjective(t *testing.T) {
	c := NewCatalog()

	cheapest := c.Rank(TaskConversation, ObjectiveCost)
	for i := 1; i < len(cheapest); i++ {
		if cheapest[i-1].BlendedCostPer1M() > cheapest[i].BlendedCostPer1M() {
			t.Fatalf("cost ranking out of order: %+v", cheapest)
		}
	}

	fastest := c.Rank(TaskConversation, ObjectiveLatency)
	for i := 1; i < len(fastest); i++ {
		if fastest[i-1].EffectiveLatency() > fastest[i].EffectiveLatency() {
			t.Fatalf("latency ranking out of order: %+v", fastest)
		}
	}
}

func TestRankOnlyReturnsModelsForTheTask(t *testing.T) {
	for _, p := range NewCatalog().Rank(TaskCreative, ObjectiveBalanced) {
		if !p.supports(TaskCreative) {
			t.Errorf("%s does not support creative work but was ranked for it", p.Model)
		}
	}
}

func TestRankNeverReturnsEmpty(t *testing.T) {
	if got := NewCatalog().Rank(TaskUnknown, ObjectiveBalanced); len(got) == 0 {
		t.Fatal("Rank returned nothing for an unmatched task; routing would dead-end")
	}
}

func TestSelectModelPicksTheTopOfTheRanking(t *testing.T) {
	c := NewCatalog()
	ranked := c.Rank(TaskAnalysis, ObjectiveCost)
	if got := c.SelectModel(TaskAnalysis, "cost"); got.Model != ranked[0].Model {
		t.Errorf("SelectModel = %q, want %q", got.Model, ranked[0].Model)
	}
}

func TestCostUSD(t *testing.T) {
	p := ModelProfile{InputCostPer1M: 3, OutputCostPer1M: 15}

	// 1M in + 1M out at these prices is $18.
	if got := p.CostUSD(1_000_000, 1_000_000); math.Abs(got-18) > 1e-9 {
		t.Errorf("CostUSD = %v, want 18", got)
	}
	if got := p.CostUSD(1000, 500); math.Abs(got-(0.003+0.0075)) > 1e-9 {
		t.Errorf("CostUSD = %v, want 0.0105", got)
	}
	// A model with no published price must not be billed at a guess.
	if got := (ModelProfile{}).CostUSD(1000, 1000); got != 0 {
		t.Errorf("unpriced model charged %v", got)
	}
}

func TestBlendedCostWeightsOutput(t *testing.T) {
	// Cheap to read, ruinous to write: ranking must not treat this as cheap.
	trap := ModelProfile{InputCostPer1M: 0.1, OutputCostPer1M: 100}
	plain := ModelProfile{InputCostPer1M: 1, OutputCostPer1M: 3}

	if trap.BlendedCostPer1M() <= plain.BlendedCostPer1M() {
		t.Errorf("blended cost ignored output price: trap=%v plain=%v",
			trap.BlendedCostPer1M(), plain.BlendedCostPer1M())
	}
}

func TestLoadCatalogMergesOverBuiltIns(t *testing.T) {
	c, err := LoadCatalog([]byte(`{"models":[
		{"model":"gpt-4o-mini","provider":"openai","latency_ms":10,
		 "input_cost_per_1m":0.01,"output_cost_per_1m":0.02,"tasks":["conversation"]},
		{"model":"local-llama","provider":"openai","latency_ms":50,
		 "input_cost_per_1m":0,"output_cost_per_1m":0,"tasks":["conversation"]}
	]}`))
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}

	if len(c.Profiles()) != len(defaultProfiles)+1 {
		t.Fatalf("expected the built-ins plus one new model, got %d", len(c.Profiles()))
	}

	overridden, ok := c.Lookup("gpt-4o-mini")
	if !ok || overridden.LatencyMs != 10 {
		t.Errorf("override did not replace the built-in entry: %+v", overridden)
	}
	if _, ok := c.Lookup("local-llama"); !ok {
		t.Error("new model was not added")
	}
}

func TestLoadCatalogReplace(t *testing.T) {
	c, err := LoadCatalog([]byte(`{"replace":true,"models":[
		{"model":"only","provider":"openai","latency_ms":1,
		 "input_cost_per_1m":1,"output_cost_per_1m":1,"tasks":["conversation"]}
	]}`))
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	if got := c.Profiles(); len(got) != 1 || got[0].Model != "only" {
		t.Errorf("replace did not discard the built-ins: %+v", got)
	}
}

func TestLoadCatalogAcceptsBareArray(t *testing.T) {
	c, err := LoadCatalog([]byte(`[{"model":"extra","provider":"google","latency_ms":5,
		"input_cost_per_1m":1,"output_cost_per_1m":1,"tasks":["analysis"]}]`))
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	if _, ok := c.Lookup("extra"); !ok {
		t.Error("bare array form was not loaded")
	}
}

func TestLoadCatalogRejectsBadEntries(t *testing.T) {
	cases := map[string]string{
		"no model":    `[{"provider":"openai","latency_ms":1,"tasks":["conversation"]}]`,
		"no provider": `[{"model":"x","latency_ms":1,"tasks":["conversation"]}]`,
		"no latency":  `[{"model":"x","provider":"openai","tasks":["conversation"]}]`,
		"no tasks":    `[{"model":"x","provider":"openai","latency_ms":1}]`,
		"negative":    `[{"model":"x","provider":"openai","latency_ms":1,"input_cost_per_1m":-1,"tasks":["conversation"]}]`,
		"not json":    `{{{`,
		"empty":       `{"replace":true,"models":[]}`,
	}
	for name, body := range cases {
		if _, err := LoadCatalog([]byte(body)); err == nil {
			t.Errorf("%s: expected an error, got none", name)
		}
	}
}

func TestObservedLatencyOverridesTheEstimate(t *testing.T) {
	c := NewCatalog()

	// gemini-1.5-flash is the catalogue's fastest conversation model on
	// estimates; measuring it as slow must demote it.
	before := c.Rank(TaskConversation, ObjectiveLatency)[0].Model
	c.ObserveLatencies(map[string]int{before: 99_000})
	after := c.Rank(TaskConversation, ObjectiveLatency)[0].Model

	if after == before {
		t.Errorf("measured latency was ignored; %q still ranks fastest", before)
	}
}

func TestObserveLatenciesIgnoresUnknownAndZero(t *testing.T) {
	c := NewCatalog()
	c.ObserveLatencies(map[string]int{"not-a-model": 10, "gpt-4o-mini": 0})

	p, _ := c.Lookup("gpt-4o-mini")
	if p.EffectiveLatency() != p.LatencyMs {
		t.Errorf("a zero measurement overwrote the estimate: %+v", p)
	}
}
