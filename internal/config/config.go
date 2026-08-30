package config

import (
	"fmt"
	"os"
	"time"
)

const defaultEncryptionKey = "change-me-in-production-32bytes!"

type Config struct {
	DatabaseURL    string
	Port           string
	EncryptionKey  []byte
	DefaultModel   string
	FallbackModels []string

	// BootstrapProviderKeys are upstream keys supplied via environment on
	// startup; they are encrypted into provider_keys so BYOK still lives in
	// Postgres, with env acting only as the seeding path.
	BootstrapProviderKeys map[string]string

	// ProviderBaseURLs overrides upstream endpoints, for proxies, gateways,
	// Azure-style deployments, or local OpenAI-compatible servers.
	ProviderBaseURLs map[string]string

	// RoutingMode selects when smart routing overrides the client's model:
	// "auto" only when the client asks for RouterModel (or sends none),
	// "always" for every request, "off" never.
	RoutingMode      string
	RoutingObjective string
	RouterModel      string

	// IoT holds the MQTT bridge and edge inference settings. An empty Broker
	// leaves the MQTT half inert; the HTTP IoT endpoints still work.
	IoT IoTConfig

	RequestTimeout  time.Duration
	BreakerCooldown time.Duration
	BreakerTrip     int
}

// IoTConfig mirrors iot.Config, kept here so the iot package does not depend
// on config.
type IoTConfig struct {
	Broker       string
	ClientID     string
	Username     string
	Password     string
	TopicPrefix  string
	EdgeEndpoint string
	// APIKey is a client key used to attribute MQTT-originated usage, which
	// has no HTTP caller to authenticate.
	APIKey string
}

func Load() *Config {
	return &Config{
		DatabaseURL:   getEnv("DATABASE_URL", "postgres://coderouter:coderouter@localhost:5432/coderouter?sslmode=disable"),
		Port:          getEnv("PORT", "8080"),
		EncryptionKey: []byte(getEnv("ENCRYPTION_KEY", defaultEncryptionKey)),
		DefaultModel:  getEnv("DEFAULT_MODEL", "gpt-4o-mini"),
		FallbackModels: []string{
			getEnv("FALLBACK_MODEL_1", "claude-3-haiku-20240307"),
			getEnv("FALLBACK_MODEL_2", "gemini-1.5-flash"),
		},
		BootstrapProviderKeys: map[string]string{
			"openai":    os.Getenv("OPENAI_API_KEY"),
			"anthropic": os.Getenv("ANTHROPIC_API_KEY"),
			"google":    os.Getenv("GOOGLE_API_KEY"),
		},
		ProviderBaseURLs: map[string]string{
			"openai":    os.Getenv("OPENAI_BASE_URL"),
			"anthropic": os.Getenv("ANTHROPIC_BASE_URL"),
			"google":    os.Getenv("GOOGLE_BASE_URL"),
		},
		IoT: IoTConfig{
			Broker:       os.Getenv("MQTT_BROKER"),
			ClientID:     getEnv("MQTT_CLIENT_ID", "coderouter"),
			Username:     os.Getenv("MQTT_USERNAME"),
			Password:     os.Getenv("MQTT_PASSWORD"),
			TopicPrefix:  getEnv("MQTT_TOPIC_PREFIX", "coderouter/v1"),
			EdgeEndpoint: os.Getenv("IOT_EDGE_ENDPOINT"),
			APIKey:       os.Getenv("IOT_API_KEY"),
		},
		RoutingMode:      getEnv("ROUTING_MODE", "auto"),
		RoutingObjective: getEnv("ROUTING_OBJECTIVE", "balanced"),
		RouterModel:      getEnv("ROUTER_MODEL", "auto"),
		RequestTimeout:   300 * time.Second,
		BreakerCooldown:  30 * time.Second,
		BreakerTrip:      3,
	}
}

// Validate rejects configurations that would fail later at request time.
func (c *Config) Validate() error {
	switch c.RoutingMode {
	case "auto", "always", "off":
	default:
		return fmt.Errorf("ROUTING_MODE must be auto, always, or off, got %q", c.RoutingMode)
	}

	switch c.RoutingObjective {
	case "balanced", "latency", "cost":
	default:
		return fmt.Errorf("ROUTING_OBJECTIVE must be balanced, latency, or cost, got %q", c.RoutingObjective)
	}

	switch len(c.EncryptionKey) {
	case 16, 24, 32:
	default:
		return fmt.Errorf("ENCRYPTION_KEY must be 16, 24, or 32 bytes, got %d", len(c.EncryptionKey))
	}
	return nil
}

// UsingDefaultEncryptionKey reports whether the committed placeholder key is
// still in use, so startup can warn about it.
func (c *Config) UsingDefaultEncryptionKey() bool {
	return string(c.EncryptionKey) == defaultEncryptionKey
}

func getEnv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}
