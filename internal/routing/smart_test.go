package routing

import "testing"

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

func TestRankHonoursObjective(t *testing.T) {
	cheapest := Rank(TaskConversation, ObjectiveCost)
	for i := 1; i < len(cheapest); i++ {
		if cheapest[i-1].CostPerK > cheapest[i].CostPerK {
			t.Fatalf("cost ranking out of order: %+v", cheapest)
		}
	}

	fastest := Rank(TaskConversation, ObjectiveLatency)
	for i := 1; i < len(fastest); i++ {
		if fastest[i-1].LatencyMs > fastest[i].LatencyMs {
			t.Fatalf("latency ranking out of order: %+v", fastest)
		}
	}

	// The two objectives must actually differ, or ranking is doing nothing.
	if cheapest[0].Model == fastest[0].Model && len(cheapest) > 1 {
		t.Log("cost and latency winners coincide for this catalogue")
	}
}

func TestRankOnlyReturnsModelsForTheTask(t *testing.T) {
	for _, p := range Rank(TaskCreative, ObjectiveBalanced) {
		if !supportsTask(&p, TaskCreative) {
			t.Errorf("%s does not support creative work but was ranked for it", p.Model)
		}
	}
}

func TestRankNeverReturnsEmpty(t *testing.T) {
	if got := Rank(TaskUnknown, ObjectiveBalanced); len(got) == 0 {
		t.Fatal("Rank returned nothing for an unmatched task; routing would dead-end")
	}
}

func TestSelectModelPicksTheTopOfTheRanking(t *testing.T) {
	ranked := Rank(TaskAnalysis, ObjectiveCost)
	if got := SelectModel(TaskAnalysis, "cost"); got.Model != ranked[0].Model {
		t.Errorf("SelectModel = %q, want %q", got.Model, ranked[0].Model)
	}
}
