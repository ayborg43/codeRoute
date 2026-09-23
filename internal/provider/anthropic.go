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

// anthropicBlock is one content block. A single struct covers the kinds this
// gateway sends; the fields of the others are omitted.
type anthropicBlock struct {
	Type string `json:"type"`

	Text string `json:"text,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
}

type anthropicMessage struct {
	Role   string
	Blocks []anthropicBlock
}

// MarshalJSON sends a text-only message as a plain string, the form Anthropic
// documents for simple turns, and anything else as a block array.
func (m anthropicMessage) MarshalJSON() ([]byte, error) {
	var content any = m.Blocks
	if len(m.Blocks) == 1 && m.Blocks[0].Type == "text" {
		content = m.Blocks[0].Text
	}
	return json.Marshal(struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	}{m.Role, content})
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicRequest struct {
	Model         string             `json:"model"`
	MaxTokens     int                `json:"max_tokens"`
	System        string             `json:"system,omitempty"`
	Messages      []anthropicMessage `json:"messages"`
	Tools         []anthropicTool    `json:"tools,omitempty"`
	ToolChoice    map[string]string  `json:"tool_choice,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
}

// anthropicBlocks renders one chat turn as the role and blocks it becomes.
// A tool result is a block inside a user turn; a tool call is a tool_use
// block after any text the assistant wrote first.
func anthropicBlocks(m Message) (string, []anthropicBlock) {
	if m.Role == "tool" {
		return "user", []anthropicBlock{{Type: "tool_result", ToolUseID: m.ToolCallID, Content: string(m.Content)}}
	}

	role := m.Role
	if role != "user" && role != "assistant" {
		role = "user"
	}

	var blocks []anthropicBlock
	// Anthropic refuses empty text blocks, so an assistant turn that only
	// called tools carries no text block at all.
	if text := string(m.Content); text != "" || len(m.ToolCalls) == 0 {
		blocks = append(blocks, anthropicBlock{Type: "text", Text: text})
	}
	if role == "assistant" {
		for _, tc := range m.ToolCalls {
			blocks = append(blocks, anthropicBlock{
				Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: toolArgs(tc.Function.Arguments),
			})
		}
	}
	return role, blocks
}

func anthropicTools(req *ChatRequest) ([]anthropicTool, map[string]string) {
	fns := req.FunctionTools()
	if len(fns) == 0 {
		return nil, nil
	}

	tools := make([]anthropicTool, 0, len(fns))
	for _, t := range fns {
		schema := t.Function.Parameters
		if len(schema) == 0 || string(schema) == "null" {
			// input_schema is required; a parameterless function takes an
			// empty object.
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		tools = append(tools, anthropicTool{Name: t.Function.Name, Description: t.Function.Description, InputSchema: schema})
	}

	var choice map[string]string
	switch mode, name := req.ToolChoiceMode(); mode {
	case "none":
		choice = map[string]string{"type": "none"}
	case "required":
		choice = map[string]string{"type": "any"}
	case "function":
		choice = map[string]string{"type": "tool", "name": name}
	}
	return tools, choice
}

func (a *Anthropic) BuildRequest(ctx context.Context, req *ChatRequest, apiKey string, stream bool) (*http.Request, error) {
	system, turns := req.SystemAndTurns()

	msgs := make([]anthropicMessage, 0, len(turns))
	for _, m := range turns {
		role, blocks := anthropicBlocks(m)
		// Roles must alternate. Turns SystemAndTurns left apart — parallel tool
		// results, or a result followed by the user's next message — join
		// one message here as separate blocks.
		if n := len(msgs); n > 0 && msgs[n-1].Role == role {
			msgs[n-1].Blocks = append(msgs[n-1].Blocks, blocks...)
			continue
		}
		msgs = append(msgs, anthropicMessage{Role: role, Blocks: blocks})
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("anthropic: request has no user or assistant messages")
	}
	// Anthropic requires the conversation to open with a user turn.
	if msgs[0].Role != "user" {
		msgs = append([]anthropicMessage{{Role: "user", Blocks: []anthropicBlock{{Type: "text", Text: "(continue)"}}}}, msgs...)
	}

	tools, toolChoice := anthropicTools(req)

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = anthropicDefaultMaxTokens
	}

	body, err := json.Marshal(anthropicRequest{
		Model:         req.Model,
		MaxTokens:     maxTokens,
		System:        system,
		Messages:      msgs,
		Tools:         tools,
		ToolChoice:    toolChoice,
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
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
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
	var calls []ToolCall
	for _, c := range resp.Content {
		switch c.Type {
		case "text":
			sb.WriteString(c.Text)
		case "tool_use":
			args := string(c.Input)
			if args == "" {
				args = "{}"
			}
			calls = append(calls, ToolCall{
				ID: c.ID, Type: "function", Function: FunctionCall{Name: c.Name, Arguments: args},
			})
		}
	}

	return &ChatResponse{
		ID:      resp.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []Choice{{
			Index:        0,
			Message:      &Message{Role: "assistant", Content: Content(sb.String()), ToolCalls: calls},
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
	Index int    `json:"index"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	ContentBlock struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
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
	// Anthropic numbers every content block; OpenAI numbers tool calls
	// alone. toolIndex maps a tool_use block's index to its call's index.
	toolIndex := map[int]int{}

	chunk := func(delta *Delta, finish *string) *ChatResponse {
		return &ChatResponse{
			ID:      id,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   model,
			Choices: []Choice{{Index: 0, Delta: delta, FinishReason: finish}},
		}
	}

	emitDelta := func(delta *Delta) error {
		// OpenAI clients expect the role on the opening chunk only.
		if first {
			first = false
			if err := emit(chunk(&Delta{Role: "assistant"}, nil)); err != nil {
				return err
			}
		}
		return emit(chunk(delta, nil))
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
		case "content_block_start":
			if ev.ContentBlock.Type != "tool_use" {
				return nil
			}
			idx := len(toolIndex)
			toolIndex[ev.Index] = idx
			return emitDelta(&Delta{ToolCalls: []ToolCall{{
				Index: &idx, ID: ev.ContentBlock.ID, Type: "function",
				Function: FunctionCall{Name: ev.ContentBlock.Name},
			}}})
		case "content_block_delta":
			switch ev.Delta.Type {
			case "input_json_delta":
				idx, ok := toolIndex[ev.Index]
				if !ok || ev.Delta.PartialJSON == "" {
					return nil
				}
				return emitDelta(&Delta{ToolCalls: []ToolCall{{
					Index: &idx, Function: FunctionCall{Arguments: ev.Delta.PartialJSON},
				}}})
			case "text_delta", "":
				if ev.Delta.Text == "" {
					return nil
				}
				return emitDelta(&Delta{Content: ev.Delta.Text})
			}
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
