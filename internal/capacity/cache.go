package capacity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	cacheFileName = "capacity_cache.json"
	cacheFormat   = 1
	cacheMaxAge   = 30 * 24 * time.Hour
)

type Source uint8

const (
	SourceUnknown Source = iota
	SourceRequest
	SourceResponseHeader
	SourceContextError = SourceResponseHeader
)

type Observation struct {
	ContextWindow       uint64
	MaxCompletionTokens uint64
	ContextSource       Source
	CompletionSource    Source
}

type Values struct {
	ContextWindow       uint64
	MaxCompletionTokens uint64
	ContextSource       Source
	CompletionSource    Source
}

// MergeObservations coalesces concurrent evidence using the same precedence as
// the persistent cache. Repeated request observations retain the largest output
// allowance, while trusted metadata can replace it in either direction.
func MergeObservations(current, next Observation) Observation {
	mergeObservedValue(&current.ContextWindow, &current.ContextSource, next.ContextWindow, next.ContextSource)
	mergeObservedValue(&current.MaxCompletionTokens, &current.CompletionSource, next.MaxCompletionTokens, next.CompletionSource)
	return current
}

type cacheEntry struct {
	ContextWindow       uint64    `json:"context_window,omitempty"`
	MaxCompletionTokens uint64    `json:"max_completion_tokens,omitempty"`
	ContextSource       Source    `json:"context_source,omitempty"`
	CompletionSource    Source    `json:"completion_source,omitempty"`
	UpdatedAt           time.Time `json:"updated_at"`
}

type cacheFile struct {
	Format  int                   `json:"format"`
	Entries map[string]cacheEntry `json:"entries"`
}

type Cache struct {
	mu      sync.Mutex
	path    string
	now     func() time.Time
	entries map[string]cacheEntry
}

func Path(dataDir string) string {
	return filepath.Join(dataDir, cacheFileName)
}

func Open(dataDir string) (*Cache, error) {
	return open(dataDir, time.Now)
}

func open(dataDir string, now func() time.Time) (*Cache, error) {
	cache := &Cache{path: Path(dataDir), now: now, entries: map[string]cacheEntry{}}
	raw, err := os.ReadFile(cache.path)
	if os.IsNotExist(err) {
		return cache, nil
	}
	if err != nil {
		return nil, err
	}
	var stored cacheFile
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, fmt.Errorf("decode capacity cache: %w", err)
	}
	if stored.Format != cacheFormat {
		return nil, fmt.Errorf("unsupported capacity cache format %d", stored.Format)
	}
	cutoff := now().Add(-cacheMaxAge)
	for key, entry := range stored.Entries {
		if validRouteKey(key) && !entry.UpdatedAt.Before(cutoff) &&
			(entry.ContextWindow > 0 || entry.MaxCompletionTokens > 0) {
			cache.entries[key] = entry
		}
	}
	return cache, nil
}

// RouteKey produces a non-reversible cache identity. The persisted file never
// contains channel URLs, model names, credentials, or user-selected labels.
func RouteKey(originBase, wireModel, backend string) string {
	normalizedOrigin := strings.TrimSpace(originBase)
	if parsed, err := url.Parse(normalizedOrigin); err == nil && parsed.Host != "" {
		parsed.User = nil
		parsed.RawQuery = ""
		parsed.Fragment = ""
		parsed.Scheme = strings.ToLower(parsed.Scheme)
		parsed.Host = strings.ToLower(parsed.Host)
		normalizedOrigin = parsed.String()
	}
	identity := normalizedOrigin + "\x00" + strings.TrimSpace(wireModel) + "\x00" + strings.ToLower(strings.TrimSpace(backend))
	digest := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(digest[:])
}

func (c *Cache) Lookup(key string) (Values, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok || c.now().Sub(entry.UpdatedAt) > cacheMaxAge {
		return Values{}, false
	}
	return entry.values(), true
}

func (c *Cache) Observe(key string, observation Observation) (Values, bool, error) {
	if !validRouteKey(key) {
		return Values{}, false, fmt.Errorf("invalid capacity route key")
	}
	if observation.ContextWindow == 0 && observation.MaxCompletionTokens == 0 {
		return Values{}, false, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now().UTC()
	previous, existed := c.entries[key]
	entry := previous
	if existed && now.Sub(previous.UpdatedAt) > cacheMaxAge {
		entry = cacheEntry{}
	}
	changed := mergeObservedValue(&entry.ContextWindow, &entry.ContextSource, observation.ContextWindow, observation.ContextSource)
	if mergeObservedValue(&entry.MaxCompletionTokens, &entry.CompletionSource, observation.MaxCompletionTokens, observation.CompletionSource) {
		changed = true
	}
	if !changed {
		return entry.values(), false, nil
	}
	entry.UpdatedAt = now
	c.entries[key] = entry
	if err := c.writeLocked(); err != nil {
		if existed {
			c.entries[key] = previous
		} else {
			delete(c.entries, key)
		}
		return Values{}, false, err
	}
	return entry.values(), true, nil
}

func mergeObservedValue(current *uint64, currentSource *Source, observed uint64, source Source) bool {
	if observed == 0 || source == SourceUnknown || source < *currentSource {
		return false
	}
	if source == SourceRequest && source == *currentSource && observed < *current {
		return false
	}
	if observed == *current && source == *currentSource {
		return false
	}
	*current = observed
	*currentSource = source
	return true
}

func (c *Cache) writeLocked() error {
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(cacheFile{Format: cacheFormat, Entries: c.entries}, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	dir := filepath.Dir(c.path)
	tmp, err := os.CreateTemp(dir, ".hellogrok-capacity-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, c.path); err != nil {
		return err
	}
	committed = true
	return nil
}

func (entry cacheEntry) values() Values {
	return Values{
		ContextWindow: entry.ContextWindow, MaxCompletionTokens: entry.MaxCompletionTokens,
		ContextSource: entry.ContextSource, CompletionSource: entry.CompletionSource,
	}
}

func validRouteKey(key string) bool {
	if len(key) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(key)
	return err == nil
}
