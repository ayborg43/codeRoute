package provider

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// toolHistory is an agent mid-task: it declared a tool, called it twice in
// one turn, got both results, and the user followed up.
func toolHistory() *ChatRequest {
	return &ChatRequest{
		Model: "m",
		Messages: []Message{
			{Role: "user", Content: "read both"},
			{Role: "assistant", ToolCalls: []ToolCall{
				{ID: "call_a", Type: "function", Function: FunctionCall{Name: "read", Arguments: `{"f":"a"}`}},
				{ID: "call_b", Type: "function", Function: FunctionCall{Name: "read", Arguments: `{"f":"b"}`}},
			}},
			{Role: "tool", ToolCallID: "call_a", Content: "A"},
			{Role: "tool", ToolCallID: "call_b", Content: "B"},
			{Role: "user", Content: "now summarise"},
		},
		Tools: []Tool{
			{Type: "function", Function: ToolFunction{
				Name: "read", Description: "read a file",
				Parameters: json.RawMessage(`{"type":"object","properties":{"f":{"type":"string"}},"additionalProperties":false}`),
			}},
			{Type: "custom", Function: ToolFunction{Name: "ignored"}},
		},
		ToolChoice: json.RawMessage(`{"type":"function","function":{"name":"read"}}`),
	}
}

func asMaps(t *testing.T, v any) []map[string]any {
	t.Helper()
	list, ok := v.([]any)
	if !ok {
		t.Fatalf("expected an array, got %T: %v", v, v)
	}
	out := make([]map[string]any, len(list))
	for i, e := range list {
		out[i], _ = e.(map[string]any)
	}
	return out
}

func TestAnthropicBuildRequestTranslatesTools(t *testing.T) {
	a := &Anthropic{BaseURL: "https://api.anthropic.com/v1"}
	hreq, err := a.BuildRequest(context.Background(), toolHistory(), "k", false)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	body := decodeBody(t, hreq.Body)

	tools := asMaps(t, body["tools"])
	if len(tools) != 1 || tools[0]["name"] != "read" || tools[0]["input_schema"] == nil {
		t.Errorf("tools = %v; want one function tool with input_schema", tools)
	}
	choice, _ := body["tool_choice"].(map[string]any)
	if choice["type"] != "tool" || choice["name"] != "read" {
		t.Errorf("tool_choice = %v, want forced tool read", choice)
	}

	msgs := asMaps(t, body["messages"])
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3 (user, assistant, user): %v", len(msgs), msgs)
	}

	uses := asMaps(t, msgs[1]["content"])
	if msgs[1]["role"] != "assistant" || len(uses) != 2 {
		t.Fatalf("assistant turn = %v; want two tool_use blocks and no empty text", msgs[1])
	}
	if uses[0]["type"] != "tool_use" || uses[0]["id"] != "call_a" || uses[1]["id"] != "call_b" {
		t.Errorf("tool_use blocks = %v", uses)
	}
	if input, _ := uses[0]["input"].(map[string]any); input["f"] != "a" {
		t.Errorf("input = %v; arguments must be sent as an object", uses[0]["input"])
	}

	// Both results and the follow-up share one user turn, results first.
	results := asMaps(t, msgs[2]["content"])
	if msgs[2]["role"] != "user" || len(results) != 3 {
		t.Fatalf("user turn = %v; want two tool_result blocks and the follow-up text", msgs[2])
	}
	if results[0]["type"] != "tool_result" || results[0]["tool_use_id"] != "call_a" || results[0]["content"] != "A" {
		t.Errorf("first result = %v", results[0])
	}
	if results[2]["type"] != "text" || results[2]["text"] != "now summarise" {
		t.Errorf("follow-up = %v", results[2])
	}
}

// A plain chat must still go out in the string form it always has.
func TestAnthropicTextTurnsStayStrings(t *testing.T) {
	a := &Anthropic{BaseURL: "https://api.anthropic.com/v1"}
	hreq, err := a.BuildRequest(context.Background(), &ChatRequest{
		Model: "m", Messages: []Message{{Role: "user", Content: "hi"}},
	}, "k", false)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	body := decodeBody(t, hreq.Body)
	msgs := asMaps(t, body["messages"])
	if msgs[0]["content"] != "hi" {
		t.Errorf("content = %v, want the string hi", msgs[0]["content"])
	}
	if _, ok := body["tools"]; ok {
		t.Error("tools must be omitted when none were declared")
	}
}

func TestAnthropicDecodeResponseReturnsToolCalls(t *testing.T) {
	body := `{"id":"msg_1","content":[
		{"type":"text","text":"checking"},
		{"type":"tool_use","id":"toolu_1","name":"read","input":{"f":"a"}}
	],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`

	resp, err := (&Anthropic{}).DecodeResponse([]byte(body), "m")
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	msg := resp.Choices[0].Message
	if msg.Content != "checking" || len(msg.ToolCalls) != 1 {
		t.Fatalf("message = %+v", msg)
	}
	tc := msg.ToolCalls[0]
	if tc.ID != "toolu_1" || tc.Function.Name != "read" || tc.Function.Arguments != `{"f":"a"}` {
		t.Errorf("tool call = %+v", tc)
	}
	if *resp.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q", *resp.Choices[0].FinishReason)
	}
}

func TestAnthropicDecodeStreamReturnsToolCalls(t *testing.T) {
	stream := `data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":3}}}

data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}

data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"read","input":{}}}

data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"f\":"}}

data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"a\"}"}}

data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_2","name":"read","input":{}}}

data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{}"}}

data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}

`
	var chunks []*ChatResponse
	if _, err := (&Anthropic{}).DecodeStream(strings.NewReader(stream), "m", collect(t, &chunks)); err != nil {
		t.Fatalf("DecodeStream: %v", err)
	}

	if got := joinDeltas(chunks); got != "ok" {
		t.Errorf("text = %q", got)
	}
	calls := reassemble(chunks)
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2: %+v", len(calls), calls)
	}
	if calls[0].ID != "toolu_1" || calls[0].Function.Name != "read" || calls[0].Function.Arguments != `{"f":"a"}` {
		t.Errorf("first call = %+v", calls[0])
	}
	if calls[1].ID != "toolu_2" || calls[1].Function.Arguments != `{}` {
		t.Errorf("second call = %+v", calls[1])
	}
	last := chunks[len(chunks)-1]
	if *last.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q", *last.Choices[0].FinishReason)
	}
}

func TestGoogleBuildRequestTranslatesTools(t *testing.T) {
	g := &Google{BaseURL: "https://generativelanguage.googleapis.com/v1beta"}
	hreq, err := g.BuildRequest(context.Background(), toolHistory(), "k", false)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	body := decodeBody(t, hreq.Body)

	tools := asMaps(t, body["tools"])
	decls := asMaps(t, tools[0]["functionDeclarations"])
	if len(decls) != 1 || decls[0]["name"] != "read" || decls[0]["parametersJsonSchema"] == nil {
		t.Errorf("functionDeclarations = %v", decls)
	}
	cfg, _ := body["toolConfig"].(map[string]any)
	fcc, _ := cfg["functionCallingConfig"].(map[string]any)
	if fcc["mode"] != "ANY" {
		t.Errorf("functionCallingConfig = %v, want mode ANY", fcc)
	}

	contents := asMaps(t, body["contents"])
	if len(contents) != 3 {
		t.Fatalf("contents = %d, want 3 (user, model, user): %v", len(contents), contents)
	}

	calls := asMaps(t, contents[1]["parts"])
	if contents[1]["role"] != "model" || len(calls) != 2 {
		t.Fatalf("model turn = %v", contents[1])
	}
	fc, _ := calls[0]["functionCall"].(map[string]any)
	if fc["name"] != "read" || calls[0]["thoughtSignature"] == nil {
		t.Errorf("functionCall part = %v", calls[0])
	}
	if args, _ := fc["args"].(map[string]any); args["f"] != "a" {
		t.Errorf("args = %v", fc["args"])
	}

	results := asMaps(t, contents[2]["parts"])
	if len(results) != 3 {
		t.Fatalf("user turn parts = %v; want two functionResponses and the follow-up", results)
	}
	fr, _ := results[0]["functionResponse"].(map[string]any)
	// The name comes from the call the result answers.
	if fr["name"] != "read" {
		t.Errorf("functionResponse = %v", fr)
	}
}

func TestGoogleDecodeStreamReturnsToolCalls(t *testing.T) {
	stream := `data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"read","args":{"f":"a"}}},{"functionCall":{"name":"read","args":{"f":"b"}}}]}}]}

data: {"candidates":[{"content":{"parts":[{"text":""}]},"finishReason":"STOP"}]}

`
	var chunks []*ChatResponse
	if _, err := (&Google{}).DecodeStream(strings.NewReader(stream), "m", collect(t, &chunks)); err != nil {
		t.Fatalf("DecodeStream: %v", err)
	}

	calls := reassemble(chunks)
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2: %+v", len(calls), calls)
	}
	if calls[0].ID == "" || calls[0].ID == calls[1].ID {
		t.Errorf("each call needs its own id: %q, %q", calls[0].ID, calls[1].ID)
	}
	if calls[1].Function.Arguments != `{"f":"b"}` {
		t.Errorf("second call = %+v", calls[1])
	}
	last := chunks[len(chunks)-1]
	if *last.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", *last.Choices[0].FinishReason)
	}
}

func TestGoogleDecodeResponseReturnsToolCalls(t *testing.T) {
	body := `{"candidates":[{"content":{"parts":[{"functionCall":{"id":"fc_1","name":"read","args":{}}}]},"finishReason":"STOP"}]}`

	resp, err := (&Google{}).DecodeResponse([]byte(body), "m")
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	calls := resp.Choices[0].Message.ToolCalls
	if len(calls) != 1 || calls[0].ID != "fc_1" || calls[0].Function.Arguments != `{}` {
		t.Errorf("tool calls = %+v", calls)
	}
	if *resp.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q", *resp.Choices[0].FinishReason)
	}
}

// reassemble folds streamed tool-call fragments back into whole calls, the
// way an OpenAI client does: by index.
func reassemble(chunks []*ChatResponse) []ToolCall {
	var calls []ToolCall
	for _, c := range chunks {
		for _, ch := range c.Choices {
			if ch.Delta == nil {
				continue
			}
			for _, tc := range ch.Delta.ToolCalls {
				i := *tc.Index
				for len(calls) <= i {
					calls = append(calls, ToolCall{})
				}
				if tc.ID != "" {
					calls[i].ID = tc.ID
				}
				if tc.Function.Name != "" {
					calls[i].Function.Name = tc.Function.Name
				}
				calls[i].Function.Arguments += tc.Function.Arguments
			}
		}
	}
	return calls
}
