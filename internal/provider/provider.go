package provider

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Client translates between the OpenAI-compatible surface CodeRouter exposes
// and one upstream provider's own wire format.
type Client interface {
	Name() string

	// BuildRequest renders req into an upstream HTTP request.
	BuildRequest(ctx context.Context, req *ChatRequest, apiKey string, stream bool) (*http.Request, error)

	// DecodeResponse normalizes a non-streaming upstream body.
	DecodeResponse(body []byte, model string) (*ChatResponse, error)

	// DecodeStream reads the upstream stream and hands normalized chunks to
	// emit, returning the usage totals once the stream ends.
	DecodeStream(r io.Reader, model string, emit func(*ChatResponse) error) (Usage, error)
}

// Registry holds one Client per provider name.
type Registry map[string]Client

func NewRegistry(overrides map[string]string) Registry {
	pick := func(name, fallback string) string {
		if v := overrides[name]; v != "" {
			return strings.TrimRight(v, "/")
		}
		return fallback
	}

	return Registry{
		"openai":    &OpenAI{BaseURL: pick("openai", "https://api.openai.com/v1")},
		"anthropic": &Anthropic{BaseURL: pick("anthropic", "https://api.anthropic.com/v1")},
		"google":    &Google{BaseURL: pick("google", "https://generativelanguage.googleapis.com/v1beta")},
	}
}

// ProviderFor maps a model name to the provider that serves it.
func ProviderFor(model string) string {
	m := strings.ToLower(model)
	switch {
	case strings.HasPrefix(m, "gpt-"), strings.HasPrefix(m, "chatgpt"),
		strings.HasPrefix(m, "o1"), strings.HasPrefix(m, "o3"), strings.HasPrefix(m, "o4"):
		return "openai"
	case strings.HasPrefix(m, "claude"):
		return "anthropic"
	case strings.HasPrefix(m, "gemini"):
		return "google"
	}
	return ""
}

// scanSSE walks an SSE body and calls fn with each `data:` payload, stopping
// at [DONE]. Providers differ in event framing but all use this envelope.
func scanSSE(r io.Reader, fn func(data []byte) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		if err := fn([]byte(payload)); err != nil {
			return err
		}
	}
	return sc.Err()
}

// HTTPError renders a failed upstream response for logs and client errors.
func HTTPError(name string, status int, body []byte) error {
	msg := strings.TrimSpace(string(body))
	if len(msg) > 500 {
		msg = msg[:500] + "..."
	}
	return fmt.Errorf("%s returned %d: %s", name, status, msg)
}
