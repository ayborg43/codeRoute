package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// OpenAI is a near-passthrough: the public surface already speaks its dialect,
// so the client's original body is forwarded with only stream flags adjusted.
type OpenAI struct {
	BaseURL string
}

func (o *OpenAI) Name() string { return "openai" }

func (o *OpenAI) BuildRequest(ctx context.Context, req *ChatRequest, apiKey string, stream bool) (*http.Request, error) {
	body, err := o.renderBody(req, stream)
	if err != nil {
		return nil, err
	}

	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Authorization", "Bearer "+apiKey)
	return hreq, nil
}

func (o *OpenAI) renderBody(req *ChatRequest, stream bool) ([]byte, error) {
	fields := map[string]json.RawMessage{}
	if len(req.Raw) > 0 {
		if err := json.Unmarshal(req.Raw, &fields); err != nil {
			return nil, fmt.Errorf("invalid request body: %w", err)
		}
	} else {
		encoded, err := json.Marshal(req)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(encoded, &fields); err != nil {
			return nil, err
		}
	}

	// The model may have been rewritten by fallback routing.
	model, err := json.Marshal(req.Model)
	if err != nil {
		return nil, err
	}
	fields["model"] = model

	if stream {
		fields["stream"] = json.RawMessage(`true`)
		// Without this OpenAI omits usage from streamed responses, which
		// would leave usage_logs with zeroed token counts.
		fields["stream_options"] = json.RawMessage(`{"include_usage":true}`)
	} else {
		delete(fields, "stream")
		delete(fields, "stream_options")
	}

	return json.Marshal(fields)
}

func (o *OpenAI) DecodeResponse(body []byte, model string) (*ChatResponse, error) {
	var resp ChatResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("openai: malformed response: %w", err)
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("openai: response contained no choices")
	}
	return &resp, nil
}

func (o *OpenAI) DecodeStream(r io.Reader, model string, emit func(*ChatResponse) error) (Usage, error) {
	var usage Usage

	err := scanSSE(r, func(data []byte) error {
		var chunk ChatResponse
		if err := json.Unmarshal(data, &chunk); err != nil {
			return fmt.Errorf("openai: malformed stream chunk: %w", err)
		}
		if chunk.Usage != nil {
			usage = *chunk.Usage
		}
		// The usage-only trailer carries no choices; nothing to relay.
		if len(chunk.Choices) == 0 {
			return nil
		}
		return emit(&chunk)
	})

	return usage, err
}
