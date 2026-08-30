package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"

	"github.com/coderouter/coderouter/internal/config"
	"github.com/coderouter/coderouter/internal/db"
	"github.com/coderouter/coderouter/internal/gateway"
	"github.com/coderouter/coderouter/internal/iot"
	"github.com/coderouter/coderouter/internal/provider"
)

// maxRequestBytes caps an inbound body; large contexts are still well under it.
const maxRequestBytes = 32 << 20

// freeRouterModel is the model name a caller asks for to stay on free models.
const freeRouterModel = "auto:free"

// modelEntry is one row of /v1/models. Prices are pointers so an unpriced
// model omits them entirely rather than reporting a misleading zero.
type modelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`

	InputCostPer1M  *float64 `json:"input_cost_per_1m,omitempty"`
	OutputCostPer1M *float64 `json:"output_cost_per_1m,omitempty"`
	Free            *bool    `json:"free,omitempty"`
}

type Handler struct {
	gw     *gateway.Gateway
	db     *sql.DB
	cfg    *config.Config
	bridge *iot.Bridge

	// throttle bars an identity after repeated failed sign-ins. It is held on
	// the handler rather than being global so tests get a fresh one.
	throttle *loginThrottle
}

func NewHandler(gw *gateway.Gateway, database *sql.DB, cfg *config.Config, bridge *iot.Bridge) http.Handler {
	h := &Handler{gw: gw, db: database, cfg: cfg, bridge: bridge, throttle: newLoginThrottle()}
	mux := http.NewServeMux()

	h.registerAuthRoutes(mux)
	h.registerAdminRoutes(mux)
	h.registerDashboardRoutes(mux)

	mux.HandleFunc("/v1/chat/completions", h.handleChatCompletions)
	mux.HandleFunc("/v1/models", h.handleModels)
	mux.HandleFunc("/v1/iot/telemetry", h.handleTelemetry)
	mux.HandleFunc("/v1/iot/inference", h.handleIoTInference)
	mux.HandleFunc("/health", h.handleHealth)
	mux.HandleFunc("/", h.serveDashboard)

	return mux
}

// authorize resolves the caller's client key. It writes the error response
// itself and reports whether the request may proceed.
//
// A valid key is now unconditional permission: there is no rate limit, no
// daily budget, and no suspension. Revocation is the only control left.
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request) (*db.ClientKey, bool) {
	key, err := db.ValidateClientKey(h.db, r.Header.Get("Authorization"))
	if err != nil {
		switch {
		case errors.Is(err, db.ErrKeyDisabled):
			writeError(w, http.StatusUnauthorized, "this API key has been revoked", "invalid_request_error")
		case errors.Is(err, db.ErrInvalidKey):
			writeError(w, http.StatusUnauthorized, "invalid API key", "invalid_request_error")
		default:
			writeError(w, http.StatusInternalServerError, "failed to verify API key", "internal_error")
		}
		return nil, false
	}

	return key, true
}

func (h *Handler) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}

	key, ok := h.authorize(w, r)
	if !ok {
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
		h.streamCompletions(w, r, &req, key)
		return
	}

	resp, err := h.gw.Complete(r.Context(), &req, key)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error(), "upstream_error")
		return
	}

	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) streamCompletions(w http.ResponseWriter, r *http.Request, req *provider.ChatRequest, key *db.ClientKey) {
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

	err := h.gw.Stream(r.Context(), req, key, emit)
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
	if _, ok := h.authorize(w, r); !ok {
		return
	}

	configured, err := h.gw.ConfiguredProviders()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load providers", "internal_error")
		return
	}

	catalog := h.gw.Catalog()
	freeOnly := r.URL.Query().Get("free") == "true" || h.gw.FreeOnly()

	// Discovered models are what a provider actually serves today; the curated
	// catalogue is the hand-tuned subset smart routing chooses between. Both
	// are advertised, deduplicated, so an editor's model list matches what the
	// gateway will accept.
	pool := catalog.Discovered()
	if freeOnly {
		pool = catalog.FreeModels()
	} else {
		pool = append(pool, catalog.Profiles()...)
	}

	seen := make(map[string]bool)
	data := []modelEntry{}
	for _, p := range pool {
		if !configured[p.Provider] || seen[p.Model] {
			continue
		}
		seen[p.Model] = true

		entry := modelEntry{ID: p.Model, Object: "model", OwnedBy: p.Provider}
		if p.PriceKnown {
			in, out, free := p.InputCostPer1M, p.OutputCostPer1M, p.Free()
			entry.InputCostPer1M, entry.OutputCostPer1M, entry.Free = &in, &out, &free
		}
		data = append(data, entry)
	}

	sort.Slice(data, func(i, j int) bool { return data[i].ID < data[j].ID })

	// Advertise the routing aliases so an editor can pick one from its model
	// dropdown without knowing any provider's model names.
	if h.cfg.RoutingMode != "off" && len(data) > 0 {
		hasFree := len(catalog.FreeModels()) > 0

		var head []modelEntry
		for _, name := range gateway.Sentinels() {
			if name == freeRouterModel {
				if !hasFree {
					continue
				}
				yes := true
				head = append(head, modelEntry{
					ID: name, Object: "model", OwnedBy: "coderouter", Free: &yes,
				})
				continue
			}
			head = append(head, modelEntry{ID: name, Object: "model", OwnedBy: "coderouter"})
		}
		data = append(head, data...)
	}

	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// handleTelemetry ingests a device reading (POST) or reads a device's recent
// history (GET ?device_id=...&limit=).
func (h *Handler) handleTelemetry(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.authorize(w, r); !ok {
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

	key, ok := h.authorize(w, r)
	if !ok {
		return
	}

	var req iot.InferenceRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid inference payload: "+err.Error(), "invalid_request_error")
		return
	}

	resp, err := h.bridge.InferAs(r.Context(), req, key)
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
	body["cache"] = h.gw.CacheStats().Enabled

	writeJSON(w, code, body)
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
