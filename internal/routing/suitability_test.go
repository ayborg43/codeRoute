package routing

import "testing"

func profile(model string, tasks ...TaskType) ModelProfile {
	return ModelProfile{Model: model, Tasks: tasks}
}

// A declared task list is a statement by the catalogue or the operator, and
// must outrank anything guessed from a name.
func TestDeclaredTasksBeatNameGuessing(t *testing.T) {
	// Named like a coding model, declared for conversation.
	p := profile("acme-coder-7b", TaskConversation)

	if got := SuitabilityFor(p, TaskConversation); got != SuitabilityStrong {
		t.Errorf("declared task ignored: %v", got)
	}
	if got := SuitabilityFor(p, TaskCodeGeneration); got != SuitabilityPoor {
		t.Errorf("a model declared only for chat was rated %v for code", got)
	}
}

func TestCodingModelsPreferredForCode(t *testing.T) {
	for _, name := range []string{
		"deepseek-coder-v2", "codestral-2508", "qwen2.5-coder-32b",
		"starcoder2-15b", "devstral-small", "codellama-70b",
	} {
		if got := SuitabilityFor(profile(name), TaskCodeGeneration); got != SuitabilityStrong {
			t.Errorf("%q rated %v for code, want strong", name, got)
		}
		// The same model is a poor pick for chat: it will answer, but there is
		// almost always a general model available.
		if got := SuitabilityFor(profile(name), TaskConversation); got != SuitabilityPoor {
			t.Errorf("%q rated %v for chat, want poor", name, got)
		}
	}
}

func TestGeneralModelsPreferredForChat(t *testing.T) {
	for _, name := range []string{
		"gemini-3-flash-preview", "claude-haiku-4.5", "gpt-4o-mini", "llama-3.3-70b",
	} {
		if got := SuitabilityFor(profile(name), TaskConversation); got != SuitabilityStrong {
			t.Errorf("%q rated %v for chat, want strong", name, got)
		}
		// General models are still perfectly usable for code, just not
		// preferred over a dedicated one.
		if got := SuitabilityFor(profile(name), TaskCodeGeneration); got != SuitabilityGeneral {
			t.Errorf("%q rated %v for code, want general", name, got)
		}
	}
}

// A fill-in-the-middle model is excellent at its job and useless at answering
// a chat turn, so it must never be preferred for either sentinel.
func TestCompletionOnlyModelsAreNeverPreferred(t *testing.T) {
	for _, name := range []string{"starcoder2-3b-fim", "codegemma-7b-base", "llama-3-8b-base"} {
		for _, task := range []TaskType{TaskCodeGeneration, TaskConversation, TaskAnalysis} {
			if got := SuitabilityFor(profile(name), task); got != SuitabilityPoor {
				t.Errorf("%q rated %v for %s, want poor", name, got, task)
			}
		}
	}
}

func TestReasoningModelsSuitAnalysisAndCode(t *testing.T) {
	for _, name := range []string{"deepseek-r1", "qwq-32b", "o3-mini"} {
		if got := SuitabilityFor(profile(name), TaskAnalysis); got != SuitabilityStrong {
			t.Errorf("%q rated %v for analysis, want strong", name, got)
		}
		if got := SuitabilityFor(profile(name), TaskCodeGeneration); got != SuitabilityStrong {
			t.Errorf("%q rated %v for code, want strong", name, got)
		}
		// Correct but ponderous for chat: ranked below a general model, not
		// excluded.
		if got := SuitabilityFor(profile(name), TaskConversation); got != SuitabilityGeneral {
			t.Errorf("%q rated %v for chat, want general", name, got)
		}
	}
}
