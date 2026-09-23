package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coderouter/coderouter/internal/config"
	"github.com/coderouter/coderouter/internal/db"
)

// These drive /v1/responses through the whole stack — auth, routing, the
// provider client, usage logging — against a stand-in upstream, because the
// unit tests either side of the translation cannot show that the two halves
// meet in the middle.

// codexTurn is the payload shape an agent client sends on its second turn:
// instructions, a tool declaration, and its own previous tool call replayed
// along with that call's result.
const codexTurn = `{
	"model": "gpt-4o-mini",
	"instructions": "You are a coding agent.",
	"input": [
		{"type":"message","role":"user","content":[{"type":"input_text","text":"what is in the directory?"}]},
		{"type":"function_call","name":"shell","arguments":"{\"command\":\"ls\"}","call_id":"call_1"},
		{"type":"function_call_output","call_id":"call_1","output":"main.go\nREADME.md"}
	],
	"tools": [{
		"type":"function","name":"shell","description":"run a shell command",
		"parameters":{"type":"object","properties":{"command":{"type":"string"}}}
	}],
	"tool_choice": "auto",
	"parallel_tool_calls": false,
	"store": false,
	"stream": %t,
	"max_output_tokens": 512
}`

// responsesUpstream captures the body the gateway forwards and replies with
// whatever the test dictates.
type responsesUpstream struct {
	server *httptest.Server
	body   chan map[string]any
}

func newResponsesUpstream(t *testing.T, reply func(w http.ResponseWriter)) *responsesUpstream {
	t.Helper()

	u := &responsesUpstream{body: make(chan map[string]any, 4)}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(raw, &parsed)
		u.body <- parsed

		reply(w)
	}))
	t.Cleanup(u.server.Close)
	return u
}

func (u *responsesUpstream) received(t *testing.T) map[string]any {
	t.Helper()
	select {
	case b := <-u.body:
		return b
	default:
		t.Fatal("upstream was never called")
		return nil
	}
}

// responsesClient wires a live handler to the stand-in and returns a usable
// client key.
func responsesClient(t *testing.T, u *responsesUpstream) (http.Handler, string) {
	t.Helper()

	h, database, cfg := liveHandlerWith(t, func(c *config.Config) {
		c.ProviderBaseURLs["openai"] = u.server.URL + "/v1"
		c.RoutingMode = "off"
	})
	if err := db.StoreProviderKey(database, cfg.EncryptionKey, "openai", "sk-test"); err != nil {
		t.Fatal(err)
	}
	key, err := db.CreateClientKey(database, cfg.EncryptionKey, "codex")
	if err != nil {
		t.Fatal(err)
	}
	return h, key
}

// The upstream must receive a chat-completions body: nested tools, and the
// agent's tool history rebuilt as assistant tool_calls plus a tool result.
func TestResponsesForwardsAChatBodyUpstream(t *testing.T) {
	u := newResponsesUpstream(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-1", "object": "chat.completion", "model": "gpt-4o-mini",
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "Two files."},
			}},
			"usage": map[string]int{"prompt_tokens": 20, "completion_tokens": 3, "total_tokens": 23},
		})
	})
	h, key := responsesClient(t, u)

	rec := do(t, h, http.MethodPost, "/v1/responses", key, strings.Replace(codexTurn, "%t", "false", 1))
	if rec.Code != http.StatusOK {
		t.Fatalf("= %d: %s", rec.Code, rec.Body.String())
	}

	sent := u.received(t)

	messages, ok := sent["messages"].([]any)
	if !ok || len(messages) != 4 {
		t.Fatalf("want system, user, assistant tool call and tool result; got %v", sent["messages"])
	}
	if first := messages[0].(map[string]any); first["role"] != "system" ||
		!strings.Contains(first["content"].(string), "coding agent") {
		t.Errorf("instructions did not become a system message: %v", first)
	}
	call := messages[2].(map[string]any)
	calls, ok := call["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("assistant turn lost its tool call: %v", call)
	}
	if fn := calls[0].(map[string]any)["function"].(map[string]any); fn["name"] != "shell" {
		t.Errorf("tool call = %v", fn)
	}
	result := messages[3].(map[string]any)
	if result["role"] != "tool" || result["tool_call_id"] != "call_1" {
		t.Errorf("tool result = %v", result)
	}

	tools, ok := sent["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools were not forwarded: %v", sent["tools"])
	}
	if _, ok := tools[0].(map[string]any)["function"]; !ok {
		t.Errorf("tool was not re-nested for chat-completions: %v", tools[0])
	}
	if _, ok := sent["input"]; ok {
		t.Error("the Responses-only input field must not reach a chat provider")
	}

	// And the reply comes back in Responses shape.
	var body map[string]any
	decode(t, rec, &body)
	if body["object"] != "response" || body["status"] != "completed" {
		t.Fatalf("response envelope = %v", body)
	}
	item := body["output"].([]any)[0].(map[string]any)
	if item["content"].([]any)[0].(map[string]any)["text"] != "Two files." {
		t.Errorf("output = %v", body["output"])
	}
	usage := body["usage"].(map[string]any)
	if usage["input_tokens"] != float64(20) || usage["output_tokens"] != float64(3) {
		t.Errorf("usage = %v", usage)
	}
}

// A tool call the model asks for has to reach the client as a function_call
// item, or an agent has nothing to execute.
func TestResponsesReturnsToolCallsToTheClient(t *testing.T) {
	u := newResponsesUpstream(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-2", "object": "chat.completion", "model": "gpt-4o-mini",
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "tool_calls",
				"message": map[string]any{
					"role": "assistant", "content": nil,
					"tool_calls": []map[string]any{{
						"id": "call_2", "type": "function",
						"function": map[string]string{"name": "shell", "arguments": `{"command":"cat main.go"}`},
					}},
				},
			}},
		})
	})
	h, key := responsesClient(t, u)

	rec := do(t, h, http.MethodPost, "/v1/responses", key, strings.Replace(codexTurn, "%t", "false", 1))
	if rec.Code != http.StatusOK {
		t.Fatalf("= %d: %s", rec.Code, rec.Body.String())
	}

	var body map[string]any
	decode(t, rec, &body)

	output := body["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("want one function_call item, got %v", output)
	}
	item := output[0].(map[string]any)
	if item["type"] != "function_call" || item["call_id"] != "call_2" || item["name"] != "shell" {
		t.Fatalf("item = %v", item)
	}
	if item["arguments"] != `{"command":"cat main.go"}` {
		t.Errorf("arguments = %v", item["arguments"])
	}
}

// The streaming path is the one Codex actually uses.
func TestResponsesStreamsEndToEnd(t *testing.T) {
	chunks := []string{
		`{"id":"chatcmpl-3","object":"chat.completion.chunk","model":"gpt-4o-mini","choices":[{"index":0,"delta":{"role":"assistant","content":"Look"}}]}`,
		`{"id":"chatcmpl-3","object":"chat.completion.chunk","model":"gpt-4o-mini","choices":[{"index":0,"delta":{"content":"ing."}}]}`,
		`{"id":"chatcmpl-3","object":"chat.completion.chunk","model":"gpt-4o-mini","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_3","type":"function","function":{"name":"shell","arguments":"{\"com"}}]}}]}`,
		`{"id":"chatcmpl-3","object":"chat.completion.chunk","model":"gpt-4o-mini","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"mand\":\"ls\"}"}}]}}]}`,
		`{"id":"chatcmpl-3","object":"chat.completion.chunk","model":"gpt-4o-mini","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	}

	u := newResponsesUpstream(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for _, c := range chunks {
			_, _ = w.Write([]byte("data: " + c + "\n\n"))
			flusher.Flush()
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	})
	h, key := responsesClient(t, u)

	rec := do(t, h, http.MethodPost, "/v1/responses", key, strings.Replace(codexTurn, "%t", "true", 1))
	if rec.Code != http.StatusOK {
		t.Fatalf("= %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
	if sent := u.received(t); sent["stream"] != true {
		t.Errorf("stream flag did not reach the upstream: %v", sent["stream"])
	}

	events := parseSSE(t, rec.Body.String())
	got := kinds(events)
	for _, required := range []string{
		"response.created",
		"response.output_text.delta",
		"response.function_call_arguments.delta",
		"response.output_item.done",
		"response.completed",
	} {
		if !strings.Contains(strings.Join(got, ","), required) {
			t.Errorf("stream is missing %s; got %v", required, got)
		}
	}
	if got[0] != "response.created" {
		t.Errorf("first event = %s", got[0])
	}
	if last := got[len(got)-1]; last != "response.completed" {
		t.Errorf("last event = %s", last)
	}

	final := events[len(events)-1].data["response"].(map[string]any)
	output := final["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("want a message and a function_call, got %v", output)
	}

	msg := output[0].(map[string]any)
	if text := msg["content"].([]any)[0].(map[string]any)["text"]; text != "Looking." {
		t.Errorf("streamed text = %v, want the deltas joined", text)
	}

	call := output[1].(map[string]any)
	if call["call_id"] != "call_3" || call["name"] != "shell" {
		t.Fatalf("function_call = %v", call)
	}
	if call["arguments"] != `{"command":"ls"}` {
		t.Errorf("streamed arguments = %v, want the fragments joined", call["arguments"])
	}
}
