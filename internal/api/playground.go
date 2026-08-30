package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/coderouter/coderouter/internal/provider"
)

// playgroundMaxTokens caps a playground reply. The dashboard is for trying a
// model out, not for generating at length, and an operator experimenting
// should not be able to spend a fortune with a stray zero.
const playgroundMaxTokens = 2048

// registerPlaygroundRoutes mounts the model try-it-out endpoint.
func (h *Handler) registerPlaygroundRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/playground", h.playground)
}

// playgroundResult is what the dashboard renders after a run.
type playgroundResult struct {
	// Requested is what was asked for, Answered is what actually served it.
	// They differ whenever a sentinel was used or failover stepped in, which
	// is the single most useful thing a playground can show.
	Requested string `json:"requested"`
	Answered  string `json:"answered"`
	Provider  string `json:"provider"`

	Content   string `json:"content"`
	LatencyMs int64  `json:"latency_ms"`

	TokensIn  int `json:"tokens_in"`
	TokensOut int `json:"tokens_out"`

	CostUSD    *float64 `json:"cost_usd,omitempty"`
	CacheHit   bool     `json:"cache_hit"`
	FreeModel  bool     `json:"free_model"`
	PriceKnown bool     `json:"price_known"`
}

func (h *Handler) playground(w http.ResponseWriter, r *http.Request) {
	if !h.dashboardAuthorized(w, r) {
		return
	}

	var body struct {
		Model       string   `json:"model"`
		System      string   `json:"system"`
		Prompt      string   `json:"prompt"`
		Temperature *float64 `json:"temperature"`
		MaxTokens   int      `json:"max_tokens"`
	}
	if !decodeBody(w, r, &body) {
		return
	}

	body.Prompt = strings.TrimSpace(body.Prompt)
	if body.Prompt == "" {
		writeError(w, http.StatusBadRequest, "prompt is required", "invalid_request_error")
		return
	}
	if body.MaxTokens <= 0 || body.MaxTokens > playgroundMaxTokens {
		body.MaxTokens = playgroundMaxTokens
	}

	messages := []provider.Message{}
	if system := strings.TrimSpace(body.System); system != "" {
		messages = append(messages, provider.Message{Role: "system", Content: provider.Content(system)})
	}
	messages = append(messages, provider.Message{Role: "user", Content: provider.Content(body.Prompt)})

	req := &provider.ChatRequest{
		Model:       strings.TrimSpace(body.Model),
		Messages:    messages,
		MaxTokens:   body.MaxTokens,
		Temperature: body.Temperature,
	}

	// Attributed to no client key: this traffic came from an operator trying a
	// model, not from anyone's editor, and logging it against a real key would
	// distort that key's usage.
	start := time.Now()
	resp, err := h.gw.Complete(r.Context(), req, nil)
	latency := time.Since(start)

	if err != nil {
		// The gateway's own message names every provider it tried and why each
		// declined, which is exactly what someone testing a model wants.
		writeError(w, http.StatusBadGateway, err.Error(), "upstream_error")
		return
	}

	result := playgroundResult{
		Requested: req.Model,
		Answered:  resp.Model,
		LatencyMs: latency.Milliseconds(),
	}
	for _, c := range resp.Choices {
		if c.Message != nil {
			result.Content += string(c.Message.Content)
		}
	}
	if resp.Usage != nil {
		result.TokensIn, result.TokensOut = resp.Usage.PromptTokens, resp.Usage.CompletionTokens
	}
	// A cached reply reports no tokens and comes back in single-digit
	// milliseconds; saying so stops it looking like an impossibly fast model.
	result.CacheHit = strings.HasPrefix(resp.ID, "chatcmpl-cache-")

	if p, ok := h.gw.Catalog().Lookup(resp.Model); ok {
		result.Provider = p.Provider
		result.PriceKnown = p.PriceKnown
		result.FreeModel = p.Free()
		if p.PriceKnown {
			cost := p.CostUSD(result.TokensIn, result.TokensOut)
			result.CostUSD = &cost
		}
	}

	writeJSON(w, http.StatusOK, result)
}
