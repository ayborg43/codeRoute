package cache

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

type SemanticCache struct {
	db *sql.DB
}

func New(db *sql.DB) *SemanticCache {
	return &SemanticCache{db: db}
}

func (c *SemanticCache) Lookup(prompt string, model string, threshold float64) (string, bool, error) {
	embedding, err := c.getEmbedding(prompt)
	if err != nil {
		return "", false, fmt.Errorf("failed to get embedding: %w", err)
	}

	var response string
	err = c.db.QueryRow(
		`SELECT response FROM cache_entries 
		 WHERE model = $1 
		 ORDER BY embedding <=> $2::vector 
		 LIMIT 1`,
		model, fmt.Sprintf("[%s]", formatVector(embedding)),
	).Scan(&response)

	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}

	return response, true, nil
}

func (c *SemanticCache) Store(prompt string, response string, model string) error {
	embedding, err := c.getEmbedding(prompt)
	if err != nil {
		return fmt.Errorf("failed to get embedding: %w", err)
	}

	_, err = c.db.Exec(
		`INSERT INTO cache_entries (embedding, response, model) VALUES ($1::vector, $2, $3)`,
		fmt.Sprintf("[%s]", formatVector(embedding)), response, model,
	)
	return err
}

func (c *SemanticCache) getEmbedding(text string) ([]float32, error) {
	// Placeholder: in production, call OpenAI embeddings API or local model
	// For now, return a simple hash-based pseudo-embedding for demonstration
	embedding := make([]float32, 1536)
	for i := 0; i < len(text) && i < 1536; i++ {
		embedding[i] = float32(text[i]) / 255.0
	}
	return embedding, nil
}

func formatVector(v []float32) string {
	b, _ := json.Marshal(v)
	return string(b[1 : len(b)-1])
}
