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

	"github.com/google/uuid"
)

type Google struct {
	BaseURL string

	// ProviderName is the registry name this client answers to. Several
	// providers share a dialect, so the name cannot be derived from the type.
	ProviderName string
}

func (g *Google) Name() string {
	if g.ProviderName != "" {
		return g.ProviderName
	}
	return "google"
}

type googlePart struct {
	Text             string              `json:"text,omitempty"`
	FunctionCall     *googleFunctionCall `json:"functionCall,omitempty"`
	FunctionResponse *googleFunctionResp `json:"functionResponse,omitempty"`

	// ThoughtSignature is required on a replayed functionCall part by Gemini
	// 3 models. The real signature is not kept across turns — OpenAI clients
	// have nowhere to carry it — so replays send the value Google documents
	// for histories that did not come from Gemini.
	ThoughtSignature string `json:"thoughtSignature,omitempty"`
}

type googleFunctionCall struct {
	ID   string          `json:"id,omitempty"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type googleFunctionResp struct {
	ID       string         `json:"id,omitempty"`
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

const googleSkipSignature = "skip_thought_signature_validator"

type googleFunctionDecl struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// parametersJsonSchema takes standard JSON Schema; the older parameters
	// field takes an OpenAPI subset that rejects common keys clients send,
	// such as additionalProperties.
	ParametersJSONSchema json.RawMessage `json:"parametersJsonSchema,omitempty"`
}

type googleTool struct {
	FunctionDeclarations []googleFunctionDecl `json:"functionDeclarations"`
}

type googleToolConfig struct {
	FunctionCallingConfig struct {
		Mode                 string   `json:"mode"`
		AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
	} `json:"functionCallingConfig"`
}

type googleContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []googlePart `json:"parts"`
}

type googleRequest struct {
	Contents          []googleContent   `json:"contents"`
	SystemInstruction *googleContent    `json:"systemInstruction,omitempty"`
	Tools             []googleTool      `json:"tools,omitempty"`
	ToolConfig        *googleToolConfig `json:"toolConfig,omitempty"`
	GenerationConfig  *googleGenConfig  `json:"generationConfig,omitempty"`
}

type googleGenConfig struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	MaxOutputTokens int      `json:"maxOutputTokens,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
}

func (g *Google) BuildRequest(ctx context.Context, req *ChatRequest, apiKey string, stream bool) (*http.Request, error) {
	system, turns := req.SystemAndTurns()

	// A functionResponse is matched by name, which a chat tool result does
	// not carry; recover it from the call it answers.
	callNames := map[string]string{}
	for _, m := range turns {
		for _, tc := range m.ToolCalls {
			callNames[tc.ID] = tc.Function.Name
		}
	}

	contents := make([]googleContent, 0, len(turns))
	for _, m := range turns {
		role, parts := googleParts(m, callNames)
		// Parallel calls' results must share one user turn, so same-role
		// turns SystemAndTurns left apart are joined here.
		if n := len(contents); n > 0 && contents[n-1].Role == role {
			contents[n-1].Parts = append(contents[n-1].Parts, parts...)
			continue
		}
		contents = append(contents, googleContent{Role: role, Parts: parts})
	}
	if len(contents) == 0 {
		return nil, fmt.Errorf("google: request has no user or assistant messages")
	}

	body := googleRequest{Contents: contents}
	body.Tools, body.ToolConfig = googleTools(req)
	if system != "" {
		body.SystemInstruction = &googleContent{Parts: []googlePart{{Text: system}}}
	}
	if req.Temperature != nil || req.TopP != nil || req.MaxTokens > 0 || len(req.Stop) > 0 {
		body.GenerationConfig = &googleGenConfig{
			Temperature:     req.Temperature,
			TopP:            req.TopP,
			MaxOutputTokens: req.MaxTokens,
			StopSequences:   req.Stop,
		}
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	method := "generateContent"
	suffix := ""
	if stream {
		method = "streamGenerateContent"
		suffix = "?alt=sse"
	}
	url := fmt.Sprintf("%s/models/%s:%s%s", g.BaseURL, req.Model, method, suffix)

	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	// Header auth rather than ?key=, so the secret stays out of URLs and logs.
	hreq.Header.Set("x-goog-api-key", apiKey)
	return hreq, nil
}

// googleParts renders one chat turn as its role and parts. Google names the
// assistant role "model", and a tool result is a functionResponse part in a
// user turn.
func googleParts(m Message, callNames map[string]string) (string, []googlePart) {
	if m.Role == "tool" {
		return "user", []googlePart{{FunctionResponse: &googleFunctionResp{
			Name:     callNames[m.ToolCallID],
			Response: map[string]any{"content": string(m.Content)},
		}}}
	}

	role := "user"
	if m.Role == "assistant" {
		role = "model"
	}

	var parts []googlePart
	if text := string(m.Content); text != "" || len(m.ToolCalls) == 0 {
		parts = append(parts, googlePart{Text: text})
	}
	if role == "model" {
		for _, tc := range m.ToolCalls {
			parts = append(parts, googlePart{
				FunctionCall:     &googleFunctionCall{Name: tc.Function.Name, Args: toolArgs(tc.Function.Arguments)},
				ThoughtSignature: googleSkipSignature,
			})
		}
	}
	return role, parts
}

func googleTools(req *ChatRequest) ([]googleTool, *googleToolConfig) {
	fns := req.FunctionTools()
	if len(fns) == 0 {
		return nil, nil
	}

	decls := make([]googleFunctionDecl, 0, len(fns))
	for _, t := range fns {
		decl := googleFunctionDecl{Name: t.Function.Name, Description: t.Function.Description}
		if p := t.Function.Parameters; len(p) > 0 && string(p) != "null" {
			decl.ParametersJSONSchema = p
		}
		decls = append(decls, decl)
	}

	var cfg *googleToolConfig
	switch mode, name := req.ToolChoiceMode(); mode {
	case "none":
		cfg = &googleToolConfig{}
		cfg.FunctionCallingConfig.Mode = "NONE"
	case "required":
		cfg = &googleToolConfig{}
		cfg.FunctionCallingConfig.Mode = "ANY"
	case "function":
		cfg = &googleToolConfig{}
		cfg.FunctionCallingConfig.Mode = "ANY"
		cfg.FunctionCallingConfig.AllowedFunctionNames = []string{name}
	}
	return []googleTool{{FunctionDeclarations: decls}}, cfg
}

type googleResponse struct {
	Candidates []struct {
		Content struct {
			Parts []googlePart `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		TotalTokenCount      int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
}

func googleFinish(reason string) *string {
	switch reason {
	case "":
		return nil
	case "MAX_TOKENS":
		return strPtr("length")
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT":
		return strPtr("content_filter")
	default:
		return strPtr("stop")
	}
}

func (r *googleResponse) text() string {
	var sb strings.Builder
	if len(r.Candidates) > 0 {
		for _, p := range r.Candidates[0].Content.Parts {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// toolCalls extracts the function calls from the first candidate. Gemini
// sends each call whole rather than in fragments, and gives it no id unless
// asked, so one is minted when absent.
func (r *googleResponse) toolCalls() []ToolCall {
	if len(r.Candidates) == 0 {
		return nil
	}
	var calls []ToolCall
	for _, p := range r.Candidates[0].Content.Parts {
		if p.FunctionCall == nil {
			continue
		}
		id := p.FunctionCall.ID
		if id == "" {
			id = "call_" + uuid.New().String()
		}
		args := string(p.FunctionCall.Args)
		if args == "" {
			args = "{}"
		}
		calls = append(calls, ToolCall{
			ID: id, Type: "function", Function: FunctionCall{Name: p.FunctionCall.Name, Arguments: args},
		})
	}
	return calls
}

// finish maps the finish reason. Gemini reports STOP after a function call,
// where an OpenAI client expects tool_calls.
func (r *googleResponse) finish(calls []ToolCall) *string {
	reason := googleFinish(r.Candidates[0].FinishReason)
	if reason != nil && *reason == "stop" && len(calls) > 0 {
		return strPtr("tool_calls")
	}
	return reason
}

func (r *googleResponse) usage() Usage {
	return Usage{
		PromptTokens:     r.UsageMetadata.PromptTokenCount,
		CompletionTokens: r.UsageMetadata.CandidatesTokenCount,
		TotalTokens:      r.UsageMetadata.TotalTokenCount,
	}
}

func (g *Google) DecodeResponse(body []byte, model string) (*ChatResponse, error) {
	var resp googleResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("google: malformed response: %w", err)
	}
	if len(resp.Candidates) == 0 {
		return nil, fmt.Errorf("google: response contained no candidates")
	}

	usage := resp.usage()
	calls := resp.toolCalls()
	return &ChatResponse{
		ID:      "chatcmpl-" + fmt.Sprint(time.Now().UnixNano()),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []Choice{{
			Index:        0,
			Message:      &Message{Role: "assistant", Content: Content(resp.text()), ToolCalls: calls},
			FinishReason: resp.finish(calls),
		}},
		Usage: &usage,
	}, nil
}

func (g *Google) DecodeStream(r io.Reader, model string, emit func(*ChatResponse) error) (Usage, error) {
	var usage Usage
	id := "chatcmpl-" + fmt.Sprint(time.Now().UnixNano())
	created := time.Now().Unix()
	first := true
	calls := 0

	err := scanSSE(r, func(data []byte) error {
		var resp googleResponse
		if err := json.Unmarshal(data, &resp); err != nil {
			return fmt.Errorf("google: malformed stream chunk: %w", err)
		}
		if u := resp.usage(); u.TotalTokens > 0 || u.PromptTokens > 0 {
			usage = u
		}
		if len(resp.Candidates) == 0 {
			return nil
		}

		delta := &Delta{Content: resp.text()}
		for _, tc := range resp.toolCalls() {
			idx := calls
			calls++
			tc.Index = &idx
			delta.ToolCalls = append(delta.ToolCalls, tc)
		}

		// The finish reason usually lands on a later chunk than the call, so
		// the running count decides whether STOP means tool_calls.
		finish := googleFinish(resp.Candidates[0].FinishReason)
		if finish != nil && *finish == "stop" && calls > 0 {
			finish = strPtr("tool_calls")
		}

		chunk := &ChatResponse{
			ID:      id,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   model,
			Choices: []Choice{{
				Index:        0,
				Delta:        delta,
				FinishReason: finish,
			}},
		}
		if first {
			first = false
			chunk.Choices[0].Delta.Role = "assistant"
		}
		return emit(chunk)
	})

	return usage, err
}
