package gateway

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/coderouter/coderouter/internal/config"
	"github.com/coderouter/coderouter/internal/provider"
)

// policyGateway is only used for the model/key helpers; the eligibility policy
// itself is exercised through requestCacheable, which needs no cache at all.
func policyGateway(t *testing.T, maxTemp float64) *Gateway {
	t.Helper()

	cfg := config.Load()
	cfg.Cache.MaxTemperature = maxTemp
	cfg.DefaultModel = "gpt-4o-mini"

	gw, err := New(nil, Options{Config: cfg})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return gw
}

// cacheableRequest applies the eligibility policy at the default limit.
func cacheableRequest(req *provider.ChatRequest, maxTemp float64) bool {
	return requestCacheable(req, maxTemp)
}

func chat(temp *float64, raw string) *provider.ChatRequest {
	return &provider.ChatRequest{
		Model:       "gpt-4o-mini",
		Messages:    []provider.Message{{Role: "user", Content: "hello"}},
		Temperature: temp,
		Raw:         json.RawMessage(raw),
	}
}

func TestCacheableIsOffWhenTheCacheIs(t *testing.T) {
	g := policyGateway(t, 0.3) // no cache at all

	if g.cacheable(chat(nil, "")) {
		t.Error("requests are cacheable with no cache configured")
	}
}

func TestTemperatureGatesCaching(t *testing.T) {
	f := func(v float64) *float64 { return &v }

	cases := map[string]struct {
		temp *float64
		want bool
	}{
		"unset":     {nil, true},
		"zero":      {f(0), true},
		"at limit":  {f(0.3), true},
		"above":     {f(0.31), false},
		"creative":  {f(1.0), false},
		"very high": {f(2.0), false},
	}

	for name, tc := range cases {
		if got := cacheableRequest(chat(tc.temp, ""), 0.3); got != tc.want {
			t.Errorf("%s: cacheable = %v, want %v", name, got, tc.want)
		}
	}
}

// A cached plain-text completion is the wrong answer to a request asking for
// tool calls or a JSON schema, so those bypass the cache entirely.
func TestPassthroughRequestsAreNeverCached(t *testing.T) {
	bypass := []string{
		`{"tools":[{"type":"function"}]}`,
		`{"functions":[{"name":"f"}]}`,
		`{"tool_choice":"auto"}`,
		`{"function_call":"auto"}`,
		`{"response_format":{"type":"json_object"}}`,
		`{"logprobs":true}`,
		`{"seed":42}`,
		`{"n":3}`,
		`not json at all`,
	}
	for _, raw := range bypass {
		if cacheableRequest(chat(nil, raw), 0.3) {
			t.Errorf("a request carrying %s was considered cacheable", raw)
		}
	}

	allowed := []string{
		``,
		`{"model":"gpt-4o-mini","messages":[]}`,
		`{"tools":null}`,
		`{"stream":true,"max_tokens":100}`,
	}
	for _, raw := range allowed {
		if !cacheableRequest(chat(nil, raw), 0.3) {
			t.Errorf("an ordinary request with raw %q was excluded from the cache", raw)
		}
	}
}

func TestEmptyConversationsAreNotCacheable(t *testing.T) {
	req := &provider.ChatRequest{Model: "gpt-4o-mini"}
	if cacheableRequest(req, 0.3) {
		t.Error("a request with no messages was considered cacheable")
	}
}

// The whole conversation is the key: the same final question after different
// context has a different answer.
func TestCacheKeyCoversTheWholeConversation(t *testing.T) {
	base := &provider.ChatRequest{Messages: []provider.Message{
		{Role: "system", Content: "You are terse."},
		{Role: "user", Content: "what is it?"},
	}}
	other := &provider.ChatRequest{Messages: []provider.Message{
		{Role: "system", Content: "You are verbose."},
		{Role: "user", Content: "what is it?"},
	}}

	if cacheKey(base) == cacheKey(other) {
		t.Error("differing system prompts produced the same cache key")
	}
	if !strings.Contains(cacheKey(base), "system: You are terse.") {
		t.Errorf("key omits the system turn: %q", cacheKey(base))
	}

	same := &provider.ChatRequest{Messages: []provider.Message{
		{Role: "system", Content: "  You are terse.  "},
		{Role: "user", Content: "what is it?"},
	}}
	if cacheKey(base) != cacheKey(same) {
		t.Error("surrounding whitespace changed the cache key")
	}
}

// Entries are filed under the model the caller asked for, so a repeated "auto"
// request hits regardless of which upstream served it first.
func TestCacheModelUsesTheRequestedModel(t *testing.T) {
	g := policyGateway(t, 0.3)

	if got := g.cacheModel(&provider.ChatRequest{Model: "auto"}); got != "auto" {
		t.Errorf("cacheModel = %q, want auto", got)
	}
	if got := g.cacheModel(&provider.ChatRequest{}); got != g.cfg.DefaultModel {
		t.Errorf("cacheModel for an empty model = %q, want the default %q", got, g.cfg.DefaultModel)
	}
}

func TestCachedResponseIsOpenAIShaped(t *testing.T) {
	resp := cachedResponse("gpt-4o-mini", "hello there")

	if resp.Object != "chat.completion" || resp.Model != "gpt-4o-mini" {
		t.Errorf("envelope = %+v", resp)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message == nil {
		t.Fatalf("choices = %+v", resp.Choices)
	}
	if got := string(resp.Choices[0].Message.Content); got != "hello there" {
		t.Errorf("content = %q", got)
	}
	if resp.Choices[0].Message.Role != "assistant" {
		t.Errorf("role = %q", resp.Choices[0].Message.Role)
	}
	if resp.Choices[0].FinishReason == nil || *resp.Choices[0].FinishReason != "stop" {
		t.Error("finish_reason is not stop")
	}
	// No tokens were bought, so none are reported.
	if resp.Usage == nil || resp.Usage.TotalTokens != 0 {
		t.Errorf("usage = %+v, want zero", resp.Usage)
	}
}

func TestCachedStreamChunksLookLikeAStream(t *testing.T) {
	chunks := cachedStreamChunks(cachedResponse("m", "hi"))

	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2", len(chunks))
	}
	for i, c := range chunks {
		if c.Object != "chat.completion.chunk" {
			t.Errorf("chunk %d object = %q", i, c.Object)
		}
		if len(c.Choices) != 1 || c.Choices[0].Delta == nil {
			t.Fatalf("chunk %d has no delta: %+v", i, c.Choices)
		}
	}
	if chunks[0].Choices[0].Delta.Content != "hi" {
		t.Errorf("first chunk carried %q", chunks[0].Choices[0].Delta.Content)
	}
	if chunks[1].Choices[0].FinishReason == nil || *chunks[1].Choices[0].FinishReason != "stop" {
		t.Error("the stream never terminates")
	}
}

func TestCompletionTextExtractsTheAssistantTurn(t *testing.T) {
	if got := completionText(nil); got != "" {
		t.Errorf("completionText(nil) = %q", got)
	}

	resp := &provider.ChatResponse{Choices: []provider.Choice{
		{Message: &provider.Message{Role: "assistant", Content: "part one "}},
		{Message: &provider.Message{Role: "assistant", Content: "part two"}},
	}}
	if got := completionText(resp); got != "part one part two" {
		t.Errorf("completionText = %q", got)
	}

	// A response with only tool calls has no text; nothing to cache.
	if got := completionText(&provider.ChatResponse{Choices: []provider.Choice{{}}}); got != "" {
		t.Errorf("completionText for a message-less choice = %q", got)
	}
}

// The cache is now global: with tenancy withdrawn, any authenticated caller is
// eligible, and an unauthenticated one is too. This test exists to make that
// change deliberate rather than accidental — it used to be the opposite.
func TestCacheEligibilityNoLongerDependsOnTheCaller(t *testing.T) {
	if !cacheableRequest(chat(nil, ""), 0.3) {
		t.Error("an ordinary request was not cacheable")
	}
}
