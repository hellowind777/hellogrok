package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hellowind777/hellogrok/internal/config"
)

const (
	reasoningProvenanceFileName = "reasoning_provenance.json"
	reasoningProvenanceVersion  = 1
	maxReasoningProvenanceItems = 8192
)

type reasoningProvenanceRecord struct {
	Domains  []string `json:"domains"`
	SeenUnix int64    `json:"seen_unix"`
}

type reasoningProvenanceFile struct {
	Version int                                  `json:"version"`
	Entries map[string]reasoningProvenanceRecord `json:"entries"`
}

// reasoningProvenanceStore maps opaque reasoning digests to the signature
// domains that emitted them. Neither opaque values nor route details are
// persisted.
type reasoningProvenanceStore struct {
	mu      sync.RWMutex
	path    string
	entries map[string]reasoningProvenanceRecord
	dirty   bool
	now     func() time.Time
}

type reasoningFilterMode uint8

const (
	keepUnknownReasoning reasoningFilterMode = iota
	dropAllOpaqueReasoning
)

type reasoningFilterStats struct {
	Opaque     int
	Compatible int
	Unknown    int
	Dropped    int
}

func newReasoningProvenanceStore(path string) (*reasoningProvenanceStore, error) {
	store := &reasoningProvenanceStore{
		path:    strings.TrimSpace(path),
		entries: map[string]reasoningProvenanceRecord{},
		now:     time.Now,
	}
	if store.path == "" {
		return store, nil
	}
	raw, err := os.ReadFile(store.path)
	if os.IsNotExist(err) {
		return store, nil
	}
	if err != nil {
		store.dirty = true
		return store, fmt.Errorf("read reasoning provenance: %w", err)
	}
	var persisted reasoningProvenanceFile
	if err := json.Unmarshal(raw, &persisted); err != nil {
		store.dirty = true
		return store, fmt.Errorf("decode reasoning provenance: %w", err)
	}
	if persisted.Version != reasoningProvenanceVersion {
		store.dirty = true
		return store, fmt.Errorf("unsupported reasoning provenance version %d", persisted.Version)
	}
	invalidEntries := 0
	for digest, record := range persisted.Entries {
		if !validSHA256Hex(digest) || record.SeenUnix <= 0 {
			store.dirty = true
			invalidEntries++
			continue
		}
		record.Domains = validUniqueDomains(record.Domains)
		if len(record.Domains) == 0 {
			store.dirty = true
			invalidEntries++
			continue
		}
		store.entries[digest] = record
	}
	store.pruneLocked()
	if invalidEntries > 0 {
		return store, fmt.Errorf("ignored %d invalid reasoning provenance entries", invalidEntries)
	}
	return store, nil
}

func reasoningDomain(route config.Route) string {
	endpoint := strings.TrimSpace(route.OriginBase)
	if parsed, err := url.Parse(endpoint); err == nil && parsed.Host != "" {
		parsed.Scheme = strings.ToLower(parsed.Scheme)
		parsed.Host = strings.ToLower(parsed.Host)
		parsed.User = nil
		parsed.Fragment = ""
		parsed.Path = strings.TrimRight(parsed.Path, "/")
		parsed.RawPath = ""
		parsed.RawQuery = parsed.Query().Encode()
		endpoint = parsed.String()
	}
	payload, _ := json.Marshal(struct {
		Channel  string `json:"channel"`
		Protocol string `json:"protocol"`
		Model    string `json:"model"`
		Endpoint string `json:"endpoint"`
	}{
		Channel:  strings.TrimSpace(route.ChannelID),
		Protocol: strings.ToLower(strings.TrimSpace(route.APIBackend)),
		Model:    strings.TrimSpace(route.WireModel),
		Endpoint: endpoint,
	})
	return sha256Hex(string(payload))
}

// filterReasoningInput leaves ordinary history byte-for-byte equivalent after
// JSON re-encoding and removes only top-level opaque reasoning items that are
// known to belong to another signature domain. Unknown legacy items remain on
// the first attempt and are removed only by the one-shot recovery path.
func filterReasoningInput(
	root map[string]any,
	route config.Route,
	store *reasoningProvenanceStore,
	mode reasoningFilterMode,
) reasoningFilterStats {
	items, ok := root["input"].([]any)
	if !ok || len(items) == 0 {
		return reasoningFilterStats{}
	}
	targetDomain := reasoningDomain(route)
	filtered := make([]any, 0, len(items))
	stats := reasoningFilterStats{}
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		signature := ""
		if item != nil && stringValue(item["type"]) == "reasoning" {
			signature = stringValue(item["encrypted_content"])
		}
		if signature == "" {
			filtered = append(filtered, raw)
			continue
		}

		stats.Opaque++
		known, compatible := store.compatible(signature, targetDomain)
		switch {
		case mode == dropAllOpaqueReasoning:
			stats.Dropped++
		case known && compatible:
			stats.Compatible++
			filtered = append(filtered, raw)
		case known:
			stats.Dropped++
		default:
			stats.Unknown++
			filtered = append(filtered, raw)
		}
	}
	if stats.Dropped > 0 {
		root["input"] = filtered
	}
	return stats
}

func filterReasoningRequest(
	root map[string]any,
	protocol wireProtocol,
	route config.Route,
	store *reasoningProvenanceStore,
	mode reasoningFilterMode,
) reasoningFilterStats {
	if protocol == wireResponses {
		return filterReasoningInput(root, route, store, mode)
	}
	if protocol != wireMessages {
		return reasoningFilterStats{}
	}
	messages, ok := root["messages"].([]any)
	if !ok {
		return reasoningFilterStats{}
	}
	targetDomain := reasoningDomain(route)
	stats := reasoningFilterStats{}
	for _, rawMessage := range messages {
		message, _ := rawMessage.(map[string]any)
		content, ok := message["content"].([]any)
		if !ok {
			continue
		}
		filtered := make([]any, 0, len(content))
		for _, rawBlock := range content {
			block, _ := rawBlock.(map[string]any)
			signature := ""
			if block != nil {
				switch stringValue(block["type"]) {
				case "thinking":
					signature = stringValue(block["signature"])
				case "redacted_thinking":
					signature = stringValue(block["data"])
				}
			}
			if signature == "" {
				filtered = append(filtered, rawBlock)
				continue
			}
			stats.Opaque++
			known, compatible := store.compatible(signature, targetDomain)
			switch {
			case mode == dropAllOpaqueReasoning:
				stats.Dropped++
			case known && compatible:
				stats.Compatible++
				filtered = append(filtered, rawBlock)
			case known:
				stats.Dropped++
			default:
				stats.Unknown++
				filtered = append(filtered, rawBlock)
			}
		}
		if len(filtered) != len(content) {
			message["content"] = filtered
		}
	}
	return stats
}

func isOpaqueReasoningRejection(status int, data []byte) bool {
	if status < 400 || len(data) == 0 {
		return false
	}
	var envelope any
	if json.Unmarshal(data, &envelope) != nil {
		return false
	}
	if root, ok := envelope.(map[string]any); ok {
		if body, exists := root["error"]; exists {
			envelope = body
		} else if stringValue(root["message"]) == "" ||
			(firstString(root, "type", "code") == "" && root["status"] == nil) {
			return false
		}
	} else {
		return false
	}
	var text strings.Builder
	var collect func(any)
	collect = func(value any) {
		switch typed := value.(type) {
		case string:
			text.WriteByte(' ')
			text.WriteString(strings.ToLower(typed))
		case map[string]any:
			for _, child := range typed {
				collect(child)
			}
		case []any:
			for _, child := range typed {
				collect(child)
			}
		}
	}
	collect(envelope)
	message := text.String()
	if strings.Contains(message, "encrypted_content") || strings.Contains(message, "encrypted content") {
		return true
	}
	if strings.Contains(message, "signature") &&
		(strings.Contains(message, "thinking") || strings.Contains(message, "reasoning") ||
			strings.Contains(message, "invalid") || strings.Contains(message, "verify") ||
			strings.Contains(message, "mismatch")) {
		return true
	}
	return (strings.Contains(message, "decrypt") || strings.Contains(message, "decryption")) &&
		(strings.Contains(message, "reasoning") || strings.Contains(message, "content"))
}

func (s *reasoningProvenanceStore) captureCanonical(domain string, value any) int {
	if s == nil || !validSHA256Hex(domain) {
		return 0
	}
	signatures := canonicalReasoningSignatures(value)
	if len(signatures) == 0 {
		return 0
	}

	now := s.now().Unix()
	added := 0
	changed := false
	s.mu.Lock()
	for signature := range signatures {
		digest := sha256Hex(signature)
		record := s.entries[digest]
		if !containsString(record.Domains, domain) {
			record.Domains = append(record.Domains, domain)
			sort.Strings(record.Domains)
			added++
			changed = true
		}
		if record.SeenUnix != now {
			record.SeenUnix = now
			changed = true
		}
		s.entries[digest] = record
	}
	if changed {
		s.dirty = true
		s.pruneLocked()
	}
	s.mu.Unlock()
	return added
}

func (s *reasoningProvenanceStore) compatible(signature, targetDomain string) (known, compatible bool) {
	if s == nil || signature == "" || !validSHA256Hex(targetDomain) {
		return false, false
	}
	s.mu.RLock()
	record, ok := s.entries[sha256Hex(signature)]
	s.mu.RUnlock()
	if !ok {
		return false, false
	}
	return true, containsString(record.Domains, targetDomain)
}

func (s *reasoningProvenanceStore) flush() error {
	if s == nil || s.path == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return nil
	}
	s.pruneLocked()
	persisted := reasoningProvenanceFile{
		Version: reasoningProvenanceVersion,
		Entries: s.entries,
	}
	raw, err := json.Marshal(persisted)
	if err != nil {
		return fmt.Errorf("encode reasoning provenance: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create reasoning provenance directory: %w", err)
	}
	if err := writePrivateFileAtomic(s.path, raw, ".hellogrok-reasoning-*"); err != nil {
		return fmt.Errorf("write reasoning provenance: %w", err)
	}
	s.dirty = false
	return nil
}

func (s *Server) captureReasoningProvenance(route config.Route, value any) {
	if s == nil || s.reasoning == nil {
		return
	}
	if added := s.reasoning.captureCanonical(reasoningDomain(route), value); added > 0 {
		s.log.Printf("UP channel=%s reasoning provenance captured=%d", route.ChannelID, added)
	}
}

func (s *Server) flushReasoningProvenance() {
	if s == nil || s.reasoning == nil {
		return
	}
	if err := s.reasoning.flush(); err != nil {
		s.log.Printf("reasoning provenance flush failed: %v", err)
	}
}

func (s *reasoningProvenanceStore) pruneLocked() {
	for len(s.entries) > maxReasoningProvenanceItems {
		oldestDigest := ""
		oldestSeen := int64(0)
		for digest, record := range s.entries {
			if oldestDigest == "" || record.SeenUnix < oldestSeen ||
				(record.SeenUnix == oldestSeen && digest < oldestDigest) {
				oldestDigest, oldestSeen = digest, record.SeenUnix
			}
		}
		delete(s.entries, oldestDigest)
		s.dirty = true
	}
}

func canonicalReasoningSignatures(value any) map[string]struct{} {
	signatures := map[string]struct{}{}
	var walk func(any)
	walk = func(current any) {
		switch typed := current.(type) {
		case map[string]any:
			typ := stringValue(typed["type"])
			switch typ {
			case "reasoning":
				if signature := stringValue(typed["encrypted_content"]); signature != "" {
					signatures[signature] = struct{}{}
				}
			case "thinking":
				if signature := stringValue(typed["signature"]); signature != "" {
					signatures[signature] = struct{}{}
				}
			case "redacted_thinking":
				if signature := stringValue(typed["data"]); signature != "" {
					signatures[signature] = struct{}{}
				}
			case "signature_delta":
				if signature := stringValue(typed["signature"]); signature != "" {
					signatures[signature] = struct{}{}
				}
			}

			keys := []string(nil)
			switch {
			case typ == "" && stringValue(typed["object"]) == "response":
				keys = []string{"output"}
			case typ == "":
				keys = []string{"output"}
			case typ == "message":
				keys = []string{"content"}
			case strings.HasPrefix(typ, "response.output_item."):
				keys = []string{"item"}
			case typ == "response.created" || typ == "response.in_progress" ||
				typ == "response.completed" || typ == "response.failed" || typ == "response.incomplete":
				keys = []string{"response"}
			case typ == "message_start":
				keys = []string{"message"}
			case typ == "content_block_start":
				keys = []string{"content_block"}
			case typ == "content_block_delta":
				keys = []string{"delta"}
			}
			for _, key := range keys {
				if child, exists := typed[key]; exists {
					walk(child)
				}
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(value)
	return signatures
}

func validUniqueDomains(domains []string) []string {
	seen := map[string]bool{}
	valid := make([]string, 0, len(domains))
	for _, domain := range domains {
		if validSHA256Hex(domain) && !seen[domain] {
			seen[domain] = true
			valid = append(valid, domain)
		}
	}
	sort.Strings(valid)
	return valid
}

func validSHA256Hex(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func writePrivateFileAtomic(path string, data []byte, pattern string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), pattern)
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
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	committed = true
	return nil
}
