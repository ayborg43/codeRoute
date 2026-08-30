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

type Message struct {
	Role    string  `json:"role"`
	Content Content `json:"content"`
}

type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Temperature *float64  `json:"temperature,omitempty"`
	TopP        *float64  `json:"top_p,omitempty"`
	Stop        []string  `json:"stop,omitempty"`
	Stream      bool      `json:"stream,omitempty"`

	// Raw is the client's original body, preserved so the OpenAI path can
	// forward fields this struct does not model (tools, response_format, ...).
	Raw json.RawMessage `json:"-"`
}

type Delta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
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
		if n := len(turns); n > 0 && turns[n-1].Role == m.Role {
			turns[n-1].Content += "\n\n" + m.Content
			continue
		}
		turns = append(turns, m)
	}

	return strings.Join(system, "\n\n"), turns
}

func strPtr(s string) *string { return &s }
