package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coderouter/coderouter/internal/provider"
)

// baseConfig is a configuration that passes Validate, so each test can break
// exactly one thing and see that Validate notices.
func baseConfig() *Config {
	return &Config{
		EncryptionKey:       []byte("0123456789abcdef"),
		RoutingMode:         "auto",
		RoutingObjective:    "balanced",
		Providers:           provider.Presets(),
		AttemptsPerProvider: 2,
		ObservationWindow:   6 * time.Hour,
		MaxAttempts:         8,
		Cache:               CacheConfig{Enabled: true, Threshold: 0.95, TTL: time.Hour},
	}
}

func TestValidateAcceptsABaseline(t *testing.T) {
	if err := baseConfig().Validate(); err != nil {
		t.Fatalf("baseline configuration was rejected: %v", err)
	}
}

func TestValidateRejectsBadValues(t *testing.T) {
	cases := map[string]func(*Config){
		"bad routing mode":  func(c *Config) { c.RoutingMode = "sometimes" },
		"bad objective":     func(c *Config) { c.RoutingObjective = "vibes" },
		"short key":         func(c *Config) { c.EncryptionKey = []byte("tooshort") },
		"threshold zero":    func(c *Config) { c.Cache.Threshold = 0 },
		"threshold over 1":  func(c *Config) { c.Cache.Threshold = 1.5 },
		"negative ttl":      func(c *Config) { c.Cache.TTL = -time.Hour },
		"broken catalogue":  func(c *Config) { c.ModelCatalog = []byte(`{{{`) },
		"no providers":      func(c *Config) { c.Providers = nil },
		"zero attempts":     func(c *Config) { c.AttemptsPerProvider = 0 },
		"negative attempts": func(c *Config) { c.AttemptsPerProvider = -1 },
		"zero max":          func(c *Config) { c.MaxAttempts = 0 },
		"bad provider url": func(c *Config) {
			c.Providers = []provider.Spec{{Name: "x", BaseURL: "nope", Kind: provider.KindOpenAI}}
		},
		"unknown kind": func(c *Config) {
			c.Providers = []provider.Spec{{Name: "x", BaseURL: "https://a/v1", Kind: "telepathy"}}
		},
		"empty catalogue": func(c *Config) { c.ModelCatalog = []byte(`{"replace":true,"models":[]}`) },
		"priceless entry": func(c *Config) { c.ModelCatalog = []byte(`[{"model":"x","provider":"openai"}]`) },
	}

	for name, break_ := range cases {
		cfg := baseConfig()
		break_(cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// Cache settings are only checked when the cache is on, so a deployment that
// has switched it off is not blocked by a stale threshold value.
func TestCacheSettingsAreIgnoredWhenDisabled(t *testing.T) {
	cfg := baseConfig()
	cfg.Cache = CacheConfig{Enabled: false, Threshold: 99, TTL: -time.Hour}

	if err := cfg.Validate(); err != nil {
		t.Errorf("a disabled cache blocked startup: %v", err)
	}
}

func TestCatalogDefaultsToTheBuiltIns(t *testing.T) {
	cfg := baseConfig()

	catalog, err := cfg.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Profiles()) == 0 {
		t.Fatal("the default catalogue is empty")
	}
}

func TestCatalogFromInlineJSON(t *testing.T) {
	t.Setenv("MODEL_CATALOG", `{"replace":true,"models":[
		{"model":"only","provider":"openai","latency_ms":10,
		 "input_cost_per_1m":1,"output_cost_per_1m":2,"tasks":["conversation"]}]}`)

	cfg := Load()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("inline catalogue rejected: %v", err)
	}

	catalog, err := cfg.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	if got := catalog.Profiles(); len(got) != 1 || got[0].Model != "only" {
		t.Errorf("catalogue = %+v", got)
	}
}

func TestCatalogFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.json")
	body := `[{"model":"from-file","provider":"google","latency_ms":10,
		"input_cost_per_1m":1,"output_cost_per_1m":2,"tasks":["analysis"]}]`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("MODEL_CATALOG_PATH", path)

	catalog, err := Load().Catalog()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := catalog.Lookup("from-file"); !ok {
		t.Error("the file's model is missing from the catalogue")
	}
}

// An unreadable catalogue path must fail the deploy, not fall back to defaults
// and route with prices the operator thought they had replaced.
func TestUnreadableCatalogPathFailsValidation(t *testing.T) {
	t.Setenv("MODEL_CATALOG_PATH", filepath.Join(t.TempDir(), "missing.json"))

	err := Load().Validate()
	if err == nil {
		t.Fatal("an unreadable MODEL_CATALOG_PATH was ignored")
	}
	if !strings.Contains(err.Error(), "missing.json") {
		t.Errorf("error does not name the path: %v", err)
	}
}

func TestEmbeddingEndpointPrefersTheOverride(t *testing.T) {
	cfg := baseConfig()
	cfg.ProviderBaseURLs = map[string]string{}

	if got := cfg.EmbeddingEndpoint(); got != "https://api.openai.com/v1" {
		t.Errorf("default = %q", got)
	}

	cfg.ProviderBaseURLs["openai"] = "http://localhost:11434/v1/"
	if got := cfg.EmbeddingEndpoint(); got != "http://localhost:11434/v1" {
		t.Errorf("fell back to the wrong endpoint: %q", got)
	}

	cfg.Cache.EmbeddingBaseURL = "http://embeddings.internal/v1/"
	if got := cfg.EmbeddingEndpoint(); got != "http://embeddings.internal/v1" {
		t.Errorf("override ignored: %q", got)
	}
}

func TestEnvParsersFallBackOnNonsense(t *testing.T) {
	t.Setenv("CR_TEST_BOOL", "maybe")
	t.Setenv("CR_TEST_FLOAT", "π")
	t.Setenv("CR_TEST_DURATION", "a fortnight")

	if got := getEnvBool("CR_TEST_BOOL", true); !got {
		t.Error("getEnvBool did not fall back")
	}
	if got := getEnvFloat("CR_TEST_FLOAT", 1.5); got != 1.5 {
		t.Errorf("getEnvFloat = %v", got)
	}
	if got := getEnvDuration("CR_TEST_DURATION", time.Minute); got != time.Minute {
		t.Errorf("getEnvDuration = %s", got)
	}
}

func TestEnvParsersReadRealValues(t *testing.T) {
	t.Setenv("CR_TEST_BOOL", "false")
	t.Setenv("CR_TEST_FLOAT", "0.85")
	t.Setenv("CR_TEST_DURATION", "90m")

	if got := getEnvBool("CR_TEST_BOOL", true); got {
		t.Error("getEnvBool = true, want false")
	}
	if got := getEnvFloat("CR_TEST_FLOAT", 1.5); got != 0.85 {
		t.Errorf("getEnvFloat = %v", got)
	}
	if got := getEnvDuration("CR_TEST_DURATION", time.Minute); got != 90*time.Minute {
		t.Errorf("getEnvDuration = %s", got)
	}
}

func TestLoadDefaults(t *testing.T) {
	cfg := Load()

	if !cfg.Cache.Enabled {
		t.Error("the semantic cache is off by default")
	}
	if cfg.Cache.Threshold != 0.95 {
		t.Errorf("default threshold = %v", cfg.Cache.Threshold)
	}
	if cfg.Cache.TTL != 24*time.Hour {
		t.Errorf("default TTL = %s", cfg.Cache.TTL)
	}
	if cfg.LatencyFeedback != 5*time.Minute {
		t.Errorf("default latency feedback = %s", cfg.LatencyFeedback)
	}
	if cfg.ObservationWindow != 6*time.Hour {
		t.Errorf("default observation window = %s; a zero window makes every score meaningless",
			cfg.ObservationWindow)
	}
	if cfg.AdminToken != "" {
		t.Error("an admin token is set by default; management must be opt-in")
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("the shipped defaults do not validate: %v", err)
	}
}

func TestUsingDefaultEncryptionKey(t *testing.T) {
	cfg := Load()
	if !cfg.UsingDefaultEncryptionKey() {
		t.Error("the committed default key was not recognised as such")
	}

	t.Setenv("ENCRYPTION_KEY", "0123456789abcdef")
	if Load().UsingDefaultEncryptionKey() {
		t.Error("a custom key was reported as the default")
	}
}

// Every setting the deployment documents must actually reach the Config.
//
// Three separate settings have been declared as fields and then never assigned
// in Load: the code compiled, the unit tests passed because they set fields
// directly, and the omission only surfaced when a real deployment behaved as
// though the variable had never been set. This walks the documented list and
// checks each one lands somewhere.
func TestEveryDocumentedEnvVarIsRead(t *testing.T) {
	// value chosen per variable so the result is unambiguous when read back
	cases := map[string]struct {
		value string
		check func(*Config) bool
	}{
		"DATABASE_URL":                  {"postgres://x/y", func(c *Config) bool { return c.DatabaseURL == "postgres://x/y" }},
		"PORT":                          {"9999", func(c *Config) bool { return c.Port == "9999" }},
		"ENCRYPTION_KEY":                {"0123456789abcdef", func(c *Config) bool { return string(c.EncryptionKey) == "0123456789abcdef" }},
		"DEFAULT_MODEL":                 {"some-model", func(c *Config) bool { return c.DefaultModel == "some-model" }},
		"ADMIN_TOKEN":                   {"tok", func(c *Config) bool { return c.AdminToken == "tok" }},
		"ADMIN_EMAIL":                   {"a@b.com", func(c *Config) bool { return c.BootstrapAdminEmail == "a@b.com" }},
		"ADMIN_PASSWORD":                {"a-long-passphrase", func(c *Config) bool { return c.BootstrapAdminPassword == "a-long-passphrase" }},
		"TRUST_PROXY_HEADERS":           {"true", func(c *Config) bool { return c.TrustProxyHeaders }},
		"ROUTING_MODE":                  {"always", func(c *Config) bool { return c.RoutingMode == "always" }},
		"ROUTING_OBJECTIVE":             {"cost", func(c *Config) bool { return c.RoutingObjective == "cost" }},
		"ROUTER_MODEL":                  {"pick", func(c *Config) bool { return c.RouterModel == "pick" }},
		"ROUTING_ATTEMPTS_PER_PROVIDER": {"3", func(c *Config) bool { return c.AttemptsPerProvider == 3 }},
		"ROUTING_MAX_ATTEMPTS":          {"11", func(c *Config) bool { return c.MaxAttempts == 11 }},
		"FREE_ONLY":                     {"true", func(c *Config) bool { return c.FreeOnly }},
		"DISCOVERY_INTERVAL":            {"7h", func(c *Config) bool { return c.DiscoveryInterval == 7*time.Hour }},
		"OBSERVATION_WINDOW":            {"9h", func(c *Config) bool { return c.ObservationWindow == 9*time.Hour }},
		"LATENCY_FEEDBACK_INTERVAL":     {"8m", func(c *Config) bool { return c.LatencyFeedback == 8*time.Minute }},
		"CACHE_ENABLED":                 {"false", func(c *Config) bool { return !c.Cache.Enabled }},
		"CACHE_THRESHOLD":               {"0.77", func(c *Config) bool { return c.Cache.Threshold == 0.77 }},
		"CACHE_TTL":                     {"3h", func(c *Config) bool { return c.Cache.TTL == 3*time.Hour }},
		"CACHE_MAX_TEMPERATURE":         {"0.42", func(c *Config) bool { return c.Cache.MaxTemperature == 0.42 }},
		"EMBEDDING_MODEL":               {"embed-1", func(c *Config) bool { return c.Cache.EmbeddingModel == "embed-1" }},
		"EMBEDDING_BASE_URL":            {"http://e/v1", func(c *Config) bool { return c.Cache.EmbeddingBaseURL == "http://e/v1" }},
		"OPENAI_API_KEY":                {"sk-x", func(c *Config) bool { return c.BootstrapProviderKeys["openai"] == "sk-x" }},
		"OPENAI_BASE_URL":               {"http://o/v1", func(c *Config) bool { return c.ProviderBaseURLs["openai"] == "http://o/v1" }},
		"MQTT_BROKER":                   {"tcp://b:1883", func(c *Config) bool { return c.IoT.Broker == "tcp://b:1883" }},
		"IOT_EDGE_ENDPOINT":             {"http://edge/v1", func(c *Config) bool { return c.IoT.EdgeEndpoint == "http://edge/v1" }},
		"PROVIDERS":                     {"groq", func(c *Config) bool { return len(c.Providers) == 1 && c.Providers[0].Name == "groq" }},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, tc.value)
			if !tc.check(Load()) {
				t.Errorf("%s=%q was not read into the configuration — the field is "+
					"probably declared but never assigned in Load", name, tc.value)
			}
		})
	}
}
