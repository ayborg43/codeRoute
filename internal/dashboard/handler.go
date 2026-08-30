package dashboard

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"time"
)

type Stats struct {
	TotalRequests int     `json:"totalRequests"`
	AvgLatency    float64 `json:"avgLatency"`
	CacheHitRate  float64 `json:"cacheHitRate"`
}

type Handler struct {
	db *sql.DB
}

func NewHandler(db *sql.DB) *Handler {
	return &Handler{db: db}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/stats", h.handleStats)
	mux.HandleFunc("/api/usage", h.handleUsage)
	mux.HandleFunc("/api/keys", h.handleKeys)
}

func (h *Handler) handleStats(w http.ResponseWriter, r *http.Request) {
	var stats Stats

	err := h.db.QueryRow(`SELECT COUNT(*) FROM usage_logs WHERE created_at > NOW() - INTERVAL '24 hours'`).Scan(&stats.TotalRequests)
	if err != nil {
		stats.TotalRequests = 0
	}

	err = h.db.QueryRow(`SELECT COALESCE(AVG(latency_ms), 0) FROM usage_logs WHERE created_at > NOW() - INTERVAL '24 hours'`).Scan(&stats.AvgLatency)
	if err != nil {
		stats.AvgLatency = 0
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

func (h *Handler) handleUsage(w http.ResponseWriter, r *http.Request) {
	rows, err := h.db.Query(`SELECT model, provider, tokens_in, tokens_out, latency_ms, status, created_at FROM usage_logs ORDER BY created_at DESC LIMIT 50`)
	if err != nil {
		http.Error(w, "failed to query usage", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var usage []map[string]interface{}
	for rows.Next() {
		var model, provider, status string
		var tokensIn, tokensOut, latencyMs int
		var createdAt time.Time
		if err := rows.Scan(&model, &provider, &tokensIn, &tokensOut, &latencyMs, &status, &createdAt); err != nil {
			continue
		}
		usage = append(usage, map[string]interface{}{
			"model":      model,
			"provider":   provider,
			"tokens_in":  tokensIn,
			"tokens_out": tokensOut,
			"latency_ms": latencyMs,
			"status":     status,
			"created_at": createdAt,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(usage)
}

func (h *Handler) handleKeys(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"key": "placeholder-key-generation-via-db-package"})
}
