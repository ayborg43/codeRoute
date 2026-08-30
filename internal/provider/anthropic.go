package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	anthropicVersion = "2023-06-01"
	// Anthropic requires max_tokens; OpenAI treats it as optional.
	anthropicDefaultMaxTokens = 4096
)

type Anthropic struct {
	BaseURL string

	// ProviderName is the registry name this client answers to. Several
	// providers share a dialect, so the name cannot be derived from the type.
	ProviderName string
}

func (a *Anthropic) Name() string {
	if a.ProviderName != "" {
		return a.ProviderName
	}
	return "anthropic"
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicRequest struct {
	Model         string             `json:"model"`
	MaxTokens     int                `json:"max_tokens"`
	System        string             `json:"system,omitempty"`
	Messages      []anthropicMessage `json:"messages"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
}

func (a *Anthropic) BuildRequest(ctx context.Context, req *ChatRequest, apiKey string, stream bool) (*http.Request, error) {
	system, turns := req.SystemAndTurns()

	msgs := make([]anthropicMessage, 0, len(turns))
	for _, m := range turns {
		role := m.Role
		if role != "user" && role != "assistant" {
			role = "user"
		}
		msgs = append(msgs, anthropicMessage{Role: role, Content: string(m.Content)})
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("anthropic: request has no user or assistant messages")
	}
	// Anthropic requires the conversation to open with a user turn.
	if msgs[0].Role != "user" {
		msgs = append([]anthropicMessage{{Role: "user", Content: "(continue)"}}, msgs...)
	}

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = anthropicDefaultMaxTokens
	}

	body, err := json.Marshal(anthropicRequest{
		Model:         req.Model,
		MaxTokens:     maxTokens,
		System:        system,
		Messages:      msgs,
		Temperature:   req.Temperature,
		TopP:          req.TopP,
		StopSequences: req.Stop,
		Stream:        stream,
	})
	if err != nil {
		return nil, err
	}

	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.BaseURL+"/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("x-api-key", apiKey)
	hreq.Header.Set("anthropic-version", anthropicVersion)
	return hreq, nil
}

type anthropicResponse struct {
	ID      string `json:"id"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func anthropicFinish(reason string) *string {
	switch reason {
	case "":
		return nil
	case "max_tokens":
		return strPtr("length")
	case "tool_use":
		return strPtr("tool_calls")
	default:
		return strPtr("stop")
	}
}

func (a *Anthropic) DecodeResponse(body []byte, model string) (*ChatResponse, error) {
	var resp anthropicResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("anthropic: malformed response: %w", err)
	}

	var sb strings.Builder
	for _, c := range resp.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}

	return &ChatResponse{
		ID:      resp.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []Choice{{
			Index:        0,
			Message:      &Message{Role: "assistant", Content: Content(sb.String())},
			FinishReason: anthropicFinish(resp.StopReason),
		}},
		Usage: &Usage{
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
			TotalTokens:      resp.Usage.InputTokens + resp.Usage.OutputTokens,
		},
	}, nil
}

type anthropicStreamEvent struct {
	Type  string `json:"type"`
	Delta struct {
		Type       string `json:"type"`
		Text       string `json:"text"`
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Message struct {
		ID    string `json:"id"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`
	Usage struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func (a *Anthropic) DecodeStream(r io.Reader, model string, emit func(*ChatResponse) error) (Usage, error) {
	var usage Usage
	id := ""
	created := time.Now().Unix()
	first := true

	chunk := func(delta *Delta, finish *string) *ChatResponse {
		return &ChatResponse{
			ID:      id,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   model,
			Choices: []Choice{{Index: 0, Delta: delta, FinishReason: finish}},
		}
	}

	err := scanSSE(r, func(data []byte) error {
		var ev anthropicStreamEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return fmt.Errorf("anthropic: malformed stream event: %w", err)
		}

		switch ev.Type {
		case "message_start":
			id = ev.Message.ID
			usage.PromptTokens = ev.Message.Usage.InputTokens
		case "content_block_delta":
			if ev.Delta.Type != "text_delta" && ev.Delta.Text == "" {
				return nil
			}
			// OpenAI clients expect the role on the opening chunk only.
			if first {
				first = false
				if err := emit(chunk(&Delta{Role: "assistant"}, nil)); err != nil {
					return err
				}
			}
			return emit(chunk(&Delta{Content: ev.Delta.Text}, nil))
		case "message_delta":
			usage.CompletionTokens = ev.Usage.OutputTokens
			return emit(chunk(&Delta{}, anthropicFinish(ev.Delta.StopReason)))
		case "error":
			return fmt.Errorf("anthropic: stream error: %s", string(data))
		}
		return nil
	})

	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	return usage, err
}
