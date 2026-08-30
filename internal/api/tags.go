package api

import (
	"net/http"
	"strings"

	"github.com/coderouter/coderouter/internal/db"
	"github.com/coderouter/coderouter/internal/routing"
)

// taggableTasks are the kinds of work a model can be marked for. Deliberately
// the same set the routing sentinels expose, so marking a model for coding is
// exactly what auto:code will then use.
var taggableTasks = []routing.TaskType{
	routing.TaskCodeGeneration,
	routing.TaskConversation,
	routing.TaskAnalysis,
	routing.TaskCreative,
}

// registerTagRoutes mounts the model marking API.
func (h *Handler) registerTagRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/admin/model-tags", h.adminListModelTags)
	mux.HandleFunc("PUT /v1/admin/model-tags", h.adminSetModelTags)
}

func (h *Handler) adminListModelTags(w http.ResponseWriter, r *http.Request) {
	if !h.adminAuthorized(w, r) {
		return
	}

	stored, err := db.ListModelTags(r.Context(), h.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "internal_error")
		return
	}

	data := []map[string]any{}
	for key, tasks := range stored {
		providerName, model, _ := strings.Cut(key, "/")
		data = append(data, map[string]any{
			"provider": providerName,
			"model":    model,
			"tasks":    db.SortedTasks(tasks),
		})
	}

	counts := map[string]int{}
	for task, n := range h.gw.Catalog().TaggedCount() {
		counts[string(task)] = n
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"object":         "list",
		"data":           data,
		"counts":         counts,
		"taggable_tasks": taskNames(),
		"note": "where any model is marked for a task, automatic routing for " +
			"that task uses only the marked models",
	})
}

func (h *Handler) adminSetModelTags(w http.ResponseWriter, r *http.Request) {
	if !h.adminAuthorized(w, r) {
		return
	}

	var body struct {
		Provider string   `json:"provider"`
		Model    string   `json:"model"`
		Tasks    []string `json:"tasks"`
	}
	if !decodeBody(w, r, &body) {
		return
	}

	body.Provider = strings.TrimSpace(body.Provider)
	body.Model = strings.TrimSpace(body.Model)
	if body.Provider == "" || body.Model == "" {
		writeError(w, http.StatusBadRequest, "provider and model are required", "invalid_request_error")
		return
	}
	if !h.gw.KnowsProvider(body.Provider) {
		writeError(w, http.StatusNotFound, "unknown provider "+body.Provider, "invalid_request_error")
		return
	}

	// An unknown task would be stored and then silently never match anything,
	// so it is refused rather than accepted and quietly ignored.
	for _, task := range body.Tasks {
		if !isTaggableTask(task) {
			writeError(w, http.StatusBadRequest,
				"unknown task "+task+"; expected one of "+strings.Join(taskNames(), ", "),
				"invalid_request_error")
			return
		}
	}

	if err := db.SetModelTags(r.Context(), h.db, body.Provider, body.Model, body.Tasks); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "internal_error")
		return
	}

	// Reload so the change applies to the very next request rather than at
	// the next restart.
	if err := h.gw.LoadModelTags(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "internal_error")
		return
	}

	counts := map[string]int{}
	for task, n := range h.gw.Catalog().TaggedCount() {
		counts[string(task)] = n
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"provider": body.Provider,
		"model":    body.Model,
		"tasks":    db.SortedTasks(body.Tasks),
		"counts":   counts,
	})
}

func isTaggableTask(task string) bool {
	for _, t := range taggableTasks {
		if string(t) == task {
			return true
		}
	}
	return false
}

func taskNames() []string {
	out := make([]string, 0, len(taggableTasks))
	for _, t := range taggableTasks {
		out = append(out, string(t))
	}
	return out
}
