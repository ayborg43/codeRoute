package config

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/coderouter/coderouter/internal/provider"
	"github.com/coderouter/coderouter/internal/routing"
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

	// Providers is the routable upstream list. It starts from the built-in
	// presets and is narrowed or extended by the PROVIDERS setting.
	Providers []provider.Spec

	// FreeOnly refuses to route to any model with a non-zero or unpublished
	// price. Callers can also ask per request with the "auto:free" model.
	FreeOnly bool

	// AttemptsPerProvider is how many of a provider's models one request may
	// try before giving up on that provider. A provider's list routinely
	// contains models a given account cannot use, so a single attempt each
	// makes the first request likelier to fail than it needs to be.
	AttemptsPerProvider int

	// MaxAttempts caps the whole chain, so a deployment with many providers
	// cannot turn one request into dozens of upstream calls.
	MaxAttempts int

	// DiscoveryInterval is how often each configured provider is asked what
	// models it serves. Zero disables refreshing after startup.
	DiscoveryInterval time.Duration

	// RoutingMode selects when smart routing overrides the client's model:
	// "auto" only when the client asks for RouterModel (or sends none),
	// "always" for every request, "off" never.
	RoutingMode      string
	RoutingObjective string
	RouterModel      string

	// IoT holds the MQTT bridge and edge inference settings. An empty Broker
	// leaves the MQTT half inert; the HTTP IoT endpoints still work.
	IoT IoTConfig

	// Cache configures the semantic cache. It only ever activates where
	// pgvector and an embedding key are both present.
	Cache CacheConfig

	// ModelCatalog is a raw JSON catalogue overriding or extending the
	// built-in model list. Empty means the built-ins stand.
	ModelCatalog []byte

	// catalogErr records a catalogue that could not even be read, so Validate
	// fails the deploy rather than silently routing with the defaults the
	// operator believed they had replaced.
	catalogErr error

	// LatencyFeedback is how often observed behaviour is folded back into the
	// routing catalogue. Zero disables the feedback loop.
	LatencyFeedback time.Duration

	// ObservationWindow is how far back routing looks when judging a model.
	// Long enough to gather a usable sample, short enough that a provider
	// that has recovered is not held to yesterday's failures.
	ObservationWindow time.Duration

	// AdminToken guards client-key and provider-key management, and remains
	// the way scripts authenticate. Signing in with an email and password is
	// the route for people; this stays for automation.
	AdminToken string

	// BootstrapAdminEmail and BootstrapAdminPassword create the first operator
	// account on a deployment that has none, so a fresh install has a way in
	// without a separate provisioning step.
	BootstrapAdminEmail    string
	BootstrapAdminPassword string

	// TrustProxyHeaders makes the gateway believe X-Forwarded-For and
	// X-Forwarded-Proto. Only enable it behind a proxy that sets them: they
	// are trivially forged otherwise, which would let an attacker evade the
	// sign-in lockout and make session cookies look secure when they are not.
	TrustProxyHeaders bool

	RequestTimeout  time.Duration
	BreakerCooldown time.Duration
	BreakerTrip     int
}

// CacheConfig controls semantic caching of completions.
type CacheConfig struct {
	Enabled bool

	// Threshold is the minimum cosine similarity for a stored completion to
	// answer a new prompt, 0..1.
	Threshold float64

	// TTL bounds how long a cached completion may be served.
	TTL time.Duration

	// MaxTemperature is the highest temperature still considered cacheable.
	// A caller asking for varied output should not be handed a replay.
	MaxTemperature float64

	// EmbeddingModel and EmbeddingBaseURL address the embeddings endpoint.
	// The base URL defaults to the configured OpenAI endpoint, so a local
	// OpenAI-compatible server serves embeddings too.
	EmbeddingModel   string
	EmbeddingBaseURL string
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
	catalog, catalogErr := loadModelCatalog()

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
		Providers:           loadProviders(),
		FreeOnly:            getEnvBool("FREE_ONLY", false),
		AttemptsPerProvider: getEnvInt("ROUTING_ATTEMPTS_PER_PROVIDER", 2),
		MaxAttempts:         getEnvInt("ROUTING_MAX_ATTEMPTS", 8),
		DiscoveryInterval:   getEnvDuration("DISCOVERY_INTERVAL", time.Hour),
		IoT: IoTConfig{
			Broker:       os.Getenv("MQTT_BROKER"),
			ClientID:     getEnv("MQTT_CLIENT_ID", "coderouter"),
			Username:     os.Getenv("MQTT_USERNAME"),
			Password:     os.Getenv("MQTT_PASSWORD"),
			TopicPrefix:  getEnv("MQTT_TOPIC_PREFIX", "coderouter/v1"),
			EdgeEndpoint: os.Getenv("IOT_EDGE_ENDPOINT"),
			APIKey:       os.Getenv("IOT_API_KEY"),
		},
		Cache: CacheConfig{
			Enabled:          getEnvBool("CACHE_ENABLED", true),
			Threshold:        getEnvFloat("CACHE_THRESHOLD", 0.95),
			TTL:              getEnvDuration("CACHE_TTL", 24*time.Hour),
			MaxTemperature:   getEnvFloat("CACHE_MAX_TEMPERATURE", 0.3),
			EmbeddingModel:   getEnv("EMBEDDING_MODEL", ""),
			EmbeddingBaseURL: os.Getenv("EMBEDDING_BASE_URL"),
		},
		ModelCatalog:           catalog,
		catalogErr:             catalogErr,
		LatencyFeedback:        getEnvDuration("LATENCY_FEEDBACK_INTERVAL", 5*time.Minute),
		ObservationWindow:      getEnvDuration("OBSERVATION_WINDOW", 6*time.Hour),
		AdminToken:             os.Getenv("ADMIN_TOKEN"),
		BootstrapAdminEmail:    os.Getenv("ADMIN_EMAIL"),
		BootstrapAdminPassword: os.Getenv("ADMIN_PASSWORD"),
		TrustProxyHeaders:      getEnvBool("TRUST_PROXY_HEADERS", false),
		RoutingMode:            getEnv("ROUTING_MODE", "auto"),
		RoutingObjective:       getEnv("ROUTING_OBJECTIVE", "balanced"),
		RouterModel:            getEnv("ROUTER_MODEL", "auto"),
		RequestTimeout:         300 * time.Second,
		BreakerCooldown:        30 * time.Second,
		BreakerTrip:            3,
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

	if c.Cache.Enabled {
		if c.Cache.Threshold <= 0 || c.Cache.Threshold > 1 {
			return fmt.Errorf("CACHE_THRESHOLD must be within (0, 1], got %v", c.Cache.Threshold)
		}
		if c.Cache.TTL < 0 {
			return fmt.Errorf("CACHE_TTL must not be negative, got %s", c.Cache.TTL)
		}
	}

	if c.ObservationWindow <= 0 {
		return fmt.Errorf("OBSERVATION_WINDOW must be positive, got %s", c.ObservationWindow)
	}
	if c.AttemptsPerProvider < 1 {
		return fmt.Errorf("ROUTING_ATTEMPTS_PER_PROVIDER must be at least 1, got %d", c.AttemptsPerProvider)
	}
	if c.MaxAttempts < 1 {
		return fmt.Errorf("ROUTING_MAX_ATTEMPTS must be at least 1, got %d", c.MaxAttempts)
	}

	if len(c.Providers) == 0 {
		return fmt.Errorf("PROVIDERS left no routable providers")
	}
	for _, p := range c.Providers {
		if err := p.Validate(); err != nil {
			return fmt.Errorf("PROVIDERS: %w", err)
		}
	}

	// Parse the catalogue here so a malformed one fails the deploy rather
	// than every request that tries to route with it.
	if _, err := c.Catalog(); err != nil {
		return err
	}
	return nil
}

// ResolvedProviders applies the per-provider base URL overrides, so
// OPENAI_BASE_URL and friends still work alongside the provider list.
func (c *Config) ResolvedProviders() []provider.Spec {
	out := make([]provider.Spec, len(c.Providers))
	copy(out, c.Providers)

	for i := range out {
		if override := c.ProviderBaseURLs[out[i].Name]; override != "" {
			out[i].BaseURL = strings.TrimRight(override, "/")
		}
	}
	return out
}

// loadProviders assembles the routable upstream list.
//
// PROVIDERS narrows or extends the built-in presets:
//
//	PROVIDERS=groq,openrouter            only these two
//	PROVIDERS=+local=http://host:8000/v1 the presets plus a custom one
//	PROVIDERS=groq,local=http://…/v1     one preset plus a custom one
//
// A bare name must match a preset. A name=url pair defines an OpenAI-compatible
// provider of any name, which is what most new vendors need.
func loadProviders() []provider.Spec {
	raw := strings.TrimSpace(os.Getenv("PROVIDERS"))
	if raw == "" {
		return provider.Presets()
	}

	var specs []provider.Spec
	if strings.HasPrefix(raw, "+") {
		specs = provider.Presets()
		raw = strings.TrimPrefix(raw, "+")
	}

	seen := make(map[string]int, len(specs))
	for i, s := range specs {
		seen[s.Name] = i
	}

	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		name, url, custom := strings.Cut(entry, "=")
		name = strings.TrimSpace(name)

		spec, isPreset := provider.Preset(name)
		switch {
		case custom:
			// An explicit URL wins, keeping a preset's label if the name
			// matches one.
			spec.Name, spec.BaseURL, spec.Kind = name, strings.TrimSpace(url), provider.KindOpenAI
			if isPreset {
				presetSpec, _ := provider.Preset(name)
				spec.Kind = presetSpec.Kind
				spec.Label, spec.ConsoleURL = presetSpec.Label, presetSpec.ConsoleURL
			}
		case isPreset:
			// spec already holds the preset.
		default:
			// Unknown bare name. Carry it through invalid so Validate reports
			// it by name instead of the deployment silently losing a provider.
			spec = provider.Spec{Name: name}
		}

		if at, ok := seen[spec.Name]; ok {
			specs[at] = spec
			continue
		}
		seen[spec.Name] = len(specs)
		specs = append(specs, spec)
	}

	return specs
}

// Catalog builds the routing catalogue this configuration describes.
func (c *Config) Catalog() (*routing.Catalog, error) {
	if c.catalogErr != nil {
		return nil, c.catalogErr
	}
	if len(c.ModelCatalog) == 0 {
		return routing.NewCatalog(), nil
	}
	return routing.LoadCatalog(c.ModelCatalog)
}

// EmbeddingEndpoint is where embeddings are fetched from: an explicit override,
// else whatever endpoint OpenAI completions already use.
func (c *Config) EmbeddingEndpoint() string {
	if c.Cache.EmbeddingBaseURL != "" {
		return strings.TrimRight(c.Cache.EmbeddingBaseURL, "/")
	}
	if v := c.ProviderBaseURLs["openai"]; v != "" {
		return strings.TrimRight(v, "/")
	}
	return "https://api.openai.com/v1"
}

// loadModelCatalog reads the catalogue from a file or straight from the
// environment. The file form keeps a long catalogue out of the process
// environment, where it would be awkward to edit and visible in `ps`.
func loadModelCatalog() ([]byte, error) {
	if path := os.Getenv("MODEL_CATALOG_PATH"); path != "" {
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("MODEL_CATALOG_PATH %q could not be read: %w", path, err)
		}
		return body, nil
	}
	if inline := os.Getenv("MODEL_CATALOG"); inline != "" {
		return []byte(inline), nil
	}
	return nil, nil
}

// UsingDefaultEncryptionKey reports whether the committed placeholder key is
// still in use, so startup can warn about it.
func (c *Config) UsingDefaultEncryptionKey() bool {
	return string(c.EncryptionKey) == defaultEncryptionKey
}

// getEnvInt reads an integer setting, falling back when unset or unparseable.
func getEnvInt(key string, fallback int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		log.Printf("%s=%q is not a number; using %d", key, raw, fallback)
		return fallback
	}
	return n
}

// getEnvBool reads a boolean setting. Anything unparseable keeps the default.
func getEnvBool(key string, fallback bool) bool {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		log.Printf("%s=%q is not a boolean; using %v", key, raw, fallback)
		return fallback
	}
	return v
}

func getEnvFloat(key string, fallback float64) float64 {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		log.Printf("%s=%q is not a number; using %v", key, raw, fallback)
		return fallback
	}
	return v
}

// getEnvDuration accepts Go duration syntax ("30m", "24h", "90s").
func getEnvDuration(key string, fallback time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		log.Printf("%s=%q is not a duration; using %s", key, raw, fallback)
		return fallback
	}
	return v
}

func getEnv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}
