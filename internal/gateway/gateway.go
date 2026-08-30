package gateway

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coderouter/coderouter/internal/config"
	"github.com/coderouter/coderouter/internal/db"
	"github.com/coderouter/coderouter/internal/provider"
	"github.com/coderouter/coderouter/internal/routing"
)

// ErrNoProvider means no upstream was both configured and available.
var ErrNoProvider = errors.New("no provider available")

type Gateway struct {
	db       *sql.DB
	cfg      *config.Config
	registry provider.Registry
	client   *http.Client

	mu       sync.Mutex
	breakers map[string]*breaker
}

// breaker trips a provider out of rotation after repeated failures and lets a
// single request probe it again once the cooldown expires.
type breaker struct {
	failures  int
	openUntil time.Time
}

type candidate struct {
	provider string
	model    string
}

func New(database *sql.DB, cfg *config.Config) *Gateway {
	return &Gateway{
		db:       database,
		cfg:      cfg,
		registry: provider.NewRegistry(cfg.ProviderBaseURLs),
		client:   &http.Client{Timeout: cfg.RequestTimeout},
		breakers: make(map[string]*breaker),
	}
}

// Complete routes a non-streaming request, falling back across providers.
func (g *Gateway) Complete(ctx context.Context, req *provider.ChatRequest, keyID string) (*provider.ChatResponse, error) {
	return g.route(ctx, req, keyID, nil)
}

// Stream routes a streaming request. Failover only applies before the first
// chunk reaches the client; after that the response is already committed.
func (g *Gateway) Stream(ctx context.Context, req *provider.ChatRequest, keyID string, emit func(*provider.ChatResponse) error) error {
	_, err := g.route(ctx, req, keyID, emit)
	return err
}

func (g *Gateway) route(ctx context.Context, req *provider.ChatRequest, keyID string, emit func(*provider.ChatResponse) error) (*provider.ChatResponse, error) {
	keys, err := db.ProviderKeys(g.db, g.cfg.EncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("failed to load provider keys: %w", err)
	}

	cands, task := g.plan(req)
	if len(cands) == 0 {
		return nil, fmt.Errorf("%w: model %q maps to no known provider", ErrNoProvider, req.Model)
	}

	var lastErr error
	for _, c := range cands {
		apiKey := keys[c.provider]
		if apiKey == "" {
			lastErr = fmt.Errorf("no API key configured for %s", c.provider)
			continue
		}
		if !g.allow(c.provider) {
			lastErr = fmt.Errorf("%s is in cooldown after repeated failures", c.provider)
			continue
		}

		attempt := *req
		attempt.Model = c.model

		sent := false
		var wrapped func(*provider.ChatResponse) error
		if emit != nil {
			wrapped = func(chunk *provider.ChatResponse) error {
				sent = true
				return emit(chunk)
			}
		}

		start := time.Now()
		resp, usage, err := g.attempt(ctx, c.provider, &attempt, apiKey, wrapped)
		latency := time.Since(start)

		if err != nil {
			g.recordFailure(c.provider)
			g.logUsage(keyID, c, latency, provider.Usage{}, "error", task)
			lastErr = err
			// The client has already seen part of this stream; retrying on
			// another provider would splice two different completions.
			if sent {
				return nil, err
			}
			continue
		}

		g.recordSuccess(c.provider)
		g.logUsage(keyID, c, latency, usage, "success", task)
		return resp, nil
	}

	if lastErr == nil {
		lastErr = ErrNoProvider
	}
	return nil, fmt.Errorf("all providers failed: %w", lastErr)
}

func (g *Gateway) attempt(ctx context.Context, name string, req *provider.ChatRequest, apiKey string, emit func(*provider.ChatResponse) error) (*provider.ChatResponse, provider.Usage, error) {
	client, ok := g.registry[name]
	if !ok {
		return nil, provider.Usage{}, fmt.Errorf("unknown provider %q", name)
	}

	streaming := emit != nil

	hreq, err := client.BuildRequest(ctx, req, apiKey, streaming)
	if err != nil {
		return nil, provider.Usage{}, err
	}

	resp, err := g.client.Do(hreq)
	if err != nil {
		return nil, provider.Usage{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, provider.Usage{}, provider.HTTPError(name, resp.StatusCode, body)
	}

	if streaming {
		usage, err := client.DecodeStream(resp.Body, req.Model, emit)
		return nil, usage, err
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, provider.Usage{}, err
	}

	decoded, err := client.DecodeResponse(body, req.Model)
	if err != nil {
		return nil, provider.Usage{}, err
	}

	var usage provider.Usage
	if decoded.Usage != nil {
		usage = *decoded.Usage
	}
	return decoded, usage, nil
}

// plan builds the ordered provider chain for a request and reports the task
// smart routing detected for it.
func (g *Gateway) plan(req *provider.ChatRequest) ([]candidate, routing.TaskType) {
	task := routing.DetectTask(lastUserPrompt(req))

	if g.smartRouting(req.Model) {
		return g.rankedCandidates(task), task
	}
	return g.staticCandidates(req.Model), task
}

// smartRouting reports whether the router picks the model for this request.
func (g *Gateway) smartRouting(model string) bool {
	switch g.cfg.RoutingMode {
	case "always":
		return true
	case "off":
		return false
	default:
		// "auto": only when the caller declines to choose.
		return model == "" || strings.EqualFold(model, g.cfg.RouterModel)
	}
}

// rankedCandidates takes each provider's best model for the task, so failover
// moves to a different provider instead of retrying one that just failed.
func (g *Gateway) rankedCandidates(task routing.TaskType) []candidate {
	var out []candidate
	seen := make(map[string]bool)

	for _, p := range routing.Rank(task, routing.Objective(g.cfg.RoutingObjective)) {
		if seen[p.Provider] {
			continue
		}
		seen[p.Provider] = true
		out = append(out, candidate{provider: p.Provider, model: p.Model})
	}

	return out
}

// staticCandidates honours the caller's model, then the configured fallbacks.
func (g *Gateway) staticCandidates(model string) []candidate {
	if model == "" {
		model = g.cfg.DefaultModel
	}

	var out []candidate
	seen := make(map[string]bool)

	add := func(m string) {
		name := provider.ProviderFor(m)
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, candidate{provider: name, model: m})
	}

	add(model)
	for _, fm := range g.cfg.FallbackModels {
		add(fm)
	}

	return out
}

// lastUserPrompt returns the text task detection reads: the most recent user
// turn, which is the actual ask.
func lastUserPrompt(req *provider.ChatRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			return string(req.Messages[i].Content)
		}
	}
	if len(req.Messages) > 0 {
		return string(req.Messages[len(req.Messages)-1].Content)
	}
	return ""
}

// ConfiguredProviders reports which providers currently have a stored key.
func (g *Gateway) ConfiguredProviders() (map[string]bool, error) {
	keys, err := db.ProviderKeys(g.db, g.cfg.EncryptionKey)
	if err != nil {
		return nil, err
	}

	out := make(map[string]bool, len(g.registry))
	for name := range g.registry {
		out[name] = keys[name] != ""
	}
	return out, nil
}

func (g *Gateway) allow(name string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	b, ok := g.breakers[name]
	if !ok {
		return true
	}
	return time.Now().After(b.openUntil)
}

func (g *Gateway) recordFailure(name string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	b, ok := g.breakers[name]
	if !ok {
		b = &breaker{}
		g.breakers[name] = b
	}

	b.failures++
	if b.failures >= g.cfg.BreakerTrip {
		b.openUntil = time.Now().Add(g.cfg.BreakerCooldown)
		// Reset so the post-cooldown probe gets a full budget rather than
		// tripping again on its first error.
		b.failures = 0
	}
}

func (g *Gateway) recordSuccess(name string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if b, ok := g.breakers[name]; ok {
		b.failures = 0
		b.openUntil = time.Time{}
	}
}

func (g *Gateway) logUsage(keyID string, c candidate, latency time.Duration, usage provider.Usage, status string, task routing.TaskType) {
	var apiKeyID any
	if keyID != "" {
		apiKeyID = keyID
	}

	_, err := g.db.Exec(
		`INSERT INTO usage_logs (api_key_id, model, tokens_in, tokens_out, latency_ms, provider, status, task)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		apiKeyID, c.model, usage.PromptTokens, usage.CompletionTokens,
		latency.Milliseconds(), c.provider, status, string(task),
	)
	if err != nil {
		// Usage logging must never fail a request that already succeeded.
		log.Printf("usage log failed: %v", err)
	}
}
