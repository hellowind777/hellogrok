package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
		inherited.APIBackend != "messages" || inherited.AuthScheme != "x_api_key" {
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
	if own.BaseURL != "https://own.test/v1" || own.APIBackend != "responses" || own.APIKey != "own-key" || own.AuthScheme != "bearer" {
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
