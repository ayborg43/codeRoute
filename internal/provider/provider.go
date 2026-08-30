package provider

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ErrNoKey means no upstream key is stored for a provider yet. It is a normal
// state on a fresh deployment, not a failure, so callers can distinguish it
// from a key that exists and does not work.
var ErrNoKey = errors.New("no API key configured")

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
