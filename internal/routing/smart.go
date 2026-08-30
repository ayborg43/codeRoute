package routing

import (
	"sort"
	"strings"
)

type TaskType string

const (
	TaskCodeGeneration TaskType = "code_generation"
	TaskConversation   TaskType = "conversation"
	TaskAnalysis       TaskType = "analysis"
	TaskCreative       TaskType = "creative"
	TaskUnknown        TaskType = "unknown"
)

type ModelProfile struct {
	Model     string
	Provider  string
	LatencyMs int
	CostPerK  float64
	Tasks     []TaskType
}

var profiles = []ModelProfile{
	{Model: "gpt-4o-mini", Provider: "openai", LatencyMs: 800, CostPerK: 0.15, Tasks: []TaskType{TaskConversation, TaskCodeGeneration}},
	{Model: "gpt-4o", Provider: "openai", LatencyMs: 2000, CostPerK: 5.0, Tasks: []TaskType{TaskAnalysis, TaskCodeGeneration}},
	{Model: "claude-3-haiku-20240307", Provider: "anthropic", LatencyMs: 600, CostPerK: 0.25, Tasks: []TaskType{TaskConversation, TaskCodeGeneration}},
	{Model: "claude-3-5-sonnet-20241022", Provider: "anthropic", LatencyMs: 1500, CostPerK: 3.0, Tasks: []TaskType{TaskAnalysis, TaskCodeGeneration}},
	{Model: "gemini-1.5-flash", Provider: "google", LatencyMs: 500, CostPerK: 0.075, Tasks: []TaskType{TaskConversation, TaskCreative}},
	{Model: "gemini-1.5-pro", Provider: "google", LatencyMs: 1800, CostPerK: 1.25, Tasks: []TaskType{TaskAnalysis, TaskCreative}},
}

func DetectTask(prompt string) TaskType {
	lower := strings.ToLower(prompt)

	codeKeywords := []string{"function", "class", "code", "implement", "debug", "fix", "refactor", "api", "endpoint", "test", "bug"}
	for _, kw := range codeKeywords {
		if strings.Contains(lower, kw) {
			return TaskCodeGeneration
		}
	}

	analysisKeywords := []string{"analyze", "explain", "compare", "evaluate", "review", "assess", "summarize"}
	for _, kw := range analysisKeywords {
		if strings.Contains(lower, kw) {
			return TaskAnalysis
		}
	}

	creativeKeywords := []string{"write", "story", "poem", "creative", "imagine", "describe", "narrative"}
	for _, kw := range creativeKeywords {
		if strings.Contains(lower, kw) {
			return TaskCreative
		}
	}

	return TaskConversation
}

// Objective is the axis smart routing optimizes along.
type Objective string

const (
	ObjectiveBalanced Objective = "balanced"
	ObjectiveLatency  Objective = "latency"
	ObjectiveCost     Objective = "cost"
)

// Score rates a profile against an objective; lower is better.
func Score(p ModelProfile, obj Objective) float64 {
	switch obj {
	case ObjectiveLatency:
		return float64(p.LatencyMs)
	case ObjectiveCost:
		return p.CostPerK
	default:
		// Balanced trades a second of latency against a unit of cost.
		return float64(p.LatencyMs)/1000 + p.CostPerK
	}
}

// Rank orders the models suited to a task, best first. If no profile claims
// the task, the whole catalogue is ranked instead so routing never dead-ends.
func Rank(task TaskType, obj Objective) []ModelProfile {
	var out []ModelProfile
	for i := range profiles {
		if supportsTask(&profiles[i], task) {
			out = append(out, profiles[i])
		}
	}
	if len(out) == 0 {
		out = append(out, profiles...)
	}

	sort.SliceStable(out, func(i, j int) bool {
		return Score(out[i], obj) < Score(out[j], obj)
	})
	return out
}

// SelectModel returns the single best model for a task.
func SelectModel(task TaskType, optimizeFor string) ModelProfile {
	return Rank(task, Objective(optimizeFor))[0]
}

func supportsTask(p *ModelProfile, task TaskType) bool {
	for _, t := range p.Tasks {
		if t == task {
			return true
		}
	}
	return false
}

// Profiles returns a copy of the known model catalogue.
func Profiles() []ModelProfile {
	out := make([]ModelProfile, len(profiles))
	copy(out, profiles)
	return out
}
