package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// EmbeddingDimensions is the vector width the cache_entries column is declared
// with. Any embedding model configured must produce exactly this many floats.
const EmbeddingDimensions = 1536

// DefaultEmbeddingModel is OpenAI's small embedding model, which is natively
// 1536-dimensional and the cheapest of the family.
const DefaultEmbeddingModel = "text-embedding-3-small"

// Embedder turns text into a vector. It is an interface so the semantic cache
// can be tested, and so a local embedding server can stand in for OpenAI.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// OpenAIEmbedder calls any OpenAI-compatible /embeddings endpoint, which
// includes local servers such as llama.cpp and Ollama.
type OpenAIEmbedder struct {
	BaseURL string

	// Key is resolved per call rather than captured once, so a key added or
	// rotated through the admin API takes effect without a restart.
	Key func() (string, error)

	Model  string
	Client *http.Client
}

func (e *OpenAIEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if e.Key == nil {
		return nil, ErrNoKey
	}
	apiKey, err := e.Key()
	if err != nil {
		return nil, err
	}
	if apiKey == "" {
		return nil, ErrNoKey
	}

	model := e.Model
	if model == "" {
		model = DefaultEmbeddingModel
	}

	body, marshalErr := json.Marshal(map[string]any{
		"model": model,
		"input": text,
		// Ask for the exact width the column is declared with. The
		// text-embedding-3 family honours this; older models ignore it and
		// are checked below instead.
		"dimensions": EmbeddingDimensions,
	})
	if marshalErr != nil {
		return nil, marshalErr
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.BaseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := e.Client
	if client == nil {
		client = http.DefaultClient
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, HTTPError("openai embeddings", resp.StatusCode, raw)
	}

	var decoded struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, err
	}
	if len(decoded.Data) == 0 {
		return nil, fmt.Errorf("embeddings response contained no vectors")
	}

	vec := decoded.Data[0].Embedding
	if len(vec) != EmbeddingDimensions {
		// Storing a mismatched width would fail at the column, one row at a
		// time; saying so here names the actual cause.
		return nil, fmt.Errorf("embedding model %q returned %d dimensions, need %d",
			model, len(vec), EmbeddingDimensions)
	}
	return vec, nil
}
