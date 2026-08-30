package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/coderouter/coderouter/internal/db"
	"github.com/coderouter/coderouter/internal/provider"
	"github.com/coderouter/coderouter/internal/routing"
)

// passthroughFields are OpenAI request fields whose responses this gateway does
// not model. A cached plain-text completion would be a wrong answer to a
// request asking for tool calls or a JSON schema, so such requests bypass the
// cache in both directions.
var passthroughFields = []string{
	"tools", "functions", "tool_choice", "function_call",
	"response_format", "logprobs", "seed", "n",
}

// cacheable reports whether a request may be answered from, or written to, the
// cache: the cache must be usable, and the request itself eligible.
func (g *Gateway) cacheable(req *provider.ChatRequest) bool {
	return g.cache.Enabled() && requestCacheable(req, g.cfg.Cache.MaxTemperature)
}

// requestCacheable is the policy on its own, with no dependence on whether a
// cache happens to be configured. Anything asking for varied or structured
// output is excluded.
func requestCacheable(req *provider.ChatRequest, maxTemperature float64) bool {
	// A caller asking for varied output should not be handed a replay. An
	// unset temperature is treated as cacheable: editors overwhelmingly send
	// deterministic completion requests, and a caller that wants fresh output
	// every time can say so by raising the temperature.
	if req.Temperature != nil && *req.Temperature > maxTemperature {
		return false
	}
	if usesPassthroughFields(req.Raw) {
		return false
	}
	return len(req.Messages) > 0
}

// usesPassthroughFields reports whether the client's original body carries any
// field whose response shape the cache cannot faithfully reproduce.
func usesPassthroughFields(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		// An unparseable body is not worth guessing about; skip the cache.
		return true
	}

	for _, name := range passthroughFields {
		v, present := fields[name]
		if present && !isJSONNull(v) {
			return true
		}
	}
	return false
}

func isJSONNull(v json.RawMessage) bool {
	return len(v) == 0 || string(v) == "null"
}

// cacheKey renders the whole conversation, not just the last turn: the same
// question after different context has a different answer.
func cacheKey(req *provider.ChatRequest) string {
	var sb strings.Builder
	for _, m := range req.Messages {
		sb.WriteString(m.Role)
		sb.WriteString(": ")
		sb.WriteString(strings.TrimSpace(string(m.Content)))
		sb.WriteByte('\n')
	}
	return strings.TrimSpace(sb.String())
}

// cacheModel is the model the entry is filed under. Requests are keyed by the
// model the *caller* asked for, not the one routing settled on, so a repeated
// "auto" request hits regardless of which upstream served it the first time.
func (g *Gateway) cacheModel(req *provider.ChatRequest) string {
	if req.Model == "" {
		return g.cfg.DefaultModel
	}
	return req.Model
}

// serveFromCache answers a request from a previous identical one. It reports
// whether it handled the request; every failure path reports false so the
// caller simply continues upstream.
func (g *Gateway) serveFromCache(ctx context.Context, req *provider.ChatRequest, key *db.ClientKey, emit func(*provider.ChatResponse) error) (*provider.ChatResponse, bool) {
	if !g.cacheable(req) {
		return nil, false
	}

	model := g.cacheModel(req)
	start := time.Now()

	content, hit, err := g.cache.Lookup(ctx, model, cacheKey(req))
	if err != nil {
		log.Printf("cache: lookup failed, continuing upstream: %v", err)
		return nil, false
	}
	if !hit {
		return nil, false
	}

	g.logUsage(usageRecord{
		key:      key,
		cand:     candidate{provider: cacheProvider, model: model},
		latency:  time.Since(start),
		status:   "success",
		task:     routing.DetectTask(lastUserPrompt(req)),
		cacheHit: true,
	})

	resp := cachedResponse(model, content)

	if emit == nil {
		return resp, true
	}

	// Replay a hit as a well-formed two-chunk stream: content, then the stop.
	// Clients that only read deltas see exactly what an upstream would send.
	for _, chunk := range cachedStreamChunks(resp) {
		if err := emit(chunk); err != nil {
			return nil, true
		}
	}
	return nil, true
}

// storeInCache saves a completion for reuse. Caching is opportunistic: a
// failure here is logged and forgotten, never surfaced to the caller whose
// request already succeeded.
func (g *Gateway) storeInCache(ctx context.Context, req *provider.ChatRequest, completion string) {
	if !g.cacheable(req) || strings.TrimSpace(completion) == "" {
		return
	}

	// The caller's context is cancelled the moment its response is written,
	// which would abort the write; give the store its own short budget.
	storeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()

	if err := g.cache.Store(storeCtx, g.cacheModel(req), cacheKey(req), completion); err != nil {
		log.Printf("cache: store failed: %v", err)
	}
}

// completionText extracts the assistant text from a non-streaming response.
func completionText(resp *provider.ChatResponse) string {
	if resp == nil {
		return ""
	}
	var sb strings.Builder
	for _, c := range resp.Choices {
		if c.Message != nil {
			sb.WriteString(string(c.Message.Content))
		}
	}
	return sb.String()
}

// cachedResponse renders stored text as an OpenAI-shaped completion. Usage is
// reported as zero because no tokens were bought; the cache_hit column in
// usage_logs is what distinguishes this from a request that used none.
func cachedResponse(model, content string) *provider.ChatResponse {
	stop := "stop"
	now := time.Now()

	return &provider.ChatResponse{
		ID:      fmt.Sprintf("chatcmpl-cache-%d", now.UnixNano()),
		Object:  "chat.completion",
		Created: now.Unix(),
		Model:   model,
		Choices: []provider.Choice{{
			Index:        0,
			Message:      &provider.Message{Role: "assistant", Content: provider.Content(content)},
			FinishReason: &stop,
		}},
		Usage: &provider.Usage{},
	}
}

// cachedStreamChunks turns a cached completion into the chunk sequence a
// streaming client expects.
func cachedStreamChunks(resp *provider.ChatResponse) []*provider.ChatResponse {
	stop := "stop"

	content := ""
	if len(resp.Choices) > 0 && resp.Choices[0].Message != nil {
		content = string(resp.Choices[0].Message.Content)
	}

	body := &provider.ChatResponse{
		ID: resp.ID, Object: "chat.completion.chunk", Created: resp.Created, Model: resp.Model,
		Choices: []provider.Choice{{
			Index: 0,
			Delta: &provider.Delta{Role: "assistant", Content: content},
		}},
	}
	final := &provider.ChatResponse{
		ID: resp.ID, Object: "chat.completion.chunk", Created: resp.Created, Model: resp.Model,
		Choices: []provider.Choice{{
			Index: 0, Delta: &provider.Delta{}, FinishReason: &stop,
		}},
	}

	return []*provider.ChatResponse{body, final}
}
