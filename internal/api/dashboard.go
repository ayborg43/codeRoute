package api

import (
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/coderouter/coderouter/internal/db"
	"github.com/coderouter/coderouter/internal/gateway"
	"github.com/coderouter/coderouter/internal/routing"
	"github.com/coderouter/coderouter/ui"
)

// maxWindow bounds how far back the dashboard will aggregate, so a stray
// ?window=8760h cannot turn a page refresh into a full-table scan.
const maxWindow = 30 * 24 * time.Hour

// registerDashboardRoutes mounts the operator dashboard and its data API.
//
// The data API is guarded by the same admin token as key management: it
// exposes all traffic through the gateway, and an early version of this
// dashboard served it, plus a key-minting endpoint, to anyone who could reach
// the port.
func (h *Handler) registerDashboardRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/stats", h.dashboardStats)
	mux.HandleFunc("GET /api/usage", h.dashboardUsage)
	mux.HandleFunc("GET /api/keys", h.dashboardKeys)
	mux.HandleFunc("GET /api/models", h.dashboardModels)
	mux.HandleFunc("GET /api/catalogue", h.dashboardCatalogue)
	mux.HandleFunc("POST /api/discover", h.dashboardDiscover)
	mux.HandleFunc("POST /api/probe", h.dashboardProbe)
	mux.HandleFunc("GET /api/probes", h.dashboardProbes)
	mux.HandleFunc("GET /api/route", h.dashboardRoute)
	mux.HandleFunc("GET /api/active", h.dashboardActive)
	mux.HandleFunc("GET /api/new-models", h.dashboardNewModels)
	mux.HandleFunc("GET /api/scores", h.dashboardScores)
	h.registerPlaygroundRoutes(mux)
	mux.HandleFunc("GET /api/settings", h.dashboardGetSettings)
	mux.HandleFunc("PUT /api/settings", h.dashboardPutSettings)
}

// dashboardAuthorized applies the admin token, and says plainly when the
// dashboard has no token configured rather than failing as if unauthorized.
func (h *Handler) dashboardAuthorized(w http.ResponseWriter, r *http.Request) bool {
	return h.adminAuthorized(w, r)
}

// window reads the ?window= duration, defaulting to a day and refusing to
// aggregate over more than maxWindow.
func window(r *http.Request) time.Duration {
	raw := r.URL.Query().Get("window")
	if raw == "" {
		return 24 * time.Hour
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 24 * time.Hour
	}
	if d > maxWindow {
		return maxWindow
	}
	return d
}

func (h *Handler) dashboardStats(w http.ResponseWriter, r *http.Request) {
	if !h.dashboardAuthorized(w, r) {
		return
	}

	summary, err := db.Summarize(r.Context(), h.db, window(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "internal_error")
		return
	}

	body := map[string]any{
		"summary": summary,
		// Process-local counters, which include lookups that missed and so
		// never reached usage_logs. The persisted rate in summary is the one
		// that survives a restart; both are worth seeing.
		"cache": h.gw.CacheStats(),
	}
	if configured, err := h.gw.ConfiguredProviders(); err == nil {
		body["providers"] = configured
	}

	writeJSON(w, http.StatusOK, body)
}

func (h *Handler) dashboardUsage(w http.ResponseWriter, r *http.Request) {
	if !h.dashboardAuthorized(w, r) {
		return
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit <= 0 || limit > 500 {
		limit = 50
	}

	// One extra row reveals whether another page follows, without the client
	// having to guess from a full page or issue a separate count query.
	entries, err := db.RecentUsage(r.Context(), h.db, limit+1, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "internal_error")
		return
	}
	hasMore := len(entries) > limit
	if hasMore {
		entries = entries[:limit]
	}

	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": entries, "has_more": hasMore})
}

// dashboardKeys lists the client keys, so the dashboard can show what exists
// and offer revocation. Hashes are never returned.
func (h *Handler) dashboardKeys(w http.ResponseWriter, r *http.Request) {
	if !h.dashboardAuthorized(w, r) {
		return
	}

	keys, err := db.ListClientKeys(h.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "internal_error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": keys})
}

func (h *Handler) dashboardModels(w http.ResponseWriter, r *http.Request) {
	if !h.dashboardAuthorized(w, r) {
		return
	}

	breakdown, err := db.ModelUsage(r.Context(), h.db, window(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "internal_error")
		return
	}

	// Pair observed traffic with the catalogue, so a model whose measured
	// latency has drifted from its estimate is visible.
	catalogue := []map[string]any{}
	for _, p := range h.gw.Catalog().Profiles() {
		catalogue = append(catalogue, map[string]any{
			"model":              p.Model,
			"provider":           p.Provider,
			"estimated_ms":       p.LatencyMs,
			"observed_ms":        p.Observed,
			"input_cost_per_1m":  p.InputCostPer1M,
			"output_cost_per_1m": p.OutputCostPer1M,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"usage":     breakdown,
		"catalogue": catalogue,
	})
}

// dashboardCatalogue lists what the configured providers actually serve, so an
// operator can see which models are reachable and which of them are free.
func (h *Handler) dashboardCatalogue(w http.ResponseWriter, r *http.Request) {
	if !h.dashboardAuthorized(w, r) {
		return
	}

	catalog := h.gw.Catalog()
	pool := catalog.Discovered()
	if r.URL.Query().Get("free") == "true" {
		pool = catalog.FreeModels()
	}

	if want := r.URL.Query().Get("provider"); want != "" {
		filtered := pool[:0:0]
		for _, p := range pool {
			if p.Provider == want {
				filtered = append(filtered, p)
			}
		}
		pool = filtered
	}

	total := len(pool)

	// A provider with hundreds of models would make the page unwieldy, but
	// truncating a provider-sorted list hid whole providers below the cut:
	// with four configured, two were invisible. Taking a share from each
	// keeps every provider represented.
	const limit = 400
	truncated := false
	if len(pool) > limit {
		pool, truncated = shareAcrossProviders(pool, limit), true
	}

	catalogue := h.gw.Catalog()

	probes, err := db.ListProbes(r.Context(), h.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "internal_error")
		return
	}
	probed := make(map[string]db.ProbeResult, len(probes))
	for _, p := range probes {
		probed[db.TagKey(p.Provider, p.Model)] = p
	}

	data := make([]map[string]any, 0, len(pool))
	for _, p := range pool {
		tags := catalogue.TagsFor(p.Provider, p.Model)
		names := make([]string, 0, len(tags))
		for _, t := range tags {
			names = append(names, string(t))
		}

		entry := map[string]any{
			"model":       p.Model,
			"provider":    p.Provider,
			"free":        p.Free(),
			"tasks":       names,
			"blacklisted": catalogue.Blacklisted(p.Provider, p.Model),
		}
		if p.PriceKnown {
			entry["input_cost_per_1m"] = p.InputCostPer1M
			entry["output_cost_per_1m"] = p.OutputCostPer1M
		}
		// Absent rather than false when unprobed: the dashboard distinguishes
		// "did not work" from "nobody has checked".
		if pr, ok := probed[db.TagKey(p.Provider, p.Model)]; ok {
			entry["probe"] = pr.OK
			if pr.Failure != "" {
				entry["probe_failure"] = pr.Failure
			}
		}
		data = append(data, entry)
	}

	counts := map[string]int{}
	for task, n := range catalogue.TaggedCount() {
		counts[string(task)] = n
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"object":       "list",
		"data":         data,
		"total":        total,
		"truncated":    truncated,
		"free_only":    h.gw.FreeOnly(),
		"tagged":       counts,
		"marked_tasks": taskNames(),
	})
}

// dashboardNewModels lists models that have appeared since the window began,
// which is what an operator watching for a new release actually wants.
func (h *Handler) dashboardNewModels(w http.ResponseWriter, r *http.Request) {
	if !h.dashboardAuthorized(w, r) {
		return
	}

	added, err := db.RecentlyAddedModels(r.Context(), h.db, window(r), 100)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "internal_error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": added})
}

// dashboardScores reports what this deployment's own traffic says about each
// model. Everything here is measured; nothing is a judgement of answer
// quality, which the gateway has no way to observe.
func (h *Handler) dashboardScores(w http.ResponseWriter, r *http.Request) {
	if !h.dashboardAuthorized(w, r) {
		return
	}

	observed, err := db.ObserveModels(r.Context(), h.db, h.cfg.ObservationWindow)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "internal_error")
		return
	}

	task := routing.TaskType(r.URL.Query().Get("task"))
	if task == "" {
		task = routing.TaskConversation
	}

	catalog := h.gw.Catalog()
	byKey := map[string]routing.ModelProfile{}
	for _, p := range append(catalog.Discovered(), catalog.Profiles()...) {
		byKey[routing.ObservationKey(p.Provider, p.Model)] = p
	}

	data := make([]modelScore, 0, len(observed))
	for _, o := range observed {
		rel := routing.Reliability{
			Attempts: o.Attempts, Successes: o.Successes, MedianLatencyMs: o.MedianLatencyMs,
		}

		entry := modelScore{
			Provider:    o.Provider,
			Model:       o.Model,
			Attempts:    o.Attempts,
			Successes:   o.Successes,
			SuccessRate: rel.SuccessRate(),
			MedianMs:    o.MedianLatencyMs,
			Confidence:  rel.Confidence(),
			SuitsTask:   "unknown",
		}

		if p, ok := byKey[routing.ObservationKey(o.Provider, o.Model)]; ok {
			p.Reliability = rel
			score := routing.ScoreWithEvidence(p, task, routing.Objective(h.cfg.RoutingObjective))
			entry.Score = &score
			entry.SuitsTask = suitabilityLabel(routing.SuitabilityFor(p, task))
			if p.PriceKnown {
				cost := p.BlendedCostPer1M()
				entry.BlendedCostPer1M = &cost
			}
		}
		data = append(data, entry)
	}

	// Best first. A model with no score yet — observed but not in the
	// catalogue — sorts after the scored ones rather than at an arbitrary
	// position among them.
	sort.SliceStable(data, func(i, j int) bool {
		a, b := data[i], data[j]
		if (a.Score == nil) != (b.Score == nil) {
			return a.Score != nil
		}
		if a.Score != nil {
			return *a.Score < *b.Score
		}
		return a.Model < b.Model
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   data,
		"task":   string(task),
		"window": h.cfg.ObservationWindow.String(),
		"note": "measured from this deployment's own traffic: whether calls " +
			"succeed and how long they take. It is not a measure of answer quality, " +
			"which the gateway never observes.",
	})
}

// modelScore is one row of the scores view. Score and cost are pointers so a
// model that has one is distinguishable from one reported as zero.
type modelScore struct {
	Provider    string  `json:"provider"`
	Model       string  `json:"model"`
	Attempts    int     `json:"attempts"`
	Successes   int     `json:"successes"`
	SuccessRate float64 `json:"success_rate"`
	MedianMs    int     `json:"median_ms"`
	Confidence  float64 `json:"confidence"`
	SuitsTask   string  `json:"suits_task"`

	Score            *float64 `json:"score,omitempty"`
	BlendedCostPer1M *float64 `json:"blended_cost_per_1m,omitempty"`
}

func suitabilityLabel(s routing.Suitability) string {
	switch s {
	case routing.SuitabilityStrong:
		return "strong"
	case routing.SuitabilityPoor:
		return "poor"
	case routing.SuitabilityGeneral:
		return "general"
	default:
		return "unknown"
	}
}

// dashboardGetSettings reports the operator-changeable settings.
func (h *Handler) dashboardGetSettings(w http.ResponseWriter, r *http.Request) {
	if !h.dashboardAuthorized(w, r) {
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"free_only":         h.gw.FreeOnly(),
		"free_only_default": h.cfg.FreeOnly,
		"free_models":       len(h.gw.Catalog().FreeModels()),
	})
}

// dashboardPutSettings changes them, persisting so a restart does not quietly
// revert what an operator set.
func (h *Handler) dashboardPutSettings(w http.ResponseWriter, r *http.Request) {
	if !h.dashboardAuthorized(w, r) {
		return
	}

	var body struct {
		FreeOnly *bool `json:"free_only"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.FreeOnly == nil {
		writeError(w, http.StatusBadRequest, "free_only is required", "invalid_request_error")
		return
	}

	// Turning it on with nothing free discovered would refuse every request,
	// which looks like a broken gateway rather than a setting doing its job.
	free := len(h.gw.Catalog().FreeModels())
	if *body.FreeOnly && free == 0 {
		writeError(w, http.StatusBadRequest,
			"no free models are known, so free-only mode would refuse every request; "+
				"add a provider that publishes zero-priced models first", "invalid_request_error")
		return
	}

	if err := db.SetFreeOnlySetting(r.Context(), h.db, *body.FreeOnly); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "internal_error")
		return
	}
	h.gw.SetFreeOnly(*body.FreeOnly)

	writeJSON(w, http.StatusOK, map[string]any{
		"free_only":   h.gw.FreeOnly(),
		"free_models": free,
	})
}

// dashboardProbe runs a sweep on demand, for an operator who has just fixed a
// billing problem and does not want to wait for the next scheduled one.
func (h *Handler) dashboardProbe(w http.ResponseWriter, r *http.Request) {
	if !h.dashboardAuthorized(w, r) {
		return
	}

	summary, err := h.gw.ProbeModels(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error(), "upstream_error")
		return
	}

	writeJSON(w, http.StatusOK, summary)
}

// dashboardProbes reports what the last sweep found.
func (h *Handler) dashboardProbes(w http.ResponseWriter, r *http.Request) {
	if !h.dashboardAuthorized(w, r) {
		return
	}

	results, err := db.ListProbes(r.Context(), h.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "internal_error")
		return
	}

	working := 0
	for _, p := range results {
		if p.OK {
			working++
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"object":    "list",
		"data":      results,
		"working":   working,
		"checked":   len(results),
		"freshness": h.cfg.ProbeFreshness.String(),
		"note": "a trial completion sent on a schedule to the models routing would " +
			"reach, so a real request does not have to discover that one is unusable",
	})
}

// dashboardRoute answers "what would this model actually do", without sending
// a request. Failover is otherwise only visible after the fact in the usage
// log, which cannot show the steps that were never reached.
func (h *Handler) dashboardRoute(w http.ResponseWriter, r *http.Request) {
	if !h.dashboardAuthorized(w, r) {
		return
	}

	model := r.URL.Query().Get("model")
	if model == "" {
		model = "auto"
	}

	steps, task, reason, err := h.gw.Plan(model)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "internal_error")
		return
	}

	body := map[string]any{
		"model":          model,
		"detected_task":  task,
		"chain":          steps,
		"failover_depth": len(steps),
	}
	if reason != "" {
		body["reason"] = reason
	}
	writeJSON(w, http.StatusOK, body)
}

// dashboardActive answers "which model is this using", which the rest of the
// dashboard only answers in aggregate. Routing picks a model per request, so
// it is not something an operator can read off the configuration, and the
// usage log is written after a call finishes — the request someone opened the
// dashboard to watch is exactly the one missing from it.
//
// The chain is included alongside because "which model" has two answers worth
// having: the one that just served, and the one the next request would reach
// for.
func (h *Handler) dashboardActive(w http.ResponseWriter, r *http.Request) {
	if !h.dashboardAuthorized(w, r) {
		return
	}

	live := h.gw.Live()

	body := map[string]any{
		"active": live.Active,
		"routing": map[string]any{
			"mode":          h.cfg.RoutingMode,
			"objective":     h.cfg.RoutingObjective,
			"default_model": h.cfg.DefaultModel,
			"router_model":  h.cfg.RouterModel,
			"free_only":     h.gw.FreeOnly(),
			"choices":       routableChoices(h.cfg.DefaultModel),
		},
	}
	if live.Last != nil {
		body["last"] = live.Last
	}

	model := r.URL.Query().Get("model")
	if model == "" {
		model = h.cfg.DefaultModel
	}

	// A chain the gateway cannot compute — stored keys that will not decrypt,
	// say — is not worth failing the whole view over: what is answering now is
	// still worth showing, and the provider table says why the keys are bad.
	steps, task, reason, err := h.gw.Plan(model)
	next := map[string]any{"model": model}
	if err != nil {
		next["reason"] = err.Error()
	} else {
		next["chain"], next["detected_task"] = steps, task
		if reason != "" {
			next["reason"] = reason
		}
	}
	body["next"] = next

	writeJSON(w, http.StatusOK, body)
}

// routableChoices lists what the status view offers to plan a chain for: the
// model an unnamed request falls back to, plus every routing alias. Anything
// else a caller might name is in the catalogue, which is far too long for a
// dropdown on a status card.
func routableChoices(defaultModel string) []string {
	choices := []string{}
	seen := map[string]bool{}
	for _, name := range append([]string{defaultModel}, gateway.Sentinels()...) {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		choices = append(choices, name)
	}
	return choices
}

// shareAcrossProviders takes a roughly equal number from each provider,
// preserving the original order within each, so a cap never silently removes
// an entire provider from the view.
func shareAcrossProviders(pool []routing.ModelProfile, limit int) []routing.ModelProfile {
	byProvider := map[string][]routing.ModelProfile{}
	var order []string
	for _, p := range pool {
		if _, seen := byProvider[p.Provider]; !seen {
			order = append(order, p.Provider)
		}
		byProvider[p.Provider] = append(byProvider[p.Provider], p)
	}

	out := make([]routing.ModelProfile, 0, limit)
	for len(out) < limit {
		progressed := false
		for _, name := range order {
			remaining := byProvider[name]
			if len(remaining) == 0 {
				continue
			}
			out = append(out, remaining[0])
			byProvider[name] = remaining[1:]
			progressed = true
			if len(out) == limit {
				return out
			}
		}
		if !progressed {
			break
		}
	}
	return out
}

// dashboardDiscover re-reads every provider's model list on demand, for when
// an operator does not want to wait for the next scheduled refresh.
func (h *Handler) dashboardDiscover(w http.ResponseWriter, r *http.Request) {
	if !h.dashboardAuthorized(w, r) {
		return
	}

	if err := h.gw.RefreshModels(r.Context()); err != nil {
		writeError(w, http.StatusBadGateway, err.Error(), "upstream_error")
		return
	}

	counts := h.gw.Catalog().DiscoveredCount()
	total := 0
	for _, n := range counts {
		total += n
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"models":    total,
		"free":      len(h.gw.Catalog().FreeModels()),
		"providers": counts,
	})
}

// serveDashboard returns the embedded operator page. The mux routes everything
// unmatched here, so only the root is a page and the rest is a real 404.
func (h *Handler) serveDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writeError(w, http.StatusNotFound, "unknown endpoint: "+r.URL.Path, "invalid_request_error")
		return
	}

	page, err := ui.FS.ReadFile("index.html")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "dashboard asset missing", "internal_error")
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The page is self-contained; saying so stops a compromised dependency
	// elsewhere from being loadable into it.
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write(page)
}
