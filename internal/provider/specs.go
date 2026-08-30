package provider

import (
	"fmt"
	"sort"
	"strings"
)

// Kind is the wire dialect a provider speaks. Only three exist because almost
// every vendor has settled on OpenAI's shape; the two exceptions are the ones
// that defined their own first.
type Kind string

const (
	KindOpenAI    Kind = "openai"
	KindAnthropic Kind = "anthropic"
	KindGoogle    Kind = "google"
)

// Spec describes one routable upstream. A provider is data, not code: adding
// an OpenAI-compatible vendor means adding a name and a URL, nothing more.
type Spec struct {
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	Kind    Kind   `json:"kind"`

	// Label is what the dashboard shows. Empty falls back to Name.
	Label string `json:"label,omitempty"`

	// ConsoleURL is where a user goes to get a key for this provider.
	ConsoleURL string `json:"console_url,omitempty"`

	// FreeTier notes that the provider offers usable free access. It is
	// advisory only: it never causes a model to be treated as free, because
	// a free account tier is not the same as a zero-priced model.
	FreeTier bool `json:"free_tier,omitempty"`

	// PublicModels marks a provider whose /models endpoint answers without
	// authentication. Saving a key for one of these proves the endpoint is
	// reachable but says nothing about whether the key works, so the gateway
	// must not report it as verified. Checked against each live endpoint.
	PublicModels bool `json:"public_models,omitempty"`
}

// presets are upstreams this build knows out of the box. Every base URL here
// was checked against the live endpoint, which is what distinguishes a correct
// URL from a plausible guess. That check also established which of them serve
// /models without authentication — see PublicModels.
var presets = []Spec{
	{Name: "openai", Kind: KindOpenAI, BaseURL: "https://api.openai.com/v1",
		Label: "OpenAI", ConsoleURL: "https://platform.openai.com/api-keys"},
	{Name: "anthropic", Kind: KindAnthropic, BaseURL: "https://api.anthropic.com/v1",
		Label: "Anthropic", ConsoleURL: "https://console.anthropic.com/settings/keys"},
	{Name: "google", Kind: KindGoogle, BaseURL: "https://generativelanguage.googleapis.com/v1beta",
		Label: "Google Gemini", ConsoleURL: "https://aistudio.google.com/apikey", FreeTier: true},

	// OpenAI-compatible. These need no translation code at all.
	{Name: "openrouter", Kind: KindOpenAI, BaseURL: "https://openrouter.ai/api/v1",
		Label: "OpenRouter", ConsoleURL: "https://openrouter.ai/keys", FreeTier: true, PublicModels: true},
	{Name: "groq", Kind: KindOpenAI, BaseURL: "https://api.groq.com/openai/v1",
		Label: "Groq", ConsoleURL: "https://console.groq.com/keys", FreeTier: true},
	{Name: "sambanova", Kind: KindOpenAI, BaseURL: "https://api.sambanova.ai/v1",
		Label: "SambaNova", ConsoleURL: "https://cloud.sambanova.ai/apis", FreeTier: true, PublicModels: true},
	{Name: "mistral", Kind: KindOpenAI, BaseURL: "https://api.mistral.ai/v1",
		Label: "Mistral", ConsoleURL: "https://console.mistral.ai/api-keys", FreeTier: true},
	{Name: "nvidia", Kind: KindOpenAI, BaseURL: "https://integrate.api.nvidia.com/v1",
		Label: "NVIDIA NIM", ConsoleURL: "https://build.nvidia.com", FreeTier: true, PublicModels: true},
	{Name: "huggingface", Kind: KindOpenAI, BaseURL: "https://router.huggingface.co/v1",
		Label: "Hugging Face", ConsoleURL: "https://huggingface.co/settings/tokens", FreeTier: true, PublicModels: true},
	{Name: "gmicloud", Kind: KindOpenAI, BaseURL: "https://api.gmi-serving.com/v1",
		Label: "GMI Cloud", ConsoleURL: "https://console.gmicloud.ai", FreeTier: true},
	{Name: "xai", Kind: KindOpenAI, BaseURL: "https://api.x.ai/v1",
		Label: "xAI Grok", ConsoleURL: "https://console.x.ai"},
	{Name: "deepseek", Kind: KindOpenAI, BaseURL: "https://api.deepseek.com/v1",
		Label: "DeepSeek", ConsoleURL: "https://platform.deepseek.com/api_keys"},
	{Name: "together", Kind: KindOpenAI, BaseURL: "https://api.together.xyz/v1",
		Label: "Together AI", ConsoleURL: "https://api.together.ai/settings/api-keys"},
	{Name: "cerebras", Kind: KindOpenAI, BaseURL: "https://api.cerebras.ai/v1",
		Label: "Cerebras", ConsoleURL: "https://cloud.cerebras.ai", FreeTier: true},

	// Aggregators that front many upstream models behind one key.
	{Name: "xkiro", Kind: KindOpenAI, BaseURL: "https://api.xkiro.com/v1",
		Label: "xKiro", ConsoleURL: "https://xkiro.com", PublicModels: true},
	{Name: "teamorouter", Kind: KindOpenAI, BaseURL: "https://api.teamorouter.com/v1",
		Label: "TeamoRouter", ConsoleURL: "https://teamorouter.com"},
	{Name: "bai", Kind: KindOpenAI, BaseURL: "https://api.b.ai/v1",
		Label: "B.AI", ConsoleURL: "https://chat.b.ai"},
}

// Presets returns a copy of the built-in provider list.
func Presets() []Spec {
	out := make([]Spec, len(presets))
	copy(out, presets)
	return out
}

// Preset finds a built-in spec by name.
func Preset(name string) (Spec, bool) {
	for _, s := range presets {
		if s.Name == name {
			return s, true
		}
	}
	return Spec{}, false
}

// Validate rejects a spec that could never be routed to.
func (s Spec) Validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("provider is missing a name")
	}
	if strings.ContainsAny(s.Name, " /?&#") {
		return fmt.Errorf("provider name %q may not contain spaces or URL punctuation", s.Name)
	}
	if !strings.HasPrefix(s.BaseURL, "http://") && !strings.HasPrefix(s.BaseURL, "https://") {
		return fmt.Errorf("provider %q needs an http(s) base URL, got %q", s.Name, s.BaseURL)
	}
	switch s.Kind {
	case KindOpenAI, KindAnthropic, KindGoogle:
	default:
		return fmt.Errorf("provider %q has unknown kind %q; expected openai, anthropic or google", s.Name, s.Kind)
	}
	return nil
}

// DisplayName is what a human should see.
func (s Spec) DisplayName() string {
	if s.Label != "" {
		return s.Label
	}
	return s.Name
}

// newClient builds the client for a spec's dialect.
func newClient(s Spec) Client {
	base := strings.TrimRight(s.BaseURL, "/")
	switch s.Kind {
	case KindAnthropic:
		return &Anthropic{BaseURL: base, ProviderName: s.Name}
	case KindGoogle:
		return &Google{BaseURL: base, ProviderName: s.Name}
	default:
		return &OpenAI{BaseURL: base, ProviderName: s.Name}
	}
}

// NewRegistry builds the routable set from specs, rejecting duplicates so a
// misconfigured deployment fails at startup rather than routing at random.
func NewRegistry(specs []Spec) (Registry, error) {
	reg := make(Registry, len(specs))
	for _, s := range specs {
		if err := s.Validate(); err != nil {
			return nil, err
		}
		if _, exists := reg[s.Name]; exists {
			return nil, fmt.Errorf("provider %q is defined twice", s.Name)
		}
		reg[s.Name] = newClient(s)
	}
	return reg, nil
}

// SortSpecs orders specs by display name, so the dashboard is stable.
func SortSpecs(specs []Spec) {
	sort.Slice(specs, func(i, j int) bool {
		return strings.ToLower(specs[i].DisplayName()) < strings.ToLower(specs[j].DisplayName())
	})
}
