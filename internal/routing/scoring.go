package routing

// Reliability is what a model's own traffic says about it.
//
// These are the only judgements this system can honestly make. It sees whether
// a call succeeded and how long it took; it does not see whether the answer was
// correct, useful, or well written. Nothing here is a measure of quality.
type Reliability struct {
	// Attempts is how many calls the window covered. A small sample is worth
	// less than a large one, which Confidence expresses.
	Attempts int

	// Successes is how many of them returned a completion.
	Successes int

	// MedianLatencyMs covers successful calls only, so a provider that fails
	// in 50ms does not look fast.
	MedianLatencyMs int
}

// SuccessRate is the share of attempts that produced an answer.
func (r Reliability) SuccessRate() float64 {
	if r.Attempts == 0 {
		return 0
	}
	return float64(r.Successes) / float64(r.Attempts)
}

// minSamples is the point below which a success rate is too noisy to act on.
// One failure out of one attempt is not evidence that a model is broken.
const minSamples = 8

// Confidence rises with the sample size, reaching 1 at minSamples. It is what
// keeps a single unlucky call from demoting a model that is otherwise fine.
func (r Reliability) Confidence() float64 {
	if r.Attempts >= minSamples {
		return 1
	}
	return float64(r.Attempts) / float64(minSamples)
}

// Penalty is how much a model's observed failures should count against it,
// from 0 (nothing known against it) to 1 (consistently failing).
//
// It is scaled by confidence, so an unproven model is treated as innocent
// rather than either trusted or condemned on scant evidence.
func (r Reliability) Penalty() float64 {
	if r.Attempts == 0 {
		return 0
	}
	return (1 - r.SuccessRate()) * r.Confidence()
}

// Weights used when ranking. Latency is in seconds and cost in dollars per
// million tokens, so they are already the same order of magnitude.
const (
	// failureWeight makes reliability decisive without being absolute: a cheap
	// model that works half the time is still worse than a dearer one that
	// always works.
	failureWeight = 12

	// Suitability is applied as a penalty on the models that fit less well,
	// never as a discount on the ones that fit. Subtracting would push good
	// candidates below zero, and clamping there would flatten every cheap
	// model to the same score — losing the distinction precisely where the
	// interesting candidates are.
	generalPenalty = 1.5
	poorPenalty    = 6

	// assumedCostPer1M stands in for a model whose provider publishes no
	// price. Treating an unstated price as zero would make every unpriced
	// model look free and win on cost, which is the same mistake free-only
	// routing exists to avoid. A mid-tier figure lets a genuinely cheap model
	// win and a genuinely dear one lose, without pretending to know.
	assumedCostPer1M = 2.0
)

// ScoreWithEvidence rates a profile for a task, lower being better.
//
// It combines four things, in descending order of how much they are trusted:
// whether the model actually works, whether it suits the task, how quickly it
// answers, and what it costs. Only the first is learned from this deployment's
// own traffic; the rest are published figures or inference from names.
func ScoreWithEvidence(p ModelProfile, task TaskType, obj Objective) float64 {
	score := Score(p, obj)

	// An unstated price is not a zero one.
	if !p.PriceKnown && obj != ObjectiveLatency {
		score += assumedCostPer1M
	}

	// A model that fails is worse than one that is slow or dear, whatever the
	// objective, so the penalty is added rather than weighted into the axis.
	score += p.Reliability.Penalty() * failureWeight

	// Penalties only, so the best candidates keep their distinct scores.
	switch SuitabilityFor(p, task) {
	case SuitabilityGeneral, SuitabilityUnknown:
		score += generalPenalty
	case SuitabilityPoor:
		score += poorPenalty
	}

	return score
}
