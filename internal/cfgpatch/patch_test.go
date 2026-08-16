package cfgpatch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestChannelProxyURLRoundTrip(t *testing.T) {
	got, err := ToChannelProxyURL("provider/model one")
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://127.0.0.1:18787/c/provider%2Fmodel%20one" {
		t.Fatalf("proxy URL = %q", got)
	}
	if id := ChannelIDFromProxyURL(got); id != "provider/model one" {
		t.Fatalf("channel id = %q", id)
	}
	if IsProxyURL("https://example.test/c/model") || IsProxyURL("http://127.0.0.1:18788/c/model") {
		t.Fatal("non-proxy URL was accepted")
	}
	for _, raw := range []string{
		"http://127.0.0.2:18787/c/model",
		"http://[::1]:18787/c/model",
		"http://LOCALHOST:18787/c/model",
	} {
		if !IsProxyURL(raw) {
			t.Fatalf("loopback facade URL %q was not classified as proxy", raw)
		}
	}
}

func TestApplyTargetsTemporarilyProjectsPerModelAutoCompactThreshold(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "[session]\nauto_compact_threshold_percent = 85\n\n" +
		"[model.one]\nbase_url = \"https://one.example/v1\"\nauto_compact_threshold_percent = 90 # user value\n\n" +
		"[model.two]\nbase_url = \"https://two.example/v1\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	one, two := uint8(58), uint8(78)
	result, err := ApplyTargets(configPath, statePath, []Target{
		{ID: "one", ContextWindow: 1_048_576, AutoCompactThresholdPercent: &one},
		{ID: "two", ContextWindow: 393_216, AutoCompactThresholdPercent: &two},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ContextWindows != 2 || result.AutoCompactThresholds != 2 {
		t.Fatalf("rewrite result = %+v", result)
	}
	patched, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(patched)
	if !strings.Contains(text, "auto_compact_threshold_percent = 58 # user value") ||
		!strings.Contains(text, "context_window = 1048576") ||
		!strings.Contains(text, "context_window = 393216") ||
		!strings.Contains(text, "auto_compact_threshold_percent = 78") {
		t.Fatalf("model thresholds were not projected independently:\n%s", text)
	}
	if _, err := Restore(configPath, statePath); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != original {
		t.Fatalf("threshold lifecycle was not byte-exact\nwant: %q\ngot:  %q", original, restored)
	}
}

func TestRestorePreservesConcurrentAutoCompactThresholdEdit(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "[model.one]\nbase_url = \"https://one.example/v1\"\nauto_compact_threshold_percent = 90\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	threshold := uint8(58)
	if _, err := ApplyTargets(configPath, statePath, []Target{{ID: "one", AutoCompactThresholdPercent: &threshold}}); err != nil {
		t.Fatal(err)
	}
	patched, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	userEdited := strings.Replace(string(patched), "auto_compact_threshold_percent = 58", "auto_compact_threshold_percent = 60", 1)
	if err := os.WriteFile(configPath, []byte(userEdited), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(configPath, statePath); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(restored), "auto_compact_threshold_percent = 60") ||
		strings.Contains(string(restored), "auto_compact_threshold_percent = 90") {
		t.Fatalf("concurrent user threshold was not preserved:\n%s", restored)
	}
}

func TestRestoreAutoCompactThresholdFromInvalidTOML(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "[model.one]\nbase_url = \"https://one.example/v1\"\nauto_compact_threshold_percent = 90\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	threshold := uint8(58)
	if _, err := ApplyTargets(configPath, statePath, []Target{{ID: "one", AutoCompactThresholdPercent: &threshold}}); err != nil {
		t.Fatal(err)
	}
	patched, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	invalid := strings.Replace(string(patched), "auto_compact_threshold_percent = 58", "auto_compact_threshold_percent = 60", 1) + "broken = [\n"
	if err := os.WriteFile(configPath, []byte(invalid), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(configPath, statePath); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(restored), "base_url = \"https://one.example/v1\"") ||
		!strings.Contains(string(restored), "auto_compact_threshold_percent = 60") ||
		!strings.Contains(string(restored), "broken = [") {
		t.Fatalf("invalid-TOML recovery lost user content:\n%s", restored)
	}
}

func TestApplyTargetsQuotesLegacyDottedModelHeaderAndRestoresExactly(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := strings.Join([]string{
		`[model.legacy.with-dot] # preserve header comment`,
		`name = "Legacy.v1-beta"`,
		`model = "wire.model-v1"`,
		`base_url = "https://legacy.example/v1"`,
		`api_key = "legacy-key"`,
		``,
		`[model."quoted.with-dot"]`,
		`name = "Quoted.v2-beta"`,
		`base_url = "https://quoted.example/v1"`,
		`api_key = "quoted-key"`,
		``,
		`[model.with-dash]`,
		`name = "Dash.v3-beta"`,
		`base_url = "https://dash.example/v1"`,
		`api_key = "dash-key"`,
		``,
	}, "\r\n")
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := ApplyTargets(configPath, statePath, []Target{
		{ID: "legacy.with-dot"},
		{ID: "quoted.with-dot"},
		{ID: "with-dash"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ModelSections != 1 || result.ValidatedTargets != 3 {
		t.Fatalf("rewrite result = %+v", result)
	}
	if len(result.LegacyModelAliases) != 1 || result.LegacyModelAliases["legacy"] != "legacy.with-dot" {
		t.Fatalf("legacy model aliases = %#v", result.LegacyModelAliases)
	}
	patched, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(patched)
	for _, want := range []string{
		`[model."legacy.with-dot"] # preserve header comment`,
		`[model."quoted.with-dot"]`,
		`[model.with-dash]`,
		`name = "Legacy.v1-beta"`,
		`base_url = "http://127.0.0.1:18787/c/legacy.with-dot"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("patched config missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, `[model.legacy.with-dot]`) {
		t.Fatalf("legacy dotted header was not normalized:\n%s", text)
	}

	if _, err := Restore(configPath, statePath); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != original {
		t.Fatalf("restore was not byte-exact\nwant: %q\ngot:  %q", original, restored)
	}
}

func TestApplyTargetsDoesNotGuessAmbiguousLegacyDottedModelAlias(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "[model.provider.one]\nbase_url = \"https://one.example/v1\"\n\n" +
		"[model.provider.two]\nbase_url = \"https://two.example/v1\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := ApplyTargets(configPath, statePath, []Target{{ID: "provider.one"}, {ID: "provider.two"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.ModelSections != 2 || len(result.LegacyModelAliases) != 0 {
		t.Fatalf("ambiguous rewrite result = %+v", result)
	}
	if _, err := Restore(configPath, statePath); err != nil {
		t.Fatal(err)
	}
	restored, _ := os.ReadFile(configPath)
	if string(restored) != original {
		t.Fatalf("ambiguous config was not restored exactly: %q", restored)
	}
}

func TestDetectCCSwitchTakeoverRequiresMarkerAndGrokLoopbackRoute(t *testing.T) {
	tests := []struct {
		name   string
		config string
		active bool
		model  string
	}{
		{
			name: "default port",
			config: "[models]\ndefault = \"grok\"\n\n[model.grok]\n" +
				"base_url = \"http://127.0.0.1:15721/grokbuild/v1\"\n" +
				"api_key = \"PROXY_MANAGED\"\napi_backend = \"responses\"\n",
			active: true,
			model:  "grok",
		},
		{
			name: "custom address port and non-default scan",
			config: "[models]\ndefault = \"other\"\n\n[model.other]\nbase_url = \"https://example.test/v1\"\n\n" +
				"[model.managed]\nbase_url = \"http://192.168.50.10:24567/grokbuild/v1/\"\napi_key = \"PROXY_MANAGED\"\n",
			active: true,
			model:  "managed",
		},
		{
			name:   "marker without route",
			config: "[model.grok]\nbase_url = \"https://example.test/grokbuild/v1\"\napi_key = \"PROXY_MANAGED\"\n",
		},
		{
			name:   "route without marker",
			config: "[model.grok]\nbase_url = \"http://127.0.0.1:15721/grokbuild/v1\"\napi_key = \"user-key\"\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(test.config), 0o600); err != nil {
				t.Fatal(err)
			}
			takeover, err := DetectCCSwitchTakeover(path)
			if err != nil {
				t.Fatal(err)
			}
			if takeover.Active() != test.active || takeover.ModelID != test.model {
				t.Fatalf("takeover = %+v, active=%t model=%q", takeover, test.active, test.model)
			}
		})
	}
}

func TestRelinquishRequiresAllHellogrokReferencesToBeGone(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	if err := os.WriteFile(statePath, []byte("state"), 0o600); err != nil {
		t.Fatal(err)
	}
	withProxy := "[model.one]\nbase_url = \"http://127.0.0.1:18787/c/one\"\n"
	if err := os.WriteFile(configPath, []byte(withProxy), 0o600); err != nil {
		t.Fatal(err)
	}
	if relinquished, err := Relinquish(configPath, statePath); err != nil || relinquished {
		t.Fatalf("relinquish with active route = %t, err=%v", relinquished, err)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("state removed while route remained: %v", err)
	}

	direct := "[model.one]\nbase_url = \"https://new-provider.example/v1\"\n"
	if err := os.WriteFile(configPath, []byte(direct), 0o600); err != nil {
		t.Fatal(err)
	}
	if relinquished, err := Relinquish(configPath, statePath); err != nil || !relinquished {
		t.Fatalf("relinquish after provider replacement = %t, err=%v", relinquished, err)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("state remains after relinquish: %v", err)
	}
}

func TestApplyTargetsAndRestoreExactConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := strings.Join([]string{
		"[models]",
		`default = "chat"`,
		`web_search = "user-owned-search"`,
		"stream_tool_calls = false",
		"",
		"[features] # preserve section comment",
		"web_fetch = false # user value",
		"",
		"[model.chat] # preserve section comment",
		`base_url = "https://session.example/v1" # preserve me`,
		`api_base_url = 'https://api.example/v1'`,
		`api_backend = "chat_completions"`,
		"supports_backend_search = false # user value",
		"",
		"[model.inherited]",
		`model_provider = "gateway"`,
		`model = "wire-model"`,
		"",
		"[model.official]",
		`model = "grok-4.5"`,
		"",
		"[model_providers.gateway]",
		`base_url = "https://gateway.example/v1"`,
		`api_backend = "messages"`,
		"",
	}, "\r\n")
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := ApplyTargets(configPath, statePath, []Target{
		{ID: "chat", APIBaseURL: true, APIBackend: "chat_completions", ProjectBackendSearch: true},
		{ID: "inherited", APIBackend: "messages"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.BaseURLs != 2 || result.APIBaseURLs != 1 ||
		result.BackendSearch != 0 || result.BackendTools != 1 || result.WebFetch != 1 || result.ValidatedTargets != 2 {
		t.Fatalf("unexpected result: %#v", result)
	}
	patchedBytes, _ := os.ReadFile(configPath)
	patched := string(patchedBytes)
	if got, want := sectionText(t, patched, "models"), sectionText(t, original, "models"); got != want {
		t.Fatalf("[models] changed while proxy was active\nwant: %q\ngot:  %q", want, got)
	}
	for _, want := range []string{
		`base_url = "http://127.0.0.1:18787/c/chat"`,
		`api_base_url = "http://127.0.0.1:18787/c/chat"`,
		`base_url = "http://127.0.0.1:18787/c/inherited"`,
		`api_backend = "chat_completions"`,
		`api_backend = "messages"`,
		`supports_backend_search = false # user value`,
		`backend_tools = true`,
		`web_fetch = true # user value`,
		`web_search = "user-owned-search"`,
		"stream_tool_calls = false",
		`base_url = "https://gateway.example/v1"`,
	} {
		if !strings.Contains(patched, want) {
			t.Fatalf("patched config missing %q:\n%s", want, patched)
		}
	}
	official := sectionText(t, patched, "model.official")
	if strings.Contains(official, "base_url") || strings.Contains(official, "supports_backend_search") {
		t.Fatalf("official model changed: %s", official)
	}
	inherited := sectionText(t, patched, "model.inherited")
	if strings.Contains(inherited, "supports_backend_search") {
		t.Fatalf("unconfigured capability overrode the remote catalog: %s", inherited)
	}

	restored, err := Restore(configPath, statePath)
	if err != nil {
		t.Fatal(err)
	}
	if restored != 6 {
		t.Fatalf("restored fields = %d", restored)
	}
	finalBytes, _ := os.ReadFile(configPath)
	if string(finalBytes) != original {
		t.Fatalf("restore was not byte-exact\nwant:\n%q\ngot:\n%q", original, string(finalBytes))
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("state file remains: %v", err)
	}
}

func TestApplyTargetsProjectsDeepSeekReasoningMenuAndRestoresMultilineValue(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := strings.Join([]string{
		"[model.pro]",
		`base_url = "https://api.deepseek.com"`,
		"supports_reasoning_effort = false # preserve original support flag",
		"reasoning_efforts = [",
		`  { value = "low", label = "Legacy Low" },`,
		`  { value = "xhigh", label = "Legacy Deep", default = true },`,
		"] # preserve original menu formatting",
		"",
	}, "\r\n")
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	efforts := []ReasoningEffortOption{
		{Value: "none", Label: "None"},
		{Value: "high", Label: "High", Default: true},
		{Value: "max", Label: "Max"},
	}
	targets := []Target{{ID: "pro", ReasoningEfforts: efforts}}
	result, err := ApplyTargets(configPath, statePath, targets)
	if err != nil {
		t.Fatal(err)
	}
	if result.SupportsReasoningEffort != 0 || result.ReasoningEfforts != 1 {
		t.Fatalf("reasoning projection result = %+v", result)
	}
	patched, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	wantMenu := `reasoning_efforts = ["none", "high", "max"]`
	if !strings.Contains(string(patched), "supports_reasoning_effort = false # preserve original support flag") ||
		!strings.Contains(string(patched), wantMenu) || strings.Contains(string(patched), "Legacy Low") {
		t.Fatalf("DeepSeek reasoning menu was not projected:\n%s", patched)
	}

	first := string(patched)
	secondResult, err := ApplyTargets(configPath, statePath, targets)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(configPath)
	if string(second) != first || secondResult.SupportsReasoningEffort != 0 || secondResult.ReasoningEfforts != 0 {
		t.Fatalf("reapply changed reasoning projection: result=%+v\n%s", secondResult, second)
	}

	if _, err := Restore(configPath, statePath); err != nil {
		t.Fatal(err)
	}
	restored, _ := os.ReadFile(configPath)
	if string(restored) != original {
		t.Fatalf("reasoning projection was not restored byte-exactly\nwant: %q\ngot:  %q", original, restored)
	}
}

func TestRestorePreservesConcurrentDeepSeekReasoningMenuEdit(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "[model.pro]\nbase_url = \"https://api.deepseek.com\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	efforts := []ReasoningEffortOption{
		{Value: "none", Label: "None"},
		{Value: "high", Label: "High", Default: true},
		{Value: "max", Label: "Max"},
	}
	if _, err := ApplyTargets(configPath, statePath, []Target{{ID: "pro", ReasoningEfforts: efforts}}); err != nil {
		t.Fatal(err)
	}
	patched, _ := os.ReadFile(configPath)
	userEdited := strings.ReplaceAll(string(patched),
		`reasoning_efforts = ["none", "high", "max"]`,
		"reasoning_efforts = [\n  { value = \"high\", label = \"Focused\", default = true },\n]\n")
	if err := os.WriteFile(configPath, []byte(userEdited), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(configPath, statePath); err != nil {
		t.Fatal(err)
	}
	current, _ := os.ReadFile(configPath)
	if !strings.Contains(string(current), `label = "Focused"`) ||
		strings.Contains(string(current), "127.0.0.1:18787") {
		t.Fatalf("concurrent reasoning edit was not preserved while the route was restored:\n%s", current)
	}
}

func TestLegacyReasoningMigrationRecoversStateBeforeConfigCrashWindow(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "[model.pro]\nbase_url = \"https://api.deepseek.com\"\n" +
		"[[model.pro.reasoning_efforts]]\nvalue = \"none\"\nlabel = \"None\"\n" +
		"[[model.pro.reasoning_efforts]]\nvalue = \"low\"\nlabel = \"Low\"\n" +
		"[[model.pro.reasoning_efforts]]\nvalue = \"high\"\nlabel = \"High\"\ndefault = true\n" +
		"[[model.pro.reasoning_efforts]]\nvalue = \"max\"\nlabel = \"Max\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	target := Target{
		ID: "pro",
		ReasoningEfforts: []ReasoningEffortOption{
			{Value: "none", Label: "None"},
			{Value: "low", Label: "Low"},
			{Value: "high", Label: "High", Default: true},
			{Value: "max", Label: "Max"},
		},
		ReasoningEffortDefault:     "high",
		MigrateLegacyReasoningMenu: true,
	}
	if _, err := ApplyTargets(configPath, statePath, []Target{target}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyTargets(configPath, statePath, []Target{target}); err != nil {
		stateBytes, _ := os.ReadFile(statePath)
		t.Fatalf("reapply after state-before-config crash window: %v\nstate=%s", err, stateBytes)
	}
	if _, err := Restore(configPath, statePath); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(current), "[[model.pro.reasoning_efforts]]") ||
		!strings.Contains(string(current), `reasoning_efforts = ["none", "low", "high", "max"]`) ||
		!strings.Contains(string(current), `reasoning_effort = "high"`) ||
		strings.Contains(string(current), "127.0.0.1:18787") {
		t.Fatalf("migration did not recover to compact durable config:\n%s", current)
	}
}

func TestApplyTargetsIgnoresSectionLikeTextInMultilineStrings(t *testing.T) {
	tests := []struct {
		name     string
		original string
	}{
		{
			name: "model section in multiline basic string",
			original: "[features]\nbackend_tools = false\nweb_fetch = false\n\n" +
				"[metadata]\nnotes = \"\"\"\n[model.one]\nbase_url = \"https://decoy.example/v1\"\n" +
				"api_backend = \"messages\"\nsupports_backend_search = true\n\"\"\"\n\n" +
				"[model.one]\nbase_url = \"https://real.example/v1\"\napi_backend = \"chat_completions\"\n",
		},
		{
			name: "features section in multiline literal string",
			original: "[metadata]\nnotes = '''\n[features]\nbackend_tools = false\nweb_fetch = false\n'''\n\n" +
				"[features]\nbackend_tools = false\nweb_fetch = false\n\n" +
				"[model.one]\nbase_url = \"https://real.example/v1\"\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.toml")
			statePath := filepath.Join(dir, "state.json")
			if err := os.WriteFile(configPath, []byte(test.original), 0o600); err != nil {
				t.Fatal(err)
			}

			if _, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}}); err != nil {
				t.Fatal(err)
			}
			patched, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if count := strings.Count(string(patched), `http://127.0.0.1:18787/c/one`); count != 1 {
				t.Fatalf("proxy URL count = %d, want one real model rewrite\n%s", count, patched)
			}
			if !strings.Contains(string(patched), `base_url = "https://decoy.example/v1"`) &&
				strings.Contains(test.original, "decoy.example") {
				t.Fatalf("multiline model text changed:\n%s", patched)
			}

			if _, err := Restore(configPath, statePath); err != nil {
				t.Fatal(err)
			}
			restored, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(restored) != test.original {
				t.Fatalf("restore was not byte-exact\nwant: %q\ngot:  %q", test.original, restored)
			}
		})
	}
}

func TestApplyTargetsRepairsOmittedSubagentEnabledAndRestoresExactly(t *testing.T) {
	tests := []struct {
		name         string
		original     string
		wantFragment string
		wantChanged  int
	}{
		{
			name: "models child table creates parent",
			original: "[subagents.models]\n" +
				`general-purpose = "one"` + "\n\n[model.one]\nbase_url = \"https://one.example/v1\"\n",
			wantFragment: "[subagents]\nenabled = true\n[subagents.models]",
			wantChanged:  1,
		},
		{
			name:         "existing empty parent",
			original:     "[subagents]\n\n[model.one]\nbase_url = \"https://one.example/v1\"\n",
			wantFragment: "[subagents]\nenabled = true\n",
			wantChanged:  1,
		},
		{
			name:         "parent without trailing newline",
			original:     "[model.one]\nbase_url = \"https://one.example/v1\"\n\n[subagents]",
			wantFragment: "[subagents]\nenabled = true\n",
			wantChanged:  1,
		},
		{
			name: "dotted subagent config",
			original: `subagents.models.general-purpose = "one"` +
				"\n\n[model.one]\nbase_url = \"https://one.example/v1\"\n",
			wantFragment: "subagents.models.general-purpose = \"one\"\nsubagents.enabled = true",
			wantChanged:  1,
		},
		{
			name: "explicit true is user owned",
			original: "[subagents]\nenabled = true # explicit\n\n[subagents.models]\n" +
				`general-purpose = "one"` + "\n\n[model.one]\nbase_url = \"https://one.example/v1\"\n",
			wantFragment: "enabled = true # explicit",
			wantChanged:  0,
		},
		{
			name: "explicit false is user owned",
			original: "[subagents]\nenabled = false # explicit\n\n[subagents.models]\n" +
				`general-purpose = "one"` + "\n\n[model.one]\nbase_url = \"https://one.example/v1\"\n",
			wantFragment: "enabled = false # explicit",
			wantChanged:  0,
		},
		{
			name:         "no subagent tree",
			original:     "[model.one]\nbase_url = \"https://one.example/v1\"\n",
			wantFragment: "[model.one]",
			wantChanged:  0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.toml")
			statePath := filepath.Join(dir, "state.json")
			if err := os.WriteFile(configPath, []byte(test.original), 0o600); err != nil {
				t.Fatal(err)
			}

			result, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}})
			if err != nil {
				t.Fatal(err)
			}
			if result.SubagentsEnabled != test.wantChanged {
				t.Fatalf("subagent changes = %d, want %d", result.SubagentsEnabled, test.wantChanged)
			}
			patched, _ := os.ReadFile(configPath)
			if !strings.Contains(string(patched), test.wantFragment) {
				t.Fatalf("patched config missing %q:\n%s", test.wantFragment, patched)
			}

			if _, err := Restore(configPath, statePath); err != nil {
				t.Fatal(err)
			}
			restored, _ := os.ReadFile(configPath)
			if string(restored) != test.original {
				t.Fatalf("restore was not byte-exact\nwant: %q\ngot:  %q", test.original, restored)
			}
		})
	}
}

func TestApplyTargetsRepairsRealSubagentSectionNotMultilineDecoy(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "[metadata]\nnotes = \"\"\"\n[subagents]\nenabled = false\n\"\"\"\n\n" +
		"[subagents.models]\ngeneral-purpose = \"one\"\n\n" +
		"[model.one]\nbase_url = \"https://one.example/v1\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.SubagentsEnabled != 1 {
		t.Fatalf("subagent changes = %d, want 1", result.SubagentsEnabled)
	}
	patched, _ := os.ReadFile(configPath)
	if strings.Count(string(patched), "enabled = true") != 1 ||
		!strings.Contains(string(patched), "notes = \"\"\"\n[subagents]\nenabled = false") {
		t.Fatalf("multiline decoy changed or real repair missing:\n%s", patched)
	}
	if _, err := Restore(configPath, statePath); err != nil {
		t.Fatal(err)
	}
	restored, _ := os.ReadFile(configPath)
	if string(restored) != original {
		t.Fatalf("restore was not byte-exact\nwant: %q\ngot:  %q", original, restored)
	}
}

func TestApplyTargetsRejectsInvalidSubagentEnabledWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "[subagents]\nenabled = \"yes\"\n\n[model.one]\nbase_url = \"https://one.example/v1\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}})
	if err == nil || !strings.Contains(err.Error(), "[subagents].enabled must be a boolean") {
		t.Fatalf("invalid subagent error = %v", err)
	}
	current, _ := os.ReadFile(configPath)
	if string(current) != original {
		t.Fatalf("config changed after validation failure: %q", current)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("rewrite state should not exist: %v", err)
	}
}

func TestApplyTargetsRejectsInlineSubagentTableWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "subagents = { models = { general-purpose = \"one\" } }\n\n" +
		"[model.one]\nbase_url = \"https://one.example/v1\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}})
	if err == nil || !strings.Contains(err.Error(), "unsupported inline subagents table") {
		t.Fatalf("inline subagent error = %v", err)
	}
	current, _ := os.ReadFile(configPath)
	if string(current) != original {
		t.Fatalf("config changed after inline-table failure: %q", current)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("rewrite state should not exist: %v", err)
	}
}

func TestSubagentRepairCrashRecoveryAndManagedEditMerge(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "[subagents.models]\ngeneral-purpose = \"one\"\n\n" +
		"[model.one]\nbase_url = \"https://one.example/v1\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}}); err != nil {
		t.Fatal(err)
	}
	preparedState, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(configPath, statePath); err != nil {
		t.Fatal(err)
	}

	// State committed before config replacement is an unapplied transaction.
	if err := os.WriteFile(statePath, preparedState, 0o600); err != nil {
		t.Fatal(err)
	}
	count, err := Restore(configPath, statePath)
	if err != nil || count != 0 {
		t.Fatalf("unapplied restore count=%d err=%v", count, err)
	}
	current, _ := os.ReadFile(configPath)
	if string(current) != original {
		t.Fatalf("unapplied transaction changed config: %q", current)
	}

	if _, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}}); err != nil {
		t.Fatal(err)
	}
	patched, _ := os.ReadFile(configPath)
	userEdited := strings.Replace(string(patched), "enabled = true", "enabled = false", 1)
	if err := os.WriteFile(configPath, []byte(userEdited), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(configPath, statePath); err != nil {
		t.Fatal(err)
	}
	current, _ = os.ReadFile(configPath)
	if !strings.Contains(string(current), "[subagents]\nenabled = false\n") {
		t.Fatalf("managed subagent edit was not preserved: %q", current)
	}
	if !strings.Contains(string(current), `base_url = "https://one.example/v1"`) || strings.Contains(string(current), "127.0.0.1:18787") {
		t.Fatalf("proxy route was not restored after preserving subagent edit: %q", current)
	}
}

func TestApplyTargetsMaterializesEffectiveBackendSearchAndRestores(t *testing.T) {
	tests := []struct {
		name        string
		original    string
		target      Target
		wantLine    string
		wantChanges int
	}{
		{
			name:        "missing stays absent for remote catalog",
			original:    "[model.one]\nbase_url = \"https://one.example/v1\"\n",
			target:      Target{ID: "one"},
			wantChanges: 0,
		},
		{
			name: "provider true is materialized on model",
			original: "[model_providers.gateway]\nbase_url = \"https://one.example/v1\"\n" +
				"supports_backend_search = true\n\n[model.one]\nmodel_provider = \"gateway\"\n",
			target:      Target{ID: "one", SupportsBackendSearch: true, ProjectBackendSearch: true},
			wantLine:    "supports_backend_search = true",
			wantChanges: 1,
		},
		{
			name: "model false overrides provider true",
			original: "[model_providers.gateway]\nbase_url = \"https://one.example/v1\"\n" +
				"supports_backend_search = true\n\n[model.one]\nmodel_provider = \"gateway\"\n" +
				"supports_backend_search = false # model wins\n",
			target:      Target{ID: "one", ProjectBackendSearch: true},
			wantLine:    "supports_backend_search = false # model wins",
			wantChanges: 0,
		},
		{
			name:     "existing true and comment stay intact",
			original: "[model.one]\nbase_url = \"https://one.example/v1\"\nsupports_backend_search = true # hosted\n",
			target: Target{
				ID:                    "one",
				SupportsBackendSearch: true,
				ProjectBackendSearch:  true,
			},
			wantLine:    "supports_backend_search = true # hosted",
			wantChanges: 0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.toml")
			statePath := filepath.Join(dir, "state.json")
			if err := os.WriteFile(configPath, []byte(test.original), 0o600); err != nil {
				t.Fatal(err)
			}

			result, err := ApplyTargets(configPath, statePath, []Target{test.target})
			if err != nil {
				t.Fatal(err)
			}
			if result.BackendSearch != test.wantChanges {
				t.Fatalf("backend-search changes = %d, want %d", result.BackendSearch, test.wantChanges)
			}
			patched, _ := os.ReadFile(configPath)
			model := sectionText(t, string(patched), "model.one")
			if test.wantLine != "" && !strings.Contains(model, test.wantLine) {
				t.Fatalf("model capability missing %q:\n%s", test.wantLine, model)
			}
			if test.wantLine == "" && strings.Contains(model, "supports_backend_search") {
				t.Fatalf("unconfigured capability overrode the remote catalog:\n%s", model)
			}

			if _, err := Restore(configPath, statePath); err != nil {
				t.Fatal(err)
			}
			restored, _ := os.ReadFile(configPath)
			if string(restored) != test.original {
				t.Fatalf("restore was not byte-exact\nwant: %q\ngot:  %q", test.original, restored)
			}
		})
	}
}

func TestApplyTargetsLeavesUnconfiguredAPIBackendCatalogOwned(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "[model.one]\nmodel = \"future-model\"\nbase_url = \"https://one.example/v1\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.APIBackends != 0 {
		t.Fatalf("api backend changes = %d, want 0", result.APIBackends)
	}
	patched, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(patched), "api_backend") {
		t.Fatalf("unconfigured api_backend was materialized:\n%s", patched)
	}
	if _, err := Restore(configPath, statePath); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != original {
		t.Fatalf("restore was not byte-exact\nwant: %q\ngot:  %q", original, restored)
	}
}

func TestApplyTargetsMaterializesOnlyInheritedMaxCompletionTokens(t *testing.T) {
	tests := []struct {
		name        string
		original    string
		target      Target
		wantLine    string
		wantAbsent  bool
		wantChanges int
	}{
		{
			name: "provider value is projected onto model",
			original: "[model_providers.gateway]\nbase_url = \"https://one.example/v1\"\n" +
				"max_completion_tokens = 16384\n\n[model.one]\nmodel_provider = \"gateway\"\n",
			target:      Target{ID: "one", MaxCompletionTokens: 16384},
			wantLine:    "max_completion_tokens = 16384",
			wantChanges: 1,
		},
		{
			name: "model value remains user owned",
			original: "[model_providers.gateway]\nbase_url = \"https://one.example/v1\"\n" +
				"max_completion_tokens = 16384\n\n[model.one]\nmodel_provider = \"gateway\"\n" +
				"max_completion_tokens = 8192 # model wins\n",
			target:      Target{ID: "one", MaxCompletionTokens: 8192},
			wantLine:    "max_completion_tokens = 8192 # model wins",
			wantChanges: 0,
		},
		{
			name:        "unconfigured capacity stays absent",
			original:    "[model.one]\nbase_url = \"https://one.example/v1\"\n",
			target:      Target{ID: "one"},
			wantAbsent:  true,
			wantChanges: 0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.toml")
			statePath := filepath.Join(dir, "state.json")
			if err := os.WriteFile(configPath, []byte(test.original), 0o600); err != nil {
				t.Fatal(err)
			}

			result, err := ApplyTargets(configPath, statePath, []Target{test.target})
			if err != nil {
				t.Fatal(err)
			}
			if result.MaxCompletionTokens != test.wantChanges {
				t.Fatalf("max-completion changes = %d, want %d", result.MaxCompletionTokens, test.wantChanges)
			}
			patched, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			model := sectionText(t, string(patched), "model.one")
			if test.wantAbsent {
				if strings.Contains(model, "max_completion_tokens") {
					t.Fatalf("unconfigured max completion tokens were invented:\n%s", model)
				}
			} else if !strings.Contains(model, test.wantLine) {
				t.Fatalf("model max completion tokens missing %q:\n%s", test.wantLine, model)
			}

			if _, err := Restore(configPath, statePath); err != nil {
				t.Fatal(err)
			}
			restored, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(restored) != test.original {
				t.Fatalf("restore was not byte-exact\nwant: %q\ngot:  %q", test.original, restored)
			}
		})
	}
}

func TestRestorePreservesConcurrentEditToMaterializedMaxCompletionTokens(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "[model_providers.gateway]\nbase_url = \"https://one.example/v1\"\n" +
		"max_completion_tokens = 16384\n\n[model.one]\nmodel_provider = \"gateway\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyTargets(configPath, statePath, []Target{{ID: "one", MaxCompletionTokens: 16384}}); err != nil {
		t.Fatal(err)
	}
	patched, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(patched),
		`base_url = "http://127.0.0.1:18787/c/one"`+"\nmax_completion_tokens = 16384",
		`base_url = "http://127.0.0.1:18787/c/one"`+"\nmax_completion_tokens = 4096", 1)
	if edited == string(patched) {
		t.Fatalf("materialized model value was not found:\n%s", patched)
	}
	if err := os.WriteFile(configPath, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(configPath, statePath); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(restored), "127.0.0.1:18787") ||
		!strings.Contains(sectionText(t, string(restored), "model.one"), "max_completion_tokens = 4096") {
		t.Fatalf("concurrent model edit was not preserved while proxy route restored:\n%s", restored)
	}
}

func TestApplyTargetsPlacesBackendSearchAfterChannelConfiguration(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := strings.Join([]string{
		"[model.one]",
		`model = "wire-model"`,
		`base_url = "https://one.example/v1"`,
		`api_base_url = "https://one.example/v1"`,
		`api_backend = "chat_completions"`,
		`api_key = "test-key"`,
		"# keep this trailing channel comment",
		"",
		"[model.two]",
		`base_url = "https://two.example/v1"`,
		"",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyTargets(configPath, statePath, []Target{{
		ID: "one", APIBaseURL: true, APIBackend: "chat_completions", ProjectBackendSearch: true,
	}}); err != nil {
		t.Fatal(err)
	}
	patched, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	one := string(patched)
	if end := strings.Index(one, "\n[model.two]"); end >= 0 {
		one = one[:end]
	}
	backend := strings.Index(one, `api_backend = "chat_completions"`)
	key := strings.Index(one, `api_key = "test-key"`)
	search := strings.Index(one, "supports_backend_search = false")
	comment := strings.Index(one, "# keep this trailing channel comment")
	if backend < 0 || key < 0 || search < 0 || comment < 0 || search < backend || search < key || search > comment {
		t.Fatalf("backend search was not placed below channel configuration:\n%s", one)
	}
	if _, err := Restore(configPath, statePath); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != original {
		t.Fatalf("restore changed original config\nwant: %q\ngot:  %q", original, restored)
	}
}

func TestApplyTargetsPreservesExistingOrderAndAppendsMissingFields(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	modelsSection := strings.Join([]string{
		"[models]",
		`default = "one"`,
		`web_search = "deepseek-v4-flash"`,
		`fallback = "two"`,
		"# keep models footer",
		"",
	}, "\n")
	original := modelsSection + strings.Join([]string{
		"[features]",
		"experimental = true",
		"web_fetch = false # keep position and comment",
		"# keep features footer",
		"",
		"[subagents]",
		`model = "one"`,
		"max_depth = 3",
		"# keep subagents footer",
		"",
		"[model.one]",
		`model = "wire-model"`,
		"supports_backend_search = false # keep position and comment",
		`api_key = "test-key"`,
		"# keep model footer",
		"",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := ApplyTargets(configPath, statePath, []Target{{ID: "one", APIBaseURL: true, APIBackend: "chat_completions"}}); err != nil {
		t.Fatal(err)
	}
	patchedBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	patched := string(patchedBytes)
	if !strings.HasPrefix(patched, modelsSection) {
		t.Fatalf("user-owned [models] section moved or changed:\n%s", patched)
	}

	features := sectionText(t, patched, "features")
	assertOrdered(t, features,
		"experimental = true",
		"web_fetch = true # keep position and comment",
		"backend_tools = true",
		"# keep features footer",
	)
	subagents := sectionText(t, patched, "subagents")
	assertOrdered(t, subagents,
		`model = "one"`,
		"max_depth = 3",
		"enabled = true",
		"# keep subagents footer",
	)
	model := sectionText(t, patched, "model.one")
	assertOrdered(t, model,
		`model = "wire-model"`,
		"supports_backend_search = false # keep position and comment",
		`api_key = "test-key"`,
		`base_url = "http://127.0.0.1:18787/c/one"`,
		`api_base_url = "http://127.0.0.1:18787/c/one"`,
		"# keep model footer",
	)

	if _, err := Restore(configPath, statePath); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != original {
		t.Fatalf("restore was not byte-exact\nwant: %q\ngot:  %q", original, restored)
	}
}

func TestApplyTargetsAppendsEitherMissingFeatureFlagAtFooter(t *testing.T) {
	tests := []struct {
		name     string
		existing string
		added    string
	}{
		{name: "backend tools missing", existing: "web_fetch = true", added: "backend_tools = true"},
		{name: "web fetch missing", existing: "backend_tools = true", added: "web_fetch = true"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.toml")
			statePath := filepath.Join(dir, "state.json")
			original := "[features]\nother = true\n" + test.existing + "\n# footer\n\n" +
				"[model.one]\nbase_url = \"https://one.example/v1\"\n"
			if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}}); err != nil {
				t.Fatal(err)
			}
			patched, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			features := sectionText(t, string(patched), "features")
			assertOrdered(t, features, "other = true", test.existing, test.added, "# footer")
			if _, err := Restore(configPath, statePath); err != nil {
				t.Fatal(err)
			}
			restored, _ := os.ReadFile(configPath)
			if string(restored) != original {
				t.Fatalf("restore was not byte-exact\nwant: %q\ngot:  %q", original, restored)
			}
		})
	}
}

func TestApplyTargetsAppendsBelowFinalLineWithoutEndingAndRestoresExactly(t *testing.T) {
	tests := []struct {
		name         string
		original     string
		wantSequence string
	}{
		{
			name: "model field with LF",
			original: "[features]\nbackend_tools = true\nweb_fetch = true\n\n" +
				"[model.one]\nmodel = \"wire-model\"",
			wantSequence: "model = \"wire-model\"\nbase_url = \"http://127.0.0.1:18787/c/one\"",
		},
		{
			name: "model field with CRLF",
			original: "[features]\r\nbackend_tools = true\r\nweb_fetch = true\r\n\r\n" +
				"[model.one]\r\nmodel = \"wire-model\"",
			wantSequence: "model = \"wire-model\"\r\nbase_url = \"http://127.0.0.1:18787/c/one\"",
		},
		{
			name: "features field",
			original: "[model.one]\nbase_url = \"http://127.0.0.1:18787/c/one\"\n" +
				"api_backend = \"chat_completions\"\nsupports_backend_search = false\n\n[features]\nother = true",
			wantSequence: "other = true\nbackend_tools = true\nweb_fetch = true",
		},
		{
			name: "subagent field",
			original: "[features]\nbackend_tools = true\nweb_fetch = true\n\n" +
				"[model.one]\nbase_url = \"http://127.0.0.1:18787/c/one\"\n" +
				"api_backend = \"chat_completions\"\nsupports_backend_search = false\n\n" +
				"[subagents]\nmodel = \"one\"",
			wantSequence: "model = \"one\"\nenabled = true",
		},
		{
			name: "multiline model field",
			original: "[features]\nbackend_tools = true\nweb_fetch = true\n\n" +
				"[model.one]\ninstructions = \"\"\"\nkeep this text\n\"\"\"",
			wantSequence: "\"\"\"\nbase_url = \"http://127.0.0.1:18787/c/one\"",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.toml")
			statePath := filepath.Join(dir, "state.json")
			if err := os.WriteFile(configPath, []byte(test.original), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}}); err != nil {
				t.Fatal(err)
			}
			patched, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(patched), test.wantSequence) {
				t.Fatalf("new fields were not appended below the final user field:\n%s", patched)
			}
			if _, err := Restore(configPath, statePath); err != nil {
				t.Fatal(err)
			}
			restored, _ := os.ReadFile(configPath)
			if string(restored) != test.original {
				t.Fatalf("restore was not byte-exact\nwant: %q\ngot:  %q", test.original, restored)
			}
		})
	}
}

func TestNoEndingRecoveryStateDoesNotCopyUserFieldValue(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	const secret = "must-not-appear-in-recovery-state"
	original := "[features]\nbackend_tools = true\nweb_fetch = true\n\n" +
		"[model.one]\napi_key = \"" + secret + "\""
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}}); err != nil {
		t.Fatal(err)
	}
	state, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(state), secret) {
		t.Fatalf("recovery state copied a user-owned field value: %s", state)
	}
	if _, err := Restore(configPath, statePath); err != nil {
		t.Fatal(err)
	}
	restored, _ := os.ReadFile(configPath)
	if string(restored) != original {
		t.Fatalf("restore was not byte-exact\nwant: %q\ngot:  %q", original, restored)
	}
}

func TestApplyTargetsCreatesAndRemovesFeaturesSectionExactly(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "[model.one]\nbase_url = \"https://one.example/v1\""
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.BackendTools != 1 || result.WebFetch != 1 {
		t.Fatalf("feature result: %#v", result)
	}
	patched, _ := os.ReadFile(configPath)
	if !strings.Contains(string(patched), "[features]\nbackend_tools = true\nweb_fetch = true\n") {
		t.Fatalf("features section missing:\n%s", patched)
	}
	if _, err := Restore(configPath, statePath); err != nil {
		t.Fatal(err)
	}
	final, _ := os.ReadFile(configPath)
	if string(final) != original {
		t.Fatalf("restore was not byte-exact\nwant: %q\ngot:  %q", original, final)
	}
}

func TestApplyTargetsLeavesNonTargetsUntouched(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "[model.one]\nbase_url = \"https://one.example/v1\"\n\n[model.two]\nbase_url = \"https://two.example/v1\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}}); err != nil {
		t.Fatal(err)
	}
	patched, _ := os.ReadFile(configPath)
	two := sectionText(t, string(patched), "model.two")
	if two != "[model.two]\nbase_url = \"https://two.example/v1\"\n" {
		t.Fatalf("non-target changed: %q", two)
	}
}

func TestApplyTargetsFailsWhenModelSectionIsMissing(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "[model.present]\nbase_url = \"https://example.test\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyTargets(configPath, statePath, []Target{{ID: "missing"}}); err == nil {
		t.Fatal("missing model section must fail")
	}
	current, _ := os.ReadFile(configPath)
	if string(current) != original {
		t.Fatal("config changed on failed apply")
	}
}

func TestApplyTargetsNormalizesWrongTypedManagedValuesWithoutChangingBackend(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := `[features]
backend_tools = "enabled" # invalid type
web_fetch = 1

[model.one]
base_url = "https://one.example/v1"
api_base_url = 7 # invalid type
api_backend = "messages"
supports_backend_search = false
`
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := ApplyTargets(configPath, statePath, []Target{{
		ID: "one", APIBaseURL: true, APIBackend: "messages", ProjectBackendSearch: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result.ValidatedTargets != 1 {
		t.Fatalf("validated targets = %d", result.ValidatedTargets)
	}
	patched, _ := os.ReadFile(configPath)
	for _, want := range []string{
		"backend_tools = true",
		"web_fetch = true",
		`base_url = "http://127.0.0.1:18787/c/one"`,
		`api_base_url = "http://127.0.0.1:18787/c/one"`,
		`api_backend = "messages"`,
		"supports_backend_search = false",
	} {
		if !strings.Contains(string(patched), want) {
			t.Fatalf("normalized config missing %q:\n%s", want, patched)
		}
	}

	if _, err := Restore(configPath, statePath); err != nil {
		t.Fatal(err)
	}
	restored, _ := os.ReadFile(configPath)
	if string(restored) != original {
		t.Fatalf("restore was not byte-exact\nwant:\n%q\ngot:\n%q", original, restored)
	}
}

func TestApplyTargetsRejectsWrongTypedBackendSearchWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "[model.one]\nbase_url = \"https://one.example/v1\"\nsupports_backend_search = \"yes\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := ApplyTargets(configPath, statePath, []Target{{ID: "one", ProjectBackendSearch: true}})
	if err == nil || !strings.Contains(err.Error(), "supports_backend_search must be a boolean") {
		t.Fatalf("wrong capability error = %v", err)
	}
	current, _ := os.ReadFile(configPath)
	if string(current) != original {
		t.Fatalf("config changed after capability validation failure: %q", current)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("rewrite state should not exist: %v", err)
	}
}

func TestRestoreRejectsIncompatibleRewriteStateFormat(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	patched := "[model.one]\nsupports_backend_search = true\n"
	incompatibleState := `{"format":"obsolete-rewrite-state","models":{"one":{"backend_search":{"managed":true,"present":true,"original_line":"supports_backend_search = false\n"}}}}`
	if err := os.WriteFile(configPath, []byte(patched), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, []byte(incompatibleState), 0o600); err != nil {
		t.Fatal(err)
	}

	restored, err := Restore(configPath, statePath)
	if err == nil || !strings.Contains(err.Error(), `unsupported rewrite state format "obsolete-rewrite-state"`) {
		t.Fatalf("incompatible state error = %v", err)
	}
	if restored != 0 {
		t.Fatalf("restored fields = %d, want 0", restored)
	}
	current, _ := os.ReadFile(configPath)
	if string(current) != patched {
		t.Fatalf("config changed after rejecting incompatible state: %q", current)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("incompatible state should remain for manual recovery: %v", err)
	}
}

func TestApplyTargetsProjectsOnlyCapableNonResponsesBackends(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := strings.Join([]string{
		"[model.messages]",
		`base_url = "https://messages.example/v1"`,
		`api_backend = "messages"`,
		"supports_backend_search = true",
		"",
		"[model.chat]",
		`base_url = "https://chat.example/v1"`,
		`api_backend = "chat_completions"`,
		"supports_backend_search = false",
		"",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := ApplyTargets(configPath, statePath, []Target{
		{ID: "messages", APIBackend: "messages", BuildAPIBackend: "responses", SupportsBackendSearch: true},
		{ID: "chat", APIBackend: "chat_completions", BuildAPIBackend: "chat_completions"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.APIBackends != 1 {
		t.Fatalf("api backend changes=%d, want 1", result.APIBackends)
	}
	patched, _ := os.ReadFile(configPath)
	messages := sectionText(t, string(patched), "model.messages")
	chat := sectionText(t, string(patched), "model.chat")
	if !strings.Contains(messages, `api_backend = "responses"`) ||
		!strings.Contains(messages, "supports_backend_search = true") {
		t.Fatalf("capable Messages projection failed: %s", messages)
	}
	if !strings.Contains(chat, `api_backend = "chat_completions"`) ||
		!strings.Contains(chat, "supports_backend_search = false") {
		t.Fatalf("non-capable Chat backend changed: %s", chat)
	}
	if _, err := Restore(configPath, statePath); err != nil {
		t.Fatal(err)
	}
	restored, _ := os.ReadFile(configPath)
	if string(restored) != original {
		t.Fatalf("projection restore was not exact\nwant: %q\ngot:  %q", original, restored)
	}
}

func TestApplyTargetsRejectsStateForDifferentConfig(t *testing.T) {
	dir := t.TempDir()
	firstPath := filepath.Join(dir, "first.toml")
	secondPath := filepath.Join(dir, "second.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "[model.one]\nbase_url = \"https://one.example/v1\"\n"
	for _, path := range []string{firstPath, secondPath} {
		if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ApplyTargets(firstPath, statePath, []Target{{ID: "one"}}); err != nil {
		t.Fatal(err)
	}

	_, err := ApplyTargets(secondPath, statePath, []Target{{ID: "one"}})
	if err == nil || !strings.Contains(err.Error(), "rewrite state belongs to") {
		t.Fatalf("wrong-config state error = %v", err)
	}
	second, _ := os.ReadFile(secondPath)
	if string(second) != original {
		t.Fatalf("unrelated config changed: %q", second)
	}
	if _, err := Restore(secondPath, statePath); err == nil || !strings.Contains(err.Error(), "rewrite state belongs to") {
		t.Fatalf("wrong-config restore error = %v", err)
	}
	if _, err := Restore(firstPath, statePath); err != nil {
		t.Fatal(err)
	}
}

func TestApplyAndRestorePreserveConfigSymlink(t *testing.T) {
	dir := t.TempDir()
	realPath := filepath.Join(dir, "real-config.toml")
	linkPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "[model.one]\nbase_url = \"https://one.example/v1\"\n"
	if err := os.WriteFile(realPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := ApplyTargets(linkPath, statePath, []Target{{ID: "one"}}); err != nil {
		t.Fatal(err)
	}
	assertSymlink := func(stage string) {
		t.Helper()
		info, err := os.Lstat(linkPath)
		if err != nil {
			t.Fatalf("%s: lstat link: %v", stage, err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s: config path is no longer a symlink", stage)
		}
	}
	assertSymlink("after apply")
	patched, _ := os.ReadFile(realPath)
	if !strings.Contains(string(patched), `base_url = "http://127.0.0.1:18787/c/one"`) {
		t.Fatalf("real config was not patched: %q", patched)
	}

	if _, err := Restore(linkPath, statePath); err != nil {
		t.Fatal(err)
	}
	assertSymlink("after restore")
	restored, _ := os.ReadFile(realPath)
	if string(restored) != original {
		t.Fatalf("restore through symlink was not byte-exact: %q", restored)
	}
}

func TestApplyTargetsRejectsInvalidPreparedTOMLWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "[model.one]\nbase_url = \"https://one.example/v1\"\nbase_url = \"https://duplicate.example/v1\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}}); err == nil || !strings.Contains(err.Error(), "parse TOML") {
		t.Fatalf("invalid prepared TOML error = %v", err)
	}
	current, _ := os.ReadFile(configPath)
	if string(current) != original {
		t.Fatal("config changed after validation failure")
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("rewrite state should not exist: %v", err)
	}
}

func TestApplyTargetsIsIdempotentAndStillRestoresOriginal(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "[model.one]\nbase_url = \"https://one.example/v1\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}}); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(configPath)
	secondResult, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}})
	if err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(configPath)
	if string(second) != string(first) || secondResult.ValidatedTargets != 1 {
		t.Fatalf("second apply changed config or skipped validation: %#v", secondResult)
	}
	if _, err := Restore(configPath, statePath); err != nil {
		t.Fatal(err)
	}
	restored, _ := os.ReadFile(configPath)
	if string(restored) != original {
		t.Fatalf("restore after repeated apply = %q, want %q", restored, original)
	}
}

func TestRestorePreservesManagedUserEdit(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "[model.one]\nbase_url = \"https://one.example/v1\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}}); err != nil {
		t.Fatal(err)
	}
	patched, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	userEdited := strings.Replace(string(patched),
		`base_url = "http://127.0.0.1:18787/c/one"`,
		`base_url = "https://new-user-value.example/v1"`, 1)
	if err := os.WriteFile(configPath, []byte(userEdited), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}}); err == nil || !strings.Contains(err.Error(), "conflicts with config") {
		t.Fatalf("reapply managed edit error = %v", err)
	}
	if _, err := Restore(configPath, statePath); err != nil {
		t.Fatal(err)
	}
	current, _ := os.ReadFile(configPath)
	want := "[model.one]\nbase_url = \"https://new-user-value.example/v1\"\n"
	if string(current) != want {
		t.Fatalf("managed user edit was not preserved while other fields were restored\nwant: %q\ngot:  %q", want, current)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("state remains after merged restore: %v", err)
	}
}

func TestRestorePreservesBackendSearchEditWhileRestoringProxyRoute(t *testing.T) {
	tests := []struct {
		name     string
		original string
		want     string
	}{
		{
			name:     "new user setting",
			original: "[model.one]\nbase_url = \"https://one.example/v1\"\n",
			want:     "[model.one]\nbase_url = \"https://one.example/v1\"\nsupports_backend_search = true\n",
		},
		{
			name:     "restored original setting",
			original: "[model.one]\nbase_url = \"https://one.example/v1\"\nsupports_backend_search = true\n",
			want:     "[model.one]\nbase_url = \"https://one.example/v1\"\nsupports_backend_search = true\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.toml")
			statePath := filepath.Join(dir, "state.json")
			if err := os.WriteFile(configPath, []byte(test.original), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := ApplyTargets(configPath, statePath, []Target{{ID: "one", ProjectBackendSearch: true}}); err != nil {
				t.Fatal(err)
			}
			patched, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			userEdited := strings.Replace(string(patched), "supports_backend_search = false", "supports_backend_search = true", 1)
			if userEdited == string(patched) {
				t.Fatalf("backend search projection was not applied: %q", patched)
			}
			if err := os.WriteFile(configPath, []byte(userEdited), 0o600); err != nil {
				t.Fatal(err)
			}

			if _, err := Restore(configPath, statePath); err != nil {
				t.Fatal(err)
			}
			current, _ := os.ReadFile(configPath)
			if string(current) != test.want {
				t.Fatalf("merged restore mismatch\nwant: %q\ngot:  %q", test.want, current)
			}
			refs, err := ActiveProxyReferences(configPath)
			if err != nil || len(refs) != 0 {
				t.Fatalf("proxy references after merged restore: refs=%v err=%v", refs, err)
			}
		})
	}
}

func TestRestorePreservesModelDeletedWhileProxyIsActive(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "[features]\nbackend_tools = false\nweb_fetch = false\n\n" +
		"[model.one]\nbase_url = \"https://one.example/v1\"\nsupports_backend_search = true\n\n" +
		"[model.two]\nbase_url = \"https://two.example/v1\"\nsupports_backend_search = true\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	targets := []Target{{ID: "one"}, {ID: "two"}}
	if _, err := ApplyTargets(configPath, statePath, targets); err != nil {
		t.Fatal(err)
	}
	patched, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(patched), "[model.one]\n")
	if start < 0 {
		t.Fatalf("patched model blocks not found: %q", patched)
	}
	endOffset := strings.Index(string(patched)[start:], "[model.two]\n")
	if endOffset < 0 {
		t.Fatalf("patched model blocks not found: %q", patched)
	}
	withoutOne := string(patched[:start]) + string(patched[start+endOffset:])
	if err := os.WriteFile(configPath, []byte(withoutOne), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Restore(configPath, statePath); err != nil {
		t.Fatal(err)
	}
	restored, _ := os.ReadFile(configPath)
	expectedStart := strings.Index(original, "[model.one]\n")
	expectedEndOffset := strings.Index(original[expectedStart:], "[model.two]\n")
	expected := original[:expectedStart] + original[expectedStart+expectedEndOffset:]
	if string(restored) != expected {
		t.Fatalf("deleted model was reintroduced or remaining config was not restored\nwant: %q\ngot:  %q", expected, restored)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("rewrite state remains after safe partial restore: %v", err)
	}
}

func TestRestorePreservesUnrelatedConcurrentEdit(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "[model.one]\nbase_url = \"https://one.example/v1\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}}); err != nil {
		t.Fatal(err)
	}
	patched, _ := os.ReadFile(configPath)
	if err := os.WriteFile(configPath, append(patched, []byte("\n[user_edit]\nvalue = 1\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(configPath, statePath); err != nil {
		t.Fatal(err)
	}
	current, _ := os.ReadFile(configPath)
	want := original + "\n[user_edit]\nvalue = 1\n"
	if string(current) != want {
		t.Fatalf("unrelated edit was not preserved\nwant: %q\ngot:  %q", want, current)
	}
}

func TestRestoreRemovesProxyRoutesWhenUnrelatedTOMLIsInvalid(t *testing.T) {
	tests := []struct {
		name       string
		corrupt    func(string) string
		wantPrefix string
		wantSuffix string
	}{
		{
			name: "unfinished user setting",
			corrupt: func(patched string) string {
				return patched + "\n[user]\nunfinished = \"\n"
			},
			wantSuffix: "\n[user]\nunfinished = \"\n",
		},
		{
			name: "mojibake byte order mark",
			corrupt: func(patched string) string {
				return "\u00ef\u00bb\u00bf" + patched
			},
			wantPrefix: "\u00ef\u00bb\u00bf",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.toml")
			statePath := filepath.Join(dir, "state.json")
			original := "[model.one]\nbase_url = \"https://one.example/v1\"\n"
			if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}}); err != nil {
				t.Fatal(err)
			}
			patched, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			corrupt := test.corrupt(string(patched))
			if err := os.WriteFile(configPath, []byte(corrupt), 0o600); err != nil {
				t.Fatal(err)
			}

			if _, err := Restore(configPath, statePath); err != nil {
				t.Fatal(err)
			}
			current, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			want := test.wantPrefix + original + test.wantSuffix
			if string(current) != want {
				t.Fatalf("invalid user edit was not preserved while restoring the route\nwant: %q\ngot:  %q", want, current)
			}
			if strings.Contains(string(current), "http://127.0.0.1:18787/c/") {
				t.Fatalf("temporary proxy route remains: %q", current)
			}
			if _, err := os.Stat(statePath); !os.IsNotExist(err) {
				t.Fatalf("rewrite state remains after recovery: %v", err)
			}
		})
	}
}

func TestRestoreInvalidTOMLKeepsStateWhenProxyReferenceCannotBeOwned(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "[model.one]\nbase_url = \"https://one.example/v1\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}}); err != nil {
		t.Fatal(err)
	}
	patched, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	conflicted := strings.Replace(string(patched), "[model.one]", "[model.renamed]", 1) + "invalid = \"\n"
	if err := os.WriteFile(configPath, []byte(conflicted), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Restore(configPath, statePath); err == nil || !strings.Contains(err.Error(), "temporary hellogrok routes") {
		t.Fatalf("unowned proxy reference error = %v", err)
	}
	current, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != conflicted {
		t.Fatalf("conflicted invalid config changed: %q", current)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("recovery state was lost after an unsafe restore: %v", err)
	}
}

func TestRestoreInvalidTOMLPreservesUserOwnedManagedValue(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "[model.one]\nbase_url = \"https://one.example/v1\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}}); err != nil {
		t.Fatal(err)
	}
	patched, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	userURL := "https://user-owned.example/v1"
	edited := strings.Replace(string(patched), "http://127.0.0.1:18787/c/one", userURL, 1) + "invalid = \"\n"
	if err := os.WriteFile(configPath, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Restore(configPath, statePath); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	want := "[model.one]\nbase_url = \"" + userURL + "\"\n[features]\ninvalid = \"\n"
	if string(current) != want {
		t.Fatalf("invalid-TOML recovery did not preserve the user-owned managed value\nwant: %q\ngot:  %q", want, current)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("rewrite state remains after preserving the user-owned value: %v", err)
	}
}

func TestRestoreInvalidTOMLDoesNotRewriteDuplicateManagedValue(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "[model.one]\nbase_url = \"https://one.example/v1\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}}); err != nil {
		t.Fatal(err)
	}
	patched, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	proxyLine := "base_url = \"http://127.0.0.1:18787/c/one\""
	duplicate := proxyLine + "\nbase_url = \"https://user-owned.example/v1\""
	edited := strings.Replace(string(patched), proxyLine, duplicate, 1) + "invalid = \"\n"
	if err := os.WriteFile(configPath, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Restore(configPath, statePath); err == nil || !strings.Contains(err.Error(), "temporary hellogrok routes") {
		t.Fatalf("ambiguous duplicate managed value error = %v", err)
	}
	current, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != edited {
		t.Fatalf("ambiguous invalid config changed: %q", current)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("recovery state was lost after an ambiguous restore: %v", err)
	}
}

func TestRestorePreservesConcurrentEditAfterOriginalFinalLine(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "[features]\nbackend_tools = true\nweb_fetch = true\n\n" +
		"[model.one]\nmodel = \"wire-model\""
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}}); err != nil {
		t.Fatal(err)
	}
	patched, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	userEdit := "[user_edit]\nvalue = 1\n"
	if err := os.WriteFile(configPath, append(patched, []byte(userEdit)...), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Restore(configPath, statePath); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	want := original + "\n" + userEdit
	if string(current) != want {
		t.Fatalf("concurrent edit was not preserved\nwant: %q\ngot:  %q", want, current)
	}
}

func TestApplyTargetsRestoresSectionsWithoutTrailingNewline(t *testing.T) {
	tests := []struct {
		name     string
		original string
	}{
		{
			name:     "empty features table is last",
			original: "[model.one]\nbase_url = \"https://one.example/v1\"\n\n[features]",
		},
		{
			name: "inherited model table is last",
			original: "[features]\nbackend_tools = false\nweb_fetch = false\n\n" +
				"[model_providers.gateway]\nbase_url = \"https://one.example/v1\"\n\n[model.one]",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.toml")
			statePath := filepath.Join(dir, "state.json")
			if err := os.WriteFile(configPath, []byte(test.original), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}}); err != nil {
				t.Fatal(err)
			}
			if _, err := Restore(configPath, statePath); err != nil {
				t.Fatal(err)
			}
			restored, _ := os.ReadFile(configPath)
			if string(restored) != test.original {
				t.Fatalf("restore was not byte-exact\nwant: %q\ngot:  %q", test.original, restored)
			}
		})
	}
}

func TestRestoreWithoutStateIsNoop(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(configPath, []byte("[models]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	count, err := Restore(configPath, filepath.Join(dir, "missing.json"))
	if err != nil || count != 0 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}

func TestRestoreDiscardsStateCommittedBeforeConfigRewrite(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "[features]\nbackend_tools = false\n\n[model.one]\nbase_url = \"https://one.example/v1\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	targets := []Target{{ID: "one"}}
	if _, err := ApplyTargets(configPath, statePath, targets); err != nil {
		t.Fatal(err)
	}
	preparedState, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(configPath, statePath); err != nil {
		t.Fatal(err)
	}
	withUserEdit := original + "\n[user_edit]\nvalue = 1\n"
	if err := os.WriteFile(configPath, []byte(withUserEdit), 0o600); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after the recovery record was committed but before the
	// atomic config rename. Unrelated edits must remain untouched.
	if err := os.WriteFile(statePath, preparedState, 0o600); err != nil {
		t.Fatal(err)
	}
	count, err := Restore(configPath, statePath)
	if err != nil || count != 0 {
		t.Fatalf("restore count=%d err=%v", count, err)
	}
	current, _ := os.ReadFile(configPath)
	if string(current) != withUserEdit {
		t.Fatalf("unapplied transaction changed config: %q", current)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("unapplied state remains: %v", err)
	}
}

func TestApplyTargetsRecoversStateCommittedBeforeConfigRewrite(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "[model.one]\nbase_url = \"https://one.example/v1\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	targets := []Target{{ID: "one"}}
	if _, err := ApplyTargets(configPath, statePath, targets); err != nil {
		t.Fatal(err)
	}
	preparedState, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(configPath, statePath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, preparedState, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := ApplyTargets(configPath, statePath, targets)
	if err != nil || result.ValidatedTargets != 1 {
		t.Fatalf("reapply result=%+v err=%v", result, err)
	}
	patched, _ := os.ReadFile(configPath)
	if !strings.Contains(string(patched), `base_url = "http://127.0.0.1:18787/c/one"`) {
		t.Fatalf("config was not rewritten after recovery: %s", patched)
	}
	if _, err := Restore(configPath, statePath); err != nil {
		t.Fatal(err)
	}
	restored, _ := os.ReadFile(configPath)
	if string(restored) != original {
		t.Fatalf("recovered transaction did not restore exactly: %q", restored)
	}
}

func sectionText(t *testing.T, text, name string) string {
	t.Helper()
	marker := "[" + name + "]"
	start := strings.Index(text, marker)
	if start < 0 {
		t.Fatalf("section %s missing", name)
	}
	rest := text[start+len(marker):]
	end := len(rest)
	if next := strings.Index(rest, "\n["); next >= 0 {
		end = next + 1
	}
	return text[start : start+len(marker)+end]
}

func assertOrdered(t *testing.T, text string, values ...string) {
	t.Helper()
	position := -1
	for _, value := range values {
		next := strings.Index(text[position+1:], value)
		if next < 0 {
			t.Fatalf("missing %q while checking order:\n%s", value, text)
		}
		next += position + 1
		if next <= position {
			t.Fatalf("%q is out of order:\n%s", value, text)
		}
		position = next
	}
}
