package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/coderouter/coderouter/internal/api"
	"github.com/coderouter/coderouter/internal/config"
	"github.com/coderouter/coderouter/internal/db"
	"github.com/coderouter/coderouter/internal/gateway"
	"github.com/coderouter/coderouter/internal/iot"
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

	gw := gateway.New(database, cfg)
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
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
	}

	return nil
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
		APIKeyID:     resolveIoTKey(database, cfg.IoT.APIKey),
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
func resolveIoTKey(database *sql.DB, raw string) string {
	if raw == "" {
		return ""
	}
	key, err := db.ValidateClientKey(database, raw)
	if err != nil {
		log.Printf("iot: IOT_API_KEY is not a valid client key (%v); MQTT usage will be unattributed", err)
		return ""
	}
	return key.ID
}
