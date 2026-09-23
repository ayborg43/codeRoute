package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/coderouter/coderouter/internal/provider"
)

// The Responses API is OpenAI's newer surface, and the only one some clients
// still speak — Codex dropped chat-completions support entirely. Nothing
// upstream of this file knows about it: a request is translated into the
// ordinary ChatRequest the gateway already routes, and the result is
// translated back. Routing, failover, pricing and usage logging are therefore
// shared with /v1/chat/completions rather than duplicated.
//
// Tool calls are the part that matters for agent clients. They survive the
// round trip on every dialect: OpenAI-dialect providers get them in the
// forwarded body, and the native Anthropic and Google clients translate
// ChatRequest.Tools and the tool-call history into their own shapes.

// responsesRequest is the subset of the Responses API this gateway acts on.
// Unmodelled fields (store, include, text, metadata, ...) are accepted and
// ignored rather than rejected, so a client sending them still works.
type responsesRequest struct {
	Model           string          `json:"model"`
	Instructions    string          `json:"instructions"`
	Input           json.RawMessage `json:"input"`
	Tools           []responsesTool `json:"tools"`
	ToolChoice      json.RawMessage `json:"tool_choice"`
	MaxOutputTokens int             `json:"max_output_tokens"`
	Temperature     *float64        `json:"temperature"`
	TopP            *float64        `json:"top_p"`
	Stream          bool            `json:"stream"`

	// PreviousResponseID would ask the gateway to recall a stored conversation.
	// Nothing here stores one, so it is refused rather than silently ignored:
	// ignoring it would send the model a conversation missing its own history.
	PreviousResponseID string `json:"previous_response_id"`
}

// responsesTool is a tool declaration. The Responses API flattens the function
// fields that chat-completions nests under a "function" key.
type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// responsesItem is one element of the input array. The API overloads a single
// shape across messages, tool calls and tool results, so most fields are only
// meaningful for one Type.
type responsesItem struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`

	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	CallID    string `json:"call_id"`

	Output json.RawMessage `json:"output"`
}

func (h *Handler) handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}

	key, ok := h.authorize(w, r)
	if !ok {
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body", "invalid_request_error")
		return
	}
	defer r.Body.Close()

	var rr responsesRequest
	if err := json.Unmarshal(raw, &rr); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error(), "invalid_request_error")
		return
	}
	req, err := h.chatRequestFrom(&rr)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}

	if rr.Stream {
		h.streamResponses(w, r, req, key)
		return
	}

	resp, err := h.gw.Complete(r.Context(), req, key)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error(), "upstream_error")
		return
	}

	writeJSON(w, http.StatusOK, responsesFromChat(resp, req.Model))
}

// chatRequestFrom translates a Responses payload into the chat request the
// gateway routes. Raw is rebuilt rather than forwarded, because the OpenAI
// client passes Raw through untouched and it must be a chat-completions body.
func (h *Handler) chatRequestFrom(rr *responsesRequest) (*provider.ChatRequest, error) {
	if rr.PreviousResponseID != "" {
		return nil, fmt.Errorf("previous_response_id is not supported: this gateway does not " +
			"store responses, so send the full conversation in input and set store to false")
	}

	messages, err := responsesMessages(rr)
	if err != nil {
		return nil, err
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("input must not be empty")
	}

	model := rr.Model
	if model == "" {
		model = h.cfg.DefaultModel
	}

	req := &provider.ChatRequest{
		Model:       model,
		Messages:    messages,
		MaxTokens:   rr.MaxOutputTokens,
		Temperature: rr.Temperature,
		TopP:        rr.TopP,
		Stream:      rr.Stream,
	}

	body := map[string]any{"model": model, "messages": messages}
	if rr.MaxOutputTokens > 0 {
		body["max_tokens"] = rr.MaxOutputTokens
	}
	if rr.Temperature != nil {
		body["temperature"] = *rr.Temperature
	}
	if rr.TopP != nil {
		body["top_p"] = *rr.TopP
	}
	if tools := chatTools(rr.Tools); len(tools) > 0 {
		req.Tools = tools
		body["tools"] = tools
	}
	if choice := chatToolChoice(rr.ToolChoice); choice != nil {
		encoded, err := json.Marshal(choice)
		if err != nil {
			return nil, err
		}
		req.ToolChoice = encoded
		body["tool_choice"] = choice
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req.Raw = raw

	return req, nil
}

// responsesMessages flattens instructions plus the input array into an ordered
// chat transcript.
func responsesMessages(rr *responsesRequest) ([]provider.Message, error) {
	var out []provider.Message
	if s := strings.TrimSpace(rr.Instructions); s != "" {
		out = append(out, provider.Message{Role: "system", Content: provider.Content(s)})
	}

	if len(rr.Input) == 0 {
		return out, nil
	}

	// input is either a bare string — shorthand for a single user turn — or an
	// array of items.
	if text, ok := jsonString(rr.Input); ok {
		return append(out, provider.Message{Role: "user", Content: provider.Content(text)}), nil
	}

	var items []responsesItem
	if err := json.Unmarshal(rr.Input, &items); err != nil {
		return nil, fmt.Errorf("input must be a string or an array of items: %w", err)
	}

	for _, it := range items {
		msg, ok, err := messageFromItem(it)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		// A turn that called several tools arrives as one function_call item
		// per call, but chat-completions wants them on a single assistant
		// message: each tool-calling message must be followed directly by its
		// results, so assistant(A), assistant(B), tool(A), tool(B) is refused.
		// The calls also join any assistant text that immediately preceded them.
		if n := len(out); n > 0 && len(msg.ToolCalls) > 0 &&
			out[n-1].Role == "assistant" && out[n-1].ToolCallID == "" {
			out[n-1].ToolCalls = append(out[n-1].ToolCalls, msg.ToolCalls...)
			continue
		}
		out = append(out, msg)
	}

	return out, nil
}

// messageFromItem converts one input item. The bool reports whether the item
// produced a message at all; reasoning items and other pass-through kinds
// deliberately produce none.
func messageFromItem(it responsesItem) (provider.Message, bool, error) {
	kind := it.Type
	// An item with a role and no type is the message shorthand.
	if kind == "" && it.Role != "" {
		kind = "message"
	}

	switch kind {
	case "message":
		role := it.Role
		if role == "" {
			role = "user"
		}
		return provider.Message{Role: role, Content: provider.Content(partsText(it.Content))}, true, nil

	case "function_call":
		if it.CallID == "" {
			return provider.Message{}, false, fmt.Errorf("function_call item is missing call_id")
		}
		return provider.Message{
			Role: "assistant",
			ToolCalls: []provider.ToolCall{{
				ID:       it.CallID,
				Type:     "function",
				Function: provider.FunctionCall{Name: it.Name, Arguments: it.Arguments},
			}},
		}, true, nil

	case "function_call_output":
		if it.CallID == "" {
			return provider.Message{}, false, fmt.Errorf("function_call_output item is missing call_id")
		}
		return provider.Message{
			Role:       "tool",
			ToolCallID: it.CallID,
			Content:    provider.Content(partsText(it.Output)),
		}, true, nil

	default:
		// Reasoning items and anything else this gateway cannot represent are
		// dropped: a chat-completions provider has nowhere to put them.
		return provider.Message{}, false, nil
	}
}

// partsText flattens a content field to plain text. It accepts a bare string,
// an array of typed parts, or an object with a text field — every shape the
// Responses API uses for content and tool output.
func partsText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if s, ok := jsonString(raw); ok {
		return s
	}

	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var sb strings.Builder
		for _, p := range parts {
			sb.WriteString(p.Text)
		}
		return sb.String()
	}

	var single struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &single); err == nil && single.Text != "" {
		return single.Text
	}

	// A tool that returned raw JSON is still worth handing to the model.
	return string(raw)
}

func jsonString(raw json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// chatTools re-nests Responses tool declarations into chat-completions shape.
func chatTools(tools []responsesTool) []provider.Tool {
	var out []provider.Tool
	for _, t := range tools {
		if t.Type != "" && t.Type != "function" {
			// Hosted tools (web_search, file_search, ...) have no
			// chat-completions equivalent and cannot be forwarded.
			continue
		}
		out = append(out, provider.Tool{
			Type: "function",
			Function: provider.ToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
				Strict:      t.Strict,
			},
		})
	}
	return out
}

// chatToolChoice translates the forced-tool form. The string forms ("auto",
// "none", "required") are identical in both APIs and pass through unchanged.
func chatToolChoice(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	if s, ok := jsonString(raw); ok {
		return s
	}

	var named struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &named); err != nil || named.Name == "" {
		return nil
	}
	return map[string]any{"type": "function", "function": map[string]any{"name": named.Name}}
}

// --- outbound translation ---

type responsesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// responsesOutput is one item of the output array. A single struct covers both
// kinds; the empty fields are omitted per kind.
type responsesOutput struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Status string `json:"status,omitempty"`

	Role    string          `json:"role,omitempty"`
	Content []responsesPart `json:"content,omitempty"`

	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type responsesPart struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Annotations []any  `json:"annotations"`
}

type responsesBody struct {
	ID        string            `json:"id"`
	Object    string            `json:"object"`
	CreatedAt int64             `json:"created_at"`
	Status    string            `json:"status"`
	Model     string            `json:"model"`
	Output    []responsesOutput `json:"output"`
	Usage     *responsesUsage   `json:"usage,omitempty"`

	// The Responses API always carries these; Codex and the OpenAI SDKs read
	// them, so they are emitted explicitly rather than left absent.
	Error             any  `json:"error"`
	IncompleteDetails any  `json:"incomplete_details"`
	ParallelToolCalls bool `json:"parallel_tool_calls"`
}

func newResponsesBody(model string) *responsesBody {
	return &responsesBody{
		ID:        "resp_" + uuid.New().String(),
		Object:    "response",
		CreatedAt: time.Now().Unix(),
		Status:    "in_progress",
		Model:     model,
		Output:    []responsesOutput{},
	}
}

// responsesFromChat renders a completed chat response as a Responses body.
func responsesFromChat(resp *provider.ChatResponse, model string) *responsesBody {
	if resp.Model != "" {
		model = resp.Model
	}

	body := newResponsesBody(model)
	body.Status = "completed"
	if resp.ID != "" {
		body.ID = "resp_" + strings.TrimPrefix(resp.ID, "chatcmpl-")
	}
	if resp.Created > 0 {
		body.CreatedAt = resp.Created
	}

	if len(resp.Choices) > 0 && resp.Choices[0].Message != nil {
		msg := resp.Choices[0].Message
		if text := string(msg.Content); text != "" {
			body.Output = append(body.Output, textOutput(text))
		}
		for _, tc := range msg.ToolCalls {
			body.Output = append(body.Output, toolOutput(tc))
		}
	}

	if resp.Usage != nil {
		body.Usage = &responsesUsage{
			InputTokens:  resp.Usage.PromptTokens,
			OutputTokens: resp.Usage.CompletionTokens,
			TotalTokens:  resp.Usage.TotalTokens,
		}
	}

	return body
}

func textOutput(text string) responsesOutput {
	return responsesOutput{
		Type:    "message",
		ID:      "msg_" + uuid.New().String(),
		Status:  "completed",
		Role:    "assistant",
		Content: []responsesPart{{Type: "output_text", Text: text, Annotations: []any{}}},
	}
}

func toolOutput(tc provider.ToolCall) responsesOutput {
	callID := tc.ID
	if callID == "" {
		callID = "call_" + uuid.New().String()
	}
	return responsesOutput{
		Type:      "function_call",
		ID:        "fc_" + uuid.New().String(),
		Status:    "completed",
		CallID:    callID,
		Name:      tc.Function.Name,
		Arguments: tc.Function.Arguments,
	}
}
