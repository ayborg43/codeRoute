package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/coderouter/coderouter/internal/config"
	"github.com/coderouter/coderouter/internal/db"
	"github.com/coderouter/coderouter/internal/gateway"
	"github.com/coderouter/coderouter/internal/iot"
	"github.com/coderouter/coderouter/internal/provider"
	"github.com/coderouter/coderouter/internal/routing"
)

// maxRequestBytes caps an inbound body; large contexts are still well under it.
const maxRequestBytes = 32 << 20

type Handler struct {
	gw     *gateway.Gateway
	db     *sql.DB
	cfg    *config.Config
	bridge *iot.Bridge
}

func NewHandler(gw *gateway.Gateway, database *sql.DB, cfg *config.Config, bridge *iot.Bridge) http.Handler {
	h := &Handler{gw: gw, db: database, cfg: cfg, bridge: bridge}
	mux := http.NewServeMux()

	mux.HandleFunc("/v1/chat/completions", h.handleChatCompletions)
	mux.HandleFunc("/v1/models", h.handleModels)
	mux.HandleFunc("/v1/iot/telemetry", h.handleTelemetry)
	mux.HandleFunc("/v1/iot/inference", h.handleIoTInference)
	mux.HandleFunc("/health", h.handleHealth)
	mux.HandleFunc("/", h.handleDashboard)

	return mux
}

// authenticate resolves the caller's client key, returning its ID for usage
// attribution.
func (h *Handler) authenticate(r *http.Request) (string, error) {
	key, err := db.ValidateClientKey(h.db, r.Header.Get("Authorization"))
	if err != nil {
		return "", err
	}
	return key.ID, nil
}

func (h *Handler) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}

	keyID, err := h.authenticate(r)
	if err != nil {
		if errors.Is(err, db.ErrInvalidKey) {
			writeError(w, http.StatusUnauthorized, "invalid API key", "invalid_request_error")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to verify API key", "internal_error")
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body", "invalid_request_error")
		return
	}
	defer r.Body.Close()

	var req provider.ChatRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error(), "invalid_request_error")
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "messages must not be empty", "invalid_request_error")
		return
	}

	req.Raw = raw
	if req.Model == "" {
		req.Model = h.cfg.DefaultModel
	}

	if req.Stream {
		h.streamCompletions(w, r, &req, keyID)
		return
	}

	resp, err := h.gw.Complete(r.Context(), &req, keyID)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error(), "upstream_error")
		return
	}

	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) streamCompletions(w http.ResponseWriter, r *http.Request, req *provider.ChatRequest, keyID string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported by server", "internal_error")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Stops reverse proxies from buffering the stream into one response.
	w.Header().Set("X-Accel-Buffering", "no")

	started := false
	emit := func(chunk *provider.ChatResponse) error {
		encoded, err := json.Marshal(chunk)
		if err != nil {
			return err
		}
		if !started {
			started = true
			w.WriteHeader(http.StatusOK)
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", encoded); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	err := h.gw.Stream(r.Context(), req, keyID, emit)
	if err != nil {
		// Nothing was written yet, so a normal HTTP error is still possible.
		if !started {
			w.Header().Del("Content-Type")
			writeError(w, http.StatusBadGateway, err.Error(), "upstream_error")
			return
		}
		// Mid-stream the status is already committed; report in-band instead.
		encoded, _ := json.Marshal(map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "upstream_error"},
		})
		fmt.Fprintf(w, "data: %s\n\n", encoded)
		flusher.Flush()
		return
	}

	if !started {
		w.WriteHeader(http.StatusOK)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	if _, err := h.authenticate(r); err != nil {
		writeError(w, http.StatusUnauthorized, "invalid API key", "invalid_request_error")
		return
	}

	configured, err := h.gw.ConfiguredProviders()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load providers", "internal_error")
		return
	}

	data := []map[string]any{}
	for _, p := range routing.Profiles() {
		if !configured[p.Provider] {
			continue
		}
		data = append(data, map[string]any{
			"id":       p.Model,
			"object":   "model",
			"owned_by": p.Provider,
		})
	}

	// Advertise the router itself so editors can pick smart routing from
	// their model dropdown.
	if h.cfg.RoutingMode != "off" && len(data) > 0 {
		data = append([]map[string]any{{
			"id":       h.cfg.RouterModel,
			"object":   "model",
			"owned_by": "coderouter",
		}}, data...)
	}

	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// handleTelemetry ingests a device reading (POST) or reads a device's recent
// history (GET ?device_id=...&limit=).
func (h *Handler) handleTelemetry(w http.ResponseWriter, r *http.Request) {
	if _, err := h.authenticate(r); err != nil {
		writeError(w, http.StatusUnauthorized, "invalid API key", "invalid_request_error")
		return
	}

	switch r.Method {
	case http.MethodPost:
		var event iot.TelemetryEvent
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&event); err != nil {
			writeError(w, http.StatusBadRequest, "invalid telemetry payload: "+err.Error(), "invalid_request_error")
			return
		}
		if event.DeviceID == "" {
			writeError(w, http.StatusBadRequest, "device_id is required", "invalid_request_error")
			return
		}
		if err := h.bridge.IngestTelemetry(r.Context(), event); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), "internal_error")
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "accepted"})

	case http.MethodGet:
		deviceID := r.URL.Query().Get("device_id")
		if deviceID == "" {
			writeError(w, http.StatusBadRequest, "device_id is required", "invalid_request_error")
			return
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))

		events, err := h.bridge.RecentTelemetry(r.Context(), deviceID, limit)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), "internal_error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"device_id": deviceID, "events": events})

	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
	}
}

// handleIoTInference is the HTTP twin of the MQTT inference topic, for devices
// that cannot speak MQTT.
func (h *Handler) handleIoTInference(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}

	keyID, err := h.authenticate(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid API key", "invalid_request_error")
		return
	}

	var req iot.InferenceRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid inference payload: "+err.Error(), "invalid_request_error")
		return
	}

	resp, err := h.bridge.InferAs(r.Context(), req, keyID)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error(), "upstream_error")
		return
	}

	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	status := "ok"
	code := http.StatusOK

	if err := h.db.Ping(); err != nil {
		status = "degraded"
		code = http.StatusServiceUnavailable
	}

	body := map[string]any{"status": status}
	if configured, err := h.gw.ConfiguredProviders(); err == nil {
		body["providers"] = configured
	}
	body["mqtt"] = h.bridge.Connected()

	writeJSON(w, code, body)
}

func (h *Handler) handleDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(`<!DOCTYPE html><html><head><title>CodeRouter</title></head><body><h1>CodeRouter Dashboard</h1><p>Coming soon</p></body></html>`))
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message, kind string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"message": message, "type": kind},
	})
}
