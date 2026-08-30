package main

import (
	"database/sql"
	"log"
	"net/http"

	"github.com/coderouter/coderouter/internal/api"
	"github.com/coderouter/coderouter/internal/config"
	"github.com/coderouter/coderouter/internal/db"
	"github.com/coderouter/coderouter/internal/gateway"
	"github.com/coderouter/coderouter/internal/iot"
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

	if err := bootstrap(database, cfg); err != nil {
		log.Fatal(err)
	}

	gw := gateway.New(database, cfg)
	bridge := startBridge(database, cfg, gw)
	defer bridge.Close()

	handler := api.NewHandler(gw, database, cfg, bridge)

	log.Printf("starting coderouter on :%s", cfg.Port)
	log.Fatal(http.ListenAndServe(":"+cfg.Port, handler))
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
