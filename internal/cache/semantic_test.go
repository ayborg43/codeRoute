package cache

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeEmbedder struct {
	vec  []float32
	err  error
	call int
}

func (f *fakeEmbedder) Embed(context.Context, string) ([]float32, error) {
	f.call++
	return f.vec, f.err
}

func TestDisabledWithoutEmbedder(t *testing.T) {
	c := New(nil, Options{Threshold: 0.95})
	if c.Enabled() {
		t.Fatal("cache enabled itself with no embedder")
	}

	if _, hit, err := c.Lookup(context.Background(), "m", "p"); hit || !errors.Is(err, ErrDisabled) {
		t.Errorf("Lookup on a disabled cache = (%v, %v), want (false, ErrDisabled)", hit, err)
	}
	if err := c.Store(context.Background(), "m", "p", "r"); !errors.Is(err, ErrDisabled) {
		t.Errorf("Store on a disabled cache = %v, want ErrDisabled", err)
	}
}

func TestDisabledWithoutDatabase(t *testing.T) {
	if New(nil, Options{Embedder: &fakeEmbedder{}, Threshold: 0.95}).Enabled() {
		t.Fatal("cache enabled itself with no database")
	}
}

func TestDisabledOnNonsenseThreshold(t *testing.T) {
	for _, th := range []float64{0, -1, 1.5} {
		if New(nil, Options{Embedder: &fakeEmbedder{}, Threshold: th}).Enabled() {
			t.Errorf("threshold %v was accepted", th)
		}
	}
}

// A nil cache is what the gateway holds when caching is switched off; every
// method must tolerate it rather than panicking on the request path.
func TestNilCacheIsInert(t *testing.T) {
	var c *SemanticCache

	if c.Enabled() {
		t.Error("nil cache reported itself enabled")
	}
	if got := c.Stats(); got.Enabled || got.Hits != 0 {
		t.Errorf("nil cache stats = %+v", got)
	}
	if _, hit, err := c.Lookup(context.Background(), "m", "p"); hit || !errors.Is(err, ErrDisabled) {
		t.Errorf("nil cache Lookup = (%v, %v)", hit, err)
	}
	if err := c.Store(context.Background(), "m", "p", "r"); !errors.Is(err, ErrDisabled) {
		t.Errorf("nil cache Store = %v", err)
	}
}

func TestHitRate(t *testing.T) {
	cases := []struct {
		stats Stats
		want  float64
	}{
		{Stats{}, 0},
		{Stats{Hits: 1, Misses: 3}, 0.25},
		{Stats{Hits: 5}, 1},
		// Errors are not lookups and must not dilute the rate.
		{Stats{Hits: 1, Misses: 1, Errors: 98}, 0.5},
	}
	for _, tc := range cases {
		if got := tc.stats.HitRate(); got != tc.want {
			t.Errorf("HitRate(%+v) = %v, want %v", tc.stats, got, tc.want)
		}
	}
}

func TestVectorLiteralIsPgvectorSyntax(t *testing.T) {
	if got := vectorLiteral([]float32{1, 0.5, -2}); got != "[1,0.5,-2]" {
		t.Errorf("vectorLiteral = %q", got)
	}
	if got := vectorLiteral(nil); got != "null" && got != "[]" {
		t.Errorf("vectorLiteral(nil) = %q", got)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("abcdef", 3); got != "abc" {
		t.Errorf("truncate = %q", got)
	}
	if got := truncate("ab", 8); got != "ab" {
		t.Errorf("truncate shortened a short string: %q", got)
	}
}

func TestOptionsCarryTTL(t *testing.T) {
	c := &SemanticCache{ttl: time.Hour}
	if c.ttl != time.Hour {
		t.Fatal("ttl not retained")
	}
}
