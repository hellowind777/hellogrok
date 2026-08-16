package cfgpatch

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/hellowind777/hellogrok/internal/tomlutil"
	"github.com/pelletier/go-toml/v2"
)

const (
	ProxyHost   = "127.0.0.1"
	ProxyPort   = "18787"
	stateFormat = "hellogrok-config-rewrite"

	ccSwitchProxyToken = "PROXY_MANAGED"
)

var (
	sectionRe                      = regexp.MustCompile(`^\[([^\]]+)\]\s*(?:#.*)?$`)
	modelSectionRe                 = regexp.MustCompile(`^model\.(?:"([^"]+)"|'([^']+)'|(.+))$`)
	baseURLLine                    = regexp.MustCompile(`^(\s*base_url\s*=\s*)(?:"[^"]*"|'[^']*')(\s*(?:#.*)?)?$`)
	apiBaseURLLine                 = regexp.MustCompile(`^(\s*api_base_url\s*=\s*)(?:"[^"]*"|'[^']*')(\s*(?:#.*)?)?$`)
	apiBackendLine                 = regexp.MustCompile(`^(\s*api_backend\s*=\s*)(?:"[^"]*"|'[^']*')(\s*(?:#.*)?)?$`)
	contextWindowLine              = regexp.MustCompile(`^(\s*context_window\s*=\s*)[0-9]+(\s*(?:#.*)?)?$`)
	maxCompletionTokensLine        = regexp.MustCompile(`^(\s*max_completion_tokens\s*=\s*)[0-9]+(\s*(?:#.*)?)?$`)
	autoCompactThresholdLine       = regexp.MustCompile(`^(\s*auto_compact_threshold_percent\s*=\s*)[0-9]+(\s*(?:#.*)?)?$`)
	backendSearchLine              = regexp.MustCompile(`^(\s*supports_backend_search\s*=\s*)(?:true|false)(\s*(?:#.*)?)?$`)
	supportsReasoningEffortLine    = regexp.MustCompile(`^(\s*supports_reasoning_effort\s*=\s*)(?:true|false)(\s*(?:#.*)?)?$`)
	reasoningEffortLine            = regexp.MustCompile(`^(\s*reasoning_effort\s*=\s*)(?:"[^"]*"|'[^']*')(\s*(?:#.*)?)?$`)
	backendToolsLine               = regexp.MustCompile(`^(\s*backend_tools\s*=\s*)(?:true|false)(\s*(?:#.*)?)?$`)
	webFetchLine                   = regexp.MustCompile(`^(\s*web_fetch\s*=\s*)(?:true|false)(\s*(?:#.*)?)?$`)
	subagentsEnabledLine           = regexp.MustCompile(`^(\s*enabled\s*=\s*)(?:true|false)(\s*(?:#.*)?)?$`)
	subagentsEnabledDottedLine     = regexp.MustCompile(`^(\s*subagents\.enabled\s*=\s*)(?:true|false)(\s*(?:#.*)?)?$`)
	baseURLAnyLine                 = regexp.MustCompile(`^(\s*base_url\s*=\s*).*$`)
	apiBaseURLAnyLine              = regexp.MustCompile(`^(\s*api_base_url\s*=\s*).*$`)
	apiBackendAnyLine              = regexp.MustCompile(`^(\s*api_backend\s*=\s*).*$`)
	contextWindowAnyLine           = regexp.MustCompile(`^(\s*context_window\s*=\s*).*$`)
	maxCompletionTokensAnyLine     = regexp.MustCompile(`^(\s*max_completion_tokens\s*=\s*).*$`)
	autoCompactThresholdAnyLine    = regexp.MustCompile(`^(\s*auto_compact_threshold_percent\s*=\s*).*$`)
	backendSearchAnyLine           = regexp.MustCompile(`^(\s*supports_backend_search\s*=\s*).*$`)
	supportsReasoningEffortAnyLine = regexp.MustCompile(`^(\s*supports_reasoning_effort\s*=\s*).*$`)
	reasoningEffortAnyLine         = regexp.MustCompile(`^(\s*reasoning_effort\s*=\s*).*$`)
	reasoningEffortsAnyLine        = regexp.MustCompile(`^(\s*reasoning_efforts\s*=\s*).*$`)
	backendToolsAnyLine            = regexp.MustCompile(`^(\s*backend_tools\s*=\s*).*$`)
	webFetchAnyLine                = regexp.MustCompile(`^(\s*web_fetch\s*=\s*).*$`)
	subagentsEnabledAnyLine        = regexp.MustCompile(`^(\s*enabled\s*=\s*).*$`)
	subagentsEnabledDottedAnyLine  = regexp.MustCompile(`^(\s*subagents\.enabled\s*=\s*).*$`)
)

// Target describes one resolved custom model. APIBaseURL is true when Grok
// could select a separate API-key endpoint, which must be routed through the
// same channel facade to prevent a direct-request bypass. APIBackend is the
// provider's real protocol; BuildAPIBackend is the protocol exposed to Grok
// Build while the facade is active. An empty protocol remains catalog-owned:
// the rewrite must not pin Grok Build's local or remote model resolution.
type Target struct {
	ID                    string
	APIBaseURL            bool
	APIBackend            string
	BuildAPIBackend       string
	SupportsBackendSearch bool
	// ProjectBackendSearch is true only when a config value or a documented
	// provider default may override Grok Build's resolved model catalog.
	ProjectBackendSearch bool
	// ContextWindow is a trusted learned provider limit. A zero value leaves
	// Grok Build's configured or catalog-owned denominator unchanged.
	ContextWindow uint64
	// MaxCompletionTokens is the effective configured model/provider limit.
	// A zero value leaves the field entirely to Grok Build and remote metadata.
	MaxCompletionTokens uint64
	// AutoCompactThresholdPercent temporarily projects a model-specific safe
	// threshold. Nil leaves the user's model/global/default resolution intact.
	AutoCompactThresholdPercent *uint8
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
	Format     string                `json:"format"`
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
	Section              ModelSectionState `json:"section,omitempty"`
	BaseURL              ManagedLineState  `json:"base_url"`
	APIBaseURL           ManagedLineState  `json:"api_base_url,omitempty"`
	APIBackend           ManagedLineState  `json:"api_backend,omitempty"`
	ContextWindow        ManagedLineState  `json:"context_window,omitempty"`
	MaxCompletionTokens  ManagedLineState  `json:"max_completion_tokens,omitempty"`
	AutoCompactThreshold ManagedLineState  `json:"auto_compact_threshold_percent,omitempty"`
	BackendSearch        ManagedLineState  `json:"backend_search"`
	// Reasoning fields are restore-only state from releases that projected a
	// DeepSeek menu. New transactions never manage user-owned reasoning config.
	SupportsReasoningEffort ManagedLineState `json:"supports_reasoning_effort,omitempty"`
	ReasoningEffort         ManagedLineState `json:"reasoning_effort,omitempty"`
	ReasoningEfforts        ManagedLineState `json:"reasoning_efforts,omitempty"`
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
	// PreApply* is set only for one-way migrations whose post-shutdown value
	// intentionally differs from the bytes present before ApplyTargets. It lets
	// crash recovery distinguish "state committed, config not replaced yet".
	PreApplyRecorded bool   `json:"pre_apply_recorded,omitempty"`
	PreApplyPresent  bool   `json:"pre_apply_present,omitempty"`
	PreApplyValue    string `json:"pre_apply_value,omitempty"`
}

type ApplyResult struct {
	ModelSections         int
	BaseURLs              int
	APIBaseURLs           int
	APIBackends           int
	ContextWindows        int
	MaxCompletionTokens   int
	AutoCompactThresholds int
	BackendSearch         int
	BackendTools          int
	WebFetch              int
	SubagentsEnabled      int
	ValidatedTargets      int
	Targets               []string
	LegacyModelAliases    map[string]string
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
			"supports_backend_search", "supports_reasoning_effort", "reasoning_effort", "reasoning_efforts",
			"context_window", "max_completion_tokens",
			"inference_idle_timeout_secs", "extra_headers", "env_http_headers":
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
	if err := tomlutil.UnmarshalFile(configPath, raw, &root); err != nil {
		return CCSwitchTakeover{}, err
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
	return activeProxyReferences(raw)
}

func activeProxyReferences(raw []byte) ([]string, error) {
	var root map[string]any
	if err := tomlutil.Unmarshal(raw, &root); err != nil {
		return activeProxyReferencesText(raw), nil
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
// projects the protocol Grok Build must consume, materializes only declared
// capabilities missing from Grok Build's provider inheritance, and enables
// Build's backend-tool and web-fetch feature gates. It preserves
// [models].web_search, stream_tool_calls, and omitted catalog-owned fields.
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
		switch target.APIBackend {
		case "", "responses", "messages", "chat_completions":
		default:
			return ApplyResult{}, fmt.Errorf("model %q uses unsupported api_backend %q", target.ID, target.APIBackend)
		}
		target.BuildAPIBackend = strings.ToLower(strings.TrimSpace(target.BuildAPIBackend))
		if target.BuildAPIBackend == "" && target.APIBackend != "" {
			target.BuildAPIBackend = target.APIBackend
		}
		switch target.BuildAPIBackend {
		case "", "responses", "messages", "chat_completions":
		default:
			return ApplyResult{}, fmt.Errorf("model %q uses unsupported Build api_backend %q", target.ID, target.BuildAPIBackend)
		}
		if target.MaxCompletionTokens > uint64(^uint32(0)) {
			return ApplyResult{}, fmt.Errorf("model %q max completion tokens must be between 1 and %d", target.ID, uint64(^uint32(0)))
		}
		if target.AutoCompactThresholdPercent != nil && *target.AutoCompactThresholdPercent > 100 {
			return ApplyResult{}, fmt.Errorf("model %q auto compact threshold must be between 0 and 100", target.ID)
		}
		targetMap[target.ID] = target
	}
	if len(targetMap) == 0 {
		return ApplyResult{}, nil
	}
	var initialRoot map[string]any
	if err := tomlutil.UnmarshalFile(configPath, raw, &initialRoot); err != nil {
		return ApplyResult{}, err
	}
	if err := validateTargetBackendSearchValues(initialRoot, targetMap); err != nil {
		return ApplyResult{}, err
	}
	if err := validateSubagentEnabledValue(initialRoot); err != nil {
		return ApplyResult{}, err
	}

	state := State{Format: stateFormat, ConfigPath: configPath, Models: map[string]ModelState{}}
	previousState, stateReadErr := os.ReadFile(statePath)
	hadPreviousState := stateReadErr == nil
	if stateReadErr != nil && !os.IsNotExist(stateReadErr) {
		return ApplyResult{}, fmt.Errorf("read rewrite state: %w", stateReadErr)
	}
	if hadPreviousState {
		existing := previousState
		if err := json.Unmarshal(existing, &state); err != nil {
			return ApplyResult{}, fmt.Errorf("decode rewrite state: %w", err)
		}
		if state.Format != stateFormat {
			return ApplyResult{}, fmt.Errorf("unsupported rewrite state format %q", state.Format)
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
			previousState = nil
			hadPreviousState = false
			state = State{Format: stateFormat, ConfigPath: configPath, Models: map[string]ModelState{}}
		} else if err := validateRestorableConfig(raw, state); err != nil {
			return ApplyResult{}, fmt.Errorf("existing rewrite state conflicts with config: %w", err)
		}
	}
	state.Format = stateFormat
	state.ConfigPath = configPath

	text, subagentEnabled, err := rewriteSubagentEnabled(string(tomlutil.StripUTF8BOM(raw)), initialRoot, &state)
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
	prepared := []byte(text)
	if err := validateManagedConfig(prepared, targetMap, state); err != nil {
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
		return ApplyResult{}, restorePreviousRewriteState(statePath, previousState, hadPreviousState, fmt.Errorf("read rewrite state after write: %w", err))
	}
	if !bytes.Equal(persistedState, encoded) {
		return ApplyResult{}, restorePreviousRewriteState(statePath, previousState, hadPreviousState, fmt.Errorf("rewrite state read-back mismatch"))
	}
	if err := writeConfigAtomic(configPath, prepared); err != nil {
		return ApplyResult{}, restorePreviousRewriteState(statePath, previousState, hadPreviousState, fmt.Errorf("write config: %w", err))
	}
	written, err := os.ReadFile(configPath)
	if err != nil {
		return ApplyResult{}, rollbackApplyAttempt(configPath, statePath, raw, previousState, hadPreviousState, fmt.Errorf("read config after rewrite: %w", err))
	}
	if err := validateManagedConfig(written, targetMap, state); err != nil {
		return ApplyResult{}, rollbackApplyAttempt(configPath, statePath, raw, previousState, hadPreviousState, fmt.Errorf("validate written config: %w", err))
	}
	result.ValidatedTargets = len(targetMap)
	return result, nil
}

// NormalizeUTF8 removes an optional UTF-8 BOM after validating the full TOML
// document. The explicit operation never attempts to transcode another encoding.
func NormalizeUTF8(configPath string) (bool, error) {
	configPath, err := canonicalConfigPath(configPath)
	if err != nil {
		return false, fmt.Errorf("解析配置路径失败：%w", err)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return false, fmt.Errorf("读取配置文件 %s 失败：%w", configPath, err)
	}
	var root map[string]any
	if err := tomlutil.UnmarshalFile(configPath, raw, &root); err != nil {
		return false, err
	}
	withoutBOM := tomlutil.StripUTF8BOM(raw)
	if len(withoutBOM) == len(raw) {
		return false, nil
	}
	if err := writeConfigAtomic(configPath, withoutBOM); err != nil {
		return false, fmt.Errorf("将配置文件转换为 UTF-8 无 BOM 失败：%w", err)
	}
	written, err := os.ReadFile(configPath)
	if err != nil {
		return false, fmt.Errorf("转换后读取配置文件失败：%w", err)
	}
	if !bytes.Equal(written, withoutBOM) {
		return false, fmt.Errorf("转换后配置文件校验失败")
	}
	return true, nil
}

func validateManagedConfig(raw []byte, targets map[string]Target, state State) error {
	var root map[string]any
	if err := tomlutil.Unmarshal(raw, &root); err != nil {
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
		if expected := targets[id].BuildAPIBackend; expected != "" {
			backend, err := effectiveModelBackend(root, model)
			if err != nil {
				return fmt.Errorf("[model.%s].api_backend %w", id, err)
			}
			if backend != expected {
				return fmt.Errorf("[model.%s].api_backend must be %q, got %q", id, expected, backend)
			}
		}
		if expected := targets[id].MaxCompletionTokens; expected > 0 {
			actual, ok := positiveUint64(model["max_completion_tokens"])
			if !ok || actual != expected {
				return fmt.Errorf("[model.%s].max_completion_tokens must be %d", id, expected)
			}
		}
		if expected := targets[id].ContextWindow; expected > 0 {
			actual, ok := positiveUint64(model["context_window"])
			if !ok || actual != expected {
				return fmt.Errorf("[model.%s].context_window must be %d", id, expected)
			}
		}
		if expected := targets[id].AutoCompactThresholdPercent; expected != nil {
			actual, ok := nonNegativeUint64(model["auto_compact_threshold_percent"])
			if !ok || actual != uint64(*expected) {
				return fmt.Errorf("[model.%s].auto_compact_threshold_percent must be %d", id, *expected)
			}
		}
		if targets[id].ProjectBackendSearch || state.Models[id].BackendSearch.Managed {
			if value, ok := model["supports_backend_search"].(bool); !ok || value != targets[id].SupportsBackendSearch {
				return fmt.Errorf("[model.%s].supports_backend_search must be %t", id, targets[id].SupportsBackendSearch)
			}
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
		if !targets[id].ProjectBackendSearch {
			continue
		}
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

func positiveUint64(value any) (uint64, bool) {
	switch typed := value.(type) {
	case int:
		if typed > 0 {
			return uint64(typed), true
		}
	case int64:
		if typed > 0 {
			return uint64(typed), true
		}
	case uint:
		if typed > 0 {
			return uint64(typed), true
		}
	case uint64:
		if typed > 0 {
			return typed, true
		}
	}
	return 0, false
}

func nonNegativeUint64(value any) (uint64, bool) {
	switch typed := value.(type) {
	case int:
		if typed >= 0 {
			return uint64(typed), true
		}
	case int64:
		if typed >= 0 {
			return uint64(typed), true
		}
	case uint:
		return uint64(typed), true
	case uint64:
		return typed, true
	}
	return 0, false
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

func rollbackApplyAttempt(configPath, statePath string, previousConfig, previousState []byte, hadPreviousState bool, cause error) error {
	if err := writeConfigAtomic(configPath, previousConfig); err != nil {
		return fmt.Errorf("%w; restore previous active config: %v", cause, err)
	}
	return restorePreviousRewriteState(statePath, previousState, hadPreviousState, cause)
}

func restorePreviousRewriteState(statePath string, previous []byte, existed bool, cause error) error {
	var err error
	if existed {
		err = writeFileAtomic(statePath, previous, existingFileMode(statePath, 0o600))
	} else {
		err = os.Remove(statePath)
		if os.IsNotExist(err) {
			err = nil
		}
	}
	if err != nil {
		return fmt.Errorf("%w; restore previous rewrite state: %v", cause, err)
	}
	return cause
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
		for end < len(lines) && (!structural[end] || tomlSection(lines[end]) == nil) {
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
	if target.BuildAPIBackend != "" && (target.BuildAPIBackend != target.APIBackend || modelState.APIBackend.Managed) {
		fields = append(fields, managedField{
			name: "api_backend", pattern: apiBackendLine, anyPattern: apiBackendAnyLine,
			value: quoteTOML(target.BuildAPIBackend), state: &modelState.APIBackend, changed: &result.APIBackends,
		})
	}
	// Grok Build inherits provider context_window but not provider
	// max_completion_tokens. Materialize only a missing model field so an
	// explicit [model.*] value remains user-owned and highest priority.
	if target.MaxCompletionTokens > 0 &&
		(modelState.MaxCompletionTokens.Managed || !modelBlockHasField(block, maxCompletionTokensAnyLine)) {
		fields = append(fields, managedField{
			name: "max_completion_tokens", pattern: maxCompletionTokensLine,
			anyPattern: maxCompletionTokensAnyLine, value: fmt.Sprintf("%d", target.MaxCompletionTokens),
			state: &modelState.MaxCompletionTokens, changed: &result.MaxCompletionTokens,
		})
	}
	if target.ContextWindow > 0 &&
		(modelState.ContextWindow.Managed || !modelBlockHasField(block, contextWindowAnyLine)) {
		fields = append(fields, managedField{
			name: "context_window", pattern: contextWindowLine,
			anyPattern: contextWindowAnyLine, value: fmt.Sprintf("%d", target.ContextWindow),
			state: &modelState.ContextWindow, changed: &result.ContextWindows,
		})
	}
	if target.AutoCompactThresholdPercent != nil {
		fields = append(fields, managedField{
			name: "auto_compact_threshold_percent", pattern: autoCompactThresholdLine,
			anyPattern: autoCompactThresholdAnyLine, value: fmt.Sprintf("%d", *target.AutoCompactThresholdPercent),
			state: &modelState.AutoCompactThreshold, changed: &result.AutoCompactThresholds,
		})
	}
	orderedFields := make([]managedField, 0, 1)
	if target.ProjectBackendSearch || modelState.BackendSearch.Managed {
		orderedFields = append(orderedFields, managedField{
			name: "supports_backend_search", pattern: backendSearchLine, anyPattern: backendSearchAnyLine,
			value: fmt.Sprintf("%t", target.SupportsBackendSearch), state: &modelState.BackendSearch, changed: &result.BackendSearch,
		})
	}
	block = rewriteManagedFields(block, 1, fields, ending)
	foundOrdered := rewriteExistingManagedFields(block, 1, orderedFields)
	block, err = insertMissingModelCapabilityFields(block, 1, orderedFields, foundOrdered, ending)
	if err != nil {
		return nil, fmt.Errorf("[model.%s] capability field order: %w", id, err)
	}
	state.Models[id] = modelState
	return block, nil
}

func modelBlockHasField(block []string, pattern *regexp.Regexp) bool {
	structural := tomlStructuralLines(block)
	for index := 1; index < len(block); index++ {
		if structural[index] && pattern.MatchString(strings.TrimRight(block[index], "\r\n")) {
			return true
		}
	}
	return false
}

// rewriteManagedFields updates existing managed values in place and appends
// missing values after the table's last field, before trailing comments and
// blank lines. firstContent is 1 for a named table and 0 for the root table.
func rewriteManagedFields(block []string, firstContent int, fields []managedField, ending string) []string {
	found := rewriteExistingManagedFields(block, firstContent, fields)

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

func rewriteExistingManagedFields(block []string, firstContent int, fields []managedField) map[string]bool {
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
	return found
}

var orderedModelCapabilityFields = []struct {
	name       string
	anyPattern *regexp.Regexp
}{
	{name: "reasoning_effort", anyPattern: reasoningEffortAnyLine},
	{name: "reasoning_efforts", anyPattern: reasoningEffortsAnyLine},
	{name: "supports_backend_search", anyPattern: backendSearchAnyLine},
}

func insertMissingModelCapabilityFields(
	block []string,
	firstContent int,
	fields []managedField,
	found map[string]bool,
	ending string,
) ([]string, error) {
	active := make(map[string]managedField, len(fields))
	for _, field := range fields {
		active[field.name] = field
	}
	for orderIndex, ordered := range orderedModelCapabilityFields {
		field, enabled := active[ordered.name]
		if !enabled || found[field.name] {
			continue
		}
		insertAt, err := orderedModelCapabilityInsertAt(block, firstContent, orderIndex)
		if err != nil {
			return nil, err
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
		*field.changed++
	}
	return block, nil
}

func orderedModelCapabilityInsertAt(block []string, firstContent, targetOrder int) (int, error) {
	previousEnd := -1
	nextStart := -1
	structural := tomlStructuralLines(block)
	for index := firstContent; index < len(block); index++ {
		if !structural[index] {
			continue
		}
		bare := strings.TrimRight(block[index], "\r\n")
		for orderIndex, ordered := range orderedModelCapabilityFields {
			if !ordered.anyPattern.MatchString(bare) {
				continue
			}
			if orderIndex < targetOrder {
				end := index + 1
				if ordered.name == "reasoning_efforts" {
					var err error
					end, err = tomlAssignmentEnd(block, index, ordered.name)
					if err != nil {
						return 0, err
					}
				}
				if end > previousEnd {
					previousEnd = end
				}
			} else if orderIndex > targetOrder && (nextStart < 0 || index < nextStart) {
				nextStart = index
			}
			break
		}
	}
	if previousEnd >= 0 && (nextStart < 0 || previousEnd <= nextStart) {
		return previousEnd, nil
	}
	if nextStart >= 0 {
		return nextStart, nil
	}
	if previousEnd >= 0 {
		return previousEnd, nil
	}
	return managedFieldFooterInsertAt(block, firstContent), nil
}

func tomlAssignmentEnd(lines []string, start int, key string) (int, error) {
	var assignment strings.Builder
	for end := start + 1; end <= len(lines); end++ {
		assignment.WriteString(lines[end-1])
		var root map[string]any
		if err := toml.Unmarshal([]byte("[managed]\n"+assignment.String()), &root); err != nil {
			continue
		}
		table, _ := root["managed"].(map[string]any)
		if _, exists := table[key]; exists {
			return end, nil
		}
	}
	return 0, fmt.Errorf("could not locate the end of its TOML value")
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
	if state.Format != stateFormat || state.Models == nil {
		return 0, fmt.Errorf("unsupported rewrite state format %q", state.Format)
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
	if err := tomlutil.ValidateUTF8File(configPath, raw); err != nil {
		return 0, err
	}
	var parsed map[string]any
	parseErr := tomlutil.Unmarshal(raw, &parsed)
	original := false
	if parseErr == nil {
		original, err = matchesOriginalManagedState(raw, state)
		if err != nil {
			return 0, fmt.Errorf("validate original managed values: %w", err)
		}
	}
	if original {
		if err := os.Remove(statePath); err != nil && !os.IsNotExist(err) {
			return 0, err
		}
		return 0, nil
	}
	if parseErr == nil {
		state, err = prepareRestoreState(raw, state)
	} else {
		state, err = prepareRestoreStateText(tomlutil.StripUTF8BOM(raw), state)
	}
	if err != nil {
		return 0, fmt.Errorf("prepare config restore: %w", err)
	}
	lines := splitKeepNL(string(tomlutil.StripUTF8BOM(raw)))
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
		for end < len(lines) && (!structural[end] || tomlSection(lines[end]) == nil) {
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
	var remaining []string
	if parseErr == nil {
		remaining, err = unexpectedProxyReferences([]byte(text), state)
		if err != nil {
			return 0, fmt.Errorf("validate restored config: %w", err)
		}
	} else {
		remaining = activeProxyReferencesText([]byte(text))
	}
	if len(remaining) != 0 {
		return 0, fmt.Errorf("config still contains temporary hellogrok routes after preserving concurrent edits: %s", strings.Join(remaining, ", "))
	}
	restoredBytes := []byte(text)
	if err := writeConfigAtomic(configPath, restoredBytes); err != nil {
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
	if err := tomlutil.Unmarshal(raw, &root); err != nil {
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
			{key: "api_backend", state: modelState.APIBackend},
			{key: "context_window", state: modelState.ContextWindow},
			{key: "max_completion_tokens", state: modelState.MaxCompletionTokens},
			{key: "auto_compact_threshold_percent", state: modelState.AutoCompactThreshold},
			{key: "supports_backend_search", state: modelState.BackendSearch},
			{key: "supports_reasoning_effort", state: modelState.SupportsReasoningEffort},
			{key: "reasoning_effort", state: modelState.ReasoningEffort},
			{key: "reasoning_efforts", state: modelState.ReasoningEfforts},
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
	if check.state.PreApplyRecorded {
		if exists != check.state.PreApplyPresent {
			return false, nil
		}
		if !exists {
			return true, nil
		}
		currentValue, ok := managedScalarString(current)
		return ok && currentValue == check.state.PreApplyValue, nil
	}
	if !check.state.Present {
		return !exists, nil
	}
	expected, err := originalManagedValue(check.key, check.state.OriginalLine)
	if err != nil {
		return false, err
	}
	currentValue, currentOK := managedScalarString(current)
	expectedValue, expectedOK := managedScalarString(expected)
	return currentOK && expectedOK && currentValue == expectedValue, nil
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
	var root map[string]any
	if toml.Unmarshal([]byte("value = "+rendered), &root) == nil {
		if value, ok := managedScalarString(root["value"]); ok {
			return value
		}
	}
	return strings.TrimSpace(rendered)
}

// prepareRestoreState performs a field-level three-way merge. Values still at
// hellogrok's applied value remain managed and are restored; values changed
// while the proxy was active become user-owned and are preserved as written.
func prepareRestoreState(raw []byte, state State) (State, error) {
	var root map[string]any
	if err := tomlutil.Unmarshal(raw, &root); err != nil {
		return State{}, fmt.Errorf("parse TOML: %w", err)
	}

	features, _ := root["features"].(map[string]any)
	var err error
	state.Features.BackendTools, _, err = managedStateForRestore("[features]", features, "backend_tools", state.Features.BackendTools)
	if err != nil {
		return State{}, err
	}
	state.Features.WebFetch, _, err = managedStateForRestore("[features]", features, "web_fetch", state.Features.WebFetch)
	if err != nil {
		return State{}, err
	}

	subagents, _ := root["subagents"].(map[string]any)
	var preserveSubagentEdit bool
	state.Subagents.Enabled, preserveSubagentEdit, err = managedStateForRestore("[subagents]", subagents, "enabled", state.Subagents.Enabled)
	if err != nil {
		return State{}, err
	}
	if preserveSubagentEdit {
		state.Subagents.DottedLineCreated = false
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
		if model == nil && sectionLines[id] == "" {
			continue
		}
		modelState := state.Models[id]
		if modelState.Section.Managed && sectionLines[id] != modelState.Section.AppliedLine {
			modelState.Section.Managed = false
		}
		section := fmt.Sprintf("[model.%s]", id)
		modelState.BaseURL, _, err = managedStateForRestore(section, model, "base_url", modelState.BaseURL)
		if err != nil {
			return State{}, err
		}
		modelState.APIBaseURL, _, err = managedStateForRestore(section, model, "api_base_url", modelState.APIBaseURL)
		if err != nil {
			return State{}, err
		}
		modelState.APIBackend, _, err = managedStateForRestore(section, model, "api_backend", modelState.APIBackend)
		if err != nil {
			return State{}, err
		}
		modelState.ContextWindow, _, err = managedStateForRestore(section, model, "context_window", modelState.ContextWindow)
		if err != nil {
			return State{}, err
		}
		modelState.MaxCompletionTokens, _, err = managedStateForRestore(section, model, "max_completion_tokens", modelState.MaxCompletionTokens)
		if err != nil {
			return State{}, err
		}
		modelState.AutoCompactThreshold, _, err = managedStateForRestore(section, model, "auto_compact_threshold_percent", modelState.AutoCompactThreshold)
		if err != nil {
			return State{}, err
		}
		modelState.BackendSearch, _, err = managedStateForRestore(section, model, "supports_backend_search", modelState.BackendSearch)
		if err != nil {
			return State{}, err
		}
		modelState.SupportsReasoningEffort, _, err = managedStateForRestore(section, model, "supports_reasoning_effort", modelState.SupportsReasoningEffort)
		if err != nil {
			return State{}, err
		}
		modelState.ReasoningEffort, _, err = managedStateForRestore(section, model, "reasoning_effort", modelState.ReasoningEffort)
		if err != nil {
			return State{}, err
		}
		modelState.ReasoningEfforts, _, err = managedStateForRestore(section, model, "reasoning_efforts", modelState.ReasoningEfforts)
		if err != nil {
			return State{}, err
		}
		state.Models[id] = modelState
	}
	return state, nil
}

// prepareRestoreStateText is the invalid-TOML recovery path. It compares each
// independently parseable managed assignment with the value hellogrok applied,
// so unrelated half-written settings do not block shutdown or get discarded.
func prepareRestoreStateText(raw []byte, state State) (State, error) {
	lines := splitKeepNL(string(tomlutil.StripUTF8BOM(raw)))
	structural := tomlStructuralLines(lines)

	if block := namedSectionBlock(lines, structural, "features"); block != nil {
		state.Features.BackendTools = managedStateForText(block, "backend_tools", backendToolsAnyLine, state.Features.BackendTools)
		state.Features.WebFetch = managedStateForText(block, "web_fetch", webFetchAnyLine, state.Features.WebFetch)
	}
	if state.Subagents.DottedLineCreated {
		rootEnd := len(lines)
		for index, line := range lines {
			if structural[index] && tomlSection(line) != nil {
				rootEnd = index
				break
			}
		}
		state.Subagents.Enabled = managedStateForText(lines[:rootEnd], "subagents.enabled", subagentsEnabledDottedAnyLine, state.Subagents.Enabled)
	} else if block := namedSectionBlock(lines, structural, "subagents"); block != nil {
		state.Subagents.Enabled = managedStateForText(block, "enabled", subagentsEnabledAnyLine, state.Subagents.Enabled)
	}

	ids := make([]string, 0, len(state.Models))
	for id := range state.Models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		block := modelSectionBlock(lines, structural, id)
		if block == nil {
			continue
		}
		modelState := state.Models[id]
		if modelState.Section.Managed && block[0] != modelState.Section.AppliedLine {
			modelState.Section.Managed = false
		}
		for _, field := range []struct {
			key     string
			pattern *regexp.Regexp
			state   *ManagedLineState
		}{
			{"base_url", baseURLAnyLine, &modelState.BaseURL},
			{"api_base_url", apiBaseURLAnyLine, &modelState.APIBaseURL},
			{"api_backend", apiBackendAnyLine, &modelState.APIBackend},
			{"context_window", contextWindowAnyLine, &modelState.ContextWindow},
			{"max_completion_tokens", maxCompletionTokensAnyLine, &modelState.MaxCompletionTokens},
			{"auto_compact_threshold_percent", autoCompactThresholdAnyLine, &modelState.AutoCompactThreshold},
			{"supports_backend_search", backendSearchAnyLine, &modelState.BackendSearch},
			{"supports_reasoning_effort", supportsReasoningEffortAnyLine, &modelState.SupportsReasoningEffort},
			{"reasoning_effort", reasoningEffortAnyLine, &modelState.ReasoningEffort},
			{"reasoning_efforts", reasoningEffortsAnyLine, &modelState.ReasoningEfforts},
		} {
			*field.state = managedStateForText(block, field.key, field.pattern, *field.state)
		}
		state.Models[id] = modelState
	}
	return state, nil
}

func managedStateForText(block []string, key string, pattern *regexp.Regexp, state ManagedLineState) ManagedLineState {
	if !state.Managed {
		return state
	}
	value, exists, valid := managedTextValue(block, key, pattern)
	if !exists || !valid || value != state.AppliedValue {
		state.Managed = false
	}
	return state
}

func managedTextValue(block []string, key string, pattern *regexp.Regexp) (string, bool, bool) {
	structural := tomlStructuralLines(block)
	start := -1
	for index, line := range block {
		if !structural[index] || !pattern.MatchString(strings.TrimRight(line, "\r\n")) {
			continue
		}
		if start >= 0 {
			return "", true, false
		}
		start = index
	}
	if start < 0 {
		return "", false, false
	}

	var assignment strings.Builder
	for end := start; end < len(block); end++ {
		assignment.WriteString(block[end])
		var root map[string]any
		if toml.Unmarshal([]byte("[managed]\n"+assignment.String()), &root) != nil {
			continue
		}
		var value any = root["managed"]
		found := true
		for _, part := range strings.Split(key, ".") {
			table, ok := value.(map[string]any)
			if !ok {
				found = false
				break
			}
			value, found = table[part]
			if !found {
				break
			}
		}
		if found {
			semantic, ok := managedScalarString(value)
			return semantic, true, ok
		}
	}
	return "", true, false
}

func namedSectionBlock(lines []string, structural []bool, name string) []string {
	start := -1
	for index, line := range lines {
		if !structural[index] {
			continue
		}
		section := tomlSection(line)
		if section == nil {
			continue
		}
		if start >= 0 {
			return lines[start:index]
		}
		if strings.EqualFold(strings.TrimSpace(section[1]), name) {
			start = index
		}
	}
	if start >= 0 {
		return lines[start:]
	}
	return nil
}

func modelSectionBlock(lines []string, structural []bool, id string) []string {
	start := -1
	for index, line := range lines {
		if !structural[index] || tomlSection(line) == nil {
			continue
		}
		if start >= 0 {
			return lines[start:index]
		}
		if modelSectionID(line) == id {
			start = index
		}
	}
	if start >= 0 {
		return lines[start:]
	}
	return nil
}

func activeProxyReferencesText(raw []byte) []string {
	lines := splitKeepNL(string(raw))
	structural := tomlStructuralLines(lines)
	currentModel := ""
	seen := make(map[string]struct{})
	for index, line := range lines {
		if !structural[index] {
			continue
		}
		if tomlSection(line) != nil {
			currentModel = modelSectionID(line)
			continue
		}
		bare := strings.TrimRight(line, "\r\n")
		for _, field := range []struct {
			key     string
			pattern *regexp.Regexp
		}{
			{"base_url", baseURLAnyLine},
			{"api_base_url", apiBaseURLAnyLine},
		} {
			if !field.pattern.MatchString(bare) {
				continue
			}
			value, _, valid := managedTextValue([]string{line}, field.key, field.pattern)
			isProxy := valid && IsProxyURL(value)
			if !valid {
				isProxy = strings.Contains(bare, "http://"+net.JoinHostPort(ProxyHost, ProxyPort)+"/c/")
			}
			if !isProxy {
				continue
			}
			reference := field.key
			if currentModel != "" {
				reference = currentModel + "." + field.key
			}
			seen[reference] = struct{}{}
		}
	}
	refs := make([]string, 0, len(seen))
	for reference := range seen {
		refs = append(refs, reference)
	}
	sort.Strings(refs)
	return refs
}

func unexpectedProxyReferences(raw []byte, state State) ([]string, error) {
	var root map[string]any
	if err := tomlutil.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("parse TOML: %w", err)
	}
	models := ModelTables(root)
	refs := make([]string, 0)
	for id, model := range models {
		modelState, recorded := state.Models[id]
		for _, field := range []struct {
			key   string
			state ManagedLineState
		}{
			{key: "base_url", state: modelState.BaseURL},
			{key: "api_base_url", state: modelState.APIBaseURL},
		} {
			endpoint, _ := model[field.key].(string)
			if !IsProxyURL(endpoint) {
				continue
			}
			if recorded && field.state.Present && field.state.OriginalLine != "" {
				original, err := originalManagedValue(field.key, field.state.OriginalLine)
				if err != nil {
					return nil, err
				}
				originalEndpoint, _ := original.(string)
				if endpoint == originalEndpoint {
					continue
				}
			}
			refs = append(refs, id+"."+field.key)
		}
	}
	sort.Strings(refs)
	return refs, nil
}

func managedStateForRestore(section string, table map[string]any, key string, state ManagedLineState) (ManagedLineState, bool, error) {
	if !state.Managed {
		return state, false, nil
	}
	if state.AppliedValue == "" {
		return state, false, fmt.Errorf("rewrite state for %s.%s has no applied value", section, key)
	}
	current, exists := table[key]
	if exists {
		value, ok := managedScalarString(current)
		if !ok {
			state.Managed = false
			return state, true, nil
		}
		if value == state.AppliedValue {
			return state, false, nil
		}
	}
	state.Managed = false
	return state, true, nil
}

func validateRestorableConfig(raw []byte, state State) error {
	var root map[string]any
	if err := tomlutil.Unmarshal(raw, &root); err != nil {
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
			{key: "api_backend", state: modelState.APIBackend},
			{key: "context_window", state: modelState.ContextWindow},
			{key: "max_completion_tokens", state: modelState.MaxCompletionTokens},
			{key: "auto_compact_threshold_percent", state: modelState.AutoCompactThreshold},
			{key: "supports_backend_search", state: modelState.BackendSearch},
			{key: "supports_reasoning_effort", state: modelState.SupportsReasoningEffort},
			{key: "reasoning_effort", state: modelState.ReasoningEffort},
			{key: "reasoning_efforts", state: modelState.ReasoningEfforts},
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
		value, ok := managedScalarString(current)
		if !ok {
			return fmt.Errorf("%s.%s has an unexpected type", section, check.key)
		}
		if value != check.state.AppliedValue {
			return fmt.Errorf("%s.%s is %q, expected proxy value %q", section, check.key, value, check.state.AppliedValue)
		}
	}
	return nil
}

func managedScalarString(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, true
	case bool:
		return fmt.Sprintf("%t", typed), true
	case int:
		return fmt.Sprintf("%d", typed), true
	case int8:
		return fmt.Sprintf("%d", typed), true
	case int16:
		return fmt.Sprintf("%d", typed), true
	case int32:
		return fmt.Sprintf("%d", typed), true
	case int64:
		return fmt.Sprintf("%d", typed), true
	case uint:
		return fmt.Sprintf("%d", typed), true
	case uint8:
		return fmt.Sprintf("%d", typed), true
	case uint16:
		return fmt.Sprintf("%d", typed), true
	case uint32:
		return fmt.Sprintf("%d", typed), true
	case uint64:
		return fmt.Sprintf("%d", typed), true
	case []any, map[string]any:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return "", false
		}
		return string(encoded), true
	default:
		return "", false
	}
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
		section := tomlSection(line)
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
		section := tomlSection(line)
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
		{apiBackendLine, apiBackendAnyLine, modelState.APIBackend},
		{contextWindowLine, contextWindowAnyLine, modelState.ContextWindow},
		{maxCompletionTokensLine, maxCompletionTokensAnyLine, modelState.MaxCompletionTokens},
		{autoCompactThresholdLine, autoCompactThresholdAnyLine, modelState.AutoCompactThreshold},
		{backendSearchLine, backendSearchAnyLine, modelState.BackendSearch},
		{supportsReasoningEffortLine, supportsReasoningEffortAnyLine, modelState.SupportsReasoningEffort},
		{reasoningEffortLine, reasoningEffortAnyLine, modelState.ReasoningEffort},
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
	block, restored = restoreManagedCompositeField(block, reasoningEffortsAnyLine, modelState.ReasoningEfforts, restored)
	if finalBlock {
		restoreTerminalBlockEnding(block, modelState.BaseURL, modelState.APIBaseURL,
			modelState.APIBackend, modelState.ContextWindow, modelState.MaxCompletionTokens, modelState.AutoCompactThreshold, modelState.BackendSearch,
			modelState.SupportsReasoningEffort, modelState.ReasoningEffort, modelState.ReasoningEfforts)
	}
	return block, restored
}

func restoreManagedCompositeField(block []string, anyPattern *regexp.Regexp, state ManagedLineState, restored int) ([]string, int) {
	if !state.Managed {
		return block, restored
	}
	start, end := -1, -1
	structural := tomlStructuralLines(block)
	for index := 1; index < len(block); index++ {
		if !structural[index] || !anyPattern.MatchString(strings.TrimRight(block[index], "\r\n")) {
			continue
		}
		assignmentEnd, err := tomlAssignmentEnd(block, index, "reasoning_efforts")
		if err != nil {
			return block, restored
		}
		start, end = index, assignmentEnd
		break
	}
	if start >= 0 {
		next := make([]string, 0, len(block)-(end-start))
		next = append(next, block[:start]...)
		if state.Present {
			next = append(next, splitKeepNL(state.OriginalLine)...)
		}
		next = append(next, block[end:]...)
		return next, restored + 1
	}
	if state.Present {
		next := make([]string, 0, len(block)+1)
		next = append(next, block[:1]...)
		next = append(next, splitKeepNL(state.OriginalLine)...)
		next = append(next, block[1:]...)
		return next, restored + 1
	}
	return block, restored
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
	section := tomlSection(line)
	if section == nil {
		return ""
	}
	model := modelSectionRe.FindStringSubmatch(strings.TrimSpace(section[1]))
	if model == nil {
		return ""
	}
	return firstNonEmpty(model[1], model[2], model[3])
}

func tomlSection(line string) []string {
	trimmed := strings.TrimSpace(line)
	trimmed = strings.TrimPrefix(trimmed, "\ufeff")
	trimmed = strings.TrimPrefix(trimmed, "\u00ef\u00bb\u00bf")
	return sectionRe.FindStringSubmatch(strings.TrimSpace(trimmed))
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

func writeConfigAtomic(path string, data []byte) error {
	if err := tomlutil.ValidateUTF8File(path, data); err != nil {
		return err
	}
	return writeFileAtomic(path, tomlutil.StripUTF8BOM(data), existingFileMode(path, 0o600))
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
	if err := renameAtomic(tmpPath, path); err != nil {
		return err
	}
	keep = true
	return nil
}

func renameAtomic(source, target string) error {
	var err error
	for attempt := 0; attempt < 8; attempt++ {
		err = os.Rename(source, target)
		if err == nil || !retryableWindowsRenameError(err) {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 10 * time.Millisecond)
	}
	return err
}

func retryableWindowsRenameError(err error) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	return errors.Is(err, os.ErrPermission) ||
		errors.Is(err, syscall.Errno(5)) || // ERROR_ACCESS_DENIED
		errors.Is(err, syscall.Errno(32)) // ERROR_SHARING_VIOLATION
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
