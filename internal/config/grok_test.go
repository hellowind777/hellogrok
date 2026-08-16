package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadModelsAcceptsUTF8BOMAndReportsInvalidFileLocation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	valid := append([]byte{0xEF, 0xBB, 0xBF}, []byte("[model.one]\nbase_url = \"https://example.test/v1\"\n")...)
	if err := os.WriteFile(path, valid, 0o600); err != nil {
		t.Fatal(err)
	}
	models, err := LoadModels(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "one" {
		t.Fatalf("models = %+v", models)
	}

	invalid := append([]byte{0xEF, 0xBB, 0xBF}, []byte("[model.one]\nbase_url =\n")...)
	if err := os.WriteFile(path, invalid, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = LoadModels(path)
	if err == nil {
		t.Fatal("invalid TOML was accepted")
	}
	for _, want := range []string{path, "第 2 行", "第 11 列"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
	if !bytes.HasPrefix(invalid, []byte{0xEF, 0xBB, 0xBF}) {
		t.Fatal("test input lost its UTF-8 BOM")
	}
}

func TestLoadModelsSupportsQuotedAndLegacyDottedChannelIDs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	raw := `[model."provider.with-dot"]
name = "Provider.v1-beta"
model = "wire.one"
base_url = "https://quoted.example/v1"
api_key = "quoted-key"

[model.legacy.with-dot]
name = "Legacy.v2-beta"
model = "wire.two"
base_url = "https://legacy.example/v1"
api_key = "legacy-key"

[model.provider-with-dash]
name = "Provider-with-dash.v3"
model = "wire-three"
base_url = "https://dash.example/v1"
api_key = "dash-key"
`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	models, err := LoadModels(path)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]Model, len(models))
	for _, model := range models {
		byID[model.ID] = model
	}
	for id, wantName := range map[string]string{
		"provider.with-dot":  "Provider.v1-beta",
		"legacy.with-dot":    "Legacy.v2-beta",
		"provider-with-dash": "Provider-with-dash.v3",
	} {
		if got := byID[id]; got.ID != id || got.Name != wantName {
			t.Fatalf("model %q = %+v, want name %q", id, got, wantName)
		}
	}
	routes, err := BuildRoutes(models)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 3 {
		t.Fatalf("routes=%d, want 3", len(routes))
	}
	for _, route := range routes {
		if route.APIKey == "" || route.ChannelID == "" {
			t.Fatalf("special-character route lost identity or auth: %+v", route)
		}
	}
}

func TestBuildRoutesRejectsDeprecatedSingularMessageBackend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	raw := `[model.claude-llmx]
model = "claude-opus-5"
base_url = "https://messages.example.test/v1"
api_key = "test-key"
api_backend = "message"
`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	models, err := LoadModels(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].APIBackend != "message" {
		t.Fatalf("models = %+v, want source value preserved for validation", models)
	}
	if _, err := BuildRoutes(models); err == nil || !strings.Contains(err.Error(), `unsupported api_backend "message"`) {
		t.Fatalf("deprecated backend was not rejected: %v", err)
	}
}

func TestLoadModelsValidatesExplicitChatSearchDialect(t *testing.T) {
	for _, dialect := range []ChatSearchDialect{
		ChatSearchDialectSearchParameters,
		ChatSearchDialectWebSearchOptions,
		ChatSearchDialectResponses,
		ChatSearchDialectMessages,
	} {
		t.Run(string(dialect), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			raw := "[model.chat]\nbase_url = \"https://relay.example/v1\"\napi_backend = \"chat_completions\"\n" +
				`chat_search_dialect = "` + string(dialect) + `"` + "\n"
			if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			models, err := LoadModels(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(models) != 1 || models[0].ChatSearchDialect != dialect {
				t.Fatalf("models=%+v", models)
			}
			routes, err := BuildRoutes(models)
			if err != nil || routes[0].ChatSearchDialect != dialect {
				t.Fatalf("routes=%+v err=%v", routes, err)
			}
		})
	}

	path := filepath.Join(t.TempDir(), "config.toml")
	raw := "[model.chat]\nbase_url = \"https://relay.example/v1\"\napi_backend = \"chat_completions\"\nchat_search_dialect = \"automatic\"\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadModels(path); err == nil || !strings.Contains(err.Error(), "chat_search_dialect") {
		t.Fatalf("invalid dialect was accepted: %v", err)
	}
}

func TestBuildRoutesRecordsWireModels(t *testing.T) {
	routes, err := BuildRoutes([]Model{
		{ID: "gpt-channel", Model: "gpt-5.6-sol", BaseURL: "https://example.test/v1", APIBackend: "responses"},
		{ID: "gpt-channel-2", Model: "gpt-5.6-sol", BaseURL: "https://example.test/v1", APIBackend: "responses"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 2 {
		t.Fatalf("routes=%d", len(routes))
	}
	for _, route := range routes {
		if route.ChannelID == "" || route.WireModel != "gpt-5.6-sol" || route.APIBackend != "responses" {
			t.Fatalf("route=%+v", route)
		}
	}
}

func TestLoadModelsResolvesAutoCompactThresholdPerModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	raw := `[session]
auto_compact_threshold_percent = 80

[model.inherited]
base_url = "https://one.example/v1"

[model.explicit]
base_url = "https://two.example/v1"
auto_compact_threshold_percent = 65
`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	models, err := LoadModels(path)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Model{}
	for _, model := range models {
		byID[model.ID] = model
	}
	if got := byID["inherited"]; got.AutoCompactThresholdPercent != 80 || !got.AutoCompactThresholdConfigured {
		t.Fatalf("global threshold was not inherited: %+v", got)
	}
	if got := byID["explicit"]; got.AutoCompactThresholdPercent != 65 || !got.AutoCompactThresholdConfigured {
		t.Fatalf("model threshold did not win: %+v", got)
	}
	routes, err := BuildRoutes(models)
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range routes {
		if route.AutoCompactThresholdPercent != byID[route.ChannelID].AutoCompactThresholdPercent {
			t.Fatalf("route lost threshold: %+v", route)
		}
	}
}

func TestLoadModelsDefaultsAutoCompactThresholdTo85AndValidatesRange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[model.one]\nbase_url = \"https://example.test/v1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	models, err := LoadModels(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].AutoCompactThresholdPercent != 85 || models[0].AutoCompactThresholdConfigured {
		t.Fatalf("default threshold = %+v", models)
	}
	if err := os.WriteFile(path, []byte("[model.one]\nbase_url = \"https://example.test/v1\"\nauto_compact_threshold_percent = 101\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadModels(path); err == nil || !strings.Contains(err.Error(), "between 0 and 100") {
		t.Fatalf("out-of-range threshold was accepted: %v", err)
	}
}

func TestOfficialDeepSeekRouteRecognitionUsesExactAPIHost(t *testing.T) {
	tests := []struct {
		name  string
		route Route
		want  bool
	}{
		{"pro", Route{Host: "api.deepseek.com", WireModel: "deepseek-v4-pro"}, true},
		{"future model", Route{Host: "api.deepseek.com:443", WireModel: "deepseek-future-model"}, true},
		{"root website", Route{Host: "deepseek.com", WireModel: "deepseek-v4-pro"}, false},
		{"other subdomain", Route{Host: "proxy.deepseek.com", WireModel: "deepseek-v4-pro"}, false},
		{"lookalike host", Route{Host: "api.deepseek.com.evil", WireModel: "deepseek-v4-pro"}, false},
		{"relay", Route{Host: "relay.example", WireModel: "deepseek-v4-pro"}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := IsOfficialDeepSeekRoute(test.route); got != test.want {
				t.Fatalf("IsOfficialDeepSeekRoute(%+v)=%t want %t", test.route, got, test.want)
			}
		})
	}
}

func TestLoadModelsAndRoutesPreserveChannelAuthAndHeaders(t *testing.T) {
	t.Setenv("DEEPSEEK_TEST_KEY", "env-deepseek-key")
	t.Setenv("TENANT_TEST_TOKEN", "tenant-token")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	raw := `[models]
extra_headers = { "X-Global" = "global", "X-Override" = "global-value" }

[model.deepseek-pro]
model = "deepseek-v4-pro"
base_url = "https://api.deepseek.com/anthropic"
api_backend = "messages"
env_key = ["UNSET_DEEPSEEK_TEST_KEY", "DEEPSEEK_TEST_KEY"]
auth_scheme = "x_api_key"
extra_headers = { "x-override" = "model-value" }
env_http_headers = { "X-Tenant" = "TENANT_TEST_TOKEN" }

[model.bearer-messages]
base_url = "https://messages.example.test/v1"
api_backend = "messages"
api_key = "static-key"
auth_scheme = "bearer"
`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	models, err := LoadModels(path)
	if err != nil {
		t.Fatal(err)
	}
	routes, err := BuildRoutes(models)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 2 {
		t.Fatalf("routes=%d", len(routes))
	}
	byID := map[string]Route{}
	for _, route := range routes {
		byID[route.ChannelID] = route
	}
	deepseek := byID["deepseek-pro"]
	if deepseek.APIKey != "env-deepseek-key" || deepseek.AuthScheme != "x_api_key" {
		t.Fatalf("DeepSeek auth mismatch: %+v", deepseek)
	}
	if headerValueTest(deepseek.ExtraHeaders, "X-Global") != "global" ||
		headerValueTest(deepseek.ExtraHeaders, "X-Override") != "model-value" ||
		headerValueTest(deepseek.ExtraHeaders, "X-Tenant") != "tenant-token" {
		t.Fatalf("merged headers mismatch: %#v", deepseek.ExtraHeaders)
	}
	bearer := byID["bearer-messages"]
	if bearer.AuthScheme != "bearer" || bearer.APIKey != "static-key" {
		t.Fatalf("explicit bearer auth was not preserved: %+v", bearer)
	}
}

func TestLoadModelsRejectsInvalidOrProtectedHTTPHeaders(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "protected protocol header",
			raw: `[model.one]
base_url = "https://one.example/v1"
extra_headers = { "Content-Type" = "text/plain" }
`,
			want: "Content-Type",
		},
		{
			name: "invalid name",
			raw: `[model.one]
base_url = "https://one.example/v1"
extra_headers = { "Bad Header" = "value" }
`,
			want: "invalid HTTP header name",
		},
		{
			name: "newline injection",
			raw: `[model.one]
base_url = "https://one.example/v1"
extra_headers = { "X-Test" = "first\nInjected: second" }
`,
			want: "invalid HTTP header value",
		},
		{
			name: "case insensitive duplicate",
			raw: `[model.one]
base_url = "https://one.example/v1"
extra_headers = { "X-Tenant" = "one", "x-tenant" = "two" }
`,
			want: "duplicate HTTP header",
		},
		{
			name: "non string value",
			raw: `[model.one]
base_url = "https://one.example/v1"
extra_headers = { "X-Test" = 7 }
`,
			want: "must be a string",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(test.raw), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadModels(path)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want text %q", err, test.want)
			}
		})
	}
}

func TestRouteHeaderValidationAllowsChannelCredentials(t *testing.T) {
	headers := map[string]string{
		"Authorization": "Bearer channel-token",
		"X-Api-Key":     "channel-key",
		"X-Tenant":      "tenant",
	}
	if err := validateRouteHeaders(headers); err != nil {
		t.Fatalf("valid channel headers rejected: %v", err)
	}
}

func TestBuildRoutesRejectsInvalidEnvironmentHeaderValue(t *testing.T) {
	t.Setenv("INJECTED_HEADER_VALUE", "valid\r\nInjected: true")
	path := filepath.Join(t.TempDir(), "config.toml")
	raw := `[model.one]
base_url = "https://one.example/v1"
env_http_headers = { "X-Tenant" = "INJECTED_HEADER_VALUE" }
`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	models, err := LoadModels(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = BuildRoutes(models)
	if err == nil || !strings.Contains(err.Error(), "invalid HTTP header value") {
		t.Fatalf("invalid environment header reached a route: %v", err)
	}
}

func TestLoadModelsInheritsModelProviderConnectionAndAuth(t *testing.T) {
	t.Setenv("PROVIDER_TEST_KEY", "provider-env-key")
	t.Setenv("PROVIDER_HEADER_ENV", "provider-header")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	raw := `[models]
extra_headers = { "X-Global" = "global" }

[model_providers.gateway]
base_url = "https://session.gateway.test/v1"
api_base_url = "https://key.gateway.test/v1"
api_backend = "messages"
auth_scheme = "x_api_key"
env_key = ["UNSET_PROVIDER_TEST_KEY", "PROVIDER_TEST_KEY"]
extra_headers = { "X-Provider" = "yes" }
env_http_headers = { "X-Provider-Token" = "PROVIDER_HEADER_ENV" }

[model.inherited]
model = "wire-inherited"
model_provider = "gateway"

[model.own]
model = "wire-own"
model_provider = "gateway"
base_url = "https://own.test/v1"
api_backend = "responses"
auth_scheme = "bearer"
api_key = "own-key"
extra_headers = { "X-Own" = "yes" }

[model_providers.dynamic]
base_url = "https://dynamic.test/v1"
auth = { command = "credential-helper" }

[model.dynamic]
model_provider = "dynamic"
`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	models, err := LoadModels(path)
	if err != nil {
		t.Fatal(err)
	}
	byModel := map[string]Model{}
	for _, model := range models {
		byModel[model.ID] = model
	}
	inherited := byModel["inherited"]
	if inherited.BaseURL != "https://session.gateway.test/v1" ||
		inherited.APIBaseURL != "https://key.gateway.test/v1" ||
		inherited.APIBackend != "messages" || !inherited.APIBackendConfigured || inherited.AuthScheme != "x_api_key" {
		t.Fatalf("provider connection was not inherited: %+v", inherited)
	}
	if ResolveAPIKey(inherited) != "provider-env-key" {
		t.Fatalf("provider env key was not inherited: %+v", inherited.EnvKeys)
	}
	if headerValueTest(inherited.ExtraHeaders, "X-Global") != "global" ||
		headerValueTest(inherited.ExtraHeaders, "X-Provider") != "yes" {
		t.Fatalf("provider/global headers mismatch: %#v", inherited.ExtraHeaders)
	}

	own := byModel["own"]
	if own.BaseURL != "https://own.test/v1" || own.APIBackend != "responses" || !own.APIBackendConfigured || own.APIKey != "own-key" || own.AuthScheme != "bearer" {
		t.Fatalf("model fields did not override provider: %+v", own)
	}
	if headerValueTest(own.ExtraHeaders, "X-Provider") != "" || headerValueTest(own.ExtraHeaders, "X-Own") != "yes" {
		t.Fatalf("provider headers must be inherited wholesale only when model headers are empty: %#v", own.ExtraHeaders)
	}

	routes, err := BuildRoutes(models)
	if err != nil {
		t.Fatal(err)
	}
	byRoute := map[string]Route{}
	for _, route := range routes {
		byRoute[route.ChannelID] = route
	}
	if got := byRoute["inherited"].OriginBase; got != "https://session.gateway.test/v1" {
		t.Fatalf("model-owned API key must retain base_url, got %q", got)
	}
	if byRoute["inherited"].IncomingAuthScheme != "bearer" || byRoute["inherited"].AuthScheme != "x_api_key" {
		t.Fatalf("provider-only auth_scheme must affect the upstream leg, not Build's local leg: %+v", byRoute["inherited"])
	}
	if !byRoute["dynamic"].DynamicAuth || byRoute["dynamic"].APIKey != "" {
		t.Fatalf("dynamic provider was not marked as channel-owned auth: %+v", byRoute["dynamic"])
	}
	if byRoute["dynamic"].IncomingAuthScheme != "bearer" {
		t.Fatalf("Build-facing default auth scheme mismatch: %+v", byRoute["dynamic"])
	}
	if byModel["dynamic"].APIBackendConfigured || byRoute["dynamic"].APIBackendConfigured || byRoute["dynamic"].APIBackend != "" {
		t.Fatalf("an omitted API backend must remain available for Grok Build catalog inheritance: model=%+v route=%+v", byModel["dynamic"], byRoute["dynamic"])
	}
}

func TestLoadModelsOnlyTrustsUsableAuthProviders(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	raw := `[auth_provider.valid]
command = "credential-helper"

[auth_provider.empty]
command = "   "

[auth_provider.malformed]
command = "credential-helper"
args = "must-be-an-array"

[model.valid]
base_url = "https://valid.example/v1"
auth_provider = "valid"

[model.undefined]
base_url = "https://undefined.example/v1"
auth_provider = "missing"

[model.empty]
base_url = "https://empty.example/v1"
auth_provider = "empty"

[model.malformed]
base_url = "https://malformed.example/v1"
auth_provider = "malformed"

[model.static-wins]
base_url = "https://static.example/v1"
api_key = "channel-key"
auth_provider = "valid"

[model_providers.inline]
base_url = "https://inline.example/v1"
auth = { command = "inline-helper", token_ttl_secs = 3600 }

[model.inline]
model_provider = "inline"

[model_providers.named-shadows-inline]
base_url = "https://shadowed.example/v1"
auth_provider = "missing"
auth = { command = "must-not-win" }

[model.named-shadows-inline]
model_provider = "named-shadows-inline"
`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	models, err := LoadModels(path)
	if err != nil {
		t.Fatal(err)
	}
	routes, err := BuildRoutes(models)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Route{}
	for _, route := range routes {
		byID[route.ChannelID] = route
	}
	if !byID["valid"].DynamicAuth {
		t.Fatal("usable named auth provider was rejected")
	}
	if !byID["inline"].DynamicAuth {
		t.Fatal("usable inline auth provider was rejected")
	}
	for _, id := range []string{"undefined", "empty", "malformed", "named-shadows-inline"} {
		if byID[id].DynamicAuth {
			t.Fatalf("unusable auth provider %q was trusted: %+v", id, byID[id])
		}
	}
	if byID["static-wins"].APIKey != "channel-key" || byID["static-wins"].DynamicAuth {
		t.Fatalf("static key did not win: %+v", byID["static-wins"])
	}
}

func TestLoadModelsInheritsBackendSearchCapability(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	raw := `[model_providers.searchable]
base_url = "https://searchable.example/v1"
supports_backend_search = true

[model.inherited]
model_provider = "searchable"

[model.disabled]
model_provider = "searchable"
supports_backend_search = false

[model.default-disabled]
base_url = "https://plain.example/v1"
`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	models, err := LoadModels(path)
	if err != nil {
		t.Fatal(err)
	}
	byModel := map[string]Model{}
	for _, model := range models {
		byModel[model.ID] = model
	}
	if !byModel["inherited"].SupportsBackendSearch {
		t.Fatal("provider backend-search capability was not inherited")
	}
	if !byModel["inherited"].SupportsBackendSearchConfigured ||
		!byModel["disabled"].SupportsBackendSearchConfigured ||
		byModel["default-disabled"].SupportsBackendSearchConfigured {
		t.Fatalf("explicit and omitted backend-search values were not distinguished: %#v", byModel)
	}
	if byModel["disabled"].SupportsBackendSearch || byModel["default-disabled"].SupportsBackendSearch {
		t.Fatalf("false or missing capability became enabled: %#v", byModel)
	}

	routes, err := BuildRoutes(models)
	if err != nil {
		t.Fatal(err)
	}
	byRoute := map[string]Route{}
	for _, route := range routes {
		byRoute[route.ChannelID] = route
	}
	if !byRoute["inherited"].SupportsBackendSearch || byRoute["disabled"].SupportsBackendSearch || byRoute["default-disabled"].SupportsBackendSearch {
		t.Fatalf("route capabilities do not match config: %#v", byRoute)
	}
	if !byRoute["inherited"].SupportsBackendSearchConfigured ||
		!byRoute["disabled"].SupportsBackendSearchConfigured ||
		byRoute["default-disabled"].SupportsBackendSearchConfigured {
		t.Fatalf("route capability provenance does not match config: %#v", byRoute)
	}
}

func TestLoadModelsPreservesConfiguredLimitPrecedence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	raw := `[models]
inference_idle_timeout_secs = 900

[model_providers.tiered]
base_url = "https://tiered.example/v1"
context_window = 262144
max_completion_tokens = 16384

[model.inherited]
model_provider = "tiered"

[model.override]
model_provider = "tiered"
context_window = 131072
max_completion_tokens = 8192
inference_idle_timeout_secs = 300

[model.remote]
base_url = "https://remote.example/v1"
`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	models, err := LoadModels(path)
	if err != nil {
		t.Fatal(err)
	}
	byModel := map[string]Model{}
	for _, model := range models {
		byModel[model.ID] = model
	}
	if got := byModel["inherited"]; !got.ContextWindowConfigured || got.ContextWindow != 262144 ||
		!got.MaxCompletionTokensConfigured || got.MaxCompletionTokens != 16384 ||
		!got.InferenceIdleTimeoutConfigured || got.InferenceIdleTimeoutSecs != 900 {
		t.Fatalf("provider limits were not inherited: %+v", got)
	}
	if got := byModel["override"]; !got.ContextWindowConfigured || got.ContextWindow != 131072 ||
		!got.MaxCompletionTokensConfigured || got.MaxCompletionTokens != 8192 ||
		!got.InferenceIdleTimeoutConfigured || got.InferenceIdleTimeoutSecs != 300 {
		t.Fatalf("model limits did not override provider: %+v", got)
	}
	if got := byModel["remote"]; got.ContextWindowConfigured || got.ContextWindow != 0 ||
		got.MaxCompletionTokensConfigured || got.MaxCompletionTokens != 0 ||
		!got.InferenceIdleTimeoutConfigured || got.InferenceIdleTimeoutSecs != 900 {
		t.Fatalf("omitted token limits or global timeout were resolved incorrectly: %+v", got)
	}

	routes, err := BuildRoutes(models)
	if err != nil {
		t.Fatal(err)
	}
	byRoute := map[string]Route{}
	for _, route := range routes {
		byRoute[route.ChannelID] = route
	}
	for _, id := range []string{"inherited", "override", "remote"} {
		model, route := byModel[id], byRoute[id]
		if route.ContextWindow != model.ContextWindow ||
			route.ContextWindowConfigured != model.ContextWindowConfigured ||
			route.MaxCompletionTokens != model.MaxCompletionTokens ||
			route.MaxCompletionTokensConfigured != model.MaxCompletionTokensConfigured ||
			route.InferenceIdleTimeoutSecs != model.InferenceIdleTimeoutSecs ||
			route.InferenceIdleTimeoutConfigured != model.InferenceIdleTimeoutConfigured {
			t.Fatalf("route %q lost limit provenance: model=%+v route=%+v", id, model, route)
		}
	}
}

func TestLoadModelsAcceptsZeroInferenceIdleTimeoutLikeGrokBuild(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	raw := "[models]\ninference_idle_timeout_secs = 0\n\n[model.zero]\nbase_url = \"https://example.test/v1\"\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	models, err := LoadModels(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || !models[0].InferenceIdleTimeoutConfigured || models[0].InferenceIdleTimeoutSecs != 0 {
		t.Fatalf("zero timeout was not preserved: %#v", models)
	}
}

func TestReasoningCapabilityDefaultsAreConfiguredFirstAndHostScoped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	raw := `[model.v4-default]
model = "deepseek-v4-pro"
base_url = "https://api.deepseek.com"

[model.future-default]
model = "deepseek-future-model"
base_url = "https://api.deepseek.com"

[model.future-configured]
model = "deepseek-future-model"
base_url = "https://api.deepseek.com"
supports_reasoning_effort = true
reasoning_efforts = [{ value = "max", label = "Future Max", default = true }]

[model.v4-disabled]
model = "deepseek-v4-flash"
base_url = "https://api.deepseek.com"
supports_reasoning_effort = false
`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	models, err := LoadModels(path)
	if err != nil {
		t.Fatal(err)
	}
	routes, err := BuildRoutes(models)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Route{}
	for _, route := range routes {
		byID[route.ChannelID] = route
	}
	if got := byID["v4-default"]; got.ReasoningEffortConfigured || !got.ReasoningEffortEnabled {
		t.Fatalf("official DeepSeek default capability = %+v", got)
	}
	if got := byID["future-default"]; got.ReasoningEffortConfigured || !got.ReasoningEffortEnabled {
		t.Fatalf("future official model missed endpoint capability = %+v", got)
	}
	if got := byID["future-configured"]; !got.ReasoningEffortConfigured || !got.ReasoningEffortEnabled {
		t.Fatalf("future explicit capability was lost = %+v", got)
	}
	if got := byID["v4-disabled"]; !got.ReasoningEffortConfigured || got.ReasoningEffortEnabled {
		t.Fatalf("explicit V4 opt-out was overridden = %+v", got)
	}
}

func TestLoadModelsRecognizesOnlyExactLegacyGeneratedReasoningMenu(t *testing.T) {
	legacy := `reasoning_efforts = [{ value = "none", label = "None" }, { value = "low", label = "Low" }, { value = "high", label = "High", default = true }, { value = "max", label = "Max" }]`
	tests := []struct {
		name       string
		menu       string
		wantLegacy bool
	}{
		{"legacy inline objects", legacy, true},
		{"compact full list", `reasoning_efforts = ["none", "low", "high", "max"]`, false},
		{"compact list without none", `reasoning_efforts = ["low", "high", "max"]`, false},
		{"explicit empty list", `reasoning_efforts = []`, false},
		{"custom labels", `reasoning_efforts = [{ value = "none", label = "Off" }, { value = "low", label = "Low" }, { value = "high", label = "High", default = true }, { value = "max", label = "Max" }]`, false},
		{"custom order", `reasoning_efforts = [{ value = "low", label = "Low" }, { value = "none", label = "None" }, { value = "high", label = "High", default = true }, { value = "max", label = "Max" }]`, false},
		{"custom default", `reasoning_efforts = [{ value = "none", label = "None", default = true }, { value = "low", label = "Low" }, { value = "high", label = "High" }, { value = "max", label = "Max" }]`, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			raw := "[model.deepseek]\nmodel = \"deepseek-v4-pro\"\nbase_url = \"https://api.deepseek.com\"\n" + test.menu + "\n"
			if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			models, err := LoadModels(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(models) != 1 || !models[0].ReasoningEffortConfigured ||
				models[0].LegacyGeneratedReasoningMenu != test.wantLegacy {
				t.Fatalf("models = %+v", models)
			}
		})
	}
}

func TestLoadModelsTracksReasoningSelectionAtModelOrGlobalScope(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	raw := `[models]
default_reasoning_effort = "low"

[model.global]
base_url = "https://api.deepseek.com"

[model.local]
base_url = "https://api.deepseek.com"
reasoning_effort = "max"
`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	models, err := LoadModels(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range models {
		if !model.ReasoningEffortSelectionConfigured {
			t.Fatalf("model %q lost explicit reasoning selection: %+v", model.ID, model)
		}
	}
}

func TestLoadModelsRejectsMalformedReasoningCapabilityFields(t *testing.T) {
	for _, body := range []string{
		"supports_reasoning_effort = \"yes\"\n",
		"reasoning_efforts = \"high\"\n",
	} {
		path := filepath.Join(t.TempDir(), "config.toml")
		raw := "[model.invalid]\nbase_url = \"https://api.deepseek.com\"\n" + body
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadModels(path); err == nil {
			t.Fatalf("malformed reasoning capability was accepted: %s", body)
		}
	}
}

func TestLoadModelsRejectsInvalidConfiguredContextWindow(t *testing.T) {
	for _, value := range []string{"0", "-1", `"131072"`, "131072.5"} {
		t.Run(value, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			raw := "[model.invalid]\nbase_url = \"https://invalid.example/v1\"\ncontext_window = " + value + "\n"
			if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadModels(path); err == nil || !strings.Contains(err.Error(), "context_window must be a positive integer") {
				t.Fatalf("invalid context window %s error = %v", value, err)
			}
		})
	}
}

func TestLoadModelsRejectsInvalidConfiguredOutputLimits(t *testing.T) {
	tests := []struct {
		field string
		value string
	}{
		{"max_completion_tokens", "0"},
		{"max_completion_tokens", "4294967296"},
		{"max_completion_tokens", `"384000"`},
	}
	for _, test := range tests {
		t.Run(test.field+"="+test.value, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			raw := "[model.invalid]\nbase_url = \"https://invalid.example/v1\"\n" + test.field + " = " + test.value + "\n"
			if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadModels(path); err == nil || !strings.Contains(err.Error(), test.field) {
				t.Fatalf("invalid %s %s error = %v", test.field, test.value, err)
			}
		})
	}
}

func TestLoadWebSearchSelectionUsesEnvironmentThenConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[models]\nweb_search = \"configured-search\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GROK_WEB_SEARCH_MODEL", " environment-search ")
	selection, err := LoadWebSearchSelection(path)
	if err != nil {
		t.Fatal(err)
	}
	if !selection.Explicit || selection.Model != "environment-search" || selection.Source != "environment" {
		t.Fatalf("environment selection = %+v", selection)
	}

	t.Setenv("GROK_WEB_SEARCH_MODEL", "  ")
	selection, err = LoadWebSearchSelection(path)
	if err != nil {
		t.Fatal(err)
	}
	if !selection.Explicit || selection.Model != "configured-search" || selection.Source != "config" {
		t.Fatalf("config selection = %+v", selection)
	}

	if err := os.WriteFile(path, []byte("[models]\ndefault = \"one\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	selection, err = LoadWebSearchSelection(path)
	if err != nil {
		t.Fatal(err)
	}
	if selection.Explicit || selection.Model != "" || selection.Source != "" {
		t.Fatalf("missing selection = %+v", selection)
	}
}

func TestLoadWebSearchSelectionPreservesExplicitEmptyAndRejectsWrongType(t *testing.T) {
	t.Setenv("GROK_WEB_SEARCH_MODEL", "")
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[models]\nweb_search = \"   \"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	selection, err := LoadWebSearchSelection(path)
	if err != nil {
		t.Fatal(err)
	}
	if !selection.Explicit || selection.Model != "" || selection.Source != "config" {
		t.Fatalf("explicit empty selection = %+v", selection)
	}

	if err := os.WriteFile(path, []byte("[models]\nweb_search = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWebSearchSelection(path); err == nil || !strings.Contains(err.Error(), "[models].web_search must be a string") {
		t.Fatalf("wrong type error = %v", err)
	}
}

func TestLoadModelsRejectsNonBooleanBackendSearchCapability(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
	}{
		{
			name: "model value",
			raw:  "[model.one]\nbase_url = \"https://one.example/v1\"\nsupports_backend_search = \"yes\"\n",
		},
		{
			name: "provider value",
			raw:  "[model_providers.gateway]\nbase_url = \"https://one.example/v1\"\nsupports_backend_search = 1\n\n[model.one]\nmodel_provider = \"gateway\"\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(test.raw), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadModels(path)
			if err == nil || !strings.Contains(err.Error(), "supports_backend_search must be a boolean") {
				t.Fatalf("non-boolean capability error = %v", err)
			}
		})
	}
}

func TestBuildRoutesRejectsInvalidCustomChannel(t *testing.T) {
	for _, rawURL := range []string{
		"://bad",
		"https://user:secret@example.com/v1",
		"https://example.com/v1#fragment",
	} {
		_, err := BuildRoutes([]Model{{ID: "broken", BaseURL: rawURL}})
		if err == nil || !strings.Contains(err.Error(), "broken") {
			t.Fatalf("invalid URL %q was silently accepted: %v", rawURL, err)
		}
	}
}

func TestBuildRoutesSkipsEveryLocalFacadeLoopbackForm(t *testing.T) {
	models := []Model{
		{ID: "ipv4", BaseURL: "http://127.0.0.1:18787/c/ipv4"},
		{ID: "ipv4-range", BaseURL: "http://127.0.0.2:18787/c/ipv4-range"},
		{ID: "ipv6", BaseURL: "http://[::1]:18787/c/ipv6"},
		{ID: "localhost-case", BaseURL: "http://LOCALHOST:18787/c/localhost-case"},
		{ID: "remote", BaseURL: "https://example.com:18787/v1"},
		{ID: "path-only", BaseURL: "https://example.org/v1/:18787"},
	}
	routes, err := BuildRoutes(models)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 2 || routes[0].ChannelID != "remote" || routes[1].ChannelID != "path-only" {
		t.Fatalf("loopback route filtering mismatch: %#v", routes)
	}
}

func headerValueTest(values map[string]string, name string) string {
	for key, value := range values {
		if strings.EqualFold(key, name) {
			return value
		}
	}
	return ""
}
