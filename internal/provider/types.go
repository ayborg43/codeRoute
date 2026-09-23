package provider

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Content is an OpenAI message content field. Clients send either a plain
// string or an array of typed parts; both decode to flat text here.
type Content string

func (c *Content) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		*c = ""
		return nil
	}

	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*c = Content(s)
		return nil
	}

	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(b, &parts); err != nil {
		return err
	}

	var sb strings.Builder
	for _, p := range parts {
		if p.Type == "text" || p.Text != "" {
			sb.WriteString(p.Text)
		}
	}
	*c = Content(sb.String())
	return nil
}

// FunctionCall is the callee half of a tool call. Arguments stays a string
// because that is how every OpenAI-dialect provider encodes it: a JSON
// document nested inside a JSON string, streamed in fragments.
type FunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// ToolCall is one tool invocation an assistant asked for. Index is only set on
// streamed deltas, where it is the sole way to tell which call a fragment
// belongs to; ID is absent on all but the first fragment of each call.
type ToolCall struct {
	Index    *int         `json:"index,omitempty"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function FunctionCall `json:"function"`
}

type Message struct {
	Role    string  `json:"role"`
	Content Content `json:"content"`

	// ToolCalls carries an assistant turn that asked for tools. ToolCallID
	// marks a "tool" role message as the result of one such call. Both are
	// needed to replay an agent's history back to a provider.
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// Tool is a function the model may call, in chat-completions shape.
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Temperature *float64  `json:"temperature,omitempty"`
	TopP        *float64  `json:"top_p,omitempty"`
	Stop        []string  `json:"stop,omitempty"`
	Stream      bool      `json:"stream,omitempty"`

	// Tools and ToolChoice are modelled so the native Anthropic and Google
	// clients can translate them; the OpenAI path forwards Raw instead.
	Tools      []Tool          `json:"tools,omitempty"`
	ToolChoice json.RawMessage `json:"tool_choice,omitempty"`

	// Raw is the client's original body, preserved so the OpenAI path can
	// forward fields this struct does not model (tools, response_format, ...).
	Raw json.RawMessage `json:"-"`
}

type Delta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`

	// ToolCalls arrive as fragments: the first carries ID and function name,
	// later ones append to Arguments, all keyed by Index.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

type Choice struct {
	Index        int      `json:"index"`
	Message      *Message `json:"message,omitempty"`
	Delta        *Delta   `json:"delta,omitempty"`
	FinishReason *string  `json:"finish_reason"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type ChatResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

// SystemAndTurns splits messages into the joined system prompt and the
// conversational turns, merging consecutive same-role turns. Anthropic and
// Google both reject runs of same-role messages that OpenAI accepts.
func (r *ChatRequest) SystemAndTurns() (string, []Message) {
	var system []string
	var turns []Message

	for _, m := range r.Messages {
		if m.Role == "system" || m.Role == "developer" {
			if s := strings.TrimSpace(string(m.Content)); s != "" {
				system = append(system, s)
			}
			continue
		}
		// Merging concatenates content only, so a turn carrying tool calls or
		// a tool result must stay whole rather than lose those fields.
		if n := len(turns); n > 0 && turns[n-1].Role == m.Role &&
			len(turns[n-1].ToolCalls) == 0 && len(m.ToolCalls) == 0 &&
			turns[n-1].ToolCallID == "" && m.ToolCallID == "" {
			turns[n-1].Content += "\n\n" + m.Content
			continue
		}
		turns = append(turns, m)
	}

	return strings.Join(system, "\n\n"), turns
}

// FunctionTools returns the function tools, skipping any other kind: neither
// native dialect has a counterpart for OpenAI's non-function tools.
func (r *ChatRequest) FunctionTools() []Tool {
	var out []Tool
	for _, t := range r.Tools {
		if (t.Type == "" || t.Type == "function") && t.Function.Name != "" {
			out = append(out, t)
		}
	}
	return out
}

// ToolChoiceMode reads tool_choice as one of "auto", "none", "required" or
// "function", with the forced function's name for the last. Unset or
// unrecognised reads as "auto".
func (r *ChatRequest) ToolChoiceMode() (mode, name string) {
	if len(r.ToolChoice) == 0 {
		return "auto", ""
	}
	var s string
	if err := json.Unmarshal(r.ToolChoice, &s); err == nil {
		switch s {
		case "none", "required":
			return s, ""
		}
		return "auto", ""
	}
	var named struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(r.ToolChoice, &named); err == nil && named.Function.Name != "" {
		return "function", named.Function.Name
	}
	return "auto", ""
}

// toolArgs parses a call's arguments string into a JSON object, which is how
// both native dialects want them. Empty or malformed arguments become {}.
func toolArgs(args string) json.RawMessage {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(args), &obj); err != nil || obj == nil {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(args)
}

func strPtr(s string) *string { return &s }
