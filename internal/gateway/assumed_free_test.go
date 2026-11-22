package gateway

import (
	"testing"

	"github.com/coderouter/coderouter/internal/config"
	"github.com/coderouter/coderouter/internal/provider"
)

// A list restored from the database carries published prices only, so the
// provider's assumption has to be reapplied on load or a restart would drop
// those models out of free-only routing until the next discovery refresh.
func TestAssumeFreeReappliesToCachedLists(t *testing.T) {
	gw, err := New(nil, Options{Config: &config.Config{Providers: []provider.Spec{
		{Name: "nvidia", Kind: provider.KindOpenAI, BaseURL: "https://integrate.api.nvidia.com/v1", AssumedFree: true},
		{Name: "plain", Kind: provider.KindOpenAI, BaseURL: "https://api.example.com/v1"},
	}}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	cached := []provider.DiscoveredModel{
		{Provider: "nvidia", Model: "unpriced"},
		{Provider: "nvidia", Model: "priced", PriceKnown: true, InputCostPer1M: 1, OutputCostPer1M: 2},
	}
	gw.assumeFree("nvidia", cached)

	if !cached[0].Free() {
		t.Error("a cached unpriced model at an assumed-free provider did not come back free")
	}
	if cached[1].Free() {
		t.Error("a cached model with a published price was overridden by the assumption")
	}

	// A provider without the mark keeps missing pricing as unknown.
	other := []provider.DiscoveredModel{{Provider: "plain", Model: "unpriced"}}
	gw.assumeFree("plain", other)
	if other[0].Free() || other[0].AssumedFree {
		t.Error("the assumption was applied to a provider that does not declare it")
	}
}
