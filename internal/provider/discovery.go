package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DiscoveredModel is one model an upstream reports that it serves.
//
// Price is per million tokens, and is only set when the provider actually
// publishes it. PriceKnown distinguishes "this model is free" from "nobody
// said what this costs", which matters a great deal when routing is asked to
// stay free: an unpriced model is never assumed to be free.
type DiscoveredModel struct {
	Provider string
	Model    string

	InputCostPer1M  float64
	OutputCostPer1M float64
	PriceKnown      bool

	ContextLength int
}

// Free reports whether the model is published as costing nothing.
func (m DiscoveredModel) Free() bool {
	return m.PriceKnown && m.InputCostPer1M == 0 && m.OutputCostPer1M == 0
}

// discoveryTimeout bounds a catalogue refresh against one provider.
const discoveryTimeout = 30 * time.Second

// ErrRejected means the provider refused the key. It is separated from other
// failures because it is the one an operator can act on: retype the key.
var ErrRejected = errors.New("the provider rejected this key")

// classify turns an upstream status into either a rejection or a plain error.
//
// Most providers answer 401 or 403 for a bad key, but not all: xAI returns 400
// with the reason in the body. Treating that as a generic failure would tell
// someone with a mistyped key to check their network.
func classify(name string, status int, body []byte) error {
	text := strings.TrimSpace(string(body))

	switch {
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return fmt.Errorf("%w (%s said %d)", ErrRejected, name, status)
	case status == http.StatusBadRequest && mentionsCredentials(text):
		return fmt.Errorf("%w (%s said %d: %s)", ErrRejected, name, status, truncate(text, 200))
	default:
		return HTTPError(name, status, body)
	}
}

// mentionsCredentials looks for a provider saying the problem is the key.
func mentionsCredentials(body string) bool {
	lower := strings.ToLower(body)
	for _, marker := range []string{"api key", "api_key", "apikey", "unauthor", "authenticat", "credential", "token"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// flexNumber accepts a JSON number or a decimal string, because vendors
// disagree about which to use for prices.
type flexNumber struct {
	Value float64
	Set   bool
}

func (f *flexNumber) UnmarshalJSON(b []byte) error {
	text := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if text == "" || text == "null" {
		return nil
	}
	v, err := strconv.ParseFloat(text, 64)
	if err != nil {
		// An unparseable price is reported as absent rather than failing the
		// whole model list over one odd row.
		return nil
	}
	f.Value, f.Set = v, true
	return nil
}

// pricing covers the two shapes vendors publish.
//
//	OpenRouter: {"prompt":"0.0000008","completion":"0.0000024"}   per token
//	xKiro:      {"unit":"per_1m_tokens","input":0.75,"output":1.5} per million
//
// The field names differ and so does the scale, so the unit governs where it
// is stated and convention fills in where it is not.
type pricing struct {
	Prompt     flexNumber `json:"prompt"`
	Completion flexNumber `json:"completion"`
	Input      flexNumber `json:"input"`
	Output     flexNumber `json:"output"`
	Unit       string     `json:"unit"`
	Currency   string     `json:"currency"`
}

// perMillion resolves a pricing block to input and output cost per million
// tokens. It reports false when nothing usable was published — which is not
// the same as free, and must never be collapsed into a zero.
func (p *pricing) perMillion() (float64, float64, bool) {
	if p == nil {
		return 0, 0, false
	}
	// A price in another currency is not comparable with the rest of the
	// catalogue, so it is treated as unpublished rather than silently mixed in.
	if p.Currency != "" && !strings.EqualFold(p.Currency, "USD") {
		return 0, 0, false
	}

	unit := strings.ToLower(strings.TrimSpace(p.Unit))
	perMillionUnit := strings.Contains(unit, "1m") || strings.Contains(unit, "million")

	switch {
	case p.Input.Set && p.Output.Set:
		if perMillionUnit {
			return p.Input.Value, p.Output.Value, true
		}
		if unit == "" {
			// input/output with no stated unit is ambiguous: 0.75 could be
			// per million or a per-token price a thousand times larger than
			// any real one. Guessing wrong either hides a cost or invents
			// one, so it is reported as unpublished.
			return 0, 0, false
		}
		return p.Input.Value * 1_000_000, p.Output.Value * 1_000_000, true

	case p.Prompt.Set && p.Completion.Set:
		// prompt/completion is OpenRouter's convention and is per token.
		if perMillionUnit {
			return p.Prompt.Value, p.Completion.Value, true
		}
		return p.Prompt.Value * 1_000_000, p.Completion.Value * 1_000_000, true
	}

	return 0, 0, false
}

// modelsEnvelope is the OpenAI /models shape, plus the pricing and context
// extensions several routers add.
type modelsEnvelope struct {
	Data []struct {
		ID            string   `json:"id"`
		Name          string   `json:"name"`
		Pricing       *pricing `json:"pricing"`
		ContextLength int      `json:"context_length"`
		MaxOutput     int      `json:"max_output_tokens"`
	} `json:"data"`

	// Google returns a different envelope entirely, and usefully declares what
	// each model can do.
	Models []struct {
		Name                       string   `json:"name"`
		SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
	} `json:"models"`
}

// ListModels asks a provider what it serves. Providers that do not offer a
// models endpoint, or reject the key, return an error the caller can log and
// move past — discovery is best-effort by nature.
func ListModels(ctx context.Context, client *http.Client, spec Spec, apiKey string) ([]DiscoveredModel, error) {
	if apiKey == "" {
		return nil, ErrNoKey
	}

	ctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()

	base := strings.TrimRight(spec.BaseURL, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/models", nil)
	if err != nil {
		return nil, err
	}
	for k, v := range authHeaders(spec, apiKey) {
		req.Header.Set(k, v)
	}

	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, classify(spec.Name, resp.StatusCode, body)
	}

	var env modelsEnvelope
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&env); err != nil {
		return nil, fmt.Errorf("could not read %s model list: %w", spec.Name, err)
	}

	out := make([]DiscoveredModel, 0, len(env.Data)+len(env.Models))

	for _, m := range env.Data {
		if strings.TrimSpace(m.ID) == "" {
			continue
		}
		dm := DiscoveredModel{
			Provider:      spec.Name,
			Model:         m.ID,
			ContextLength: m.ContextLength,
		}
		if in, out, ok := m.Pricing.perMillion(); ok && in >= 0 && out >= 0 {
			dm.InputCostPer1M, dm.OutputCostPer1M, dm.PriceKnown = in, out, true
		}
		out = append(out, dm)
	}

	// Google's list is "models/gemini-1.5-flash"; trim to the bare id. It also
	// includes embedding, image, audio and question-answering models that
	// cannot serve a chat completion at all, so the declared methods are the
	// filter: without it routing picks one of those and earns a 400.
	for _, m := range env.Models {
		id := strings.TrimPrefix(m.Name, "models/")
		if id == "" || !supportsChat(m.SupportedGenerationMethods) {
			continue
		}
		out = append(out, DiscoveredModel{Provider: spec.Name, Model: id})
	}

	return out, nil
}

// supportsChat reports whether a Google model can serve a chat completion.
// An empty list means the provider did not say, which is not grounds for
// excluding it.
func supportsChat(methods []string) bool {
	if len(methods) == 0 {
		return true
	}
	for _, m := range methods {
		if strings.EqualFold(m, "generateContent") || strings.EqualFold(m, "streamGenerateContent") {
			return true
		}
	}
	return false
}

// authHeaders renders the auth a provider's dialect expects.
func authHeaders(spec Spec, apiKey string) map[string]string {
	switch spec.Kind {
	case KindAnthropic:
		return map[string]string{"x-api-key": apiKey, "anthropic-version": anthropicVersion}
	case KindGoogle:
		return map[string]string{"x-goog-api-key": apiKey}
	default:
		return map[string]string{"Authorization": "Bearer " + apiKey}
	}
}
