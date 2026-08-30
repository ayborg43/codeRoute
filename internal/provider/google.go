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
	Text string `json:"text"`
}

type googleContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []googlePart `json:"parts"`
}

type googleRequest struct {
	Contents          []googleContent  `json:"contents"`
	SystemInstruction *googleContent   `json:"systemInstruction,omitempty"`
	GenerationConfig  *googleGenConfig `json:"generationConfig,omitempty"`
}

type googleGenConfig struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	MaxOutputTokens int      `json:"maxOutputTokens,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
}

func (g *Google) BuildRequest(ctx context.Context, req *ChatRequest, apiKey string, stream bool) (*http.Request, error) {
	system, turns := req.SystemAndTurns()

	contents := make([]googleContent, 0, len(turns))
	for _, m := range turns {
		// Google names the assistant role "model".
		role := "user"
		if m.Role == "assistant" {
			role = "model"
		}
		contents = append(contents, googleContent{Role: role, Parts: []googlePart{{Text: string(m.Content)}}})
	}
	if len(contents) == 0 {
		return nil, fmt.Errorf("google: request has no user or assistant messages")
	}

	body := googleRequest{Contents: contents}
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
	return &ChatResponse{
		ID:      "chatcmpl-" + fmt.Sprint(time.Now().UnixNano()),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []Choice{{
			Index:        0,
			Message:      &Message{Role: "assistant", Content: Content(resp.text())},
			FinishReason: googleFinish(resp.Candidates[0].FinishReason),
		}},
		Usage: &usage,
	}, nil
}

func (g *Google) DecodeStream(r io.Reader, model string, emit func(*ChatResponse) error) (Usage, error) {
	var usage Usage
	id := "chatcmpl-" + fmt.Sprint(time.Now().UnixNano())
	created := time.Now().Unix()
	first := true

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

		chunk := &ChatResponse{
			ID:      id,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   model,
			Choices: []Choice{{
				Index:        0,
				Delta:        &Delta{Content: resp.text()},
				FinishReason: googleFinish(resp.Candidates[0].FinishReason),
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
