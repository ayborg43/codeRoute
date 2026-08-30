package provider

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func decodeBody(t *testing.T, body io.Reader) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.NewDecoder(body).Decode(&out); err != nil {
		t.Fatalf("decoding request body: %v", err)
	}
	return out
}

func collect(t *testing.T, chunks *[]*ChatResponse) func(*ChatResponse) error {
	t.Helper()
	return func(c *ChatResponse) error {
		*chunks = append(*chunks, c)
		return nil
	}
}

func joinDeltas(chunks []*ChatResponse) string {
	var sb strings.Builder
	for _, c := range chunks {
		for _, ch := range c.Choices {
			if ch.Delta != nil {
				sb.WriteString(ch.Delta.Content)
			}
		}
	}
	return sb.String()
}

func TestContentAcceptsStringAndParts(t *testing.T) {
	var m Message
	if err := json.Unmarshal([]byte(`{"role":"user","content":"plain"}`), &m); err != nil {
		t.Fatalf("string content: %v", err)
	}
	if m.Content != "plain" {
		t.Errorf("string content = %q, want %q", m.Content, "plain")
	}

	if err := json.Unmarshal([]byte(`{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}`), &m); err != nil {
		t.Fatalf("part content: %v", err)
	}
	if m.Content != "ab" {
		t.Errorf("part content = %q, want %q", m.Content, "ab")
	}
}

func TestSystemAndTurnsMergesConsecutiveRoles(t *testing.T) {
	req := &ChatRequest{Messages: []Message{
		{Role: "system", Content: "be terse"},
		{Role: "user", Content: "one"},
		{Role: "user", Content: "two"},
		{Role: "assistant", Content: "ok"},
		{Role: "system", Content: "and precise"},
	}}

	system, turns := req.SystemAndTurns()

	if system != "be terse\n\nand precise" {
		t.Errorf("system = %q", system)
	}
	if len(turns) != 2 {
		t.Fatalf("turns = %d, want 2", len(turns))
	}
	if turns[0].Content != "one\n\ntwo" {
		t.Errorf("merged user turn = %q", turns[0].Content)
	}
}

func TestProviderFor(t *testing.T) {
	cases := map[string]string{
		"gpt-4o-mini":                "openai",
		"o3-mini":                    "openai",
		"claude-3-5-sonnet-20241022": "anthropic",
		"gemini-1.5-flash":           "google",
		"llama-3":                    "",
	}
	for model, want := range cases {
		if got := ProviderFor(model); got != want {
			t.Errorf("ProviderFor(%q) = %q, want %q", model, got, want)
		}
	}
}

func TestOpenAIPreservesUnmodeledFields(t *testing.T) {
	raw := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function"}],"response_format":{"type":"json_object"}}`
	req := &ChatRequest{Model: "gpt-4o", Raw: json.RawMessage(raw)}

	o := &OpenAI{BaseURL: "https://api.openai.com/v1"}
	hreq, err := o.BuildRequest(context.Background(), req, "sk-test", true)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}

	body := decodeBody(t, hreq.Body)
	if _, ok := body["tools"]; !ok {
		t.Error("tools were dropped from the forwarded body")
	}
	if _, ok := body["response_format"]; !ok {
		t.Error("response_format was dropped from the forwarded body")
	}
	// Fallback routing may rewrite the model; the override must win.
	if body["model"] != "gpt-4o" {
		t.Errorf("model = %v, want gpt-4o", body["model"])
	}
	if body["stream"] != true {
		t.Errorf("stream = %v, want true", body["stream"])
	}
	if _, ok := body["stream_options"]; !ok {
		t.Error("stream_options missing; streamed usage would be lost")
	}
	if got := hreq.Header.Get("Authorization"); got != "Bearer sk-test" {
		t.Errorf("Authorization = %q", got)
	}
}

func TestAnthropicBuildRequest(t *testing.T) {
	req := &ChatRequest{
		Model: "claude-3-haiku-20240307",
		Messages: []Message{
			{Role: "system", Content: "be terse"},
			{Role: "assistant", Content: "earlier reply"},
			{Role: "user", Content: "hi"},
		},
	}

	a := &Anthropic{BaseURL: "https://api.anthropic.com/v1"}
	hreq, err := a.BuildRequest(context.Background(), req, "sk-ant", false)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}

	if got := hreq.URL.String(); got != "https://api.anthropic.com/v1/messages" {
		t.Errorf("URL = %q, want /v1/messages", got)
	}
	if got := hreq.Header.Get("x-api-key"); got != "sk-ant" {
		t.Errorf("x-api-key = %q", got)
	}
	if hreq.Header.Get("anthropic-version") == "" {
		t.Error("anthropic-version header is required and missing")
	}

	body := decodeBody(t, hreq.Body)
	if body["system"] != "be terse" {
		t.Errorf("system = %v; system messages must be hoisted out of messages", body["system"])
	}
	// Anthropic rejects a request whose max_tokens is absent.
	if body["max_tokens"] != float64(anthropicDefaultMaxTokens) {
		t.Errorf("max_tokens = %v, want default %d", body["max_tokens"], anthropicDefaultMaxTokens)
	}

	msgs, _ := body["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3 (a synthetic user turn must lead)", len(msgs))
	}
	if first, _ := msgs[0].(map[string]any); first["role"] != "user" {
		t.Errorf("first role = %v, want user", first["role"])
	}
}

func TestAnthropicDecodeResponse(t *testing.T) {
	body := `{"id":"msg_1","content":[{"type":"text","text":"Hello"}],"stop_reason":"max_tokens","usage":{"input_tokens":10,"output_tokens":5}}`

	a := &Anthropic{}
	resp, err := a.DecodeResponse([]byte(body), "claude-3-haiku-20240307")
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}

	if resp.Choices[0].Message.Content != "Hello" {
		t.Errorf("content = %q", resp.Choices[0].Message.Content)
	}
	if got := *resp.Choices[0].FinishReason; got != "length" {
		t.Errorf("finish_reason = %q, want length", got)
	}
	if resp.Usage.TotalTokens != 15 {
		t.Errorf("total_tokens = %d, want 15", resp.Usage.TotalTokens)
	}
}

func TestAnthropicDecodeStream(t *testing.T) {
	stream := `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":10}}}

event: content_block_delta
data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"Hel"}}

event: content_block_delta
data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"lo"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}

`

	var chunks []*ChatResponse
	a := &Anthropic{}
	usage, err := a.DecodeStream(strings.NewReader(stream), "claude-3-haiku-20240307", collect(t, &chunks))
	if err != nil {
		t.Fatalf("DecodeStream: %v", err)
	}

	if got := joinDeltas(chunks); got != "Hello" {
		t.Errorf("streamed text = %q, want Hello", got)
	}
	if chunks[0].Choices[0].Delta.Role != "assistant" {
		t.Error("first chunk must carry the assistant role")
	}
	if chunks[0].Object != "chat.completion.chunk" {
		t.Errorf("object = %q", chunks[0].Object)
	}
	last := chunks[len(chunks)-1]
	if last.Choices[0].FinishReason == nil || *last.Choices[0].FinishReason != "stop" {
		t.Error("final chunk must carry a finish_reason")
	}
	if usage.PromptTokens != 10 || usage.CompletionTokens != 5 {
		t.Errorf("usage = %+v, want 10/5", usage)
	}
}

func TestGoogleBuildRequest(t *testing.T) {
	req := &ChatRequest{
		Model: "gemini-1.5-flash",
		Messages: []Message{
			{Role: "system", Content: "be terse"},
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "hello"},
		},
	}

	g := &Google{BaseURL: "https://generativelanguage.googleapis.com/v1beta"}
	hreq, err := g.BuildRequest(context.Background(), req, "goog-key", true)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}

	want := "https://generativelanguage.googleapis.com/v1beta/models/gemini-1.5-flash:streamGenerateContent?alt=sse"
	if got := hreq.URL.String(); got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
	if got := hreq.Header.Get("x-goog-api-key"); got != "goog-key" {
		t.Errorf("x-goog-api-key = %q", got)
	}
	// The key must never ride along in the query string.
	if strings.Contains(hreq.URL.RawQuery, "goog-key") {
		t.Error("API key leaked into the URL query")
	}

	body := decodeBody(t, hreq.Body)
	if _, ok := body["systemInstruction"]; !ok {
		t.Error("systemInstruction missing")
	}
	contents, _ := body["contents"].([]any)
	if len(contents) != 2 {
		t.Fatalf("contents = %d, want 2", len(contents))
	}
	second, _ := contents[1].(map[string]any)
	if second["role"] != "model" {
		t.Errorf("assistant role = %v, want model", second["role"])
	}
}

func TestGoogleDecodeStream(t *testing.T) {
	stream := `data: {"candidates":[{"content":{"parts":[{"text":"Hel"}]}}]}

data: {"candidates":[{"content":{"parts":[{"text":"lo"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}

`

	var chunks []*ChatResponse
	g := &Google{}
	usage, err := g.DecodeStream(strings.NewReader(stream), "gemini-1.5-flash", collect(t, &chunks))
	if err != nil {
		t.Fatalf("DecodeStream: %v", err)
	}

	if got := joinDeltas(chunks); got != "Hello" {
		t.Errorf("streamed text = %q, want Hello", got)
	}
	if usage.TotalTokens != 5 {
		t.Errorf("total_tokens = %d, want 5", usage.TotalTokens)
	}
	last := chunks[len(chunks)-1]
	if last.Choices[0].FinishReason == nil || *last.Choices[0].FinishReason != "stop" {
		t.Error("STOP must map to finish_reason stop")
	}
}
