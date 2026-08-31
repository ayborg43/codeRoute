package gateway

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coderouter/coderouter/internal/cache"
	"github.com/coderouter/coderouter/internal/config"
	"github.com/coderouter/coderouter/internal/db"
	"github.com/coderouter/coderouter/internal/provider"
	"github.com/coderouter/coderouter/internal/routing"
)

// ErrNoProvider means no upstream was both configured and available.
var ErrNoProvider = errors.New("no provider available")

// cacheProvider is the name recorded in usage_logs for a served cache hit, so
// it is never mistaken for traffic that reached an upstream.
const cacheProvider = "cache"

type Gateway struct {
	db       *sql.DB
	cfg      *config.Config
	registry provider.Registry
	client   *http.Client
	catalog  *routing.Catalog
	cache    *cache.SemanticCache
	specs    []provider.Spec

	// freeOnly is togglable at runtime, so it is held here rather than read
	// from the config: an operator turning it on should not need a restart.
	freeOnlyFlag atomic.Bool

	mu       sync.Mutex
	breakers map[string]*breaker

	// benchMu guards models an account has been refused access to. The
	// circuit breaker works per provider, which is the wrong grain here: an
	// aggregator serves many models and an account is typically entitled to
	// some but not all of them. Benching the provider would throw away the
	// models that do work.
	benchMu sync.Mutex
	bench   map[string]time.Time
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

// Options are the gateway's collaborators. Only Config is required; the rest
// default to inert equivalents so tests can construct a gateway cheaply.
type Options struct {
	Config  *config.Config
	Catalog *routing.Catalog
	Cache   *cache.SemanticCache
}

func New(database *sql.DB, opts Options) (*Gateway, error) {
	if opts.Catalog == nil {
		opts.Catalog = routing.NewCatalog()
	}

	specs := opts.Config.ResolvedProviders()
	registry, err := provider.NewRegistry(specs)
	if err != nil {
		return nil, err
	}

	gw := &Gateway{
		db:       database,
		cfg:      opts.Config,
		catalog:  opts.Catalog,
		cache:    opts.Cache,
		specs:    specs,
		registry: registry,
		client:   &http.Client{Timeout: opts.Config.RequestTimeout},
		breakers: make(map[string]*breaker),
		bench:    make(map[string]time.Time),
	}
	gw.freeOnlyFlag.Store(opts.Config.FreeOnly)
	return gw, nil
}

// FreeOnly reports whether routing is currently restricted to models published
// at zero cost.
func (g *Gateway) FreeOnly() bool { return g.freeOnlyFlag.Load() }

// SetFreeOnly changes the restriction for subsequent requests.
func (g *Gateway) SetFreeOnly(on bool) {
	if g.freeOnlyFlag.Swap(on) != on {
		log.Printf("routing: free-only mode %s", map[bool]string{true: "enabled", false: "disabled"}[on])
	}
}

// Specs lists the configured upstreams, ordered for display.
func (g *Gateway) Specs() []provider.Spec {
	out := make([]provider.Spec, len(g.specs))
	copy(out, g.specs)
	provider.SortSpecs(out)
	return out
}

// Spec finds one configured upstream by name.
func (g *Gateway) Spec(name string) (provider.Spec, bool) {
	for _, s := range g.specs {
		if s.Name == name {
			return s, true
		}
	}
	return provider.Spec{}, false
}

// PlannedStep is one link in the chain a request would follow.
type PlannedStep struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Benched  bool   `json:"benched"`
	Cooldown bool   `json:"provider_in_cooldown"`
}

// Plan reports the chain a request for this model would try, in order, without
// sending anything. Routing decisions are otherwise only visible in hindsight
// through the usage log, which is a poor way to answer "what will it do".
func (g *Gateway) Plan(model string) ([]PlannedStep, string, string, error) {
	keys, err := g.ConfiguredKeys()
	if err != nil {
		return nil, "", "", err
	}

	req := &provider.ChatRequest{
		Model:    model,
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
	}
	cands, task := g.plan(req, keys)

	steps := make([]PlannedStep, 0, len(cands))
	for _, c := range cands {
		steps = append(steps, PlannedStep{
			Provider: c.provider,
			Model:    c.model,
			Benched:  g.benched(c),
			Cooldown: !g.allow(c.provider),
		})
	}

	// An empty chain is exactly when an explanation is worth most, and
	// returning a bare empty list left the operator to guess between "no keys",
	// "marks too narrow" and "everything is sidelined".
	var reason string
	if len(steps) == 0 {
		reason = g.unroutable(req, keys).Error()
	}
	return steps, string(task), reason, nil
}

// Catalog exposes the routing catalogue, for the models endpoint and pricing.
func (g *Gateway) Catalog() *routing.Catalog { return g.catalog }

// CacheStats reports semantic cache effectiveness since process start.
func (g *Gateway) CacheStats() cache.Stats { return g.cache.Stats() }

// Complete routes a non-streaming request, falling back across providers.
// key may be nil for traffic with no authenticated caller.
func (g *Gateway) Complete(ctx context.Context, req *provider.ChatRequest, key *db.ClientKey) (*provider.ChatResponse, error) {
	return g.route(ctx, req, key, nil)
}

// Stream routes a streaming request. Failover only applies before the first
// chunk reaches the client; after that the response is already committed.
func (g *Gateway) Stream(ctx context.Context, req *provider.ChatRequest, key *db.ClientKey, emit func(*provider.ChatResponse) error) error {
	_, err := g.route(ctx, req, key, emit)
	return err
}

func (g *Gateway) route(ctx context.Context, req *provider.ChatRequest, key *db.ClientKey, emit func(*provider.ChatResponse) error) (*provider.ChatResponse, error) {
	// A cache hit costs nothing upstream, so it is worth checking before the
	// provider keys are even loaded.
	if resp, ok := g.serveFromCache(ctx, req, key, emit); ok {
		return resp, nil
	}

	keys, err := db.ProviderKeys(g.db, g.cfg.EncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("failed to load provider keys: %w", err)
	}

	cands, task := g.plan(req, keys)
	if len(cands) == 0 {
		return nil, g.unroutable(req, keys)
	}

	return g.attemptChain(ctx, req, cands, keys, key, task, emit)
}

// attemptChain walks the candidates in order, stopping at the first that
// answers. A caller only sees an error once every one of them has failed.
//
// It is separate from route so the walk can be exercised against stand-in
// upstreams without a database behind it.
func (g *Gateway) attemptChain(
	ctx context.Context,
	req *provider.ChatRequest,
	cands []candidate,
	keys map[string]string,
	key *db.ClientKey,
	task routing.TaskType,
	emit func(*provider.ChatResponse) error,
) (*provider.ChatResponse, error) {
	var failures []attemptFailure
	for _, c := range cands {
		apiKey := keys[c.provider]
		if apiKey == "" {
			failures = append(failures, attemptFailure{c, "no API key configured"})
			continue
		}
		if !g.allow(c.provider) {
			failures = append(failures, attemptFailure{c, "in cooldown after repeated failures"})
			continue
		}

		attempt := *req
		attempt.Model = c.model

		// Collect the streamed text so a successful stream can be cached the
		// same way a non-streaming completion is.
		sent := false
		var streamed strings.Builder
		var wrapped func(*provider.ChatResponse) error
		if emit != nil {
			wrapped = func(chunk *provider.ChatResponse) error {
				sent = true
				for _, ch := range chunk.Choices {
					if ch.Delta != nil {
						streamed.WriteString(ch.Delta.Content)
					}
				}
				return emit(chunk)
			}
		}

		start := time.Now()
		resp, usage, err := g.attempt(ctx, c.provider, &attempt, apiKey, wrapped)
		latency := time.Since(start)

		if err != nil {
			// The circuit breaker is about provider health — timeouts, 5xx,
			// a vendor having a bad day. A model this account may not use, or
			// that cannot do chat, says nothing about the provider, and
			// counting it would take the whole provider out over a handful of
			// unusable entries in its own model list.
			if !isAccessDenied(err) {
				g.recordFailure(c.provider)
			}
			g.benchIfAccessDenied(c, err)
			g.logUsage(usageRecord{
				key: key, cand: c, latency: latency, status: "error", task: task,
				failure: err.Error(),
			})
			failures = append(failures, attemptFailure{c, err.Error()})
			// The client has already seen part of this stream; retrying on
			// another provider would splice two different completions.
			if sent {
				return nil, err
			}
			continue
		}

		g.recordSuccess(c.provider)
		g.logUsage(usageRecord{key: key, cand: c, latency: latency, usage: usage, status: "success", task: task})

		completion := streamed.String()
		if emit == nil {
			completion = completionText(resp)
		}
		g.storeInCache(ctx, req, completion)

		return resp, nil
	}

	return nil, summarizeFailures(failures)
}

// attemptFailure records one candidate that could not serve the request.
type attemptFailure struct {
	cand   candidate
	reason string
}

// summarizeFailures renders every attempt, not just the last one.
//
// Reporting only the final error made a multi-provider failure look like a
// single-provider one: a chain where the first provider had a bad key and the
// second was out of credit would report only the second, and the first problem
// stayed invisible however many times the request was retried.
func summarizeFailures(failures []attemptFailure) error {
	if len(failures) == 0 {
		return ErrNoProvider
	}
	if len(failures) == 1 {
		f := failures[0]
		return fmt.Errorf("%s (%s) failed: %s", f.cand.provider, f.cand.model, f.reason)
	}

	// A client seeing a 502 is usually told the problem is temporary. When
	// every provider refused on credentials or credit, it is not, and saying
	// so saves the user from retrying a request that cannot succeed.
	permanent := true
	for _, f := range failures {
		if !isAccessDenied(errors.New(f.reason)) {
			permanent = false
			break
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "all %d providers failed", len(failures))
	if permanent {
		b.WriteString(" (every one refused on credentials, credit or entitlement — " +
			"retrying will not help; fix the key or pick a different model)")
	}
	b.WriteString(":")
	for _, f := range failures {
		fmt.Fprintf(&b, "\n  • %s (%s): %s", f.cand.provider, f.cand.model, truncateReason(f.reason))
	}
	return errors.New(b.String())
}

// truncateReason keeps one verbose upstream body from burying the others.
func truncateReason(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	const max = 240
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
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

// Sentinels a caller can name instead of a model. They ask the gateway to
// choose, and say what for.
const (
	autoSentinel  = "auto"
	freeSentinel  = "auto:free"
	codeSentinel  = "auto:code"
	chatSentinel  = "auto:chat"
	fastSentinel  = "auto:fast"
	cheapSentinel = "auto:cheap"
)

// sentinelIntent is what a sentinel asks for: which task to rank against, and
// which axis to favour. An empty task means "read it from the prompt".
type sentinelIntent struct {
	task      routing.TaskType
	objective routing.Objective
	freeOnly  bool
	known     bool
}

// intentFor reads a sentinel. Anything else is a real model name.
func intentFor(model string) sentinelIntent {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case autoSentinel, "":
		return sentinelIntent{known: true}
	case freeSentinel:
		return sentinelIntent{freeOnly: true, known: true}
	case codeSentinel:
		// Coding wants correctness over thrift, so balanced rather than cost.
		return sentinelIntent{task: routing.TaskCodeGeneration, objective: routing.ObjectiveBalanced, known: true}
	case chatSentinel:
		return sentinelIntent{task: routing.TaskConversation, objective: routing.ObjectiveBalanced, known: true}
	case fastSentinel:
		return sentinelIntent{objective: routing.ObjectiveLatency, known: true}
	case cheapSentinel:
		return sentinelIntent{objective: routing.ObjectiveCost, known: true}
	}
	return sentinelIntent{}
}

// Sentinels lists the routing aliases, for advertising in /v1/models.
func Sentinels() []string {
	return []string{autoSentinel, freeSentinel, codeSentinel, chatSentinel, fastSentinel, cheapSentinel}
}

// plan builds the ordered provider chain for a request and reports the task
// smart routing detected for it, with anything an operator has blacklisted
// removed. The exclusion applies here rather than in each candidate-producing
// path below so it covers all of them uniformly, including a request that
// names a blacklisted model directly — a blacklist is a veto, not a ranking
// signal automatic routing alone would respect.
func (g *Gateway) plan(req *provider.ChatRequest, keys map[string]string) ([]candidate, routing.TaskType) {
	cands, task := g.planCandidates(req, keys)
	return g.excludeBlacklisted(cands), task
}

// excludeBlacklisted drops any candidate an operator has forbidden routing
// from choosing.
func (g *Gateway) excludeBlacklisted(in []candidate) []candidate {
	out := make([]candidate, 0, len(in))
	for _, c := range in {
		if !g.catalog.Blacklisted(c.provider, c.model) {
			out = append(out, c)
		}
	}
	return out
}

func (g *Gateway) planCandidates(req *provider.ChatRequest, keys map[string]string) ([]candidate, routing.TaskType) {
	task := routing.DetectTask(lastUserPrompt(req))

	if g.freeOnly(req.Model) {
		intent := intentFor(req.Model)
		if intent.task != "" {
			task = intent.task
		}

		// A named model that is already free is honoured exactly. Free-only
		// mode exists to stop spending, not to override a choice that costs
		// nothing — replacing it would be a surprise with no benefit.
		if named := g.freeNamed(req.Model, keys); len(named) > 0 {
			return named, task
		}
		return g.freeCandidates(keys, task, intent), task
	}
	if g.smartRouting(req.Model) {
		// A sentinel may name the task outright, which is more reliable than
		// reading it out of the prompt: "auto:code" is unambiguous where a
		// question about a function might not be.
		intent := intentFor(req.Model)
		if intent.task != "" {
			task = intent.task
		}

		cands := g.usable(g.rankedCandidates(task, intent), keys)
		if len(cands) == 0 {
			// The curated catalogue covers only the three vendors with
			// hand-tuned entries. A deployment keyed on any of the other
			// providers would otherwise find "auto" unroutable, which is the
			// opposite of what asking for automatic routing should do.
			cands = g.discoveredCandidates(keys, task, intent)
		}
		return cands, task
	}
	return g.namedCandidates(req.Model, keys), task
}

// usable narrows automatic candidates to those actually worth an attempt: the
// provider must hold a key, still list the model, and not have refused it
// recently.
//
// The staleness check matters because the curated catalogue is hand-written
// and vendors retire models. Without it a retired entry keeps its slot in the
// chain and spends a round trip earning a 404 on every request.
func (g *Gateway) usable(in []candidate, keys map[string]string) []candidate {
	out := make([]candidate, 0, len(in))
	for _, c := range in {
		switch {
		case keys[c.provider] == "":
		case g.benched(c):
		case !g.catalog.ServesModel(c.provider, c.model):
			log.Printf("routing: %s no longer lists %s; leaving it out of automatic routing",
				c.provider, c.model)
		default:
			out = append(out, c)
		}
	}
	return out
}

// freeOnly reports whether this request must stay on zero-priced models,
// either because the deployment is in free-only mode or because the caller
// asked for it by name.
func (g *Gateway) freeOnly(model string) bool {
	return g.FreeOnly() || intentFor(model).freeOnly
}

// freeNamed returns the caller's own model when it is free, so free-only mode
// substitutes only where the named model would actually have cost something.
func (g *Gateway) freeNamed(model string, keys map[string]string) []candidate {
	if model == "" || strings.EqualFold(strings.TrimSpace(model), freeSentinel) {
		return nil
	}

	var out []candidate
	for _, c := range g.filterToKeyed(g.placements(model), keys) {
		for _, p := range g.catalog.Resolve(c.model) {
			if p.Provider == c.provider && p.Free() {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

// freeCandidates offers only models published as costing nothing, one per
// provider so failover moves between vendors rather than retrying one.
func (g *Gateway) freeCandidates(keys map[string]string, task routing.TaskType, intent sentinelIntent) []candidate {
	pool := g.restrictToTagged(g.catalog.FreeModels(), task)
	pool = g.catalog.RankForTask(pool, task, g.objectiveFor(intent))
	return g.pickOnePerProvider(g.catalog.PreferConfirmed(pool), keys)
}

// restrictToTagged narrows a pool to the models an operator marked for the
// task, where they have marked any.
//
// This is the difference between a preference and an instruction. Inference
// orders candidates; a tag decides which candidates exist. Until at least one
// model is tagged for a task the pool is returned untouched, so tagging is
// opt-in and an untagged deployment behaves exactly as before.
func (g *Gateway) restrictToTagged(pool []routing.ModelProfile, task routing.TaskType) []routing.ModelProfile {
	tagged := g.catalog.TaggedFor(pool, task)
	if tagged == nil {
		return pool
	}
	return tagged
}

// smartRouting reports whether the router picks the model for this request.
func (g *Gateway) smartRouting(model string) bool {
	switch g.cfg.RoutingMode {
	case "always":
		return true
	case "off":
		return false
	default:
		// "auto": only when the caller declines to choose, either by sending
		// no model or by naming one of the routing sentinels.
		return model == "" || strings.EqualFold(model, g.cfg.RouterModel) ||
			intentFor(model).known
	}
}

// rankedCandidates takes each provider's best model for the task, so failover
// moves to a different provider instead of retrying one that just failed.
func (g *Gateway) rankedCandidates(task routing.TaskType, intent sentinelIntent) []candidate {
	var out []candidate
	seen := make(map[string]bool)

	ranked := g.catalog.Rank(task, g.objectiveFor(intent))
	if tagged := g.catalog.TaggedFor(ranked, task); tagged != nil {
		ranked = tagged
	}

	for _, p := range ranked {
		if seen[p.Provider] {
			continue
		}
		seen[p.Provider] = true
		out = append(out, candidate{provider: p.Provider, model: p.Model})
	}

	return out
}

// discoveredCandidates falls back to what the providers themselves report,
// one model per provider so failover still moves between vendors.
//
// Discovered models carry no task suitability and no measured latency, so the
// only axis available is price: cheapest first, with unpriced models last
// because an unknown price is more likely to be a surprise than a bargain.
func (g *Gateway) discoveredCandidates(keys map[string]string, task routing.TaskType, intent sentinelIntent) []candidate {
	pool := g.restrictToTagged(g.catalog.Discovered(), task)

	// Rank on everything known — measured reliability and latency, published
	// price, and inferred fitness for the task — then break remaining ties in
	// favour of general-purpose models over specialised endpoints.
	pool = g.catalog.RankForTask(pool, task, g.objectiveFor(intent))

	sort.SliceStable(pool, func(i, j int) bool {
		sa, sb := specialised(pool[i].Model), specialised(pool[j].Model)
		return !sa && sb
	})

	// Models a probe has confirmed working move to the front. Applied last so
	// it outranks price and inference: knowing a model answers for this
	// account is worth more than knowing it is cheap.
	pool = g.catalog.PreferConfirmed(pool)

	return g.pickOnePerProvider(pool, keys)
}

// objectiveFor lets a sentinel override the deployment's default axis.
func (g *Gateway) objectiveFor(intent sentinelIntent) routing.Objective {
	if intent.objective != "" {
		return intent.objective
	}
	return routing.Objective(g.cfg.RoutingObjective)
}

// pickOnePerProvider builds the automatic chain from an ordered pool.
//
// Each provider contributes up to AttemptsPerProvider models, interleaved so
// the chain alternates vendors before returning to one: a provider that is
// entirely down should not consume two slots before another is tried. The
// whole chain is capped by MaxAttempts so a deployment with many providers
// cannot turn one request into dozens of upstream calls.
//
// Benched models are stepped over here rather than being left in the chain to
// be skipped later: skipping at that point would cost the provider its slot
// entirely, so an account whose cheapest model was out of allowance would stop
// using that provider instead of moving to its next one.
func (g *Gateway) pickOnePerProvider(pool []routing.ModelProfile, keys map[string]string) []candidate {
	perProvider := map[string][]candidate{}
	var order []string

	depth := g.cfg.AttemptsPerProvider
	if depth < 1 {
		depth = 1
	}

	for _, p := range pool {
		c := candidate{provider: p.Provider, model: p.Model}
		if keys[p.Provider] == "" || g.benched(c) {
			continue
		}
		if _, seen := perProvider[p.Provider]; !seen {
			order = append(order, p.Provider)
		}
		if len(perProvider[p.Provider]) < depth {
			perProvider[p.Provider] = append(perProvider[p.Provider], c)
		}
	}

	// The cap must never stop a provider being tried once. With more providers
	// than the cap, truncating the first round silently excluded the ones at
	// the end of it — a deployment with eleven providers and a cap of eight
	// would never reach three of them, however healthy they were. The cap is
	// really about how many *retries* to spend, so it applies from the second
	// round on.
	limit := g.cfg.MaxAttempts
	if limit < len(order) {
		limit = len(order)
	}
	if limit < 1 {
		limit = 1
	}

	var out []candidate
	for round := 0; round < depth && len(out) < limit; round++ {
		for _, name := range order {
			if round >= len(perProvider[name]) {
				continue
			}
			out = append(out, perProvider[name][round])
			if len(out) == limit {
				return out
			}
		}
	}
	return out
}

// specialisationMarkers appear in the names of models built for something
// other than general chat. This is a hint for ordering, never a filter: a
// caller naming such a model explicitly still gets it.
var specialisationMarkers = []string{
	"tts", "text-to-speech", "speech", "audio", "voice",
	"image", "vision-only", "video", "embed", "embedding", "rerank",
	"deep-research", "computer-use", "guard", "moderation",
	"transcribe", "whisper", "ocr", "live", "realtime", "interactions",
}

// specialised reports whether a model name suggests a non-chat purpose.
func specialised(model string) bool {
	m := strings.ToLower(model)
	for _, marker := range specialisationMarkers {
		if strings.Contains(m, marker) {
			return true
		}
	}
	return false
}

// namedCandidates honours the model the caller asked for, then the configured
// fallbacks. A model may be served by several providers, so this resolves
// through the catalogue rather than guessing from the model's name.
//
// The caller's own model is only filtered by whether its provider has a key.
// Naming a model is an instruction, not a suggestion: if it is retired or the
// account cannot use it, the right answer is the provider saying so, not the
// gateway quietly substituting something else. The fallbacks after it are the
// gateway's own choice, so they are screened the same way automatic routing
// screens its candidates.
func (g *Gateway) namedCandidates(model string, keys map[string]string) []candidate {
	if model == "" {
		model = g.cfg.DefaultModel
	}

	seen := make(map[string]bool)
	collect := func(in []candidate) []candidate {
		out := make([]candidate, 0, len(in))
		for _, c := range in {
			k := c.provider + "\x00" + c.model
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, c)
		}
		return out
	}

	out := collect(g.filterToKeyed(g.placements(model), keys))

	var fallbacks []candidate
	for _, fm := range g.cfg.FallbackModels {
		fallbacks = append(fallbacks, g.placements(fm)...)
	}
	return append(out, collect(g.usable(fallbacks, keys))...)
}

// placements lists where one named model can be served.
//
// A caller may pin a provider with "provider:model" — the prefix has to match
// a configured provider name, which keeps it from colliding with model ids
// that legitimately contain a colon, such as OpenRouter's ":free" suffix.
func (g *Gateway) placements(model string) []candidate {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil
	}

	if prefix, rest, found := strings.Cut(model, ":"); found {
		if _, known := g.registry[prefix]; known && rest != "" {
			return []candidate{{provider: prefix, model: rest}}
		}
	}

	var out []candidate
	for _, p := range g.catalog.Resolve(model) {
		out = append(out, candidate{provider: p.Provider, model: p.Model})
	}
	if len(out) > 0 {
		return out
	}

	// Nothing in the catalogue knows it. Before discovery has run, or for a
	// model released since, fall back to the naming conventions of the three
	// providers that have them.
	if name := nativeProviderFor(model); name != "" {
		return []candidate{{provider: name, model: model}}
	}
	return nil
}

// nativeProviderFor maps a model name to a provider by naming convention. It
// covers only the vendors whose model names are unambiguous; every other
// provider serves open-weight models under names its competitors also use, so
// guessing from the name there would route to the wrong place.
func nativeProviderFor(model string) string {
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

// filterToKeyed drops candidates whose provider has no stored key, so failover
// spends its attempts on upstreams that can actually answer.
func (g *Gateway) filterToKeyed(in []candidate, keys map[string]string) []candidate {
	out := make([]candidate, 0, len(in))
	for _, c := range in {
		if keys[c.provider] != "" {
			out = append(out, c)
		}
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

// usageRecord is one row destined for usage_logs, gathered in one place so the
// call sites do not each have to remember the column order.
type usageRecord struct {
	key      *db.ClientKey
	cand     candidate
	latency  time.Duration
	usage    provider.Usage
	status   string
	task     routing.TaskType
	cacheHit bool

	// failure is the upstream's own words, kept so the dashboard can say why
	// a request failed instead of only that it did.
	failure string
}

func (g *Gateway) logUsage(rec usageRecord) {
	if g.db == nil {
		return
	}

	var apiKeyID any
	if rec.key != nil {
		apiKeyID = rec.key.ID
	}

	// A model absent from the catalogue has no published price; cost stays
	// NULL rather than being invented.
	var cost any
	if p, ok := g.catalog.Lookup(rec.cand.model); ok {
		cost = p.CostUSD(rec.usage.PromptTokens, rec.usage.CompletionTokens)
	}

	var failure any
	if rec.failure != "" {
		failure = truncateReason(rec.failure)
	}

	_, err := g.db.Exec(
		`INSERT INTO usage_logs (api_key_id, model, tokens_in, tokens_out, latency_ms, cost_usd, provider, status, task, cache_hit, failure)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		apiKeyID, rec.cand.model, rec.usage.PromptTokens, rec.usage.CompletionTokens,
		rec.latency.Milliseconds(), cost, rec.cand.provider, rec.status, string(rec.task),
		rec.cacheHit, failure,
	)
	if err != nil {
		// Usage logging must never fail a request that already succeeded.
		log.Printf("usage log failed: %v", err)
	}
}

// RefreshObservations folds what this deployment's own traffic says about each
// model back into the catalogue: whether it works, and how quickly.
//
// This is the whole of the gateway's "learning". It can tell a model that
// fails from one that does not, and a slow one from a fast one. It cannot tell
// a good answer from a bad one, because it never sees whether an answer was
// any good — so nothing here claims to rank models by quality.
func (g *Gateway) RefreshObservations(ctx context.Context) error {
	observed, err := db.ObserveModels(ctx, g.db, g.cfg.ObservationWindow)
	if err != nil {
		return err
	}

	measured := make(map[string]routing.Reliability, len(observed))
	for _, o := range observed {
		measured[routing.ObservationKey(o.Provider, o.Model)] = routing.Reliability{
			Attempts:        o.Attempts,
			Successes:       o.Successes,
			MedianLatencyMs: o.MedianLatencyMs,
		}
	}
	g.catalog.ObserveReliability(measured)

	return nil
}

// StartLatencyFeedback keeps the catalogue's view of each model current until
// ctx is cancelled. A zero or negative interval disables the loop, leaving
// routing to use published figures alone.
func (g *Gateway) StartLatencyFeedback(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		log.Print("observation feedback disabled; routing will use published figures only")
		return
	}

	go func() {
		if err := g.RefreshObservations(ctx); err != nil && ctx.Err() == nil {
			log.Printf("initial observation refresh failed: %v", err)
		}

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := g.RefreshObservations(ctx); err != nil && ctx.Err() == nil {
					log.Printf("observation refresh failed: %v", err)
				}
			}
		}
	}()
}

// ProviderNames lists the upstreams this build knows how to talk to, in a
// stable order, so the admin API can reject a name it could never route to.
func (g *Gateway) ProviderNames() []string {
	names := make([]string, 0, len(g.registry))
	for name := range g.registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ConfiguredKeys returns the decrypted upstream keys, by provider.
//
// A gateway with no database has no stored keys rather than being an error:
// that is the shape used by tests, and a nil dereference is a poor way to say
// "nothing is configured".
func (g *Gateway) ConfiguredKeys() (map[string]string, error) {
	if g.db == nil {
		return map[string]string{}, nil
	}
	return db.ProviderKeys(g.db, g.cfg.EncryptionKey)
}

// KnowsProvider reports whether a name maps to a real upstream client.
func (g *Gateway) KnowsProvider(name string) bool {
	_, ok := g.registry[name]
	return ok
}

// VerifyProviderKey checks a key against its upstream before it is stored, so
// a typo is caught at the point of entry rather than by the next completion.
//
// Verification is a model listing, which doubles as discovery: the models come
// back so the caller can record them without asking the provider twice.
func (g *Gateway) VerifyProviderKey(ctx context.Context, name, apiKey string) ([]provider.DiscoveredModel, error) {
	spec, ok := g.Spec(name)
	if !ok {
		return nil, fmt.Errorf("unknown provider %q", name)
	}

	models, err := provider.ListModels(ctx, g.client, spec, apiKey)
	if err != nil {
		return nil, err
	}
	return models, nil
}

// AdoptModels records a model list obtained during key verification, so a
// freshly saved provider is routable without a second round trip.
func (g *Gateway) AdoptModels(ctx context.Context, name string, models []provider.DiscoveredModel) {
	if len(models) == 0 {
		return
	}
	g.catalog.SetDiscovered(name, models)

	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if _, err := db.ReplaceDiscoveredModels(saveCtx, g.db, name, models); err != nil {
		log.Printf("discovery: could not cache %s models: %v", name, err)
	}
}

// unroutable explains why a request found no candidate. With a dozen possible
// providers the bare "no provider available" was uninformative: the useful
// distinction is between having no keys at all, asking for free models when
// none are known, and naming a model nothing serves.
func (g *Gateway) unroutable(req *provider.ChatRequest, keys map[string]string) error {
	var keyed []string
	for name := range g.registry {
		if keys[name] != "" {
			keyed = append(keyed, name)
		}
	}
	sort.Strings(keyed)

	if len(keyed) == 0 {
		return fmt.Errorf("%w: no provider keys are stored; add one from the dashboard", ErrNoProvider)
	}

	if g.freeOnly(req.Model) {
		return fmt.Errorf("%w: no free models are known for %s. Free routing only uses models "+
			"whose provider publishes a zero price, and none of the configured providers has "+
			"reported one yet", ErrNoProvider, strings.Join(keyed, ", "))
	}

	if g.smartRouting(req.Model) {
		task := routing.DetectTask(lastUserPrompt(req))
		if intent := intentFor(req.Model); intent.task != "" {
			task = intent.task
		}

		// A tag list that has gone stale is a likely cause and an easy fix,
		// so it is worth naming rather than leaving the operator to guess.
		if g.catalog.HasTagsFor(task) {
			return fmt.Errorf("%w: every model marked for %s is unavailable — "+
				"its provider has no key, it has been withdrawn, or it recently refused "+
				"access. Providers with keys: %s. Mark another model for %s, or clear the "+
				"marks to let routing choose",
				ErrNoProvider, task, strings.Join(keyed, ", "), task)
		}

		return fmt.Errorf("%w: automatic routing found nothing to use. Providers with keys: %s, "+
			"but none has reported a model yet — try again once discovery has run, "+
			"or name a model directly", ErrNoProvider, strings.Join(keyed, ", "))
	}

	model := req.Model
	if model == "" {
		model = g.cfg.DefaultModel
	}
	return fmt.Errorf("%w: nothing serves model %q. Providers with keys: %s. "+
		"Pin one with \"provider:model\" if the model list is not yet discovered",
		ErrNoProvider, model, strings.Join(keyed, ", "))
}

// benchDuration is how long a model stays out of automatic selection after the
// account was refused access to it. Long enough that a retry loop does not
// keep picking it, short enough that upgrading a plan takes effect the same
// session.
const benchDuration = time.Hour

// benchKey identifies one model at one provider.
func benchKey(c candidate) string { return c.provider + "/" + c.model }

// benched reports whether a model is currently sidelined. Only automatic
// selection consults this: a caller naming a model explicitly gets to try it
// and see the provider's own error.
func (g *Gateway) benched(c candidate) bool {
	g.benchMu.Lock()
	defer g.benchMu.Unlock()

	until, ok := g.bench[benchKey(c)]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(g.bench, benchKey(c))
		return false
	}
	return true
}

// benchIfAccessDenied sidelines a model this account cannot currently use.
//
// A 401, 403 or 404 from one model is a statement about entitlement, and an
// exhausted balance or daily allowance is a statement about budget. Neither is
// fixed by an immediate retry: it costs a round trip on every request and
// buries the providers that would have worked. Ordinary rate limiting is
// excluded, because that genuinely does pass in seconds.
func (g *Gateway) benchIfAccessDenied(c candidate, err error) {
	if err == nil || !isAccessDenied(err) {
		return
	}

	g.benchOne(c)

	// A free allowance is a pool, not a per-model budget. When a provider says
	// the free quota is spent, every free model it serves is spent with it —
	// benching them one failed request at a time would walk the whole free
	// list before reaching a paid model that would have worked.
	//
	// This applies only to a spent allowance. A plain entitlement refusal is
	// a statement about one model, and says nothing about the others.
	if isAllowanceSpent(err) && g.isFreeAt(c) {
		var spent int
		for _, p := range g.catalog.FreeModels() {
			if p.Provider != c.provider || p.Model == c.model {
				continue
			}
			g.benchOne(candidate{provider: p.Provider, model: p.Model})
			spent++
		}
		if spent > 0 {
			log.Printf("routing: %s reports its free allowance spent; setting aside %d "+
				"further free models there for %s", c.provider, spent, benchDuration)
		}
	}
}

// benchOne sidelines a single model.
func (g *Gateway) benchOne(c candidate) {
	g.benchMu.Lock()
	defer g.benchMu.Unlock()

	if _, already := g.bench[benchKey(c)]; !already {
		log.Printf("routing: %s will not serve %s to this account; skipping it for %s",
			c.provider, c.model, benchDuration)
	}
	g.bench[benchKey(c)] = time.Now().Add(benchDuration)
}

// isFreeAt reports whether this specific provider serves this model free.
func (g *Gateway) isFreeAt(c candidate) bool {
	for _, p := range g.catalog.Resolve(c.model) {
		if p.Provider == c.provider {
			return p.Free()
		}
	}
	return false
}

// exhaustionMarkers are the words providers use when an account is out of
// money or out of allowance, as opposed to merely going too fast. The
// distinction matters: the first is not fixed by waiting a few seconds, and
// several providers report both with the same status code.
var exhaustionMarkers = []string{
	"insufficient", "balance", "deposit", "credit", "billing",
	"quota", "余额不足", "payment",
}

// isAccessDenied recognises an upstream refusal that an immediate retry will
// not fix — a wrong entitlement, an empty wallet, a spent allowance, or a
// model that cannot do this kind of work at all.
func isAccessDenied(err error) bool {
	msg := strings.ToLower(err.Error())

	for _, marker := range []string{"returned 401", "returned 403", "returned 404"} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return isAllowanceSpent(err) || isWrongKindOfModel(err)
}

// isWrongKindOfModel catches a provider saying the model does not do chat.
//
// Discovery lists whatever the provider serves, and not every entry is a chat
// model: embedding, image, audio and speech endpoints appear in the same list.
// Where a provider declares capabilities they are filtered out beforehand;
// this is the net for the ones that only say so when asked.
//
// The wording is required rather than the status code, because a 400 is
// usually the caller's own malformed request, and benching a good model for
// that would be worse than the round trip it saves.
func isWrongKindOfModel(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"only supports", "does not support", "not supported for",
		"is not a chat model", "unsupported operation",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// isAllowanceSpent recognises a provider saying the account is out of money or
// out of allowance, as distinct from being refused one particular model. Only
// this justifies setting aside a whole pool of models at once.
//
// The markers are required, so a plain "rate limit reached, try again in 20s"
// stays retryable.
func isAllowanceSpent(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, marker := range exhaustionMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// ClearBench forgets every sidelined model, for when a key changes and the
// account's entitlements may have changed with it.
func (g *Gateway) ClearBench() {
	g.benchMu.Lock()
	defer g.benchMu.Unlock()
	g.bench = make(map[string]time.Time)
}

// LoadModelTags reads the operator's model markings into the catalogue.
func (g *Gateway) LoadModelTags(ctx context.Context) error {
	stored, err := db.ListModelTags(ctx, g.db)
	if err != nil {
		return err
	}

	tags := make(map[string][]routing.TaskType, len(stored))
	for key, tasks := range stored {
		converted := make([]routing.TaskType, 0, len(tasks))
		for _, t := range tasks {
			converted = append(converted, routing.TaskType(t))
		}
		tags[key] = converted
	}
	g.catalog.SetTags(tags)

	if counts := g.catalog.TaggedCount(); len(counts) > 0 {
		for task, n := range counts {
			log.Printf("routing: %d model(s) marked for %s; only those will serve it", n, task)
		}
	}
	return nil
}

// LoadBlacklist reads the operator's routing exclusions into the catalogue.
func (g *Gateway) LoadBlacklist(ctx context.Context) error {
	stored, err := db.ListBlacklistedModels(ctx, g.db)
	if err != nil {
		return err
	}

	blacklist := make(map[string]bool, len(stored))
	for _, m := range stored {
		blacklist[routing.TagKey(m.Provider, m.Model)] = true
	}
	g.catalog.SetBlacklist(blacklist)

	if n := g.catalog.BlacklistCount(); n > 0 {
		log.Printf("routing: %d model(s) blacklisted; routing will never choose them", n)
	}
	return nil
}
