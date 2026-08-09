package cfgpatch

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

const (
	ProxyHost    = "127.0.0.1"
	ProxyPort    = "18787"
	stateVersion = 8

	ccSwitchProxyToken = "PROXY_MANAGED"
)

var (
	sectionRe                     = regexp.MustCompile(`^\[([^\]]+)\]\s*(?:#.*)?$`)
	modelSectionRe                = regexp.MustCompile(`^model\.(?:"([^"]+)"|'([^']+)'|(.+))$`)
	baseURLLine                   = regexp.MustCompile(`^(\s*base_url\s*=\s*)(?:"[^"]*"|'[^']*')(\s*(?:#.*)?)?$`)
	apiBaseURLLine                = regexp.MustCompile(`^(\s*api_base_url\s*=\s*)(?:"[^"]*"|'[^']*')(\s*(?:#.*)?)?$`)
	apiBackendLine                = regexp.MustCompile(`^(\s*api_backend\s*=\s*)(?:"[^"]*"|'[^']*')(\s*(?:#.*)?)?$`)
	backendSearchLine             = regexp.MustCompile(`^(\s*supports_backend_search\s*=\s*)(?:true|false)(\s*(?:#.*)?)?$`)
	backendToolsLine              = regexp.MustCompile(`^(\s*backend_tools\s*=\s*)(?:true|false)(\s*(?:#.*)?)?$`)
	webFetchLine                  = regexp.MustCompile(`^(\s*web_fetch\s*=\s*)(?:true|false)(\s*(?:#.*)?)?$`)
	subagentsEnabledLine          = regexp.MustCompile(`^(\s*enabled\s*=\s*)(?:true|false)(\s*(?:#.*)?)?$`)
	subagentsEnabledDottedLine    = regexp.MustCompile(`^(\s*subagents\.enabled\s*=\s*)(?:true|false)(\s*(?:#.*)?)?$`)
	baseURLAnyLine                = regexp.MustCompile(`^(\s*base_url\s*=\s*).*$`)
	apiBaseURLAnyLine             = regexp.MustCompile(`^(\s*api_base_url\s*=\s*).*$`)
	apiBackendAnyLine             = regexp.MustCompile(`^(\s*api_backend\s*=\s*).*$`)
	backendSearchAnyLine          = regexp.MustCompile(`^(\s*supports_backend_search\s*=\s*).*$`)
	backendToolsAnyLine           = regexp.MustCompile(`^(\s*backend_tools\s*=\s*).*$`)
	webFetchAnyLine               = regexp.MustCompile(`^(\s*web_fetch\s*=\s*).*$`)
	subagentsEnabledAnyLine       = regexp.MustCompile(`^(\s*enabled\s*=\s*).*$`)
	subagentsEnabledDottedAnyLine = regexp.MustCompile(`^(\s*subagents\.enabled\s*=\s*).*$`)
)

// Target describes one resolved custom model. APIBaseURL is true when Grok
// could select a separate API-key endpoint, which must be routed through the
// same channel facade to prevent a direct-request bypass. APIBackend is the
// provider's real protocol; BuildAPIBackend is the protocol exposed to Grok
// Build while the facade is active.
type Target struct {
	ID                    string
	APIBaseURL            bool
	APIBackend            string
	BuildAPIBackend       string
	SupportsBackendSearch bool
}

// CCSwitchTakeover identifies CC Switch's Grok Build live-proxy projection.
// Both applications rewrite ~/.grok/config.toml, so this state must not be
// wrapped by hellogrok or treated as an ordinary custom provider.
type CCSwitchTakeover struct {
	ModelID string
	BaseURL string
}

func (t CCSwitchTakeover) Active() bool {
	return t.ModelID != ""
}

// State contains the exact lines replaced by hellogrok. It is written before
// config.toml so an unclean exit can restore every managed field.
type State struct {
	Version    int                   `json:"version"`
	ConfigPath string                `json:"config_path"`
	Models     map[string]ModelState `json:"models"`
	Features   FeatureState          `json:"features,omitempty"`
	Subagents  SubagentState         `json:"subagents,omitempty"`
}

type FeatureState struct {
	SectionCreated bool             `json:"section_created,omitempty"`
	AppendPrefix   string           `json:"append_prefix,omitempty"`
	BackendTools   ManagedLineState `json:"backend_tools"`
	WebFetch       ManagedLineState `json:"web_fetch"`
}

type SubagentState struct {
	SectionCreated    bool             `json:"section_created,omitempty"`
	DottedLineCreated bool             `json:"dotted_line_created,omitempty"`
	Enabled           ManagedLineState `json:"enabled"`
}

type ModelState struct {
	Section    ModelSectionState `json:"section,omitempty"`
	BaseURL    ManagedLineState  `json:"base_url"`
	APIBaseURL ManagedLineState  `json:"api_base_url,omitempty"`
	// APIBackend is nil when the exposed and provider protocols are identical.
	// A pointer also keeps v5/v6 rewrite states decodable during restoration.
	APIBackend    *ManagedLineState `json:"api_backend,omitempty"`
	BackendSearch ManagedLineState  `json:"backend_search"`
}

type ModelSectionState struct {
	Managed      bool   `json:"managed"`
	OriginalLine string `json:"original_line,omitempty"`
	AppliedLine  string `json:"applied_line,omitempty"`
}

type ManagedLineState struct {
	Managed          bool   `json:"managed"`
	Present          bool   `json:"present"`
	OriginalLine     string `json:"original_line,omitempty"`
	AppliedValue     string `json:"applied_value"`
	PreviousLineHash string `json:"previous_line_hash,omitempty"`
}

type ApplyResult struct {
	ModelSections      int
	BaseURLs           int
	APIBaseURLs        int
	APIBackends        int
	BackendSearch      int
	BackendTools       int
	WebFetch           int
	SubagentsEnabled   int
	ValidatedTargets   int
	Targets            []string
	LegacyModelAliases map[string]string
}

// ModelTables returns [model.<id>] entries keyed by their effective ID. TOML
// treats an unquoted dotted key such as [model.foo.bar] as nested tables. Grok
// Build expects dotted model IDs to be quoted, but flattening those leaf tables
// here lets hellogrok repair the header before Grok reloads the config.
func ModelTables(root map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	models, _ := root["model"].(map[string]any)
	for id, value := range models {
		collectModelTables(out, id, value)
	}
	return out
}

func collectModelTables(out map[string]map[string]any, id string, value any) {
	table, ok := value.(map[string]any)
	if !ok {
		return
	}
	if isModelEntryTable(table) {
		out[id] = table
		return
	}
	foundChild := false
	for childID, child := range table {
		if _, ok := child.(map[string]any); !ok {
			continue
		}
		foundChild = true
		collectModelTables(out, id+"."+childID, child)
	}
	if !foundChild {
		out[id] = table
	}
}

func isModelEntryTable(table map[string]any) bool {
	for key, value := range table {
		switch key {
		case "model", "name", "model_provider", "base_url", "api_base_url",
			"api_backend", "api_key", "env_key", "auth_provider", "auth_scheme",
			"supports_backend_search", "extra_headers", "env_http_headers":
			return true
		}
		if _, nested := value.(map[string]any); !nested {
			return true
		}
	}
	return false
}

func StatePath(dataDir string) string {
	return filepath.Join(dataDir, "config_rewrite_state.json")
}

func ToChannelProxyURL(channelID string) (string, error) {
	channelID = strings.TrimSpace(channelID)
	if channelID == "" {
		return "", fmt.Errorf("empty channel id")
	}
	return fmt.Sprintf("http://%s:%s/c/%s", ProxyHost, ProxyPort, url.PathEscape(channelID)), nil
}

func IsProxyURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	host := strings.TrimSpace(u.Hostname())
	ip := net.ParseIP(host)
	loopback := strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback())
	return loopback && u.Port() == ProxyPort &&
		strings.HasPrefix(u.EscapedPath(), "/c/")
}

// DetectCCSwitchTakeover looks for the two fields CC Switch writes together
// when its Grok Build proxy takeover is active. The listen address and port are
// configurable, so detection uses the dedicated route and token marker instead
// of assuming 127.0.0.1:15721.
func DetectCCSwitchTakeover(configPath string) (CCSwitchTakeover, error) {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return CCSwitchTakeover{}, err
	}
	var root map[string]any
	if err := toml.Unmarshal(raw, &root); err != nil {
		return CCSwitchTakeover{}, fmt.Errorf("parse TOML: %w", err)
	}
	models := ModelTables(root)
	if len(models) == 0 {
		return CCSwitchTakeover{}, nil
	}

	ids := make([]string, 0, len(models))
	if selection, _ := root["models"].(map[string]any); selection != nil {
		if id, _ := selection["default"].(string); strings.TrimSpace(id) != "" {
			ids = append(ids, strings.TrimSpace(id))
		}
	}
	remaining := make([]string, 0, len(models))
	for id := range models {
		if len(ids) == 0 || id != ids[0] {
			remaining = append(remaining, id)
		}
	}
	sort.Strings(remaining)
	ids = append(ids, remaining...)

	for _, id := range ids {
		model := models[id]
		apiKey, _ := model["api_key"].(string)
		baseURL, _ := model["base_url"].(string)
		if strings.TrimSpace(apiKey) == ccSwitchProxyToken && isCCSwitchGrokProxyURL(baseURL) {
			return CCSwitchTakeover{ModelID: id, BaseURL: strings.TrimSpace(baseURL)}, nil
		}
	}
	return CCSwitchTakeover{}, nil
}

func isCCSwitchGrokProxyURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !strings.EqualFold(u.Scheme, "http") {
		return false
	}
	return strings.TrimSpace(u.Hostname()) != "" && u.Port() != "" &&
		strings.TrimRight(u.EscapedPath(), "/") == "/grokbuild/v1"
}

// ActiveProxyReferences reports config fields that still point at this
// hellogrok instance. It is used before relinquishing a recovery transaction
// after another application has replaced the live provider configuration.
func ActiveProxyReferences(configPath string) ([]string, error) {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	var root map[string]any
	if err := toml.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("parse TOML: %w", err)
	}
	models := ModelTables(root)
	refs := make([]string, 0)
	for id, model := range models {
		for _, field := range []string{"base_url", "api_base_url"} {
			if endpoint, _ := model[field].(string); IsProxyURL(endpoint) {
				refs = append(refs, id+"."+field)
			}
		}
	}
	sort.Strings(refs)
	return refs, nil
}

// Relinquish removes hellogrok's recovery transaction only when the live
// config no longer contains a hellogrok route. This preserves a complete
// external provider switch without leaving a stale transaction that blocks the
// next start.
func Relinquish(configPath, statePath string) (bool, error) {
	refs, err := ActiveProxyReferences(configPath)
	if err != nil {
		return false, err
	}
	if len(refs) != 0 {
		return false, nil
	}
	if err := os.Remove(statePath); err != nil && !os.IsNotExist(err) {
		return false, err
	}
	return true, nil
}

func ChannelIDFromProxyURL(raw string) string {
	if !IsProxyURL(raw) {
		return ""
	}
	u, _ := url.Parse(strings.TrimSpace(raw))
	parts := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	if len(parts) != 2 || parts[0] != "c" {
		return ""
	}
	id, err := url.PathUnescape(parts[1])
	if err != nil {
		return ""
	}
	return id
}

// ApplyTargets gives every resolved custom model a channel-scoped facade URL,
// projects the protocol Grok Build must consume, materializes its effective
// backend-search capability, and enables Build's backend-tool and web-fetch
// feature gates. It preserves [models].web_search and stream_tool_calls.
func ApplyTargets(configPath, statePath string, targets []Target) (ApplyResult, error) {
	configPath, err := canonicalConfigPath(configPath)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("resolve config path: %w", err)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return ApplyResult{}, err
	}
	targetMap := map[string]Target{}
	for _, target := range targets {
		target.ID = strings.TrimSpace(target.ID)
		if target.ID == "" {
			continue
		}
		target.APIBackend = strings.ToLower(strings.TrimSpace(target.APIBackend))
		if target.APIBackend == "" {
			target.APIBackend = "chat_completions"
		}
		switch target.APIBackend {
		case "responses", "messages", "chat_completions":
		default:
			return ApplyResult{}, fmt.Errorf("model %q uses unsupported api_backend %q", target.ID, target.APIBackend)
		}
		target.BuildAPIBackend = strings.ToLower(strings.TrimSpace(target.BuildAPIBackend))
		if target.BuildAPIBackend == "" {
			target.BuildAPIBackend = target.APIBackend
		}
		switch target.BuildAPIBackend {
		case "responses", "messages", "chat_completions":
		default:
			return ApplyResult{}, fmt.Errorf("model %q uses unsupported Build api_backend %q", target.ID, target.BuildAPIBackend)
		}
		targetMap[target.ID] = target
	}
	if len(targetMap) == 0 {
		return ApplyResult{}, nil
	}
	var initialRoot map[string]any
	if err := toml.Unmarshal(raw, &initialRoot); err != nil {
		return ApplyResult{}, fmt.Errorf("parse TOML: %w", err)
	}
	if err := validateTargetBackendSearchValues(initialRoot, targetMap); err != nil {
		return ApplyResult{}, err
	}
	if err := validateSubagentEnabledValue(initialRoot); err != nil {
		return ApplyResult{}, err
	}

	state := State{Version: stateVersion, ConfigPath: configPath, Models: map[string]ModelState{}}
	if existing, readErr := os.ReadFile(statePath); readErr == nil {
		if err := json.Unmarshal(existing, &state); err != nil {
			return ApplyResult{}, fmt.Errorf("decode rewrite state: %w", err)
		}
		if !supportedStateVersion(state.Version) {
			return ApplyResult{}, fmt.Errorf("unsupported rewrite state version %d", state.Version)
		}
		if state.Models == nil {
			return ApplyResult{}, fmt.Errorf("invalid rewrite state: missing models")
		}
		if !sameConfigPath(state.ConfigPath, configPath) {
			return ApplyResult{}, fmt.Errorf("rewrite state belongs to %q, not %q", state.ConfigPath, configPath)
		}
		original, err := matchesOriginalManagedState(raw, state)
		if err != nil {
			return ApplyResult{}, fmt.Errorf("validate existing rewrite state: %w", err)
		}
		if original {
			if err := os.Remove(statePath); err != nil && !os.IsNotExist(err) {
				return ApplyResult{}, fmt.Errorf("discard unapplied rewrite state: %w", err)
			}
			state = State{Version: stateVersion, ConfigPath: configPath, Models: map[string]ModelState{}}
		} else if err := validateRestorableConfig(raw, state); err != nil {
			return ApplyResult{}, fmt.Errorf("existing rewrite state conflicts with config: %w", err)
		}
	}
	state.Version = stateVersion
	state.ConfigPath = configPath

	text, subagentEnabled, err := rewriteSubagentEnabled(string(raw), initialRoot, &state)
	if err != nil {
		return ApplyResult{}, err
	}
	text, featureResult, err := rewriteFeatureFlags(text, &state)
	if err != nil {
		return ApplyResult{}, err
	}
	text, result, found, err := rewriteConfig(text, targetMap, &state)
	if err != nil {
		return ApplyResult{}, err
	}
	result.BackendTools = featureResult.BackendTools
	result.WebFetch = featureResult.WebFetch
	result.SubagentsEnabled = subagentEnabled
	for id := range targetMap {
		if !found[id] {
			return ApplyResult{}, fmt.Errorf("model section %q disappeared while preparing rewrite", id)
		}
		result.Targets = append(result.Targets, id)
	}
	for legacyID, targetID := range result.LegacyModelAliases {
		_, collidesWithTarget := targetMap[legacyID]
		if targetID == "" || collidesWithTarget {
			delete(result.LegacyModelAliases, legacyID)
		}
	}
	sort.Strings(result.Targets)
	if err := validateManagedConfig([]byte(text), targetMap, state); err != nil {
		return ApplyResult{}, fmt.Errorf("validate prepared config: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
		return ApplyResult{}, fmt.Errorf("create state directory: %w", err)
	}
	encoded, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return ApplyResult{}, fmt.Errorf("encode rewrite state: %w", err)
	}
	if err := writeFileAtomic(statePath, encoded, 0o600); err != nil {
		return ApplyResult{}, fmt.Errorf("write rewrite state: %w", err)
	}
	persistedState, err := os.ReadFile(statePath)
	if err != nil {
		return ApplyResult{}, discardUncommittedState(statePath, fmt.Errorf("read rewrite state after write: %w", err))
	}
	if !bytes.Equal(persistedState, encoded) {
		return ApplyResult{}, discardUncommittedState(statePath, fmt.Errorf("rewrite state read-back mismatch"))
	}
	if err := writeFileAtomic(configPath, []byte(text), existingFileMode(configPath, 0o600)); err != nil {
		return ApplyResult{}, rollbackAppliedConfig(configPath, statePath, fmt.Errorf("write config: %w", err))
	}
	written, err := os.ReadFile(configPath)
	if err != nil {
		return ApplyResult{}, rollbackAppliedConfig(configPath, statePath, fmt.Errorf("read config after rewrite: %w", err))
	}
	if err := validateManagedConfig(written, targetMap, state); err != nil {
		return ApplyResult{}, rollbackAppliedConfig(configPath, statePath, fmt.Errorf("validate written config: %w", err))
	}
	result.ValidatedTargets = len(targetMap)
	return result, nil
}

func validateManagedConfig(raw []byte, targets map[string]Target, state State) error {
	var root map[string]any
	if err := toml.Unmarshal(raw, &root); err != nil {
		return fmt.Errorf("parse TOML: %w", err)
	}
	features, ok := root["features"].(map[string]any)
	if !ok {
		return fmt.Errorf("missing [features] table")
	}
	if value, ok := features["backend_tools"].(bool); !ok || !value {
		return fmt.Errorf("[features].backend_tools must be true")
	}
	if value, ok := features["web_fetch"].(bool); !ok || !value {
		return fmt.Errorf("[features].web_fetch must be true")
	}
	if state.Subagents.Enabled.Managed {
		subagents, _ := root["subagents"].(map[string]any)
		if value, ok := subagents["enabled"].(bool); !ok || !value {
			return fmt.Errorf("[subagents].enabled must be true when subagent defaults are repaired")
		}
	}

	models := ModelTables(root)
	if len(models) == 0 {
		return fmt.Errorf("missing [model.*] tables")
	}
	ids := make([]string, 0, len(targets))
	for id := range targets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	sectionLines := modelSectionLines(raw)
	for _, id := range ids {
		model, ok := models[id]
		if !ok {
			return fmt.Errorf("missing [model.%s] table", id)
		}
		if section := state.Models[id].Section; section.Managed && sectionLines[id] != section.AppliedLine {
			return fmt.Errorf("[model.%s] header changed while preparing config", id)
		}
		proxyURL, err := ToChannelProxyURL(id)
		if err != nil {
			return err
		}
		if value, ok := model["base_url"].(string); !ok || value != proxyURL {
			return fmt.Errorf("[model.%s].base_url must be %q", id, proxyURL)
		}
		if targets[id].APIBaseURL {
			if value, ok := model["api_base_url"].(string); !ok || value != proxyURL {
				return fmt.Errorf("[model.%s].api_base_url must be %q", id, proxyURL)
			}
		}
		backend, err := effectiveModelBackend(root, model)
		if err != nil {
			return fmt.Errorf("[model.%s].api_backend %w", id, err)
		}
		if backend != targets[id].BuildAPIBackend {
			return fmt.Errorf("[model.%s].api_backend must be %q, got %q", id, targets[id].BuildAPIBackend, backend)
		}
		if value, ok := model["supports_backend_search"].(bool); !ok || value != targets[id].SupportsBackendSearch {
			return fmt.Errorf("[model.%s].supports_backend_search must be %t", id, targets[id].SupportsBackendSearch)
		}
	}
	return nil
}

func effectiveModelBackend(root, model map[string]any) (string, error) {
	value, exists := model["api_backend"]
	if !exists {
		providerID, _ := model["model_provider"].(string)
		providers, _ := root["model_providers"].(map[string]any)
		provider, _ := providers[strings.TrimSpace(providerID)].(map[string]any)
		value, exists = provider["api_backend"]
	}
	if !exists {
		return "chat_completions", nil
	}
	backend, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("must be a string")
	}
	backend = strings.ToLower(strings.TrimSpace(backend))
	switch backend {
	case "responses", "messages", "chat_completions":
		return backend, nil
	default:
		return "", fmt.Errorf("uses unsupported value %q", backend)
	}
}

func validateTargetBackendSearchValues(root map[string]any, targets map[string]Target) error {
	models := ModelTables(root)
	providers, _ := root["model_providers"].(map[string]any)
	for id := range targets {
		model := models[id]
		if model == nil {
			continue
		}
		if value, exists := model["supports_backend_search"]; exists {
			if _, ok := value.(bool); !ok {
				return fmt.Errorf("[model.%s].supports_backend_search must be a boolean", id)
			}
			continue
		}
		providerID, _ := model["model_provider"].(string)
		providerID = strings.TrimSpace(providerID)
		provider, _ := providers[providerID].(map[string]any)
		if value, exists := provider["supports_backend_search"]; exists {
			if _, ok := value.(bool); !ok {
				return fmt.Errorf("[model_providers.%s].supports_backend_search must be a boolean (used by [model.%s])", providerID, id)
			}
		}
	}
	return nil
}

func validateSubagentEnabledValue(root map[string]any) error {
	subagents, _ := root["subagents"].(map[string]any)
	if subagents == nil {
		return nil
	}
	if value, exists := subagents["enabled"]; exists {
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("[subagents].enabled must be a boolean")
		}
	}
	return nil
}

func rollbackAppliedConfig(configPath, statePath string, cause error) error {
	if _, err := Restore(configPath, statePath); err != nil {
		return fmt.Errorf("%w; automatic config rollback failed: %v", cause, err)
	}
	return cause
}

func discardUncommittedState(statePath string, cause error) error {
	if err := os.Remove(statePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("%w; remove uncommitted rewrite state: %v", cause, err)
	}
	return cause
}

func supportedStateVersion(version int) bool {
	return version == 5 || version == 6 || version == 7 || version == stateVersion
}

// rewriteSubagentEnabled repairs a Grok Build 0.2.118 defaulting bug. When a
// [subagents] tree exists only to configure models/toggles, an omitted enabled
// value is deserialized as false even though the documented default is true.
// Explicit true and false values remain user-owned and are never changed.
func rewriteSubagentEnabled(text string, root map[string]any, state *State) (string, int, error) {
	subagents, _ := root["subagents"].(map[string]any)
	if subagents == nil {
		return text, 0, nil
	}
	if _, exists := subagents["enabled"]; exists {
		return text, 0, nil
	}

	lines := splitKeepNL(text)
	structural := tomlStructuralLines(lines)
	ending := preferredLineEnding(text)
	sectionStart := -1
	sectionEnd := -1
	firstChildSection := -1
	for index, line := range lines {
		if !structural[index] {
			continue
		}
		section := sectionRe.FindStringSubmatch(strings.TrimSpace(line))
		if section == nil {
			continue
		}
		name := strings.TrimSpace(section[1])
		if name == "subagents" {
			sectionStart = index
			continue
		}
		if sectionStart >= 0 {
			sectionEnd = index
			break
		}
		if firstChildSection < 0 && strings.HasPrefix(name, "subagents.") {
			firstChildSection = index
		}
	}
	if sectionStart >= 0 {
		if sectionEnd < 0 {
			sectionEnd = len(lines)
		}
		block := append([]string(nil), lines[sectionStart:sectionEnd]...)
		changed := 0
		fields := []managedField{{
			name:       "enabled",
			pattern:    subagentsEnabledLine,
			anyPattern: subagentsEnabledAnyLine,
			value:      "true",
			state:      &state.Subagents.Enabled,
			changed:    &changed,
		}}
		block = rewriteManagedFields(block, 1, fields, ending)
		updated := make([]string, 0, len(lines)+1)
		updated = append(updated, lines[:sectionStart]...)
		updated = append(updated, block...)
		updated = append(updated, lines[sectionEnd:]...)
		return strings.Join(updated, ""), changed, nil
	}

	state.Subagents.Enabled = ManagedLineState{Managed: true, Present: false, AppliedValue: "true"}
	if firstChildSection >= 0 {
		state.Subagents.SectionCreated = true
		inserted := []string{"[subagents]" + ending, "enabled = true" + ending}
		updated := make([]string, 0, len(lines)+len(inserted))
		updated = append(updated, lines[:firstChildSection]...)
		updated = append(updated, inserted...)
		updated = append(updated, lines[firstChildSection:]...)
		return strings.Join(updated, ""), 1, nil
	}

	// Dotted keys belong to the root table. Append the default at that table's
	// footer; inline sealed tables reject the candidate instead of being
	// reformatted destructively.
	rootEnd := len(lines)
	for index, line := range lines {
		if structural[index] && sectionRe.MatchString(strings.TrimSpace(line)) {
			rootEnd = index
			break
		}
	}
	rootBlock := append([]string(nil), lines[:rootEnd]...)
	changed := 0
	fields := []managedField{{
		name:       "subagents.enabled",
		pattern:    subagentsEnabledDottedLine,
		anyPattern: subagentsEnabledDottedAnyLine,
		value:      "true",
		state:      &state.Subagents.Enabled,
		changed:    &changed,
	}}
	rootBlock = rewriteManagedFields(rootBlock, 0, fields, ending)
	candidateLines := make([]string, 0, len(lines)+1)
	candidateLines = append(candidateLines, rootBlock...)
	candidateLines = append(candidateLines, lines[rootEnd:]...)
	candidate := strings.Join(candidateLines, "")
	var candidateRoot map[string]any
	if err := toml.Unmarshal([]byte(candidate), &candidateRoot); err != nil {
		return text, 0, fmt.Errorf("repair omitted [subagents].enabled: unsupported inline subagents table: %w", err)
	}
	patchedSubagents, _ := candidateRoot["subagents"].(map[string]any)
	if value, ok := patchedSubagents["enabled"].(bool); !ok || !value {
		return text, 0, fmt.Errorf("repair omitted [subagents].enabled: dotted key did not produce a boolean")
	}
	state.Subagents.DottedLineCreated = true
	return candidate, changed, nil
}

func rewriteFeatureFlags(text string, state *State) (string, ApplyResult, error) {
	lines := splitKeepNL(text)
	structural := tomlStructuralLines(lines)
	ending := preferredLineEnding(text)
	if ending == "" {
		ending = "\n"
	}
	result := ApplyResult{}
	sectionStart := -1
	sectionEnd := -1
	for index, line := range lines {
		if !structural[index] {
			continue
		}
		section := sectionRe.FindStringSubmatch(strings.TrimSpace(line))
		if section == nil {
			continue
		}
		if sectionStart >= 0 {
			sectionEnd = index
			break
		}
		if strings.EqualFold(strings.TrimSpace(section[1]), "features") {
			sectionStart = index
		}
	}
	if sectionStart >= 0 && sectionEnd < 0 {
		sectionEnd = len(lines)
	}
	if sectionStart < 0 {
		prefix := ""
		if text != "" && !strings.HasSuffix(text, "\n") {
			prefix = ending
		}
		state.Features.SectionCreated = true
		state.Features.AppendPrefix = prefix
		text += prefix + "[features]" + ending
		lines = splitKeepNL(text)
		sectionStart = len(lines) - 1
		sectionEnd = len(lines)
	}

	block := append([]string(nil), lines[sectionStart:sectionEnd]...)
	fields := []managedField{
		{name: "backend_tools", pattern: backendToolsLine, anyPattern: backendToolsAnyLine, value: "true", state: &state.Features.BackendTools, changed: &result.BackendTools},
		{name: "web_fetch", pattern: webFetchLine, anyPattern: webFetchAnyLine, value: "true", state: &state.Features.WebFetch, changed: &result.WebFetch},
	}
	block = rewriteManagedFields(block, 1, fields, ending)
	updated := make([]string, 0, len(lines)+len(block)-(sectionEnd-sectionStart))
	updated = append(updated, lines[:sectionStart]...)
	updated = append(updated, block...)
	updated = append(updated, lines[sectionEnd:]...)
	return strings.Join(updated, ""), result, nil
}

func rewriteConfig(text string, targets map[string]Target, state *State) (string, ApplyResult, map[string]bool, error) {
	lines := splitKeepNL(text)
	structural := tomlStructuralLines(lines)
	ending := preferredLineEnding(text)
	result := ApplyResult{LegacyModelAliases: map[string]string{}}
	found := map[string]bool{}
	out := make([]string, 0, len(lines)+len(targets)*3)

	for i := 0; i < len(lines); {
		if !structural[i] {
			out = append(out, lines[i])
			i++
			continue
		}
		id := modelSectionID(lines[i])
		if id == "" {
			out = append(out, lines[i])
			i++
			continue
		}
		end := i + 1
		for end < len(lines) && (!structural[end] || sectionRe.FindStringSubmatch(strings.TrimSpace(lines[end])) == nil) {
			end++
		}
		block := append([]string(nil), lines[i:end]...)
		target, ok := targets[id]
		if ok {
			found[id] = true
			var err error
			block, err = rewriteModelBlock(block, id, target, state, ending, &result)
			if err != nil {
				return "", ApplyResult{}, nil, err
			}
		}
		out = append(out, block...)
		i = end
	}
	return strings.Join(out, ""), result, found, nil
}

type managedField struct {
	name       string
	pattern    *regexp.Regexp
	anyPattern *regexp.Regexp
	value      string
	state      *ManagedLineState
	changed    *int
}

func replacementForManagedLine(bare, original string, field managedField) (string, bool) {
	if match := field.pattern.FindStringSubmatch(bare); match != nil {
		return match[1] + field.value + match[2] + lineEnding(original), true
	}
	if match := field.anyPattern.FindStringSubmatch(bare); match != nil {
		return match[1] + field.value + lineEnding(original), true
	}
	return "", false
}

func rewriteModelBlock(block []string, id string, target Target, state *State, ending string, result *ApplyResult) ([]string, error) {
	proxyURL, err := ToChannelProxyURL(id)
	if err != nil {
		return nil, err
	}
	modelState := state.Models[id]
	if len(block) == 0 {
		return nil, fmt.Errorf("model section %q is empty", id)
	}
	if applied, changed := canonicalModelSectionLine(block[0], id); changed {
		if !modelState.Section.Managed {
			modelState.Section = ModelSectionState{
				Managed:      true,
				OriginalLine: block[0],
				AppliedLine:  applied,
			}
			result.ModelSections++
		}
		legacyID := strings.SplitN(id, ".", 2)[0]
		if existing, found := result.LegacyModelAliases[legacyID]; !found {
			result.LegacyModelAliases[legacyID] = id
		} else if existing != id {
			result.LegacyModelAliases[legacyID] = ""
		}
		block[0] = applied
	}
	fields := []managedField{
		{name: "base_url", pattern: baseURLLine, anyPattern: baseURLAnyLine, value: quoteTOML(proxyURL), state: &modelState.BaseURL, changed: &result.BaseURLs},
	}
	if target.APIBaseURL {
		fields = append(fields, managedField{name: "api_base_url", pattern: apiBaseURLLine, anyPattern: apiBaseURLAnyLine, value: quoteTOML(proxyURL), state: &modelState.APIBaseURL, changed: &result.APIBaseURLs})
	}
	if target.BuildAPIBackend != target.APIBackend || modelState.APIBackend != nil {
		if modelState.APIBackend == nil {
			modelState.APIBackend = &ManagedLineState{}
		}
		fields = append(fields, managedField{
			name: "api_backend", pattern: apiBackendLine, anyPattern: apiBackendAnyLine,
			value: quoteTOML(target.BuildAPIBackend), state: modelState.APIBackend, changed: &result.APIBackends,
		})
	}
	fields = append(fields,
		managedField{name: "supports_backend_search", pattern: backendSearchLine, anyPattern: backendSearchAnyLine, value: fmt.Sprintf("%t", target.SupportsBackendSearch), state: &modelState.BackendSearch, changed: &result.BackendSearch},
	)

	block = rewriteManagedFields(block, 1, fields, ending)
	state.Models[id] = modelState
	return block, nil
}

// rewriteManagedFields updates existing managed values in place and appends
// missing values after the table's last field, before trailing comments and
// blank lines. firstContent is 1 for a named table and 0 for the root table.
func rewriteManagedFields(block []string, firstContent int, fields []managedField, ending string) []string {
	found := make(map[string]bool, len(fields))
	structural := tomlStructuralLines(block)
	for index := firstContent; index < len(block); index++ {
		if !structural[index] {
			continue
		}
		bare := strings.TrimRight(block[index], "\r\n")
		for fieldIndex := range fields {
			field := &fields[fieldIndex]
			replacement, matches := replacementForManagedLine(bare, block[index], *field)
			if !matches {
				continue
			}
			found[field.name] = true
			if !field.state.Managed {
				field.state.Managed = true
				field.state.Present = true
				field.state.OriginalLine = block[index]
			}
			field.state.AppliedValue = managedSemanticValue(field.value)
			if block[index] != replacement {
				*field.changed++
				block[index] = replacement
			}
			break
		}
	}

	insertAt := managedFieldFooterInsertAt(block, firstContent)
	for index := range fields {
		field := &fields[index]
		if found[field.name] {
			continue
		}
		if !field.state.Managed {
			*field.state = ManagedLineState{Managed: true, Present: false}
		}
		field.state.AppliedValue = managedSemanticValue(field.value)
		if insertAt > 0 && lineEnding(block[insertAt-1]) == "" {
			if field.state.PreviousLineHash == "" {
				field.state.PreviousLineHash = lineFingerprint(block[insertAt-1])
			}
			block[insertAt-1] += ending
		}
		block = insertBlockLine(block, insertAt, field.name+" = "+field.value+ending)
		insertAt++
		*field.changed++
	}
	return block
}

func insertBlockLine(block []string, index int, line string) []string {
	block = append(block, "")
	copy(block[index+1:], block[index:])
	block[index] = line
	return block
}

func managedFieldFooterInsertAt(block []string, firstContent int) int {
	insertAt := len(block)
	structural := tomlStructuralLines(block)
	for insertAt > firstContent {
		index := insertAt - 1
		if !structural[index] {
			break
		}
		bare := strings.TrimSpace(strings.TrimRight(block[index], "\r\n"))
		if bare != "" && !strings.HasPrefix(bare, "#") {
			break
		}
		insertAt--
	}
	return insertAt
}

// Restore puts every managed line back exactly as it appeared before startup.
func Restore(configPath, statePath string) (int, error) {
	encoded, err := os.ReadFile(statePath)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var state State
	if err := json.Unmarshal(encoded, &state); err != nil {
		return 0, fmt.Errorf("decode rewrite state: %w", err)
	}
	if !supportedStateVersion(state.Version) || state.Models == nil {
		return 0, fmt.Errorf("unsupported rewrite state version %d", state.Version)
	}
	configPath, err = canonicalConfigPath(configPath)
	if err != nil {
		return 0, fmt.Errorf("resolve config path: %w", err)
	}
	if !sameConfigPath(state.ConfigPath, configPath) {
		return 0, fmt.Errorf("rewrite state belongs to %q, not %q", state.ConfigPath, configPath)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return 0, err
	}
	original, err := matchesOriginalManagedState(raw, state)
	if err != nil {
		return 0, fmt.Errorf("validate original managed values: %w", err)
	}
	if original {
		if err := os.Remove(statePath); err != nil && !os.IsNotExist(err) {
			return 0, err
		}
		return 0, nil
	}
	if err := validateRestorableConfig(raw, state); err != nil {
		return 0, fmt.Errorf("config changed in a proxy-managed field; refusing to overwrite it: %w", err)
	}
	lines := splitKeepNL(string(raw))
	structural := tomlStructuralLines(lines)
	out := make([]string, 0, len(lines))
	restored := 0
	for i := 0; i < len(lines); {
		if !structural[i] {
			out = append(out, lines[i])
			i++
			continue
		}
		id := modelSectionID(lines[i])
		if id == "" {
			out = append(out, lines[i])
			i++
			continue
		}
		end := i + 1
		for end < len(lines) && (!structural[end] || sectionRe.FindStringSubmatch(strings.TrimSpace(lines[end])) == nil) {
			end++
		}
		block := append([]string(nil), lines[i:end]...)
		if modelState, ok := state.Models[id]; ok {
			block, restored = restoreModelBlock(block, modelState, restored, end == len(lines))
			if modelState.Section.Managed {
				block[0] = modelState.Section.OriginalLine
				restored++
			}
		}
		out = append(out, block...)
		i = end
	}
	text, featureRestored := restoreFeatureFlags(strings.Join(out, ""), state.Features)
	restored += featureRestored
	text, subagentRestored := restoreSubagentEnabled(text, state.Subagents)
	restored += subagentRestored
	if err := writeFileAtomic(configPath, []byte(text), existingFileMode(configPath, 0o600)); err != nil {
		return 0, err
	}
	if err := os.Remove(statePath); err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	return restored, nil
}

// matchesOriginalManagedState identifies the crash window after the recovery
// record was committed but before config.toml was atomically replaced. Every
// managed value must match its exact pre-apply semantic value; a mixed state is
// still treated as a user conflict.
func matchesOriginalManagedState(raw []byte, state State) (bool, error) {
	var root map[string]any
	if err := toml.Unmarshal(raw, &root); err != nil {
		return false, fmt.Errorf("parse TOML: %w", err)
	}
	features, _ := root["features"].(map[string]any)
	for _, check := range []managedValueCheck{
		{key: "backend_tools", state: state.Features.BackendTools},
		{key: "web_fetch", state: state.Features.WebFetch},
	} {
		matches, err := managedValueMatchesOriginal(features, check)
		if err != nil || !matches {
			return false, err
		}
	}
	subagents, _ := root["subagents"].(map[string]any)
	if matches, err := managedValueMatchesOriginal(subagents, managedValueCheck{
		key: "enabled", state: state.Subagents.Enabled,
	}); err != nil || !matches {
		return false, err
	}

	models := ModelTables(root)
	sectionLines := modelSectionLines(raw)
	for id, modelState := range state.Models {
		if modelState.Section.Managed && sectionLines[id] != modelState.Section.OriginalLine {
			return false, nil
		}
		model := models[id]
		for _, check := range []managedValueCheck{
			{key: "base_url", state: modelState.BaseURL},
			{key: "api_base_url", state: modelState.APIBaseURL},
			{key: "api_backend", state: legacyManagedState(modelState.APIBackend)},
			{key: "supports_backend_search", state: modelState.BackendSearch},
		} {
			matches, err := managedValueMatchesOriginal(model, check)
			if err != nil || !matches {
				return false, err
			}
		}
	}
	return true, nil
}

func managedValueMatchesOriginal(table map[string]any, check managedValueCheck) (bool, error) {
	if !check.state.Managed {
		return true, nil
	}
	current, exists := table[check.key]
	if !check.state.Present {
		return !exists, nil
	}
	expected, err := originalManagedValue(check.key, check.state.OriginalLine)
	if err != nil {
		return false, err
	}
	return fmt.Sprint(current) == fmt.Sprint(expected), nil
}

func originalManagedValue(key, line string) (any, error) {
	var root map[string]any
	if err := toml.Unmarshal([]byte("[managed]\n"+line), &root); err != nil {
		return nil, fmt.Errorf("parse original %s line: %w", key, err)
	}
	table, _ := root["managed"].(map[string]any)
	value, ok := table[key]
	if !ok {
		return nil, fmt.Errorf("original %s line does not define the managed key", key)
	}
	return value, nil
}

func canonicalConfigPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

func sameConfigPath(left, right string) bool {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func managedSemanticValue(rendered string) string {
	var decoded string
	if json.Unmarshal([]byte(rendered), &decoded) == nil {
		return decoded
	}
	return strings.TrimSpace(rendered)
}

func validateRestorableConfig(raw []byte, state State) error {
	var root map[string]any
	if err := toml.Unmarshal(raw, &root); err != nil {
		return fmt.Errorf("parse TOML: %w", err)
	}
	features, _ := root["features"].(map[string]any)
	if err := validateManagedTable("[features]", features, []managedValueCheck{
		{key: "backend_tools", state: state.Features.BackendTools},
		{key: "web_fetch", state: state.Features.WebFetch},
	}); err != nil {
		return err
	}
	subagents, _ := root["subagents"].(map[string]any)
	if err := validateManagedTable("[subagents]", subagents, []managedValueCheck{
		{key: "enabled", state: state.Subagents.Enabled},
	}); err != nil {
		return err
	}
	models := ModelTables(root)
	sectionLines := modelSectionLines(raw)
	ids := make([]string, 0, len(state.Models))
	for id := range state.Models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		model := models[id]
		modelState := state.Models[id]
		// Removing an entire model while the proxy is active is an unambiguous
		// user-owned channel deletion. There is no proxy URL left to restore for
		// that model, so preserve the deletion while restoring all remaining
		// managed fields. Edits inside an existing model still fail closed below.
		if model == nil && sectionLines[id] == "" {
			continue
		}
		if modelState.Section.Managed && sectionLines[id] != modelState.Section.AppliedLine {
			return fmt.Errorf("[model.%s] header changed while proxy was active", id)
		}
		if err := validateManagedTable(fmt.Sprintf("[model.%s]", id), model, []managedValueCheck{
			{key: "base_url", state: modelState.BaseURL},
			{key: "api_base_url", state: modelState.APIBaseURL},
			{key: "api_backend", state: legacyManagedState(modelState.APIBackend)},
			{key: "supports_backend_search", state: modelState.BackendSearch},
		}); err != nil {
			return err
		}
	}
	return nil
}

type managedValueCheck struct {
	key   string
	state ManagedLineState
}

func validateManagedTable(section string, table map[string]any, checks []managedValueCheck) error {
	for _, check := range checks {
		if !check.state.Managed {
			continue
		}
		if check.state.AppliedValue == "" {
			return fmt.Errorf("rewrite state for %s.%s has no applied value", section, check.key)
		}
		current, exists := table[check.key]
		if !exists {
			if check.state.Present {
				return fmt.Errorf("%s.%s was removed", section, check.key)
			}
			continue
		}
		var value string
		switch typed := current.(type) {
		case string:
			value = typed
		case bool:
			value = fmt.Sprintf("%t", typed)
		default:
			return fmt.Errorf("%s.%s has an unexpected type", section, check.key)
		}
		if value != check.state.AppliedValue {
			return fmt.Errorf("%s.%s is %q, expected proxy value %q", section, check.key, value, check.state.AppliedValue)
		}
	}
	return nil
}

func restoreFeatureFlags(text string, state FeatureState) (string, int) {
	if !state.BackendTools.Managed && !state.WebFetch.Managed && !state.SectionCreated {
		return text, 0
	}
	lines := splitKeepNL(text)
	structural := tomlStructuralLines(lines)
	sectionStart := -1
	sectionEnd := -1
	for index, line := range lines {
		if !structural[index] {
			continue
		}
		section := sectionRe.FindStringSubmatch(strings.TrimSpace(line))
		if section == nil {
			continue
		}
		if sectionStart >= 0 {
			sectionEnd = index
			break
		}
		if strings.EqualFold(strings.TrimSpace(section[1]), "features") {
			sectionStart = index
		}
	}
	if sectionStart < 0 {
		return text, 0
	}
	if sectionEnd < 0 {
		sectionEnd = len(lines)
	}
	block := append([]string(nil), lines[sectionStart:sectionEnd]...)
	restored := 0
	for _, field := range []struct {
		pattern    *regexp.Regexp
		anyPattern *regexp.Regexp
		state      ManagedLineState
	}{
		{backendToolsLine, backendToolsAnyLine, state.BackendTools},
		{webFetchLine, webFetchAnyLine, state.WebFetch},
	} {
		if !field.state.Managed {
			continue
		}
		found := false
		next := make([]string, 0, len(block))
		blockStructural := tomlStructuralLines(block)
		for index, line := range block {
			bare := strings.TrimRight(line, "\r\n")
			if !blockStructural[index] || (!field.pattern.MatchString(bare) && !field.anyPattern.MatchString(bare)) {
				next = append(next, line)
				continue
			}
			if field.state.Present && !found {
				next = append(next, field.state.OriginalLine)
			}
			found = true
			restored++
		}
		if field.state.Present && !found {
			next = append(next[:1], append([]string{field.state.OriginalLine}, next[1:]...)...)
			restored++
		}
		block = next
	}
	if sectionEnd == len(lines) {
		restoreTerminalBlockEnding(block, state.BackendTools, state.WebFetch)
	}
	removeCreatedSection := state.SectionCreated && featureSectionHasNoContent(block)
	updated := make([]string, 0, len(lines))
	updated = append(updated, lines[:sectionStart]...)
	if !removeCreatedSection {
		updated = append(updated, block...)
	} else {
		updated = append(updated, block[1:]...)
		restored++
	}
	updated = append(updated, lines[sectionEnd:]...)
	result := strings.Join(updated, "")
	if removeCreatedSection && state.AppendPrefix != "" && strings.HasSuffix(result, state.AppendPrefix) {
		result = strings.TrimSuffix(result, state.AppendPrefix)
	}
	return result, restored
}

func restoreSubagentEnabled(text string, state SubagentState) (string, int) {
	if !state.Enabled.Managed && !state.SectionCreated && !state.DottedLineCreated {
		return text, 0
	}
	lines := splitKeepNL(text)
	structural := tomlStructuralLines(lines)
	restored := 0

	if state.DottedLineCreated {
		updated := make([]string, 0, len(lines))
		for index, line := range lines {
			bare := strings.TrimRight(line, "\r\n")
			if structural[index] && (subagentsEnabledDottedLine.MatchString(bare) || subagentsEnabledDottedAnyLine.MatchString(bare)) {
				restored++
				continue
			}
			updated = append(updated, line)
		}
		restoreTerminalBlockEnding(updated, state.Enabled)
		return strings.Join(updated, ""), restored
	}

	sectionStart := -1
	sectionEnd := -1
	for index, line := range lines {
		if !structural[index] {
			continue
		}
		section := sectionRe.FindStringSubmatch(strings.TrimSpace(line))
		if section == nil {
			continue
		}
		if sectionStart >= 0 {
			sectionEnd = index
			break
		}
		if strings.TrimSpace(section[1]) == "subagents" {
			sectionStart = index
		}
	}
	if sectionStart < 0 {
		return text, restored
	}
	if sectionEnd < 0 {
		sectionEnd = len(lines)
	}

	block := append([]string(nil), lines[sectionStart:sectionEnd]...)
	if state.Enabled.Managed {
		found := false
		next := make([]string, 0, len(block))
		blockStructural := tomlStructuralLines(block)
		for index, line := range block {
			bare := strings.TrimRight(line, "\r\n")
			if !blockStructural[index] || (!subagentsEnabledLine.MatchString(bare) && !subagentsEnabledAnyLine.MatchString(bare)) {
				next = append(next, line)
				continue
			}
			if state.Enabled.Present && !found {
				next = append(next, state.Enabled.OriginalLine)
			}
			found = true
			restored++
		}
		if state.Enabled.Present && !found {
			next = append(next[:1], append([]string{state.Enabled.OriginalLine}, next[1:]...)...)
			restored++
		}
		block = next
	}
	if sectionEnd == len(lines) {
		restoreTerminalBlockEnding(block, state.Enabled)
	}
	removeCreatedSection := state.SectionCreated && featureSectionHasNoContent(block)
	updated := make([]string, 0, len(lines))
	updated = append(updated, lines[:sectionStart]...)
	if removeCreatedSection {
		updated = append(updated, block[1:]...)
		restored++
	} else {
		updated = append(updated, block...)
	}
	updated = append(updated, lines[sectionEnd:]...)
	return strings.Join(updated, ""), restored
}

func featureSectionHasNoContent(block []string) bool {
	if len(block) == 0 {
		return false
	}
	for _, line := range block[1:] {
		if strings.TrimSpace(line) != "" {
			return false
		}
	}
	return true
}

func restoreModelBlock(block []string, modelState ModelState, restored int, finalBlock bool) ([]string, int) {
	fields := []struct {
		pattern    *regexp.Regexp
		anyPattern *regexp.Regexp
		state      ManagedLineState
	}{
		{baseURLLine, baseURLAnyLine, modelState.BaseURL},
		{apiBaseURLLine, apiBaseURLAnyLine, modelState.APIBaseURL},
		{backendSearchLine, backendSearchAnyLine, modelState.BackendSearch},
	}
	if modelState.APIBackend != nil {
		fields = append(fields, struct {
			pattern    *regexp.Regexp
			anyPattern *regexp.Regexp
			state      ManagedLineState
		}{apiBackendLine, apiBackendAnyLine, *modelState.APIBackend})
	}
	for _, field := range fields {
		if !field.state.Managed {
			continue
		}
		found := false
		next := make([]string, 0, len(block))
		structural := tomlStructuralLines(block)
		for index, line := range block {
			bare := strings.TrimRight(line, "\r\n")
			if !structural[index] || (!field.pattern.MatchString(bare) && !field.anyPattern.MatchString(bare)) {
				next = append(next, line)
				continue
			}
			if field.state.Present && !found {
				next = append(next, field.state.OriginalLine)
			}
			found = true
			restored++
		}
		if field.state.Present && !found {
			next = append(next[:1], append([]string{field.state.OriginalLine}, next[1:]...)...)
			restored++
		}
		block = next
	}
	if finalBlock {
		restoreTerminalBlockEnding(block, modelState.BaseURL, modelState.APIBaseURL, legacyManagedState(modelState.APIBackend), modelState.BackendSearch)
	}
	return block, restored
}

func legacyManagedState(state *ManagedLineState) ManagedLineState {
	if state == nil {
		return ManagedLineState{}
	}
	return *state
}

func restoreTerminalBlockEnding(lines []string, states ...ManagedLineState) {
	if len(lines) == 0 {
		return
	}
	last := len(lines) - 1
	ending := lineEnding(lines[last])
	if ending == "" {
		return
	}
	bare := strings.TrimSuffix(lines[last], ending)
	for _, state := range states {
		if state.PreviousLineHash != "" && lineFingerprint(bare) == state.PreviousLineHash {
			lines[last] = bare
			return
		}
	}
}

func lineFingerprint(line string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(line)))
}

type tomlStringMode uint8

const (
	tomlCode tomlStringMode = iota
	tomlMultilineBasic
	tomlMultilineLiteral
)

// tomlStructuralLines marks lines whose first token is outside a multiline
// string. Section-like text inside instructions or other multiline values must
// never participate in the byte-preserving config rewrite.
func tomlStructuralLines(lines []string) []bool {
	structural := make([]bool, len(lines))
	mode := tomlCode
	for index, line := range lines {
		structural[index] = mode == tomlCode
		scanTOMLStringState(strings.TrimRight(line, "\r\n"), &mode)
	}
	return structural
}

func scanTOMLStringState(line string, mode *tomlStringMode) {
	for index := 0; index < len(line); {
		switch *mode {
		case tomlMultilineBasic:
			if hasUnescapedTripleQuote(line, index, '"') {
				*mode = tomlCode
				index += 3
				continue
			}
			index++
		case tomlMultilineLiteral:
			if hasUnescapedTripleQuote(line, index, '\'') {
				*mode = tomlCode
				index += 3
				continue
			}
			index++
		default:
			switch line[index] {
			case '#':
				return
			case '"':
				if strings.HasPrefix(line[index:], `"""`) {
					*mode = tomlMultilineBasic
					index += 3
					continue
				}
				index = skipTOMLBasicString(line, index+1)
			case '\'':
				if strings.HasPrefix(line[index:], `'''`) {
					*mode = tomlMultilineLiteral
					index += 3
					continue
				}
				index = skipTOMLLiteralString(line, index+1)
			default:
				index++
			}
		}
	}
}

func hasUnescapedTripleQuote(line string, index int, quote byte) bool {
	if index+3 > len(line) || line[index] != quote || line[index+1] != quote || line[index+2] != quote {
		return false
	}
	if quote == '\'' {
		return true
	}
	backslashes := 0
	for cursor := index - 1; cursor >= 0 && line[cursor] == '\\'; cursor-- {
		backslashes++
	}
	return backslashes%2 == 0
}

func skipTOMLBasicString(line string, index int) int {
	for index < len(line) {
		switch line[index] {
		case '\\':
			index += 2
		case '"':
			return index + 1
		default:
			index++
		}
	}
	return index
}

func skipTOMLLiteralString(line string, index int) int {
	if end := strings.IndexByte(line[index:], '\''); end >= 0 {
		return index + end + 1
	}
	return len(line)
}

func modelSectionID(line string) string {
	section := sectionRe.FindStringSubmatch(strings.TrimSpace(line))
	if section == nil {
		return ""
	}
	model := modelSectionRe.FindStringSubmatch(strings.TrimSpace(section[1]))
	if model == nil {
		return ""
	}
	return firstNonEmpty(model[1], model[2], model[3])
}

func canonicalModelSectionLine(line, id string) (string, bool) {
	if !strings.Contains(id, ".") {
		return line, false
	}
	section := sectionRe.FindStringSubmatch(strings.TrimSpace(line))
	if section == nil {
		return line, false
	}
	name := strings.TrimSpace(section[1])
	key := strings.TrimSpace(strings.TrimPrefix(name, "model."))
	if strings.HasPrefix(key, `"`) || strings.HasPrefix(key, `'`) {
		return line, false
	}
	bare := strings.TrimRight(line, "\r\n")
	open := strings.IndexByte(bare, '[')
	close := strings.IndexByte(bare, ']')
	if open < 0 || close <= open {
		return line, false
	}
	applied := bare[:open] + "[model." + quoteTOML(id) + "]" + bare[close+1:] + lineEnding(line)
	return applied, applied != line
}

func modelSectionLines(raw []byte) map[string]string {
	lines := splitKeepNL(string(raw))
	structural := tomlStructuralLines(lines)
	out := make(map[string]string)
	for index, line := range lines {
		if !structural[index] {
			continue
		}
		if id := modelSectionID(line); id != "" {
			out[id] = line
		}
	}
	return out
}

func quoteTOML(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func preferredLineEnding(text string) string {
	if strings.Contains(text, "\r\n") {
		return "\r\n"
	}
	return "\n"
}

func existingFileMode(path string, fallback os.FileMode) os.FileMode {
	if info, err := os.Stat(path); err == nil {
		return info.Mode().Perm()
	}
	return fallback
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".hellogrok-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	keep := false
	defer func() {
		_ = tmp.Close()
		if !keep {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
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
	keep = true
	return nil
}

func lineEnding(line string) string {
	if strings.HasSuffix(line, "\r\n") {
		return "\r\n"
	}
	if strings.HasSuffix(line, "\n") {
		return "\n"
	}
	return ""
}

func splitKeepNL(text string) []string {
	if text == "" {
		return nil
	}
	var lines []string
	start := 0
	for index := 0; index < len(text); index++ {
		if text[index] == '\n' {
			lines = append(lines, text[start:index+1])
			start = index + 1
		}
	}
	if start < len(text) {
		lines = append(lines, text[start:])
	}
	return lines
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
