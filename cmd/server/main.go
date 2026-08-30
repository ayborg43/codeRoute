package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/coderouter/coderouter/internal/api"
	"github.com/coderouter/coderouter/internal/cache"
	"github.com/coderouter/coderouter/internal/config"
	"github.com/coderouter/coderouter/internal/db"
	"github.com/coderouter/coderouter/internal/gateway"
	"github.com/coderouter/coderouter/internal/iot"
	"github.com/coderouter/coderouter/internal/provider"
	"github.com/coderouter/coderouter/migrations"
)

func main() {
	cfg := config.Load()
	if err := cfg.Validate(); err != nil {
		log.Fatal(err)
	}
	if cfg.UsingDefaultEncryptionKey() {
		log.Print("WARNING: ENCRYPTION_KEY is the committed default; set your own before storing real provider keys")
	}

	database, err := db.Connect(cfg.DatabaseURL)
	if err != nil {
		log.Fatal(err)
	}
	defer database.Close()

	// The binary owns its schema: initdb only fires on an empty volume, so a
	// redeploy against an existing database would never see new migrations.
	if err := db.Migrate(database, migrations.FS); err != nil {
		log.Fatal(err)
	}

	if err := bootstrap(database, cfg); err != nil {
		log.Fatal(err)
	}

	catalog, err := cfg.Catalog()
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("routing catalogue holds %d models", len(catalog.Profiles()))

	// ctx lives for the process, cancelling the background loops on shutdown.
	ctx, shutdown := context.WithCancel(context.Background())
	defer shutdown()

	gw, err := gateway.New(database, gateway.Options{
		Config:  cfg,
		Catalog: catalog,
		Cache:   buildCache(database, cfg),
	})
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("%d providers configured", len(gw.Specs()))

	if err := gw.LoadDiscoveredModels(ctx); err != nil {
		log.Printf("could not load cached model lists: %v", err)
	}
	if err := gw.LoadModelTags(ctx); err != nil {
		log.Printf("could not load model markings: %v", err)
	}
	gw.StartDiscovery(ctx, cfg.DiscoveryInterval)
	gw.StartLatencyFeedback(ctx, cfg.LatencyFeedback)

	// A runtime toggle outlives the process, so a stored override wins over
	// the environment default — otherwise a restart would silently undo it.
	switch stored, err := db.FreeOnlySetting(ctx, database); {
	case err == nil:
		gw.SetFreeOnly(stored)
	case errors.Is(err, db.ErrNoSetting):
		// Never toggled; the environment default stands.
	default:
		log.Printf("could not read the free-only setting (%v); using FREE_ONLY=%v", err, cfg.FreeOnly)
	}

	if gw.FreeOnly() {
		log.Print("free-only mode is on; only models published at zero cost will be routed to")
	}

	bridge := startBridge(database, cfg, gw)
	handler := api.NewHandler(gw, database, cfg, bridge)

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
	}

	go func() {
		log.Printf("starting coderouter on :%s", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	// Platforms like Dokploy stop containers with SIGTERM during a rolling
	// deploy; draining lets in-flight completions and streams finish.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Print("shutdown signal received, draining connections")
	shutdown()

	drainCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(drainCtx); err != nil {
		log.Printf("graceful shutdown timed out: %v", err)
	}
	bridge.Close()
	log.Print("stopped")
}

// bootstrap seeds provider keys from the environment and mints a first client
// key so a fresh deployment is usable without a separate admin step.
func bootstrap(database *sql.DB, cfg *config.Config) error {
	for name, key := range cfg.BootstrapProviderKeys {
		if key == "" {
			continue
		}
		if err := db.StoreProviderKey(database, cfg.EncryptionKey, name, key); err != nil {
			return err
		}
		log.Printf("stored provider key for %s (fingerprint %s)", name, db.FingerprintKey(key))
	}

	count, err := db.CountClientKeys(database)
	if err != nil {
		return err
	}
	if count == 0 {
		rawKey, err := db.CreateClientKey(database, "bootstrap")
		if err != nil {
			return err
		}
		log.Printf("no client keys found; created one (shown once): %s", rawKey)
		log.Print("WARNING: client keys carry no rate limit or spend cap; " +
			"a leaked key means uncapped spend against your provider keys")
	}

	return nil
}

// buildCache assembles the semantic cache. Embeddings are billed to the stored
// OpenAI key, which is resolved per call rather than captured here: a key added
// through the dashboard after startup brings the cache to life without a
// restart, and a rotated one takes effect within providerKeyTTL.
func buildCache(database *sql.DB, cfg *config.Config) *cache.SemanticCache {
	if !cfg.Cache.Enabled {
		log.Print("cache: CACHE_ENABLED is false; semantic cache disabled")
		return nil
	}

	resolver := &providerKeyResolver{db: database, encryptionKey: cfg.EncryptionKey}

	return cache.New(database, cache.Options{
		Embedder: &provider.OpenAIEmbedder{
			BaseURL: cfg.EmbeddingEndpoint(),
			Key:     func() (string, error) { return resolver.get("openai") },
			Model:   cfg.Cache.EmbeddingModel,
			Client:  &http.Client{Timeout: 30 * time.Second},
		},
		Threshold: cfg.Cache.Threshold,
		TTL:       cfg.Cache.TTL,
	})
}

// providerKeyTTL bounds how stale a cached provider key may be. Short enough
// that rotating a key through the dashboard takes effect promptly, long enough
// that embedding traffic does not query and decrypt on every request.
const providerKeyTTL = 30 * time.Second

// providerKeyResolver reads upstream keys on demand, memoising briefly.
type providerKeyResolver struct {
	db            *sql.DB
	encryptionKey []byte

	mu     sync.Mutex
	key    string
	name   string
	loaded time.Time
}

func (r *providerKeyResolver) get(name string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.name == name && time.Since(r.loaded) < providerKeyTTL {
		return r.key, nil
	}

	key, err := db.ProviderKey(r.db, r.encryptionKey, name)
	if errors.Is(err, db.ErrNoProviderKey) {
		// Not yet configured. Cache the absence too, so a deployment with no
		// key does not query on every request.
		r.name, r.key, r.loaded = name, "", time.Now()
		return "", nil
	}
	if err != nil {
		return "", err
	}

	r.name, r.key, r.loaded = name, key, time.Now()
	return key, nil
}

// startBridge wires the IoT bridge. A broker that will not connect is logged
// and left disconnected rather than taking the gateway down with it.
func startBridge(database *sql.DB, cfg *config.Config, gw *gateway.Gateway) *iot.Bridge {
	bridge := iot.NewBridge(iot.Config{
		Broker:       cfg.IoT.Broker,
		ClientID:     cfg.IoT.ClientID,
		Username:     cfg.IoT.Username,
		Password:     cfg.IoT.Password,
		TopicPrefix:  cfg.IoT.TopicPrefix,
		EdgeEndpoint: cfg.IoT.EdgeEndpoint,
		APIKey:       resolveIoTKey(database, cfg.IoT.APIKey),
	}, gw, iot.NewStore(database))

	if cfg.IoT.EdgeEndpoint != "" {
		log.Printf("iot: edge inference endpoint %s (cloud fallback enabled)", cfg.IoT.EdgeEndpoint)
	}

	if !bridge.Enabled() {
		log.Print("iot: no MQTT_BROKER set; MQTT bridge disabled, HTTP IoT endpoints still active")
		return bridge
	}

	if err := bridge.Connect(); err != nil {
		log.Printf("iot: mqtt bridge unavailable: %v", err)
		return bridge
	}
	log.Printf("iot: mqtt bridge connected to %s", cfg.IoT.Broker)
	return bridge
}

// resolveIoTKey turns the configured raw client key into an id for usage
// attribution of MQTT traffic, which has no HTTP caller to authenticate.
func resolveIoTKey(database *sql.DB, raw string) *db.ClientKey {
	if raw == "" {
		return nil
	}
	key, err := db.ValidateClientKey(database, raw)
	if err != nil {
		log.Printf("iot: IOT_API_KEY is not a valid client key (%v); MQTT usage will be unattributed", err)
		return nil
	}
	return key
}
