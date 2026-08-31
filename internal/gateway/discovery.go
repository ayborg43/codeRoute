package gateway

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/coderouter/coderouter/internal/db"
	"github.com/coderouter/coderouter/internal/provider"
)

// LoadDiscoveredModels seeds the catalogue from what was cached on a previous
// run, so a restart can resolve models immediately rather than being unable to
// route anything until the first refresh finishes.
func (g *Gateway) LoadDiscoveredModels(ctx context.Context) error {
	byProvider, err := db.DiscoveredModels(ctx, g.db)
	if err != nil {
		return err
	}

	for name, models := range byProvider {
		g.catalog.SetDiscovered(name, models)
	}
	if len(byProvider) > 0 {
		log.Printf("discovery: loaded cached model lists for %d providers", len(byProvider))
	}
	return nil
}

// RefreshModels asks every provider holding a key what it serves, and records
// the answers. Providers are queried concurrently because one slow or
// unreachable upstream should not hold up the rest.
//
// A provider that fails keeps whatever was previously known about it: a
// transient outage must not empty the catalogue and make its models
// unroutable.
func (g *Gateway) RefreshModels(ctx context.Context) error {
	keys, err := g.ConfiguredKeys()
	if err != nil {
		return err
	}

	var wg sync.WaitGroup
	for _, spec := range g.specs {
		apiKey := keys[spec.Name]
		if apiKey == "" {
			continue
		}

		wg.Add(1)
		go func(spec provider.Spec, apiKey string) {
			defer wg.Done()
			g.refreshOne(ctx, spec, apiKey)
		}(spec, apiKey)
	}
	wg.Wait()

	counts := g.catalog.DiscoveredCount()
	total, free := 0, len(g.catalog.FreeModels())
	for _, n := range counts {
		total += n
	}
	log.Printf("discovery: %d models across %d providers, %d free", total, len(counts), free)
	return nil
}

func (g *Gateway) refreshOne(ctx context.Context, spec provider.Spec, apiKey string) {
	models, err := provider.ListModels(ctx, g.client, spec, apiKey)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			log.Printf("discovery: %s did not answer (%v); keeping the previous list", spec.Name, err)
		}
		return
	}
	if len(models) == 0 {
		log.Printf("discovery: %s reported no models; keeping the previous list", spec.Name)
		return
	}

	g.catalog.SetDiscovered(spec.Name, models)

	// Persist outside the caller's context so a refresh triggered by an
	// admin request is not abandoned when that request returns.
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	change, err := db.ReplaceDiscoveredModels(saveCtx, g.db, spec.Name, models)
	if err != nil {
		log.Printf("discovery: could not cache %s models: %v", spec.Name, err)
		return
	}
	if len(change.Added) > 0 {
		log.Printf("discovery: %s added %d model(s): %s",
			spec.Name, len(change.Added), strings.Join(clip(change.Added, 5), ", "))
	}
	if len(change.Removed) > 0 {
		log.Printf("discovery: %s withdrew %d model(s): %s",
			spec.Name, len(change.Removed), strings.Join(clip(change.Removed, 5), ", "))
	}
}

// clip shortens a list for a log line, saying how many were left out.
func clip(in []string, max int) []string {
	if len(in) <= max {
		return in
	}
	out := append([]string{}, in[:max]...)
	return append(out, fmt.Sprintf("and %d more", len(in)-max))
}

// RefreshProvider re-reads one provider, for immediately after its key changes.
func (g *Gateway) RefreshProvider(ctx context.Context, name string) {
	spec, ok := g.Spec(name)
	if !ok {
		return
	}

	keys, err := g.ConfiguredKeys()
	if err != nil {
		log.Printf("discovery: could not read keys: %v", err)
		return
	}
	if keys[name] == "" {
		// The key was removed; its models are no longer reachable.
		g.catalog.ForgetDiscovered(name)
		if err := db.ForgetDiscoveredModels(ctx, g.db, name); err != nil {
			log.Printf("discovery: could not drop %s models: %v", name, err)
		}
		if err := db.ForgetProbes(ctx, g.db, name); err != nil {
			log.Printf("discovery: could not drop %s probe results: %v", name, err)
		}
		return
	}

	g.refreshOne(ctx, spec, keys[name])
}

// StartDiscovery refreshes the model lists on an interval until ctx is done.
// A zero or negative interval runs discovery once at startup and no more.
func (g *Gateway) StartDiscovery(ctx context.Context, interval time.Duration) {
	go func() {
		if err := g.RefreshModels(ctx); err != nil && ctx.Err() == nil {
			log.Printf("discovery: initial refresh failed: %v", err)
		}

		if interval <= 0 {
			log.Print("discovery: refreshing is disabled; the model list will not update")
			return
		}

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := g.RefreshModels(ctx); err != nil && ctx.Err() == nil {
					log.Printf("discovery: refresh failed: %v", err)
				}
			}
		}
	}()
}
