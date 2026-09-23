package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coderouter/coderouter/internal/config"
	"github.com/coderouter/coderouter/internal/provider"
)

// --- inbound translation ----------------------------------------------------

func TestResponsesInputAcceptsBareString(t *testing.T) {
	msgs, err := responsesMessages(&responsesRequest{
		Instructions: "be terse",
		Input:        json.RawMessage(`"hello"`),
	})
	if err != nil {
		t.Fatalf("responsesMessages: %v", err)
	}

	want := []provider.Message{
		{Role: "system", Content: "be terse"},
		{Role: "user", Content: "hello"},
	}
	if len(msgs) != len(want) {
		t.Fatalf("got %d messages, want %d: %+v", len(msgs), len(want), msgs)
	}
	for i := range want {
		if msgs[i].Role != want[i].Role || msgs[i].Content != want[i].Content {
			t.Errorf("message %d = %+v, want %+v", i, msgs[i], want[i])
		}
	}
}

// An agent replays its own tool calls and their results on every turn. Losing
// either half leaves the model unable to see what it already ran.
func TestResponsesInputCarriesToolCallRoundTrip(t *testing.T) {
	input := `[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"list files"}]},
		{"type":"function_call","name":"shell","arguments":"{\"cmd\":\"ls\"}","call_id":"call_1"},
		{"type":"function_call_output","call_id":"call_1","output":"a.txt"}
	]`

	msgs, err := responsesMessages(&responsesRequest{Input: json.RawMessage(input)})
	if err != nil {
		t.Fatalf("responsesMessages: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3: %+v", len(msgs), msgs)
	}

	if msgs[0].Role != "user" || msgs[0].Content != "list files" {
		t.Errorf("user turn = %+v", msgs[0])
	}

	call := msgs[1]
	if call.Role != "assistant" || len(call.ToolCalls) != 1 {
		t.Fatalf("assistant turn lost its tool call: %+v", call)
	}
	if call.ToolCalls[0].ID != "call_1" || call.ToolCalls[0].Function.Name != "shell" {
		t.Errorf("tool call = %+v", call.ToolCalls[0])
	}
	if call.ToolCalls[0].Function.Arguments != `{"cmd":"ls"}` {
		t.Errorf("arguments = %q", call.ToolCalls[0].Function.Arguments)
	}

	if msgs[2].Role != "tool" || msgs[2].ToolCallID != "call_1" || msgs[2].Content != "a.txt" {
		t.Errorf("tool result = %+v", msgs[2])
	}
}

// The merge that keeps Anthropic and Google happy used to fold same-role turns
// together by concatenating content, which would erase a tool call.
func TestSystemAndTurnsKeepsToolCallsWhole(t *testing.T) {
	req := &provider.ChatRequest{Messages: []provider.Message{
		{Role: "assistant", Content: "one"},
		{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "call_1"}}},
	}}

	_, turns := req.SystemAndTurns()
	if len(turns) != 2 {
		t.Fatalf("got %d turns, want 2 — the tool call was merged away: %+v", len(turns), turns)
	}
	if len(turns[1].ToolCalls) != 1 {
		t.Errorf("second turn lost its tool call: %+v", turns[1])
	}
}

// A turn that called two tools replays as two function_call items. Chat wants
// them on one assistant message, directly followed by both results; split
// across two messages, the provider refuses the request.
func TestResponsesInputMergesParallelToolCalls(t *testing.T) {
	input := `[
		{"type":"message","role":"user","content":"read both"},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"reading"}]},
		{"type":"function_call","name":"read","arguments":"{\"f\":\"a\"}","call_id":"call_a"},
		{"type":"function_call","name":"read","arguments":"{\"f\":\"b\"}","call_id":"call_b"},
		{"type":"function_call_output","call_id":"call_a","output":"A"},
		{"type":"function_call_output","call_id":"call_b","output":"B"},
		{"type":"function_call","name":"read","arguments":"{}","call_id":"call_c"}
	]`

	msgs, err := responsesMessages(&responsesRequest{Input: json.RawMessage(input)})
	if err != nil {
		t.Fatalf("responsesMessages: %v", err)
	}
	if len(msgs) != 5 {
		t.Fatalf("got %d messages, want 5: %+v", len(msgs), msgs)
	}

	turn := msgs[1]
	if turn.Role != "assistant" || turn.Content != "reading" || len(turn.ToolCalls) != 2 {
		t.Fatalf("parallel calls were not merged onto the assistant turn: %+v", turn)
	}
	if turn.ToolCalls[0].ID != "call_a" || turn.ToolCalls[1].ID != "call_b" {
		t.Errorf("tool calls out of order: %+v", turn.ToolCalls)
	}
	if msgs[2].ToolCallID != "call_a" || msgs[3].ToolCallID != "call_b" {
		t.Errorf("results = %+v, %+v", msgs[2], msgs[3])
	}
	// A call after tool results starts a new assistant turn rather than
	// joining the tool message before it.
	if msgs[4].Role != "assistant" || len(msgs[4].ToolCalls) != 1 || msgs[4].ToolCalls[0].ID != "call_c" {
		t.Errorf("later call = %+v", msgs[4])
	}
}

func TestResponsesItemWithoutTypeIsAMessage(t *testing.T) {
	msgs, err := responsesMessages(&responsesRequest{
		Input: json.RawMessage(`[{"role":"user","content":"hi"}]`),
	})
	if err != nil {
		t.Fatalf("responsesMessages: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Role != "user" || msgs[0].Content != "hi" {
		t.Errorf("got %+v, want one user message", msgs)
	}
}

func TestResponsesDropsUnrepresentableItems(t *testing.T) {
	msgs, err := responsesMessages(&responsesRequest{
		Input: json.RawMessage(`[{"type":"reasoning","summary":[]},{"role":"user","content":"hi"}]`),
	})
	if err != nil {
		t.Fatalf("responsesMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Errorf("reasoning item should be dropped, got %+v", msgs)
	}
}

func TestResponsesRejectsToolCallWithoutCallID(t *testing.T) {
	_, err := responsesMessages(&responsesRequest{
		Input: json.RawMessage(`[{"type":"function_call","name":"shell","arguments":"{}"}]`),
	})
	if err == nil {
		t.Fatal("a function_call with no call_id should be rejected, not silently dropped")
	}
}

// The two APIs disagree about where a function's fields live: flat in
// Responses, nested under "function" in chat-completions.
func TestChatToolsReNestFunctionFields(t *testing.T) {
	strict := true
	got := chatTools([]responsesTool{
		{Type: "function", Name: "shell", Description: "run", Parameters: json.RawMessage(`{"type":"object"}`), Strict: &strict},
		{Type: "web_search"},
	})

	if len(got) != 1 {
		t.Fatalf("hosted tools have no chat equivalent and must be dropped, got %d", len(got))
	}
	fn := got[0].Function
	if got[0].Type != "function" || fn.Name != "shell" || fn.Description != "run" ||
		fn.Strict == nil || !*fn.Strict || string(fn.Parameters) != `{"type":"object"}` {
		t.Errorf("tool = %+v", got[0])
	}
}

func TestChatToolChoiceTranslatesNamedForm(t *testing.T) {
	if got := chatToolChoice(json.RawMessage(`"auto"`)); got != "auto" {
		t.Errorf("string form should pass through, got %v", got)
	}

	got, ok := chatToolChoice(json.RawMessage(`{"type":"function","name":"shell"}`)).(map[string]any)
	if !ok {
		t.Fatalf("named form was not translated: %v", got)
	}
	fn, _ := got["function"].(map[string]any)
	if fn == nil || fn["name"] != "shell" {
		t.Errorf("tool_choice = %+v", got)
	}
}

// The OpenAI client forwards Raw verbatim, so it has to be a chat body.
func TestChatRequestFromBuildsAChatCompletionsBody(t *testing.T) {
	h := &Handler{cfg: config.Load()}

	req, err := h.chatRequestFrom(&responsesRequest{
		Model:           "gpt-test",
		Instructions:    "be terse",
		Input:           json.RawMessage(`"hi"`),
		MaxOutputTokens: 256,
		Tools:           []responsesTool{{Type: "function", Name: "shell"}},
	})
	if err != nil {
		t.Fatalf("chatRequestFrom: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal(req.Raw, &body); err != nil {
		t.Fatalf("Raw is not JSON: %v", err)
	}
	if body["model"] != "gpt-test" {
		t.Errorf("model = %v", body["model"])
	}
	if body["max_tokens"] != float64(256) {
		t.Errorf("max_output_tokens should become max_tokens, got %v", body["max_tokens"])
	}
	if _, ok := body["max_output_tokens"]; ok {
		t.Error("max_output_tokens is not a chat-completions field")
	}
	if _, ok := body["messages"].([]any); !ok {
		t.Errorf("messages missing from Raw: %+v", body)
	}
	if _, ok := body["tools"].([]any); !ok {
		t.Errorf("tools missing from Raw: %+v", body)
	}
}

// Silently ignoring previous_response_id would send the model a conversation
// with its own history missing, which reads as the model losing its mind
// rather than as an unsupported feature.
func TestResponsesRejectsStoredConversationReference(t *testing.T) {
	h := &Handler{cfg: config.Load()}

	_, err := h.chatRequestFrom(&responsesRequest{
		Model:              "auto",
		PreviousResponseID: "resp_1",
		Input:              json.RawMessage(`"hi"`),
	})
	if err == nil {
		t.Fatal("previous_response_id must not be accepted")
	}
	if !strings.Contains(err.Error(), "previous_response_id") {
		t.Errorf("error should name the field: %v", err)
	}
}

func TestResponsesRejectsNonPostMethods(t *testing.T) {
	h := testHandler(t, testAdminToken)
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		if rec := do(t, h, method, "/v1/responses", "", ""); rec.Code != 405 {
			t.Errorf("%s /v1/responses = %d, want 405", method, rec.Code)
		}
	}
}

// --- outbound translation ---------------------------------------------------

func TestResponsesFromChatRendersTextAndToolCalls(t *testing.T) {
	body := responsesFromChat(&provider.ChatResponse{
		ID:    "chatcmpl-abc",
		Model: "gpt-test",
		Choices: []provider.Choice{{
			Message: &provider.Message{
				Role:    "assistant",
				Content: "running it",
				ToolCalls: []provider.ToolCall{{
					ID: "call_1", Type: "function",
					Function: provider.FunctionCall{Name: "shell", Arguments: `{"cmd":"ls"}`},
				}},
			},
		}},
		Usage: &provider.Usage{PromptTokens: 10, CompletionTokens: 4, TotalTokens: 14},
	}, "auto")

	if body.Object != "response" || body.Status != "completed" {
		t.Errorf("envelope = %+v", body)
	}
	if body.Model != "gpt-test" {
		t.Errorf("model should be the one that actually served, got %q", body.Model)
	}
	if len(body.Output) != 2 {
		t.Fatalf("want a message item and a function_call item, got %+v", body.Output)
	}
	if body.Output[0].Type != "message" || body.Output[0].Content[0].Text != "running it" {
		t.Errorf("message item = %+v", body.Output[0])
	}
	if body.Output[1].Type != "function_call" || body.Output[1].CallID != "call_1" {
		t.Errorf("function_call item = %+v", body.Output[1])
	}
	if body.Usage == nil || body.Usage.InputTokens != 10 || body.Usage.OutputTokens != 4 {
		t.Errorf("usage = %+v", body.Usage)
	}
}

// --- streaming --------------------------------------------------------------

type sseEvent struct {
	kind string
	data map[string]any
}

func parseSSE(t *testing.T, raw string) []sseEvent {
	t.Helper()

	var out []sseEvent
	for _, block := range strings.Split(strings.TrimSpace(raw), "\n\n") {
		var ev sseEvent
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.kind = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev.data); err != nil {
					t.Fatalf("event data is not JSON: %v", err)
				}
			}
		}
		if ev.kind != "" {
			out = append(out, ev)
		}
	}
	return out
}

func kinds(events []sseEvent) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.kind
	}
	return out
}

func newTestStream(rec *httptest.ResponseRecorder) *responsesStream {
	return &responsesStream{
		w: rec, flusher: rec,
		body:  newResponsesBody("gpt-test"),
		tools: map[int]*toolStream{},
	}
}

func textChunk(text string) *provider.ChatResponse {
	return &provider.ChatResponse{
		Model:   "gpt-test",
		Choices: []provider.Choice{{Delta: &provider.Delta{Content: text}}},
	}
}

func TestStreamFramesTextAsAnItemLifecycle(t *testing.T) {
	rec := httptest.NewRecorder()
	s := newTestStream(rec)

	for _, part := range []string{"Hel", "lo"} {
		if err := s.consume(textChunk(part)); err != nil {
			t.Fatalf("consume: %v", err)
		}
	}
	s.finish()

	events := parseSSE(t, rec.Body.String())
	want := []string{
		"response.created", "response.in_progress",
		"response.output_item.added", "response.content_part.added",
		"response.output_text.delta", "response.output_text.delta",
		"response.output_text.done", "response.content_part.done",
		"response.output_item.done", "response.completed",
	}
	if got := kinds(events); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("event sequence:\n got %v\nwant %v", got, want)
	}

	// Clients read the final answer off response.completed, not by
	// reassembling the deltas, so the whole text has to be there.
	final := events[len(events)-1].data["response"].(map[string]any)
	output := final["output"].([]any)
	item := output[0].(map[string]any)
	content := item["content"].([]any)[0].(map[string]any)
	if content["text"] != "Hello" {
		t.Errorf("completed text = %v, want %q", content["text"], "Hello")
	}
	if final["status"] != "completed" {
		t.Errorf("status = %v", final["status"])
	}
}

func TestStreamSequenceNumbersAreContiguous(t *testing.T) {
	rec := httptest.NewRecorder()
	s := newTestStream(rec)
	if err := s.consume(textChunk("hi")); err != nil {
		t.Fatalf("consume: %v", err)
	}
	s.finish()

	for i, ev := range parseSSE(t, rec.Body.String()) {
		if got, ok := ev.data["sequence_number"].(float64); !ok || int(got) != i {
			t.Errorf("event %d (%s) has sequence_number %v", i, ev.kind, ev.data["sequence_number"])
		}
	}
}

// Tool-call arguments arrive split across chunks, with the id and name only on
// the first fragment.
func TestStreamReassemblesToolCallFragments(t *testing.T) {
	rec := httptest.NewRecorder()
	s := newTestStream(rec)

	idx := 0
	fragments := []provider.ToolCall{
		{Index: &idx, ID: "call_1", Type: "function", Function: provider.FunctionCall{Name: "shell", Arguments: `{"cmd`}},
		{Index: &idx, Function: provider.FunctionCall{Arguments: `":"ls"}`}},
	}
	for _, f := range fragments {
		chunk := &provider.ChatResponse{
			Model:   "gpt-test",
			Choices: []provider.Choice{{Delta: &provider.Delta{ToolCalls: []provider.ToolCall{f}}}},
		}
		if err := s.consume(chunk); err != nil {
			t.Fatalf("consume: %v", err)
		}
	}
	s.finish()

	events := parseSSE(t, rec.Body.String())

	var added, deltas int
	for _, e := range events {
		switch e.kind {
		case "response.output_item.added":
			added++
		case "response.function_call_arguments.delta":
			deltas++
		}
	}
	if added != 1 {
		t.Errorf("two fragments of one call should open one item, got %d", added)
	}
	if deltas != 2 {
		t.Errorf("want 2 argument deltas, got %d", deltas)
	}

	final := events[len(events)-1].data["response"].(map[string]any)
	item := final["output"].([]any)[0].(map[string]any)
	if item["type"] != "function_call" || item["call_id"] != "call_1" || item["name"] != "shell" {
		t.Fatalf("function_call item = %+v", item)
	}
	if item["arguments"] != `{"cmd":"ls"}` {
		t.Errorf("arguments = %v, want the two fragments joined", item["arguments"])
	}
}

// Text and tool calls in one turn must land on distinct output indexes,
// otherwise a client overwrites one with the other.
func TestStreamGivesTextAndToolCallsDistinctIndexes(t *testing.T) {
	rec := httptest.NewRecorder()
	s := newTestStream(rec)

	if err := s.consume(textChunk("thinking")); err != nil {
		t.Fatalf("consume: %v", err)
	}
	idx := 0
	if err := s.consume(&provider.ChatResponse{
		Choices: []provider.Choice{{Delta: &provider.Delta{ToolCalls: []provider.ToolCall{
			{Index: &idx, ID: "call_1", Function: provider.FunctionCall{Name: "shell", Arguments: "{}"}},
		}}}},
	}); err != nil {
		t.Fatalf("consume: %v", err)
	}
	s.finish()

	events := parseSSE(t, rec.Body.String())
	final := events[len(events)-1].data["response"].(map[string]any)
	output := final["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("want both items in output, got %+v", output)
	}

	seen := map[float64]bool{}
	for _, e := range events {
		if e.kind != "response.output_item.done" {
			continue
		}
		i := e.data["output_index"].(float64)
		if seen[i] {
			t.Errorf("output_index %v used twice", i)
		}
		seen[i] = true
	}
	if len(seen) != 2 {
		t.Errorf("want 2 distinct output indexes, got %d", len(seen))
	}
}

// A stream that yields nothing still has to be a well-formed Responses stream.
func TestStreamWithNoOutputStillOpensAndCloses(t *testing.T) {
	rec := httptest.NewRecorder()
	s := newTestStream(rec)
	s.finish()

	want := []string{"response.created", "response.in_progress", "response.completed"}
	if got := kinds(parseSSE(t, rec.Body.String())); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestStreamReportsMidStreamFailureInBand(t *testing.T) {
	rec := httptest.NewRecorder()
	s := newTestStream(rec)
	if err := s.consume(textChunk("partial")); err != nil {
		t.Fatalf("consume: %v", err)
	}
	s.fail(errUpstream{})

	events := parseSSE(t, rec.Body.String())
	last := events[len(events)-1]
	if last.kind != "response.failed" {
		t.Fatalf("last event = %s, want response.failed", last.kind)
	}
	final := last.data["response"].(map[string]any)
	if final["status"] != "failed" || final["error"] == nil {
		t.Errorf("failed response = %+v", final)
	}
}

type errUpstream struct{}

func (errUpstream) Error() string { return "provider exploded" }
