package gateway

import (
	"context"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/coderouter/coderouter/internal/db"
	"github.com/coderouter/coderouter/internal/provider"
	"github.com/coderouter/coderouter/internal/routing"
)

// probePrompt is the smallest useful request: one token in, one token out.
// The point is to learn whether the account may use this model at all, which
// the provider decides before it generates anything.
var probeMessages = []provider.Message{{Role: "user", Content: "hi"}}

// probeTimeout keeps one unresponsive model from holding up a sweep.
const probeTimeout = 30 * time.Second

// ProbeSummary is what one sweep established.
type ProbeSummary struct {
	Checked    int            `json:"checked"`
	Working    int            `json:"working"`
	Failed     int            `json:"failed"`
	ByProvider map[string]int `json:"working_by_provider"`
	Duration   string         `json:"duration"`
}

// ProbeModels sends a trial completion to a bounded sample of models and
// records which ones this account may actually use.
//
// It deliberately does not probe everything. A dozen providers serving several
// hundred models each would cost real money and burn the free allowances the
// probing is meant to protect. It takes the models routing would actually
// reach — the head of each provider's ranked list — because those are the only
// ones whose health changes what happens to a request.
func (g *Gateway) ProbeModels(ctx context.Context) (ProbeSummary, error) {
	started := time.Now()
	summary := ProbeSummary{ByProvider: map[string]int{}}

	keys, err := g.ConfiguredKeys()
	if err != nil {
		return summary, err
	}

	targets := g.probeTargets(keys)
	if len(targets) == 0 {
		return summary, nil
	}

	var mu sync.Mutex
	var wg sync.WaitGroup

	// Providers are probed concurrently but each provider serially, so a sweep
	// does not look like a burst of traffic to any one vendor's rate limiter.
	byProvider := map[string][]candidate{}
	for _, c := range targets {
		byProvider[c.provider] = append(byProvider[c.provider], c)
	}

	for name, cands := range byProvider {
		wg.Add(1)
		go func(name string, cands []candidate) {
			defer wg.Done()

			for _, c := range cands {
				if ctx.Err() != nil {
					return
				}
				result := g.probeOne(ctx, c, keys[c.provider])

				mu.Lock()
				summary.Checked++
				if result.OK {
					summary.Working++
					summary.ByProvider[c.provider]++
				} else {
					summary.Failed++
				}
				mu.Unlock()

				saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
				if err := db.RecordProbe(saveCtx, g.db, result); err != nil {
					log.Printf("probe: could not record %s/%s: %v", c.provider, c.model, err)
				}
				cancel()
			}
		}(name, cands)
	}
	wg.Wait()

	if err := g.LoadConfirmedModels(ctx); err != nil {
		log.Printf("probe: could not reload confirmed models: %v", err)
	}

	summary.Duration = time.Since(started).Round(time.Millisecond).String()
	log.Printf("probe: checked %d model(s) — %d working, %d refused (%s)",
		summary.Checked, summary.Working, summary.Failed, summary.Duration)
	return summary, nil
}

// probeOne tries a single model and reports what happened.
func (g *Gateway) probeOne(ctx context.Context, c candidate, apiKey string) db.ProbeResult {
	result := db.ProbeResult{Provider: c.provider, Model: c.model}
	if apiKey == "" {
		result.Failure = "no API key configured"
		return result
	}

	attemptCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	req := &provider.ChatRequest{
		Model:     c.model,
		Messages:  probeMessages,
		MaxTokens: 1,
	}

	start := time.Now()
	_, _, err := g.attempt(attemptCtx, c.provider, req, apiKey, nil)
	result.LatencyMs = int(time.Since(start).Milliseconds())

	if err != nil {
		result.Failure = truncateReason(err.Error())
		// A probe is real evidence, so it feeds the same bench that real
		// traffic does — no reason to make a request rediscover this.
		g.benchIfAccessDenied(c, err)
		return result
	}

	result.OK = true
	// A model that has just answered should not stay sidelined from an
	// earlier failure: entitlements and balances change.
	g.unbench(c)
	return result
}

// probeTargets picks which models are worth checking.
func (g *Gateway) probeTargets(keys map[string]string) []candidate {
	perProvider := g.cfg.ProbeModelsPerProvider
	if perProvider < 1 {
		perProvider = 1
	}

	pool := g.catalog.Discovered()

	// General-purpose models first, then cheapest: the same order routing uses,
	// so what gets probed is what would get used.
	pool = g.catalog.RankForTask(pool, routing.TaskConversation, routing.Objective(g.cfg.RoutingObjective))
	sort.SliceStable(pool, func(i, j int) bool {
		return !specialised(pool[i].Model) && specialised(pool[j].Model)
	})

	counts := map[string]int{}
	var out []candidate
	for _, p := range pool {
		if keys[p.Provider] == "" || counts[p.Provider] >= perProvider {
			continue
		}
		counts[p.Provider]++
		out = append(out, candidate{provider: p.Provider, model: p.Model})
	}

	// Anything an operator marked is probed too, whatever its ranking: those
	// are the models routing is restricted to, so their health matters most.
	for _, p := range g.catalog.Discovered() {
		if keys[p.Provider] == "" || len(g.catalog.TagsFor(p.Provider, p.Model)) == 0 {
			continue
		}
		c := candidate{provider: p.Provider, model: p.Model}
		if !containsCandidate(out, c) {
			out = append(out, c)
		}
	}
	return out
}

func containsCandidate(in []candidate, want candidate) bool {
	for _, c := range in {
		if c == want {
			return true
		}
	}
	return false
}

// unbench clears a sidelining, for a model that has just been shown to work.
func (g *Gateway) unbench(c candidate) {
	g.benchMu.Lock()
	defer g.benchMu.Unlock()
	delete(g.bench, benchKey(c))
}

// LoadConfirmedModels reads the probe results into the catalogue.
func (g *Gateway) LoadConfirmedModels(ctx context.Context) error {
	confirmed, err := db.ConfirmedModels(ctx, g.db, g.cfg.ProbeFreshness)
	if err != nil {
		return err
	}
	g.catalog.SetConfirmed(confirmed)
	return nil
}

// StartProbing sweeps on an interval until ctx is done. A zero or negative
// interval disables it, leaving routing to learn from real traffic alone.
func (g *Gateway) StartProbing(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		log.Print("probing disabled; routing will learn which models work from real traffic")
		return
	}

	go func() {
		// A first sweep shortly after startup, once discovery has had a chance
		// to populate the catalogue — probing an empty catalogue does nothing.
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
		if _, err := g.ProbeModels(ctx); err != nil && ctx.Err() == nil {
			log.Printf("probe: initial sweep failed: %v", err)
		}

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := g.ProbeModels(ctx); err != nil && ctx.Err() == nil {
					log.Printf("probe: sweep failed: %v", err)
				}
			}
		}
	}()
}

// ConfirmedSummary reports how many models are confirmed working per provider.
func (g *Gateway) ConfirmedSummary() map[string]int {
	out := map[string]int{}
	for _, p := range g.catalog.Discovered() {
		if g.catalog.Confirmed(p.Provider, p.Model) {
			out[p.Provider]++
		}
	}
	return out
}
