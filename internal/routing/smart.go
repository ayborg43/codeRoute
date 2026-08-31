// Package routing picks which model should serve a request. The catalogue it
// ranks over is data, not code: the built-in defaults can be replaced or
// extended at deploy time, and measured latencies feed back into the ranking.
package routing

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/coderouter/coderouter/internal/provider"
)

type TaskType string

const (
	TaskCodeGeneration TaskType = "code_generation"
	TaskConversation   TaskType = "conversation"
	TaskAnalysis       TaskType = "analysis"
	TaskCreative       TaskType = "creative"
	TaskUnknown        TaskType = "unknown"
)

// ModelProfile describes one routable model. Prices are USD per million
// tokens, matching how every provider publishes them.
type ModelProfile struct {
	Model           string     `json:"model"`
	Provider        string     `json:"provider"`
	LatencyMs       int        `json:"latency_ms"`
	InputCostPer1M  float64    `json:"input_cost_per_1m"`
	OutputCostPer1M float64    `json:"output_cost_per_1m"`
	Tasks           []TaskType `json:"tasks"`

	// Observed is the median latency measured from real traffic. Zero means
	// nothing has been measured yet and LatencyMs stands in.
	Observed int `json:"-"`

	// PriceKnown records whether the prices above were actually published.
	// A curated entry always states its price; a discovered one may not, and
	// unpriced is not the same as free.
	PriceKnown bool `json:"-"`

	// Reliability is what this deployment's own traffic says about the model.
	// Empty until enough calls have been made to say anything.
	Reliability Reliability `json:"-"`
}

// Free reports whether the model is published as costing nothing. A model with
// no published price is never free — that distinction is what keeps free-only
// routing from quietly spending money.
func (p ModelProfile) Free() bool {
	return p.PriceKnown && p.InputCostPer1M == 0 && p.OutputCostPer1M == 0
}

// EffectiveLatency prefers measured latency over the catalogue's estimate.
func (p ModelProfile) EffectiveLatency() int {
	if p.Observed > 0 {
		return p.Observed
	}
	return p.LatencyMs
}

// BlendedCostPer1M weights input against output at a 3:1 ratio, roughly what a
// coding assistant sends. Ranking on input price alone would flatter models
// that charge little to read and a great deal to write.
func (p ModelProfile) BlendedCostPer1M() float64 {
	return 0.75*p.InputCostPer1M + 0.25*p.OutputCostPer1M
}

// CostUSD prices one completion. Returns 0 for a model with no published
// price, so an unknown model is never billed at a guess.
func (p ModelProfile) CostUSD(promptTokens, completionTokens int) float64 {
	if p.InputCostPer1M == 0 && p.OutputCostPer1M == 0 {
		return 0
	}
	const perMillion = 1_000_000
	return float64(promptTokens)/perMillion*p.InputCostPer1M +
		float64(completionTokens)/perMillion*p.OutputCostPer1M
}

func (p *ModelProfile) supports(task TaskType) bool {
	for _, t := range p.Tasks {
		if t == task {
			return true
		}
	}
	return false
}

// defaultProfiles is the lineup a deployment gets with no MODEL_CATALOG set.
var defaultProfiles = []ModelProfile{
	{Model: "gpt-4o-mini", Provider: "openai", LatencyMs: 800, InputCostPer1M: 0.15, OutputCostPer1M: 0.60,
		Tasks: []TaskType{TaskConversation, TaskCodeGeneration}},
	{Model: "gpt-4o", Provider: "openai", LatencyMs: 2000, InputCostPer1M: 2.50, OutputCostPer1M: 10.00,
		Tasks: []TaskType{TaskAnalysis, TaskCodeGeneration}},
	{Model: "claude-3-haiku-20240307", Provider: "anthropic", LatencyMs: 600, InputCostPer1M: 0.25, OutputCostPer1M: 1.25,
		Tasks: []TaskType{TaskConversation, TaskCodeGeneration}},
	{Model: "claude-3-5-sonnet-20241022", Provider: "anthropic", LatencyMs: 1500, InputCostPer1M: 3.00, OutputCostPer1M: 15.00,
		Tasks: []TaskType{TaskAnalysis, TaskCodeGeneration}},
	{Model: "gemini-1.5-flash", Provider: "google", LatencyMs: 500, InputCostPer1M: 0.075, OutputCostPer1M: 0.30,
		Tasks: []TaskType{TaskConversation, TaskCreative}},
	{Model: "gemini-1.5-pro", Provider: "google", LatencyMs: 1800, InputCostPer1M: 1.25, OutputCostPer1M: 5.00,
		Tasks: []TaskType{TaskAnalysis, TaskCreative}},
}

// Catalog is the set of models routing may choose between. It is safe for
// concurrent use: request handling reads it while latency feedback rewrites it.
type Catalog struct {
	mu       sync.RWMutex
	profiles []ModelProfile

	// confirmed holds models a probe has shown this account may actually use.
	// Kept apart from tags because it is evidence rather than instruction, and
	// it expires.
	confirmed map[string]bool

	// tags are an operator's own statements about which models suit which
	// work, keyed by provider/model. Where a task has any tagged model,
	// routing for that task uses only those — a tag is an instruction, not
	// another hint to weigh against the rest.
	tags map[string][]TaskType

	// discovered holds what each provider says it serves, keyed by provider.
	// It is kept apart from the curated profiles because the two answer
	// different questions: curated entries carry task suitability and drive
	// smart routing, while discovered entries resolve a model the caller
	// named, price it, and supply the free-only pool. A provider reporting
	// four hundred models must not drown six hand-tuned ones.
	discovered map[string][]ModelProfile
}

// NewCatalog returns the built-in catalogue.
func NewCatalog() *Catalog {
	out := make([]ModelProfile, len(defaultProfiles))
	copy(out, defaultProfiles)
	for i := range out {
		out[i].PriceKnown = true
	}
	return &Catalog{
		profiles:   out,
		discovered: map[string][]ModelProfile{},
		tags:       map[string][]TaskType{},
		confirmed:  map[string]bool{},
	}
}

// LoadCatalog parses a JSON catalogue. An object with "replace": true discards
// the built-ins; otherwise its models are merged over them, so a deployment can
// correct one price without restating the whole lineup.
func LoadCatalog(data []byte) (*Catalog, error) {
	var doc struct {
		Replace bool           `json:"replace"`
		Models  []ModelProfile `json:"models"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		// Also accept a bare array, which is the obvious thing to write.
		var bare []ModelProfile
		if err2 := json.Unmarshal(data, &bare); err2 != nil {
			return nil, fmt.Errorf("model catalogue is not valid JSON: %w", err)
		}
		doc.Models = bare
	}

	c := NewCatalog()
	if doc.Replace {
		c.profiles = nil
	}
	for _, p := range doc.Models {
		if err := validateProfile(p); err != nil {
			return nil, err
		}
		// An operator writing a price means it; that is what makes marking a
		// self-hosted model as free possible.
		p.PriceKnown = true
		c.upsert(p)
	}

	if len(c.profiles) == 0 {
		return nil, fmt.Errorf("model catalogue is empty; routing would have nothing to choose from")
	}
	return c, nil
}

func validateProfile(p ModelProfile) error {
	if strings.TrimSpace(p.Model) == "" {
		return fmt.Errorf("catalogue entry is missing a model name")
	}
	if strings.TrimSpace(p.Provider) == "" {
		return fmt.Errorf("catalogue entry %q is missing a provider", p.Model)
	}
	if p.LatencyMs <= 0 {
		return fmt.Errorf("catalogue entry %q needs a positive latency_ms estimate", p.Model)
	}
	if p.InputCostPer1M < 0 || p.OutputCostPer1M < 0 {
		return fmt.Errorf("catalogue entry %q has a negative price", p.Model)
	}
	if len(p.Tasks) == 0 {
		return fmt.Errorf("catalogue entry %q lists no tasks; it could never be selected", p.Model)
	}
	return nil
}

// upsert replaces a same-named entry in place, keeping catalogue order stable.
func (c *Catalog) upsert(p ModelProfile) {
	for i := range c.profiles {
		if c.profiles[i].Model == p.Model {
			c.profiles[i] = p
			return
		}
	}
	c.profiles = append(c.profiles, p)
}

// Profiles returns a snapshot of the catalogue.
func (c *Catalog) Profiles() []ModelProfile {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make([]ModelProfile, len(c.profiles))
	copy(out, c.profiles)
	return out
}

// ObserveLatencies records median latencies measured from real traffic, keyed
// by model. Models absent from the map keep whatever they had.
func (c *Catalog) ObserveLatencies(measured map[string]int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for i := range c.profiles {
		if ms, ok := measured[c.profiles[i].Model]; ok && ms > 0 {
			c.profiles[i].Observed = ms
		}
	}
}

// ObservationKey identifies a model at one provider. The same model served by
// two providers can behave quite differently, so they are scored apart.
func ObservationKey(providerName, model string) string {
	return providerName + "/" + model
}

// ObserveReliability folds measured behaviour into the catalogue. Entries
// absent from the map keep whatever was previously known about them, so a
// quiet hour does not erase a model's history.
func (c *Catalog) ObserveReliability(measured map[string]Reliability) {
	c.mu.Lock()
	defer c.mu.Unlock()

	apply := func(p *ModelProfile) {
		r, ok := measured[ObservationKey(p.Provider, p.Model)]
		if !ok {
			return
		}
		p.Reliability = r
		if r.MedianLatencyMs > 0 {
			p.Observed = r.MedianLatencyMs
		}
	}

	for i := range c.profiles {
		apply(&c.profiles[i])
	}
	for name := range c.discovered {
		models := c.discovered[name]
		for i := range models {
			apply(&models[i])
		}
		c.discovered[name] = models
	}
}

// RankForTask orders models for a kind of work using everything known: whether
// they work, whether they suit the task, speed, and price.
func (c *Catalog) RankForTask(pool []ModelProfile, task TaskType, obj Objective) []ModelProfile {
	out := make([]ModelProfile, len(pool))
	copy(out, pool)

	sort.SliceStable(out, func(i, j int) bool {
		return ScoreWithEvidence(out[i], task, obj) < ScoreWithEvidence(out[j], task, obj)
	})
	return out
}

// Objective is the axis smart routing optimizes along.
type Objective string

const (
	ObjectiveBalanced Objective = "balanced"
	ObjectiveLatency  Objective = "latency"
	ObjectiveCost     Objective = "cost"
)

// Score rates a profile against an objective; lower is better.
//
// Every axis is expressed on a comparable scale — latency in seconds, cost in
// dollars per million tokens — so that a penalty applied on top means the same
// thing whichever objective is in force. Returning raw milliseconds here would
// make a latency score two or three orders of magnitude larger than a cost
// one, and any penalty added to it would vanish into the noise.
func Score(p ModelProfile, obj Objective) float64 {
	latencySeconds := float64(p.EffectiveLatency()) / 1000

	switch obj {
	case ObjectiveLatency:
		return latencySeconds
	case ObjectiveCost:
		return p.BlendedCostPer1M()
	default:
		// Balanced trades a second of latency against a dollar per million.
		return latencySeconds + p.BlendedCostPer1M()
	}
}

// Rank orders the models suited to a task, best first. If no profile claims
// the task, the whole catalogue is ranked instead so routing never dead-ends.
func (c *Catalog) Rank(task TaskType, obj Objective) []ModelProfile {
	all := c.Profiles()

	var out []ModelProfile
	for i := range all {
		if all[i].supports(task) {
			out = append(out, all[i])
		}
	}
	if len(out) == 0 {
		out = all
	}

	sort.SliceStable(out, func(i, j int) bool {
		return Score(out[i], obj) < Score(out[j], obj)
	})
	return out
}

// SelectModel returns the single best model for a task.
func (c *Catalog) SelectModel(task TaskType, optimizeFor string) ModelProfile {
	return c.Rank(task, Objective(optimizeFor))[0]
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

// SetDiscovered replaces what is known about one provider's models. Discovery
// is per-provider so a provider that fails to answer keeps its previous list
// rather than disappearing from routing.
func (c *Catalog) SetDiscovered(providerName string, models []provider.DiscoveredModel) {
	converted := make([]ModelProfile, 0, len(models))
	for _, m := range models {
		converted = append(converted, ModelProfile{
			Model:           m.Model,
			Provider:        m.Provider,
			InputCostPer1M:  m.InputCostPer1M,
			OutputCostPer1M: m.OutputCostPer1M,
			PriceKnown:      m.PriceKnown,
			// Discovered models carry no task suitability, so they are never
			// picked by task-based smart routing — only named, or chosen from
			// the free pool.
			LatencyMs: 0,
		})
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.discovered == nil {
		c.discovered = map[string][]ModelProfile{}
	}
	c.discovered[providerName] = converted
}

// ForgetDiscovered drops a provider's models, for when its key is removed.
func (c *Catalog) ForgetDiscovered(providerName string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.discovered, providerName)
}

// Discovered returns every discovered model across all providers.
func (c *Catalog) Discovered() []ModelProfile {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var out []ModelProfile
	for _, models := range c.discovered {
		out = append(out, models...)
	}
	sortByProviderThenModel(out)
	return out
}

// DiscoveredCount reports how many models each provider reported.
func (c *Catalog) DiscoveredCount() map[string]int {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make(map[string]int, len(c.discovered))
	for name, models := range c.discovered {
		out[name] = len(models)
	}
	return out
}

// Resolve lists every place a named model can be served, curated entries
// first. An empty result means nothing knows that model.
func (c *Catalog) Resolve(model string) []ModelProfile {
	if model == "" {
		return nil
	}
	target := strings.ToLower(model)

	c.mu.RLock()
	defer c.mu.RUnlock()

	var out []ModelProfile
	for _, p := range c.profiles {
		if strings.ToLower(p.Model) == target {
			out = append(out, p)
		}
	}
	for _, models := range c.discovered {
		for _, p := range models {
			if strings.ToLower(p.Model) == target {
				out = append(out, p)
			}
		}
	}

	// Several providers serve the same open-weight models. Cheapest first, so
	// naming a model without naming a provider does not cost more than it has
	// to; the caller can pin a provider by prefixing "provider/model".
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].BlendedCostPer1M() < out[j].BlendedCostPer1M()
	})
	return out
}

// FreeModels returns every model published as costing nothing, cheapest-first
// ordering being meaningless here, so widest context first instead.
func (c *Catalog) FreeModels() []ModelProfile {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var out []ModelProfile
	for _, p := range c.profiles {
		if p.Free() {
			out = append(out, p)
		}
	}
	for _, models := range c.discovered {
		for _, p := range models {
			if p.Free() {
				out = append(out, p)
			}
		}
	}

	sortByProviderThenModel(out)
	return out
}

// Lookup finds a profile by model name, for pricing a completion. Curated
// entries win, then whichever discovered entry is cheapest.
func (c *Catalog) Lookup(model string) (ModelProfile, bool) {
	matches := c.Resolve(model)
	if len(matches) == 0 {
		return ModelProfile{}, false
	}
	return matches[0], true
}

func sortByProviderThenModel(in []ModelProfile) {
	sort.SliceStable(in, func(i, j int) bool {
		if in[i].Provider != in[j].Provider {
			return in[i].Provider < in[j].Provider
		}
		return in[i].Model < in[j].Model
	})
}

// ServesModel reports whether a provider still lists a model.
//
// Discovery is the current truth; the curated catalogue is a hand-written list
// that ages. When a provider has been discovered and does not mention a model,
// that model is gone — vendors retire them — and routing to it wastes an
// attempt on a guaranteed 404. Providers that have not been discovered return
// true, because no information is not the same as a negative.
func (c *Catalog) ServesModel(providerName, model string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	models, known := c.discovered[providerName]
	if !known || len(models) == 0 {
		return true
	}

	target := strings.ToLower(model)
	for _, p := range models {
		if strings.ToLower(p.Model) == target {
			return true
		}
	}
	return false
}

// TagKey identifies one model at one provider, matching the storage layer.
func TagKey(providerName, model string) string { return providerName + "/" + model }

// SetTags replaces the operator's declarations wholesale.
func (c *Catalog) SetTags(tags map[string][]TaskType) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.tags = make(map[string][]TaskType, len(tags))
	for k, v := range tags {
		c.tags[k] = append([]TaskType{}, v...)
	}
}

// TagsFor returns what an operator has said about one model.
func (c *Catalog) TagsFor(providerName, model string) []TaskType {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return append([]TaskType{}, c.tags[TagKey(providerName, model)]...)
}

// HasTagsFor reports whether any model has been marked for a task. This is
// what turns tagging from a preference into a restriction: until an operator
// has named at least one model for a kind of work, routing keeps inferring.
func (c *Catalog) HasTagsFor(task TaskType) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, tasks := range c.tags {
		for _, t := range tasks {
			if t == task {
				return true
			}
		}
	}
	return false
}

// TaggedFor narrows a pool to the models an operator marked for a task.
//
// Returns nil when nothing is tagged, which callers read as "no restriction"
// rather than "no models" — the two are very different, and conflating them
// would make an untagged deployment unroutable.
func (c *Catalog) TaggedFor(pool []ModelProfile, task TaskType) []ModelProfile {
	if !c.HasTagsFor(task) {
		return nil
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	var out []ModelProfile
	for _, p := range pool {
		for _, t := range c.tags[TagKey(p.Provider, p.Model)] {
			if t == task {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

// TaggedCount reports how many models are marked for each task.
func (c *Catalog) TaggedCount() map[TaskType]int {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := map[TaskType]int{}
	for _, tasks := range c.tags {
		for _, t := range tasks {
			out[t]++
		}
	}
	return out
}

// SetConfirmed records which models a probe has shown to work for this
// account, keyed provider/model.
func (c *Catalog) SetConfirmed(confirmed map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.confirmed = make(map[string]bool, len(confirmed))
	for k, v := range confirmed {
		if v {
			c.confirmed[k] = true
		}
	}
}

// Confirmed reports whether a probe has recently shown this model to work.
func (c *Catalog) Confirmed(providerName, model string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.confirmed[TagKey(providerName, model)]
}

// AnyConfirmed reports whether probing has established anything at all.
//
// This is what makes the preference safe to apply: until a sweep has run there
// is nothing to prefer, and treating an unprobed deployment as "nothing works"
// would make it unroutable.
func (c *Catalog) AnyConfirmed() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.confirmed) > 0
}

// PreferConfirmed reorders a pool so models shown to work come first.
//
// A preference rather than a filter: a model that has not been probed is not
// known to be broken, and excluding it would make routing depend on a sweep
// having reached everything — which it deliberately never does.
func (c *Catalog) PreferConfirmed(pool []ModelProfile) []ModelProfile {
	if !c.AnyConfirmed() {
		return pool
	}

	out := make([]ModelProfile, len(pool))
	copy(out, pool)

	sort.SliceStable(out, func(i, j int) bool {
		a := c.Confirmed(out[i].Provider, out[i].Model)
		b := c.Confirmed(out[j].Provider, out[j].Model)
		return a && !b
	})
	return out
}
