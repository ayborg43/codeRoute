package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// pricinglessModels is NVIDIA's shape: ids and nothing else for most entries,
// with one entry that does publish a price, so the assumption is shown to lose
// to real data rather than override it.
const pricinglessModels = `{"object":"list","data":[
	{"id":"meta/llama-3.1-8b-instruct","object":"model"},
	{"id":"nvidia/nemotron-4-340b-instruct","object":"model","pricing":{"prompt":"0.000001","completion":"0.000002"}}
]}`

func listFromFake(t *testing.T, spec Spec) []DiscoveredModel {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(pricinglessModels))
	}))
	t.Cleanup(srv.Close)

	spec.BaseURL = srv.URL
	models, err := ListModels(context.Background(), srv.Client(), spec, "test-key")
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("got %d models, want 2", len(models))
	}
	return models
}

func TestListModelsAssumedFreeFillsInForMissingPricing(t *testing.T) {
	models := listFromFake(t, Spec{Name: "nvidia", Kind: KindOpenAI, AssumedFree: true})

	unpriced := models[0]
	if !unpriced.Free() {
		t.Errorf("%s is not free on an assumed-free provider", unpriced.Model)
	}
	if unpriced.PriceKnown {
		t.Error("an assumed-free model was reported as having a published price")
	}
	if unpriced.InputCostPer1M != 0 || unpriced.OutputCostPer1M != 0 {
		t.Errorf("a price was invented: %v/%v", unpriced.InputCostPer1M, unpriced.OutputCostPer1M)
	}

	// A published price always wins, so the assumption can never hide a cost.
	priced := models[1]
	if priced.Free() {
		t.Errorf("%s has a published price but was reported free", priced.Model)
	}
	if !priced.PriceKnown || priced.AssumedFree {
		t.Errorf("%s: PriceKnown=%v AssumedFree=%v, want true/false", priced.Model, priced.PriceKnown, priced.AssumedFree)
	}
}

// Without the mark, missing pricing stays unknown — the rule that keeps
// free-only routing from quietly spending money.
func TestListModelsUnpricedStaysUnknownWithoutAssumption(t *testing.T) {
	models := listFromFake(t, Spec{Name: "plain", Kind: KindOpenAI, FreeTier: true})

	if models[0].Free() {
		t.Error("an unpriced model was treated as free merely because of a free tier")
	}
	if models[0].AssumedFree {
		t.Error("an unpriced model was marked assumed-free on a provider that does not declare it")
	}
}
