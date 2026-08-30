package routing

import "strings"

// Suitability is how well a model is thought to fit a kind of work.
//
// It is inferred, not measured. Nothing here observes answer quality, because
// nothing in this system can: the gateway sees requests and responses, never
// whether an answer was any good. What follows is the vendor's own naming
// convention plus whatever an operator has declared, and it is used only to
// order candidates — never to exclude one outright.
type Suitability int

const (
	// SuitabilityUnknown is the honest default for a model nobody has said
	// anything about.
	SuitabilityUnknown Suitability = iota

	// SuitabilityGeneral is a model that serves ordinary requests.
	SuitabilityGeneral

	// SuitabilityStrong is a model named or declared for this kind of work.
	SuitabilityStrong

	// SuitabilityPoor is a model whose specialism makes it a bad fit here —
	// a code-completion model asked to hold a conversation, say.
	SuitabilityPoor
)

// codeMarkers appear in the names of models trained or tuned for programming.
// Vendors are consistent about this, which makes it a usable signal where no
// declared capability exists.
var codeMarkers = []string{
	"code", "coder", "codestral", "starcoder", "devstral",
	"codellama", "codegemma", "codegeex", "codeqwen", "polycoder",
}

// completionOnlyMarkers name models built to fill in code rather than converse.
// They answer a chat turn poorly even though they are excellent at their job.
var completionOnlyMarkers = []string{
	"fim", "fill-in-middle", "-base", "base-", "completion",
}

// reasoningMarkers name models tuned to work through a problem step by step,
// which suits analysis and code but makes them slow and verbose for chat.
var reasoningMarkers = []string{
	"reason", "think", "-r1", "o1-", "o3-", "qwq", "deep-research",
}

// SuitabilityFor rates a model for a kind of work, from its declared tasks
// where it has them and its name where it does not.
//
// Where a catalogue is available, prefer CatalogSuitabilityFor: it consults an
// operator's own tags first, which outrank anything inferred here.
func SuitabilityFor(p ModelProfile, task TaskType) Suitability {
	// A declared task list is an operator's or the catalogue's own statement,
	// and outranks anything guessed from a name.
	if len(p.Tasks) > 0 {
		if p.supports(task) {
			return SuitabilityStrong
		}
		return SuitabilityPoor
	}

	name := strings.ToLower(p.Model)
	isCode := containsAny(name, codeMarkers)
	isCompletionOnly := containsAny(name, completionOnlyMarkers)
	isReasoning := containsAny(name, reasoningMarkers)

	switch task {
	case TaskCodeGeneration:
		switch {
		case isCompletionOnly:
			// Superb at completing code, hopeless at being asked for it.
			return SuitabilityPoor
		case isCode, isReasoning:
			return SuitabilityStrong
		default:
			return SuitabilityGeneral
		}

	case TaskConversation:
		switch {
		case isCompletionOnly:
			return SuitabilityPoor
		case isCode:
			// A coding model will answer, but it is the wrong tool and there
			// is almost always a general model available.
			return SuitabilityPoor
		case isReasoning:
			// Correct but ponderous for chat, so ranked below a general model
			// rather than excluded.
			return SuitabilityGeneral
		default:
			return SuitabilityStrong
		}

	case TaskAnalysis:
		switch {
		case isCompletionOnly:
			return SuitabilityPoor
		case isReasoning:
			return SuitabilityStrong
		default:
			return SuitabilityGeneral
		}

	default:
		if isCompletionOnly {
			return SuitabilityPoor
		}
		return SuitabilityGeneral
	}
}

// CatalogSuitabilityFor rates a model, consulting the operator's tags first.
// A tag is a statement about the model and settles the question; only an
// untagged model falls through to inference.
func (c *Catalog) CatalogSuitabilityFor(p ModelProfile, task TaskType) Suitability {
	tags := c.TagsFor(p.Provider, p.Model)
	if len(tags) > 0 {
		for _, t := range tags {
			if t == task {
				return SuitabilityStrong
			}
		}
		// Tagged, but for something else. The operator has considered this
		// model and did not choose it for this work.
		return SuitabilityPoor
	}
	return SuitabilityFor(p, task)
}

func containsAny(s string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}
