package gateway

import (
	"testing"

	"github.com/coderouter/coderouter/internal/config"
	"github.com/coderouter/coderouter/internal/provider"
	"github.com/coderouter/coderouter/internal/routing"
)

// testGateway builds a gateway for planning tests; plan() never touches the DB.
func testGateway(mode, objective string) *Gateway {
	cfg := config.Load()
	cfg.RoutingMode = mode
	cfg.RoutingObjective = objective
	cfg.RouterModel = "auto"
	return New(nil, cfg)
}

func req(model, prompt string) *provider.ChatRequest {
	return &provider.ChatRequest{
		Model:    model,
		Messages: []provider.Message{{Role: "user", Content: provider.Content(prompt)}},
	}
}

func TestPlanHonoursAnExplicitModel(t *testing.T) {
	g := testGateway("auto", "balanced")

	cands, task := g.plan(req("claude-3-5-sonnet-20241022", "refactor this function"))

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
	g := testGateway("auto", "cost")

	cands, _ := g.plan(req("auto", "chat with me"))

	if len(cands) == 0 {
		t.Fatal("smart routing produced no candidates")
	}
	if cands[0].model == "auto" {
		t.Fatal("the router sentinel leaked through as a real model")
	}

	// Cheapest-first for a conversation task.
	ranked := routing.Rank(routing.TaskConversation, routing.ObjectiveCost)
	if cands[0].model != ranked[0].Model {
		t.Errorf("primary = %q, want cheapest %q", cands[0].model, ranked[0].Model)
	}
}

func TestPlanUsesSmartRoutingWhenNoModelGiven(t *testing.T) {
	g := testGateway("auto", "latency")

	cands, _ := g.plan(req("", "hello there"))

	if len(cands) == 0 || cands[0].model == "" {
		t.Fatalf("no candidates for an empty model: %+v", cands)
	}
	ranked := routing.Rank(routing.TaskConversation, routing.ObjectiveLatency)
	if cands[0].model != ranked[0].Model {
		t.Errorf("primary = %q, want fastest %q", cands[0].model, ranked[0].Model)
	}
}

func TestAlwaysModeOverridesTheClientsModel(t *testing.T) {
	g := testGateway("always", "cost")

	cands, _ := g.plan(req("gpt-4o", "write me a story"))

	ranked := routing.Rank(routing.TaskCreative, routing.ObjectiveCost)
	if cands[0].model != ranked[0].Model {
		t.Errorf("primary = %q, want %q; always mode must override", cands[0].model, ranked[0].Model)
	}
}

func TestOffModeFallsBackToStaticChain(t *testing.T) {
	g := testGateway("off", "balanced")

	cands, _ := g.plan(req("", "anything"))

	if cands[0].model != g.cfg.DefaultModel {
		t.Errorf("primary = %q, want default %q", cands[0].model, g.cfg.DefaultModel)
	}
}

func TestCandidateChainHasOneEntryPerProvider(t *testing.T) {
	g := testGateway("always", "balanced")

	cands, _ := g.plan(req("", "implement a parser"))

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
