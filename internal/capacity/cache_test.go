package capacity

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestCachePersistsOnlyHashedRouteIdentity(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	cache, err := open(dir, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	key := RouteKey("https://user:secret@example.invalid/v1?token=private", "private-model", "responses")
	values, changed, err := cache.Observe(key, Observation{
		ContextWindow: 1_048_576, ContextSource: SourceResponseHeader,
		MaxCompletionTokens: 384_000, CompletionSource: SourceResponseHeader,
	})
	if err != nil || !changed || values.ContextWindow != 1_048_576 || values.MaxCompletionTokens != 384_000 {
		t.Fatalf("Observe() values=%+v changed=%t err=%v", values, changed, err)
	}

	raw, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.Valid(raw) || bytes.HasPrefix(raw, []byte{0xEF, 0xBB, 0xBF}) {
		t.Fatal("capacity cache is not UTF-8 without BOM")
	}
	for _, sensitive := range []string{"secret", "example.invalid", "private-model", "token=private"} {
		if strings.Contains(string(raw), sensitive) {
			t.Fatalf("cache leaked %q: %s", sensitive, raw)
		}
	}

	reloaded, err := open(dir, func() time.Time { return now.Add(time.Hour) })
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := reloaded.Lookup(key); !ok || got != values {
		t.Fatalf("Lookup() = %+v, %t; want %+v", got, ok, values)
	}
}

func TestCacheMergesRequestMaxWithoutOverridingTrustedMetadata(t *testing.T) {
	cache, err := open(t.TempDir(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	key := RouteKey("https://example.invalid/v1", "model", "responses")
	if _, _, err := cache.Observe(key, Observation{
		MaxCompletionTokens: 4_096, CompletionSource: SourceRequest,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := cache.Observe(key, Observation{
		MaxCompletionTokens: 8_192, CompletionSource: SourceRequest,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := cache.Observe(key, Observation{
		MaxCompletionTokens: 6_144, CompletionSource: SourceResponseHeader,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := cache.Observe(key, Observation{
		MaxCompletionTokens: 32_768, CompletionSource: SourceRequest,
	}); err != nil {
		t.Fatal(err)
	}
	got, ok := cache.Lookup(key)
	if !ok || got.MaxCompletionTokens != 6_144 {
		t.Fatalf("trusted response metadata was overridden: %+v", got)
	}
}

func TestCacheRetriesObservationAfterWriteFailure(t *testing.T) {
	dir := t.TempDir()
	cache, err := open(dir, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(Path(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	key := RouteKey("https://example.invalid/v1", "model", "responses")
	observation := Observation{
		ContextWindow: 262_144, ContextSource: SourceResponseHeader,
	}
	if _, changed, err := cache.Observe(key, observation); err == nil || changed {
		t.Fatalf("failed Observe() changed=%t err=%v", changed, err)
	}
	if _, ok := cache.Lookup(key); ok {
		t.Fatal("failed write committed the observation in memory")
	}
	if err := os.Remove(Path(dir)); err != nil {
		t.Fatal(err)
	}
	values, changed, err := cache.Observe(key, observation)
	if err != nil || !changed || values.ContextWindow != observation.ContextWindow {
		t.Fatalf("retried Observe() values=%+v changed=%t err=%v", values, changed, err)
	}
}

func TestCacheExpiresRollingModelCapacity(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	cache, err := open(dir, func() time.Time { return base })
	if err != nil {
		t.Fatal(err)
	}
	key := RouteKey("https://example.invalid", "rolling", "messages")
	if _, _, err := cache.Observe(key, Observation{
		ContextWindow: 131_072, ContextSource: SourceContextError,
	}); err != nil {
		t.Fatal(err)
	}

	expired, err := open(dir, func() time.Time { return base.Add(cacheMaxAge + time.Second) })
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := expired.Lookup(key); ok {
		t.Fatalf("expired capacity remained usable: %+v", got)
	}
}

func TestCacheExpiresWithoutProcessRestart(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base
	cache, err := open(t.TempDir(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	key := RouteKey("https://example.invalid", "rolling", "messages")
	if _, _, err := cache.Observe(key, Observation{
		MaxCompletionTokens: 32_768, CompletionSource: SourceResponseHeader,
	}); err != nil {
		t.Fatal(err)
	}
	now = base.Add(cacheMaxAge + time.Second)
	values, changed, err := cache.Observe(key, Observation{
		MaxCompletionTokens: 4_096, CompletionSource: SourceRequest,
	})
	if err != nil || !changed || values.MaxCompletionTokens != 4_096 || values.CompletionSource != SourceRequest {
		t.Fatalf("post-expiry Observe() values=%+v changed=%t err=%v", values, changed, err)
	}
}
