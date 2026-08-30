package routing

import "testing"

func TestReliabilityBasics(t *testing.T) {
	empty := Reliability{}
	if empty.SuccessRate() != 0 || empty.Confidence() != 0 || empty.Penalty() != 0 {
		t.Errorf("an unobserved model is not neutral: %+v", empty)
	}

	perfect := Reliability{Attempts: 20, Successes: 20}
	if perfect.SuccessRate() != 1 || perfect.Penalty() != 0 {
		t.Errorf("a flawless model was penalised: %+v", perfect)
	}

	broken := Reliability{Attempts: 20, Successes: 0}
	if broken.Penalty() != 1 {
		t.Errorf("a consistently failing model was not fully penalised: %v", broken.Penalty())
	}
}

// One unlucky call must not condemn a model. The penalty is scaled by how much
// evidence there is.
func TestSmallSamplesArePenalisedLightly(t *testing.T) {
	oneFailure := Reliability{Attempts: 1, Successes: 0}
	manyFailures := Reliability{Attempts: 16, Successes: 0}

	if oneFailure.Penalty() >= manyFailures.Penalty() {
		t.Errorf("a single failure counted as heavily as sixteen: %v vs %v",
			oneFailure.Penalty(), manyFailures.Penalty())
	}
	if oneFailure.Confidence() >= 1 {
		t.Errorf("one sample was treated as conclusive: %v", oneFailure.Confidence())
	}
}

// A model that works beats a cheaper one that does not, whatever the objective.
func TestFailingModelsLoseToWorkingOnes(t *testing.T) {
	cheapBroken := ModelProfile{
		Model: "cheap", Provider: "a", LatencyMs: 100,
		InputCostPer1M: 0, OutputCostPer1M: 0, PriceKnown: true,
		Reliability: Reliability{Attempts: 20, Successes: 2},
	}
	dearWorking := ModelProfile{
		Model: "dear", Provider: "b", LatencyMs: 2000,
		InputCostPer1M: 5, OutputCostPer1M: 15, PriceKnown: true,
		Reliability: Reliability{Attempts: 20, Successes: 20},
	}

	for _, obj := range []Objective{ObjectiveCost, ObjectiveLatency, ObjectiveBalanced} {
		broken := ScoreWithEvidence(cheapBroken, TaskConversation, obj)
		working := ScoreWithEvidence(dearWorking, TaskConversation, obj)
		if working >= broken {
			t.Errorf("%s: a model failing 90%% of the time scored %v, better than a working one at %v",
				obj, broken, working)
		}
	}
}

// Suitability nudges the order; it does not override evidence of failure.
func TestSuitabilityDoesNotOverrideFailure(t *testing.T) {
	suitedButBroken := ModelProfile{
		Model: "deepseek-coder", Provider: "a", LatencyMs: 500,
		InputCostPer1M: 1, OutputCostPer1M: 1, PriceKnown: true,
		Reliability: Reliability{Attempts: 20, Successes: 1},
	}
	generalAndWorking := ModelProfile{
		Model: "gpt-4o-mini", Provider: "b", LatencyMs: 500,
		InputCostPer1M: 1, OutputCostPer1M: 1, PriceKnown: true,
		Reliability: Reliability{Attempts: 20, Successes: 20},
	}

	if ScoreWithEvidence(generalAndWorking, TaskCodeGeneration, ObjectiveBalanced) >=
		ScoreWithEvidence(suitedButBroken, TaskCodeGeneration, ObjectiveBalanced) {
		t.Error("a broken coding model outranked a working general one for code")
	}
}

// With reliability equal, the model suited to the task wins.
func TestSuitabilityBreaksTiesBetweenEqualModels(t *testing.T) {
	base := ModelProfile{
		Provider: "a", LatencyMs: 500, InputCostPer1M: 1, OutputCostPer1M: 1, PriceKnown: true,
		Reliability: Reliability{Attempts: 20, Successes: 20},
	}
	coder, general := base, base
	coder.Model, general.Model = "qwen-coder-32b", "some-general-model"

	if ScoreWithEvidence(coder, TaskCodeGeneration, ObjectiveBalanced) >=
		ScoreWithEvidence(general, TaskCodeGeneration, ObjectiveBalanced) {
		t.Error("the coding model did not win for code")
	}
	if ScoreWithEvidence(general, TaskConversation, ObjectiveBalanced) >=
		ScoreWithEvidence(coder, TaskConversation, ObjectiveBalanced) {
		t.Error("the general model did not win for chat")
	}
}

// An unobserved model is neither trusted nor condemned: it sorts on its
// published figures alone.
func TestUnobservedModelsAreNotPenalised(t *testing.T) {
	p := ModelProfile{
		Model: "brand-new", Provider: "a", LatencyMs: 500,
		InputCostPer1M: 1, OutputCostPer1M: 1, PriceKnown: true,
	}
	if got, want := ScoreWithEvidence(p, TaskConversation, ObjectiveBalanced), Score(p, ObjectiveBalanced); got > want+0.01 {
		t.Errorf("an unobserved model was penalised: %v vs a base score of %v", got, want)
	}
}

func TestRankForTaskOrdersByEvidence(t *testing.T) {
	c := NewCatalog()
	pool := []ModelProfile{
		{Model: "broken", Provider: "a", LatencyMs: 100, PriceKnown: true,
			Reliability: Reliability{Attempts: 20, Successes: 0}},
		{Model: "solid", Provider: "b", LatencyMs: 900, InputCostPer1M: 1, OutputCostPer1M: 1, PriceKnown: true,
			Reliability: Reliability{Attempts: 20, Successes: 20}},
	}

	ranked := c.RankForTask(pool, TaskConversation, ObjectiveBalanced)
	if ranked[0].Model != "solid" {
		t.Errorf("ranking put %q first; the failing model should lose", ranked[0].Model)
	}
}

// The objective axes must be on comparable scales, or a penalty applied on top
// means something different depending on which objective is in force.
func TestObjectiveScalesAreComparable(t *testing.T) {
	slowAndDear := ModelProfile{
		LatencyMs: 3000, InputCostPer1M: 10, OutputCostPer1M: 30, PriceKnown: true,
	}

	latency := Score(slowAndDear, ObjectiveLatency)
	cost := Score(slowAndDear, ObjectiveCost)

	// Both should land in single or low double digits, not differ by orders
	// of magnitude.
	if latency > 100 || cost > 100 {
		t.Errorf("scores are not on a comparable scale: latency=%v cost=%v", latency, cost)
	}
	if ratio := latency / cost; ratio > 20 || ratio < 0.05 {
		t.Errorf("latency and cost scores differ by %vx", ratio)
	}
}

// Rescaling must not disturb the ordering within an objective.
func TestLatencyOrderingSurvivesRescaling(t *testing.T) {
	fast := ModelProfile{Model: "fast", LatencyMs: 200}
	slow := ModelProfile{Model: "slow", LatencyMs: 2000}

	if Score(fast, ObjectiveLatency) >= Score(slow, ObjectiveLatency) {
		t.Error("latency ordering was inverted")
	}
}

// Clamping scores at zero flattened every cheap model to the same value,
// destroying the ordering exactly where the interesting candidates are.
func TestCheapModelsKeepDistinctScores(t *testing.T) {
	free := ModelProfile{Model: "free-one", PriceKnown: true}
	nearlyFree := ModelProfile{Model: "cheap-one", InputCostPer1M: 1, OutputCostPer1M: 1, PriceKnown: true}

	a := ScoreWithEvidence(free, TaskConversation, ObjectiveBalanced)
	b := ScoreWithEvidence(nearlyFree, TaskConversation, ObjectiveBalanced)

	if a == b {
		t.Errorf("a free model and a $1/1M model scored identically (%v)", a)
	}
	if a >= b {
		t.Errorf("the free model scored %v, worse than the priced one at %v", a, b)
	}
}

// An unstated price must not read as a zero one, or every unpriced model wins
// on cost — the same mistake free-only routing exists to avoid.
func TestUnpricedIsNotTreatedAsFree(t *testing.T) {
	unpriced := ModelProfile{Model: "mystery"}
	genuinelyFree := ModelProfile{Model: "gratis", PriceKnown: true}

	if ScoreWithEvidence(genuinelyFree, TaskConversation, ObjectiveBalanced) >=
		ScoreWithEvidence(unpriced, TaskConversation, ObjectiveBalanced) {
		t.Error("an unpriced model ranked at least as well as a genuinely free one")
	}

	// It should still beat something known to be expensive.
	expensive := ModelProfile{Model: "dear", InputCostPer1M: 20, OutputCostPer1M: 60, PriceKnown: true}
	if ScoreWithEvidence(unpriced, TaskConversation, ObjectiveBalanced) >=
		ScoreWithEvidence(expensive, TaskConversation, ObjectiveBalanced) {
		t.Error("an unpriced model was treated as worse than a known-expensive one")
	}
}
