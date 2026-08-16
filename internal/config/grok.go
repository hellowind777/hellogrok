package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hellowind777/hellogrok/internal/cfgpatch"
	"github.com/pelletier/go-toml/v2"
)

// Model is one [model.*] entry from Grok config.toml.
type Model struct {
	ID         string
	Model      string
	Name       string
	BaseURL    string
	APIBaseURL string
	APIBackend string
	// APIBackendConfigured distinguishes an explicit model/provider protocol
	// from the protocol inherited by Grok Build from its model catalog.
	APIBackendConfigured  bool
	ChatSearchDialect     ChatSearchDialect
	SupportsBackendSearch bool
	// SupportsBackendSearchConfigured distinguishes an explicit false from an
	// omitted capability so first-party providers can supply documented defaults.
	SupportsBackendSearchConfigured bool
	// ReasoningEffortConfigured records whether the model explicitly declares
	// supports_reasoning_effort or reasoning_efforts. Provider tables do not
	// expose these fields in Grok Build, so only [model.*] participates.
	ReasoningEffortConfigured bool
	// ReasoningEffortEnabled tells the protocol facade whether an omitted
	// Messages effort can represent the user's explicit None selection.
	ReasoningEffortEnabled bool
	// LegacyGeneratedReasoningMenu identifies the exact object menu emitted by
	// older hellogrok releases. It is safe to compact without treating custom
	// labels, values, ordering, or defaults as proxy-owned.
	LegacyGeneratedReasoningMenu bool
	// ReasoningEffortSelectionConfigured preserves an explicit per-model or
	// global default while compact bare-string menus are projected.
	ReasoningEffortSelectionConfigured bool
	// ContextWindowConfigured distinguishes a user-selected model/provider
	// window from a remotely discovered maximum. Grok Build must keep the
	// configured value as the auto-compaction denominator when it is present.
	ContextWindow           uint64
	ContextWindowConfigured bool
	// MaxCompletionTokensConfigured keeps remote metadata from replacing an
	// explicit model/provider output cap.
	MaxCompletionTokens           uint64
	MaxCompletionTokensConfigured bool
	// InferenceIdleTimeoutSecs mirrors Grok Build's per-chunk timeout. The
	// facade also uses it while waiting for upstream response headers, a phase
	// Grok Build's stream-level timer does not cover.
	InferenceIdleTimeoutSecs       uint64
	InferenceIdleTimeoutConfigured bool
	AuthScheme                     string
	IncomingAuthScheme             string
	APIKey                         string
	EnvKey                         string // single name or first of array (resolved later)
	EnvKeys                        []string
	AuthProvider                   string
	DynamicAuth                    bool
	ExtraHeaders                   map[string]string
	EnvHTTPHeaders                 map[string]string
}

// ChatSearchDialect identifies the provider extension or protocol bridge used
// to enable hosted web search. The name matches Grok Build's config field.
type ChatSearchDialect string

const (
	ChatSearchDialectSearchParameters ChatSearchDialect = "search_parameters"
	ChatSearchDialectWebSearchOptions ChatSearchDialect = "web_search_options"
	ChatSearchDialectResponses        ChatSearchDialect = "responses"
	ChatSearchDialectMessages         ChatSearchDialect = "messages"
)

// authProviderConfig mirrors the fields Grok Build accepts in
// [auth_provider.*] and [model_providers.*.auth]. Keeping this typed prevents
// a malformed helper declaration from being mistaken for usable dynamic auth.
type authProviderConfig struct {
	Command      string   `toml:"command"`
	Args         []string `toml:"args"`
	TokenTTLSecs *uint64  `toml:"token_ttl_secs"`
	TimeoutSecs  *uint64  `toml:"timeout_secs"`
	CWD          *string  `toml:"cwd"`
}

// Route is a local-routing target derived from a model base_url.
type Route struct {
	ChannelID  string
	Host       string // e.g. congee.pro
	OriginBase string // effective upstream base_url before proxy rewriting
	APIBackend string // original upstream backend
	// APIBackendConfigured is false when Grok Build is free to inherit the
	// effective protocol from its local or remote model catalog. In that case
	// the facade follows the protocol of the request Grok Build actually sends.
	APIBackendConfigured bool
	// ChatSearchDialect selects the provider extension or protocol bridge used
	// by hosted search and the fixed Responses WebSearchClient adapter.
	ChatSearchDialect ChatSearchDialect
	WireModel         string // model value sent to the upstream
	APIKey            string // resolved channel credential
	AuthScheme        string // upstream bearer | x_api_key
	// IncomingAuthScheme is the scheme Grok Build uses on the local facade.
	// Unlike the upstream default for Messages, Build defaults every backend to
	// bearer unless auth_scheme is explicitly configured.
	IncomingAuthScheme string
	DynamicAuth        bool // an explicit auth_provider owns the incoming auth token
	// ExtraHeaders are channel-owned values from extra_headers and resolved
	// env_http_headers. They are safe to reapply after discarding session auth.
	ExtraHeaders map[string]string
	// SupportsBackendSearch controls whether the facade routes a structured
	// web_search declaration to this channel's hosted search API. False keeps
	// Build's configured client-search model or authenticated official fallback.
	SupportsBackendSearch bool
	// SupportsBackendSearchConfigured preserves an explicit user opt-out from a
	// provider capability that hellogrok can otherwise infer deterministically.
	SupportsBackendSearchConfigured bool
	// DefaultSearchModel records the runtime selection made through
	// [models].web_search or GROK_WEB_SEARCH_MODEL. A selected custom route is
	// exposed to Grok Build as hosted-search capable while the facade is active.
	DefaultSearchModel                 bool
	ReasoningEffortConfigured          bool
	ReasoningEffortEnabled             bool
	LegacyGeneratedReasoningMenu       bool
	ReasoningEffortSelectionConfigured bool
	// ContextWindow is the effective explicit [model.*] or inherited
	// [model_providers.*] value. When it is not configured, the facade leaves
	// provider model-metadata responses free to supply the maximum window.
	ContextWindow                  uint64
	ContextWindowConfigured        bool
	MaxCompletionTokens            uint64
	MaxCompletionTokensConfigured  bool
	InferenceIdleTimeoutSecs       uint64
	InferenceIdleTimeoutConfigured bool
}

// IsOfficialDeepSeekRoute identifies DeepSeek's first-party API independently
// of model name so rolling aliases and future models inherit protocol handling.
// Relays that reuse a DeepSeek model name deliberately do not qualify.
func IsOfficialDeepSeekRoute(route Route) bool {
	host := strings.ToLower(strings.TrimSpace(route.Host))
	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		host = parsedHost
	}
	host = strings.Trim(host, "[]")
	return host == "api.deepseek.com"
}

// WebSearchSelection is an explicitly selected Build client-search model.
// Build also has an authenticated compiled default; Explicit deliberately does
// not report that implicit fallback.
type WebSearchSelection struct {
	Model    string
	Explicit bool
	Source   string
}

// GrokHome returns ~/.grok (or GROK_HOME).
func GrokHome() string {
	if v := os.Getenv("GROK_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".grok")
}

// ConfigPath is ~/.grok/config.toml
func ConfigPath() string {
	return filepath.Join(GrokHome(), "config.toml")
}

// LoadModels parses [model.*] tables. Never writes the file.
func LoadModels(path string) ([]Model, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var root map[string]any
	if err := toml.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("parse toml: %w", err)
	}
	modelTable := cfgpatch.ModelTables(root)
	if len(modelTable) == 0 {
		return nil, nil
	}
	providerTable, _ := root["model_providers"].(map[string]any)
	authProviderTable, _ := root["auth_provider"].(map[string]any)
	usableAuthProviders := make(map[string]bool, len(authProviderTable))
	for name, rawProvider := range authProviderTable {
		usableAuthProviders[name] = usableAuthProvider(rawProvider)
	}
	modelsConfig, _ := root["models"].(map[string]any)
	globalHeaders := map[string]string{}
	if modelsConfig != nil {
		var headerErr error
		globalHeaders, headerErr = configuredRouteHeaders(modelsConfig["extra_headers"], "[models].extra_headers")
		if headerErr != nil {
			return nil, headerErr
		}
	}
	ids := make([]string, 0, len(modelTable))
	for id := range modelTable {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []Model
	for _, id := range ids {
		m := modelTable[id]
		providerID := strings.TrimSpace(str(m["model_provider"]))
		provider, _ := providerTable[providerID].(map[string]any)

		baseURL := inheritedString(m, provider, "base_url")
		apiBaseURL := inheritedString(m, provider, "api_base_url")
		apiBackend := normalizeAPIBackend(inheritedString(m, provider, "api_backend"))
		apiBackendConfigured := inheritedFieldExists(m, provider, "api_backend")
		chatSearchDialect, err := normalizeChatSearchDialect(inheritedString(m, provider, "chat_search_dialect"))
		if err != nil {
			return nil, fmt.Errorf("[model.%s].chat_search_dialect %w", id, err)
		}
		supportsBackendSearch, err := inheritedBool(m, provider, "supports_backend_search")
		if err != nil {
			return nil, fmt.Errorf("[model.%s].supports_backend_search %w", id, err)
		}
		supportsBackendSearchConfigured := inheritedFieldExists(m, provider, "supports_backend_search")
		supportsReasoningEffort, err := inheritedBool(m, nil, "supports_reasoning_effort")
		if err != nil {
			return nil, fmt.Errorf("[model.%s].supports_reasoning_effort %w", id, err)
		}
		reasoningEffortsConfigured := false
		reasoningEffortsEnabled := false
		legacyGeneratedReasoningMenu := false
		if rawEfforts, exists := m["reasoning_efforts"]; exists {
			reasoningEffortsConfigured = true
			efforts, ok := rawEfforts.([]any)
			if !ok {
				return nil, fmt.Errorf("[model.%s].reasoning_efforts must be an array", id)
			}
			reasoningEffortsEnabled = len(efforts) > 0
			legacyGeneratedReasoningMenu = isLegacyGeneratedReasoningMenu(efforts)
		}
		reasoningEffortConfigured := inheritedFieldExists(m, nil, "supports_reasoning_effort") || reasoningEffortsConfigured
		reasoningEffortSelectionConfigured := inheritedFieldExists(m, nil, "reasoning_effort") ||
			inheritedFieldExists(modelsConfig, nil, "default_reasoning_effort")
		contextWindow, contextWindowConfigured, err := inheritedPositiveUint64(m, provider, "context_window")
		if err != nil {
			return nil, fmt.Errorf("[model.%s].context_window %w", id, err)
		}
		maxCompletionTokens, maxCompletionTokensConfigured, err := inheritedPositiveUint64(m, provider, "max_completion_tokens")
		if err != nil {
			return nil, fmt.Errorf("[model.%s].max_completion_tokens %w", id, err)
		}
		if maxCompletionTokens > uint64(^uint32(0)) {
			return nil, fmt.Errorf("[model.%s].max_completion_tokens must be between 1 and %d", id, uint64(^uint32(0)))
		}
		inferenceIdleTimeoutSecs, inferenceIdleTimeoutConfigured, err := inheritedUint64(m, modelsConfig, "inference_idle_timeout_secs")
		if err != nil {
			return nil, fmt.Errorf("[model.%s].inference_idle_timeout_secs %w", id, err)
		}

		modelEnvKeys := envKeyList(m["env_key"])
		modelAPIKey := strings.TrimSpace(str(m["api_key"]))
		modelAuthProvider, modelHasStringAuthProvider := stringField(m, "auth_provider")
		_, modelHasAuthProvider := m["auth_provider"]
		modelOwnsAuth := modelAPIKey != "" || len(modelEnvKeys) > 0 || modelHasAuthProvider
		apiKey := modelAPIKey
		envKeys := modelEnvKeys
		authProvider := strings.TrimSpace(modelAuthProvider)
		dynamicAuth := modelHasStringAuthProvider && usableAuthProviders[authProvider]
		if !modelOwnsAuth && provider != nil {
			apiKey = strings.TrimSpace(str(provider["api_key"]))
			envKeys = envKeyList(provider["env_key"])
			providerAuthProvider, providerHasStringAuthProvider := stringField(provider, "auth_provider")
			_, providerHasAuthProvider := provider["auth_provider"]
			if providerHasAuthProvider {
				authProvider = strings.TrimSpace(providerAuthProvider)
				dynamicAuth = providerHasStringAuthProvider && usableAuthProviders[authProvider]
			} else if inlineAuth, ok := provider["auth"]; ok {
				authProvider = "model_provider:" + providerID
				dynamicAuth = usableAuthProvider(inlineAuth)
			} else {
				authProvider = ""
				dynamicAuth = false
			}
		}

		modelHeaders, err := configuredRouteHeaders(m["extra_headers"], fmt.Sprintf("[model.%s].extra_headers", id))
		if err != nil {
			return nil, err
		}
		if len(modelHeaders) == 0 {
			modelHeaders, err = configuredRouteHeaders(provider["extra_headers"], fmt.Sprintf("[model_providers.%s].extra_headers", providerID))
			if err != nil {
				return nil, err
			}
		}
		extraHeaders := cloneStringMap(globalHeaders)
		for key, value := range modelHeaders {
			setStringMapCaseInsensitive(extraHeaders, key, value)
		}
		envHTTPHeaders, err := configuredEnvHTTPHeaders(m["env_http_headers"], fmt.Sprintf("[model.%s].env_http_headers", id))
		if err != nil {
			return nil, err
		}
		if len(envHTTPHeaders) == 0 {
			envHTTPHeaders, err = configuredEnvHTTPHeaders(provider["env_http_headers"], fmt.Sprintf("[model_providers.%s].env_http_headers", providerID))
			if err != nil {
				return nil, err
			}
		}
		modelAuthScheme := normalizeAuthScheme(inheritedString(m, nil, "auth_scheme"))
		upstreamAuthScheme := modelAuthScheme
		if upstreamAuthScheme == "" {
			upstreamAuthScheme = normalizeAuthScheme(inheritedString(nil, provider, "auth_scheme"))
		}
		out = append(out, Model{
			ID:                                 id,
			Model:                              str(m["model"]),
			Name:                               str(m["name"]),
			BaseURL:                            baseURL,
			APIBaseURL:                         apiBaseURL,
			APIBackend:                         apiBackend,
			APIBackendConfigured:               apiBackendConfigured,
			ChatSearchDialect:                  chatSearchDialect,
			SupportsBackendSearch:              supportsBackendSearch,
			SupportsBackendSearchConfigured:    supportsBackendSearchConfigured,
			ReasoningEffortConfigured:          reasoningEffortConfigured,
			ReasoningEffortEnabled:             supportsReasoningEffort || reasoningEffortsEnabled,
			LegacyGeneratedReasoningMenu:       legacyGeneratedReasoningMenu,
			ReasoningEffortSelectionConfigured: reasoningEffortSelectionConfigured,
			ContextWindow:                      contextWindow,
			ContextWindowConfigured:            contextWindowConfigured,
			MaxCompletionTokens:                maxCompletionTokens,
			MaxCompletionTokensConfigured:      maxCompletionTokensConfigured,
			InferenceIdleTimeoutSecs:           inferenceIdleTimeoutSecs,
			InferenceIdleTimeoutConfigured:     inferenceIdleTimeoutConfigured,
			AuthScheme:                         upstreamAuthScheme,
			IncomingAuthScheme:                 modelAuthScheme,
			APIKey:                             apiKey,
			EnvKeys:                            envKeys,
			EnvKey:                             first(envKeys),
			AuthProvider:                       authProvider,
			DynamicAuth:                        dynamicAuth,
			ExtraHeaders:                       extraHeaders,
			EnvHTTPHeaders:                     envHTTPHeaders,
		})
	}
	return out, nil
}

func isLegacyGeneratedReasoningMenu(efforts []any) bool {
	wantValues := []string{"none", "low", "high", "max"}
	wantLabels := []string{"None", "Low", "High", "Max"}
	if len(efforts) != len(wantValues) {
		return false
	}
	for index, raw := range efforts {
		option, ok := raw.(map[string]any)
		if !ok || str(option["value"]) != wantValues[index] || str(option["label"]) != wantLabels[index] {
			return false
		}
		isDefault, hasDefault := option["default"].(bool)
		if index == 2 {
			if !hasDefault || !isDefault || len(option) != 3 {
				return false
			}
		} else if hasDefault || len(option) != 2 {
			return false
		}
	}
	return true
}

func usableAuthProvider(raw any) bool {
	encoded, err := toml.Marshal(raw)
	if err != nil {
		return false
	}
	var provider authProviderConfig
	if err := toml.Unmarshal(encoded, &provider); err != nil {
		return false
	}
	return strings.TrimSpace(provider.Command) != ""
}

func inheritedString(model, provider map[string]any, key string) string {
	if value, ok := stringField(model, key); ok {
		return strings.TrimSpace(value)
	}
	if value, ok := stringField(provider, key); ok {
		return strings.TrimSpace(value)
	}
	return ""
}

func inheritedBool(model, provider map[string]any, key string) (bool, error) {
	for _, values := range []map[string]any{model, provider} {
		if values == nil {
			continue
		}
		raw, exists := values[key]
		if !exists {
			continue
		}
		value, ok := raw.(bool)
		if !ok {
			return false, fmt.Errorf("must be a boolean")
		}
		return value, nil
	}
	return false, nil
}

func inheritedFieldExists(model, provider map[string]any, key string) bool {
	for _, values := range []map[string]any{model, provider} {
		if values == nil {
			continue
		}
		if _, exists := values[key]; exists {
			return true
		}
	}
	return false
}

func inheritedPositiveUint64(model, provider map[string]any, key string) (uint64, bool, error) {
	for _, values := range []map[string]any{model, provider} {
		if values == nil {
			continue
		}
		raw, exists := values[key]
		if !exists {
			continue
		}
		value, ok := positiveUint64(raw)
		if !ok {
			return 0, false, fmt.Errorf("must be a positive integer")
		}
		return value, true, nil
	}
	return 0, false, nil
}

func inheritedUint64(model, fallback map[string]any, key string) (uint64, bool, error) {
	for _, values := range []map[string]any{model, fallback} {
		if values == nil {
			continue
		}
		raw, exists := values[key]
		if !exists {
			continue
		}
		value, ok := nonNegativeUint64(raw)
		if !ok {
			return 0, false, fmt.Errorf("must be a non-negative integer")
		}
		return value, true, nil
	}
	return 0, false, nil
}

func nonNegativeUint64(value any) (uint64, bool) {
	switch typed := value.(type) {
	case int:
		if typed >= 0 {
			return uint64(typed), true
		}
	case int8:
		if typed >= 0 {
			return uint64(typed), true
		}
	case int16:
		if typed >= 0 {
			return uint64(typed), true
		}
	case int32:
		if typed >= 0 {
			return uint64(typed), true
		}
	case int64:
		if typed >= 0 {
			return uint64(typed), true
		}
	case uint:
		return uint64(typed), true
	case uint8:
		return uint64(typed), true
	case uint16:
		return uint64(typed), true
	case uint32:
		return uint64(typed), true
	case uint64:
		return typed, true
	}
	return 0, false
}

func positiveUint64(value any) (uint64, bool) {
	switch typed := value.(type) {
	case int:
		if typed > 0 {
			return uint64(typed), true
		}
	case int8:
		if typed > 0 {
			return uint64(typed), true
		}
	case int16:
		if typed > 0 {
			return uint64(typed), true
		}
	case int32:
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
	case uint8:
		if typed > 0 {
			return uint64(typed), true
		}
	case uint16:
		if typed > 0 {
			return uint64(typed), true
		}
	case uint32:
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

// LoadWebSearchSelection resolves only explicit client-search choices. The
// environment has the same precedence as Grok Build and an empty value is
// treated as absent.
func LoadWebSearchSelection(path string) (WebSearchSelection, error) {
	if value := strings.TrimSpace(os.Getenv("GROK_WEB_SEARCH_MODEL")); value != "" {
		return WebSearchSelection{Model: value, Explicit: true, Source: "environment"}, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return WebSearchSelection{}, err
	}
	var root map[string]any
	if err := toml.Unmarshal(raw, &root); err != nil {
		return WebSearchSelection{}, fmt.Errorf("parse toml: %w", err)
	}
	models, _ := root["models"].(map[string]any)
	if models == nil {
		return WebSearchSelection{}, nil
	}
	value, exists := models["web_search"]
	if !exists {
		return WebSearchSelection{}, nil
	}
	model, ok := value.(string)
	if !ok {
		return WebSearchSelection{}, fmt.Errorf("[models].web_search must be a string")
	}
	return WebSearchSelection{Model: strings.TrimSpace(model), Explicit: true, Source: "config"}, nil
}

func stringField(values map[string]any, key string) (string, bool) {
	if values == nil {
		return "", false
	}
	value, ok := values[key]
	if !ok {
		return "", false
	}
	text, ok := value.(string)
	return text, ok
}

func cloneStringMap(values map[string]string) map[string]string {
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func setStringMapCaseInsensitive(values map[string]string, key, value string) {
	for existing := range values {
		if strings.EqualFold(existing, key) {
			delete(values, existing)
			break
		}
	}
	values[key] = value
}

func normalizeAuthScheme(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch strings.ReplaceAll(value, "-", "_") {
	case "x_api_key", "xapikey":
		return "x_api_key"
	case "bearer":
		return "bearer"
	default:
		return ""
	}
}

func normalizeAPIBackend(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func normalizeChatSearchDialect(value string) (ChatSearchDialect, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	switch ChatSearchDialect(value) {
	case "":
		return "", nil
	case ChatSearchDialectSearchParameters, ChatSearchDialectWebSearchOptions,
		ChatSearchDialectResponses, ChatSearchDialectMessages:
		return ChatSearchDialect(value), nil
	default:
		return "", fmt.Errorf("must be %q, %q, %q, or %q",
			ChatSearchDialectWebSearchOptions, ChatSearchDialectSearchParameters,
			ChatSearchDialectResponses, ChatSearchDialectMessages)
	}
}

func envKeyList(v any) []string {
	switch t := v.(type) {
	case string:
		t = strings.TrimSpace(t)
		if t == "" {
			return nil
		}
		return []string{t}
	case []any:
		var out []string
		for _, el := range t {
			if s := strings.TrimSpace(str(el)); s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func first(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	return ss[0]
}

func str(v any) string {
	if v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

// ResolveAPIKey implements Grok credential order for static config fields:
// api_key > env_key (first non-empty env) — OAuth / XAI_API_KEY are session-level and
// handled by Grok itself; hellogrok only injects when a channel key exists.
func ResolveAPIKey(m Model) string {
	if k := strings.TrimSpace(m.APIKey); k != "" {
		return k
	}
	for _, name := range m.EnvKeys {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
	}
	return ""
}

func resolveExtraHeaders(m Model) (map[string]string, error) {
	headers := cloneStringMap(m.ExtraHeaders)
	for header, envName := range m.EnvHTTPHeaders {
		if value := strings.TrimSpace(os.Getenv(strings.TrimSpace(envName))); value != "" {
			if err := validateRouteHeader(header, value); err != nil {
				return nil, fmt.Errorf("environment variable %q: %w", strings.TrimSpace(envName), err)
			}
			setStringMapCaseInsensitive(headers, header, value)
		}
	}
	return headers, nil
}

// EffectiveOriginBase returns a direct upstream URL. Per-channel proxy URLs do
// not encode their origin; the running process keeps that origin in memory and
// crash recovery keeps it in the rewrite state file.
func EffectiveOriginBase(baseURL string) string {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return ""
	}
	if cfgpatch.IsProxyURL(baseURL) {
		return ""
	}
	return baseURL
}

// BuildRoutes groups models by upstream host for local routing.
// Config files are never modified here; routes are computed in memory only.
// Already-proxied values are skipped because their origin is intentionally not
// encoded in the public local URL.
func BuildRoutes(models []Model) ([]Route, error) {
	var out []Route
	for _, m := range models {
		if m.ContextWindowConfigured && m.ContextWindow == 0 {
			return nil, fmt.Errorf("model %q context window must be greater than zero", m.ID)
		}
		if m.MaxCompletionTokensConfigured && (m.MaxCompletionTokens == 0 || m.MaxCompletionTokens > uint64(^uint32(0))) {
			return nil, fmt.Errorf("model %q max completion tokens must be between 1 and %d", m.ID, uint64(^uint32(0)))
		}
		key := ResolveAPIKey(m)
		origin := m.BaseURL
		// Grok Build uses base_url for a model-owned api_key/env_key, an
		// auth-provider token, and a login session. api_base_url is only its
		// global XAI_API_KEY fallback. hellogrok intentionally does not treat
		// that global fallback as a credential for an arbitrary custom channel,
		// so channel-owned requests must retain the original base_url.
		if origin == "" {
			origin = m.APIBaseURL
		}
		origin = EffectiveOriginBase(origin)
		if origin == "" {
			continue
		}
		u, err := url.Parse(origin)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("model %q has an invalid custom base URL", m.ID)
		}
		scheme := strings.ToLower(u.Scheme)
		if scheme != "http" && scheme != "https" {
			return nil, fmt.Errorf("model %q uses unsupported URL scheme %q", m.ID, scheme)
		}
		if u.User != nil {
			return nil, fmt.Errorf("model %q custom base URL must not contain user info", m.ID)
		}
		if u.Fragment != "" {
			return nil, fmt.Errorf("model %q custom base URL must not contain a fragment", m.ID)
		}
		host := u.Hostname()
		if host == "" {
			return nil, fmt.Errorf("model %q custom base URL has no host", m.ID)
		}
		// Never route the local compat proxy into itself as an "upstream host".
		ip := net.ParseIP(host)
		loopback := strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback())
		if loopback && u.Port() == "18787" {
			continue
		}
		displayHost := host
		if u.Port() != "" && u.Port() != "80" && u.Port() != "443" {
			displayHost = net.JoinHostPort(host, u.Port())
		}
		backend := strings.TrimSpace(strings.ToLower(m.APIBackend))
		if backend != "" && backend != "responses" && backend != "messages" && backend != "chat_completions" {
			return nil, fmt.Errorf("model %q uses unsupported api_backend %q", m.ID, backend)
		}
		wireModel := strings.TrimSpace(m.Model)
		if wireModel == "" {
			wireModel = m.ID
		}
		incomingAuthScheme := m.IncomingAuthScheme
		if incomingAuthScheme == "" {
			incomingAuthScheme = "bearer"
		}
		authScheme := m.AuthScheme
		if authScheme == "" {
			authScheme = "bearer"
		}
		extraHeaders, err := resolveExtraHeaders(m)
		if err != nil {
			return nil, fmt.Errorf("model %q has invalid HTTP headers: %w", m.ID, err)
		}
		route := Route{
			ChannelID:                          m.ID,
			Host:                               displayHost,
			OriginBase:                         origin,
			APIBackend:                         backend,
			APIBackendConfigured:               m.APIBackendConfigured,
			ChatSearchDialect:                  m.ChatSearchDialect,
			WireModel:                          wireModel,
			APIKey:                             key,
			AuthScheme:                         authScheme,
			IncomingAuthScheme:                 incomingAuthScheme,
			DynamicAuth:                        m.DynamicAuth && key == "",
			ExtraHeaders:                       extraHeaders,
			SupportsBackendSearch:              m.SupportsBackendSearch,
			SupportsBackendSearchConfigured:    m.SupportsBackendSearchConfigured,
			ReasoningEffortConfigured:          m.ReasoningEffortConfigured,
			ReasoningEffortEnabled:             m.ReasoningEffortEnabled,
			LegacyGeneratedReasoningMenu:       m.LegacyGeneratedReasoningMenu,
			ReasoningEffortSelectionConfigured: m.ReasoningEffortSelectionConfigured,
			ContextWindow:                      m.ContextWindow,
			ContextWindowConfigured:            m.ContextWindowConfigured,
			MaxCompletionTokens:                m.MaxCompletionTokens,
			MaxCompletionTokensConfigured:      m.MaxCompletionTokensConfigured,
			InferenceIdleTimeoutSecs:           m.InferenceIdleTimeoutSecs,
			InferenceIdleTimeoutConfigured:     m.InferenceIdleTimeoutConfigured,
		}
		// DeepSeek documents the effort surface at the first-party protocol
		// endpoints. Keep this independent of model IDs so rolling aliases and
		// future models inherit it without a hellogrok release.
		if !route.ReasoningEffortConfigured && IsOfficialDeepSeekRoute(route) {
			route.ReasoningEffortEnabled = true
		}
		out = append(out, route)
	}
	return out, nil
}
