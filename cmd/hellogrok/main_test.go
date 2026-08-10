package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hellowind777/hellogrok/internal/appinfo"
	"github.com/hellowind777/hellogrok/internal/cfgpatch"
	"github.com/hellowind777/hellogrok/internal/config"
	"github.com/hellowind777/hellogrok/internal/groksync"
	"github.com/hellowind777/hellogrok/internal/proxy"
)

func TestUsageIncludesApplicationVersion(t *testing.T) {
	var output bytes.Buffer
	printUsage(&output)
	if !strings.Contains(output.String(), "hellogrok "+appinfo.Version) ||
		!strings.Contains(output.String(), "version               print the application version") {
		t.Fatalf("usage does not expose the application version:\n%s", output.String())
	}
}

func TestRouteAuthStatusIgnoresNonCredentialHeaders(t *testing.T) {
	tests := []struct {
		name  string
		route config.Route
		want  string
	}{
		{name: "ordinary header", route: config.Route{ExtraHeaders: map[string]string{"X-Tenant": "one"}}, want: "missing"},
		{name: "api key", route: config.Route{APIKey: "secret"}, want: "channel-owned"},
		{name: "authorization header", route: config.Route{ExtraHeaders: map[string]string{"authorization": "Bearer secret"}}, want: "channel-owned"},
		{name: "x api key header", route: config.Route{ExtraHeaders: map[string]string{"X-API-KEY": "secret"}}, want: "channel-owned"},
		{name: "empty auth header", route: config.Route{ExtraHeaders: map[string]string{"Authorization": "  "}}, want: "missing"},
		{name: "dynamic provider", route: config.Route{DynamicAuth: true}, want: "auth-provider"},
		{name: "static wins", route: config.Route{APIKey: "secret", DynamicAuth: true}, want: "channel-owned"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := routeAuthStatus(test.route); got != test.want {
				t.Fatalf("routeAuthStatus() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestSecondInstanceDoesNotRestoreActiveInstanceConfig(t *testing.T) {
	dir := t.TempDir()
	grokHome := filepath.Join(dir, "grok")
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(grokHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_HOME", grokHome)
	configPath := filepath.Join(grokHome, "config.toml")
	statePath := filepath.Join(dataDir, "config_rewrite_state.json")
	original := "[model.one]\nbase_url = \"https://one.example/v1\"\napi_key = \"test-key\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := cfgpatch.ApplyTargets(configPath, statePath, []cfgpatch.Target{{ID: "one"}}); err != nil {
		t.Fatal(err)
	}
	patchedBefore, _ := os.ReadFile(configPath)
	stateBefore, _ := os.ReadFile(statePath)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	logger := log.New(io.Discard, "", 0)
	server := proxy.New(logger)
	server.PathAddr = listener.Addr().String()
	app := &App{logger: logger, dataDir: dataDir, server: server}
	if err := app.Start(); err == nil {
		t.Fatal("second instance unexpectedly started on an occupied address")
	} else if !strings.Contains(err.Error(), "已被占用") {
		t.Fatalf("occupied address error = %v", err)
	}
	if err := app.Stop(); err != nil {
		t.Fatalf("stopping non-owning second instance: %v", err)
	}

	patchedAfter, _ := os.ReadFile(configPath)
	stateAfter, _ := os.ReadFile(statePath)
	if !bytes.Equal(patchedAfter, patchedBefore) {
		t.Fatalf("second instance restored active config\nbefore: %q\nafter:  %q", patchedBefore, patchedAfter)
	}
	if !bytes.Equal(stateAfter, stateBefore) {
		t.Fatal("second instance start/stop changed active rewrite state")
	}
}

func TestAppRemembersProxyEnabledStateAcrossInstances(t *testing.T) {
	dataDir := t.TempDir()
	first := &App{dataDir: dataDir}
	if enabled, err := first.ProxyEnabledOnLaunch(); err != nil || !enabled {
		t.Fatalf("initial enabled=%v err=%v", enabled, err)
	}
	if err := first.SetProxyEnabledOnLaunch(false); err != nil {
		t.Fatal(err)
	}
	second := &App{dataDir: dataDir}
	if enabled, err := second.ProxyEnabledOnLaunch(); err != nil || enabled {
		t.Fatalf("remembered disabled=%v err=%v", enabled, err)
	}
	if err := second.SetProxyEnabledOnLaunch(true); err != nil {
		t.Fatal(err)
	}
	if enabled, err := first.ProxyEnabledOnLaunch(); err != nil || !enabled {
		t.Fatalf("remembered enabled=%v err=%v", enabled, err)
	}
}

func TestStatusDetailPutsEachSectionFirstItemOnHeadingLine(t *testing.T) {
	server := proxy.New(log.New(io.Discard, "", 0))
	server.PathAddr = "127.0.0.1:18787"
	app := &App{
		running:        true,
		server:         server,
		patchedIDs:     []string{"model-b", "model-a"},
		grokSyncStatus: "已刷新 1/1 个空闲自定义模型会话",
	}
	detail := app.StatusDetail()
	for _, want := range []string{
		"【代理】 本地入口：http://127.0.0.1:18787/c/<渠道>/responses",
		"【配置恢复】 临时改写：2 个渠道，停止代理：恢复原值",
		"【渠道】 配置校验：已通过；数量：2 个\n列表：model-a, model-b",
		"【Grok 会话】 热切换：已刷新 1/1 个空闲自定义模型会话",
		"【协议与搜索】 Grok 消费：搜索开启时投影为 Responses\n上游协议：保持渠道真实格式\n搜索分流：开启走当前渠道，关闭走客户端搜索",
	} {
		if !strings.Contains(detail, want) {
			t.Fatalf("status detail is missing %q:\n%s", want, detail)
		}
	}
	for _, category := range []string{"【代理】", "【渠道】", "【Grok 会话】", "【协议与搜索】", "【配置恢复】"} {
		if strings.Contains(detail, category+"\n") {
			t.Fatalf("category %s is separated from its first item:\n%s", category, detail)
		}
	}
}

func TestStoppedStatusDetailPutsFirstItemOnHeadingLine(t *testing.T) {
	app := &App{lastError: "test warning"}
	detail := app.StatusDetail()
	want := "【代理】 状态：已停止\n配置：未改写\n本地端口：未监听\n\n【上次错误】 test warning"
	if detail != want {
		t.Fatalf("stopped status detail = %q, want %q", detail, want)
	}
}

func TestAppHotRefreshesDottedCustomModelOnEnableAndDisable(t *testing.T) {
	dir := t.TempDir()
	grokHome := filepath.Join(dir, "grok")
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(grokHome, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_HOME", grokHome)
	configPath := filepath.Join(grokHome, "config.toml")
	original := "[model.provider.v1-beta]\n" +
		"name = \"Provider.v1-beta\"\n" +
		"base_url = \"https://provider.example/v1\"\n" +
		"api_key = \"test-key\"\n" +
		"supports_backend_search = false\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	var refreshTargets []map[string]string
	refresh := func(_ context.Context, selections map[string]string) (groksync.Result, error) {
		captured := make(map[string]string, len(selections))
		for currentID, desiredID := range selections {
			captured[currentID] = desiredID
		}
		refreshTargets = append(refreshTargets, captured)
		return groksync.Result{
			GrokFound:         true,
			ReachableLeaders:  1,
			TargetSessions:    1,
			RefreshedSessions: 1,
		}, nil
	}
	logger := log.New(io.Discard, "", 0)
	server := proxy.New(logger)
	server.PathAddr = "127.0.0.1:0"
	app := &App{
		logger:              logger,
		dataDir:             dataDir,
		server:              server,
		refreshGrokSessions: refresh,
	}
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	patched, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(patched), `[model."provider.v1-beta"]`) {
		t.Fatalf("dotted model header was not normalized:\n%s", patched)
	}
	if detail := app.StatusDetail(); !strings.Contains(detail, "已刷新 1/1 个空闲自定义模型会话") {
		t.Fatalf("hot-reload status missing:\n%s", detail)
	}
	if err := app.Stop(); err != nil {
		t.Fatal(err)
	}
	if len(refreshTargets) != 2 {
		t.Fatalf("refresh calls = %d, want enable and disable", len(refreshTargets))
	}
	if got := refreshTargets[0]; got["provider.v1-beta"] != "provider.v1-beta" || got["provider"] != "provider.v1-beta" {
		t.Fatalf("enable refresh targets = %v", got)
	}
	if got := refreshTargets[1]; len(got) != 2 || got["provider.v1-beta"] != "provider" || got["provider"] != "provider" {
		t.Fatalf("disable refresh targets = %v", got)
	}
	restored, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != original {
		t.Fatalf("config was not restored exactly\nwant: %q\ngot:  %q", original, restored)
	}
}

func TestGrokSessionSelectionsKeepModelsWithoutLegacyAliases(t *testing.T) {
	targets := []string{"provider.v1-beta", "ordinary"}
	aliases := map[string]string{"provider": "provider.v1-beta"}
	enable := enableGrokSessionSelections(targets, aliases)
	if enable["provider.v1-beta"] != "provider.v1-beta" || enable["provider"] != "provider.v1-beta" || enable["ordinary"] != "ordinary" {
		t.Fatalf("enable selections = %v", enable)
	}
	disable := disableGrokSessionSelections(targets, aliases)
	if disable["provider.v1-beta"] != "provider" || disable["provider"] != "provider" || disable["ordinary"] != "ordinary" {
		t.Fatalf("disable selections = %v", disable)
	}
}

func TestAppStartRejectsEmptyCustomRouteSet(t *testing.T) {
	dir := t.TempDir()
	grokHome := filepath.Join(dir, "grok")
	if err := os.MkdirAll(grokHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_HOME", grokHome)
	if err := os.WriteFile(filepath.Join(grokHome, "config.toml"), []byte("[model.official]\nmodel = \"grok-4.5\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := proxy.New(log.New(io.Discard, "", 0))
	server.PathAddr = "127.0.0.1:0"
	app := &App{logger: log.New(io.Discard, "", 0), dataDir: filepath.Join(dir, "data"), server: server}
	err := app.Start()
	if err == nil || !strings.Contains(err.Error(), "no explicit custom model endpoints") {
		t.Fatalf("start error = %v", err)
	}
	if app.IsRunning() {
		t.Fatal("app remained running without a custom route")
	}
}

func TestAppStartRejectsUnrecoverableProxyURLAmongValidRoutes(t *testing.T) {
	dir := t.TempDir()
	grokHome := filepath.Join(dir, "grok")
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(grokHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_HOME", grokHome)
	configPath := filepath.Join(grokHome, "config.toml")
	original := "[model.stale]\nbase_url = \"http://127.0.0.1:18787/c/stale\"\napi_key = \"test-key\"\n\n" +
		"[model.valid]\nbase_url = \"https://valid.example/v1\"\napi_key = \"test-key\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	server := proxy.New(log.New(io.Discard, "", 0))
	server.PathAddr = "127.0.0.1:0"
	app := &App{logger: log.New(io.Discard, "", 0), dataDir: dataDir, server: server}

	err := app.Start()
	if err == nil || !strings.Contains(err.Error(), "no restorable origin is available") {
		t.Fatalf("start error = %v", err)
	}
	if app.IsRunning() {
		t.Fatal("app remained running with an unrecoverable proxy URL")
	}
	current, _ := os.ReadFile(configPath)
	if string(current) != original {
		t.Fatalf("config changed after stale proxy rejection: %q", current)
	}
	if _, err := os.Stat(cfgpatch.StatePath(dataDir)); !os.IsNotExist(err) {
		t.Fatalf("rewrite state unexpectedly created: %v", err)
	}
}

func TestAppStartStopLifecycleRestoresConfigExactly(t *testing.T) {
	dir := t.TempDir()
	grokHome := filepath.Join(dir, "grok")
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(grokHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_HOME", grokHome)
	configPath := filepath.Join(grokHome, "config.toml")
	original := strings.Join([]string{
		"[subagents.models]",
		`general-purpose = "one"`,
		"",
		"[features]",
		"backend_tools = false",
		"web_fetch = false",
		"",
		"[model.one]",
		`base_url = "https://api.example.test/v1"`,
		`api_key = "test-key"`,
		`api_backend = "chat_completions"`,
		"supports_backend_search = true",
		"",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	server := proxy.New(log.New(io.Discard, "", 0))
	server.PathAddr = "127.0.0.1:0"
	app := &App{
		logger:  log.New(io.Discard, "", 0),
		dataDir: dataDir,
		server:  server,
	}
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	if !app.IsRunning() {
		t.Fatal("app did not enter running state")
	}
	patched, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		`base_url = "http://127.0.0.1:18787/c/one"`,
		`api_backend = "responses"`,
		"supports_backend_search = true",
		"backend_tools = true",
		"web_fetch = true",
		"enabled = true",
	} {
		if !strings.Contains(string(patched), expected) {
			t.Fatalf("running config missing %q:\n%s", expected, patched)
		}
	}
	if _, err := os.Stat(cfgpatch.StatePath(dataDir)); err != nil {
		t.Fatalf("rewrite state missing while running: %v", err)
	}

	if err := app.Stop(); err != nil {
		t.Fatal(err)
	}
	if app.IsRunning() {
		t.Fatal("app remained in running state after stop")
	}
	restored, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != original {
		t.Fatalf("lifecycle restore was not byte-exact\nwant: %q\ngot:  %q", original, restored)
	}
	if _, err := os.Stat(cfgpatch.StatePath(dataDir)); !os.IsNotExist(err) {
		t.Fatalf("rewrite state remains after stop: %v", err)
	}
}

func TestAppStartRejectsCCSwitchTakeoverBeforeRecovery(t *testing.T) {
	dir := t.TempDir()
	grokHome := filepath.Join(dir, "grok")
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(grokHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_HOME", grokHome)
	configPath := filepath.Join(grokHome, "config.toml")
	statePath := cfgpatch.StatePath(dataDir)
	original := "[models]\ndefault = \"one\"\n\n[model.one]\n" +
		"base_url = \"https://one.example/v1\"\napi_key = \"test-key\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := cfgpatch.ApplyTargets(configPath, statePath, []cfgpatch.Target{{ID: "one"}}); err != nil {
		t.Fatal(err)
	}
	stateBefore, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	ccSwitchConfig := "[models]\ndefault = \"one\"\n\n[model.one]\n" +
		"base_url = \"http://127.0.0.1:15721/grokbuild/v1\"\n" +
		"api_key = \"PROXY_MANAGED\"\napi_backend = \"responses\"\n"
	if err := os.WriteFile(configPath, []byte(ccSwitchConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	server := proxy.New(log.New(io.Discard, "", 0))
	server.PathAddr = "127.0.0.1:0"
	app := &App{logger: log.New(io.Discard, "", 0), dataDir: dataDir, server: server}
	err = app.Start()
	if err == nil || !strings.Contains(err.Error(), "CC Switch") {
		t.Fatalf("start error = %v", err)
	}
	if app.IsRunning() {
		t.Fatal("app started while CC Switch owned the Grok config")
	}
	current, _ := os.ReadFile(configPath)
	if string(current) != ccSwitchConfig {
		t.Fatalf("CC Switch config changed: %q", current)
	}
	stateAfter, _ := os.ReadFile(statePath)
	if !bytes.Equal(stateAfter, stateBefore) {
		t.Fatal("recovery state changed before CC Switch takeover was released")
	}
}

func TestAppStopWaitsForCCSwitchThenRestoresInSafeOrder(t *testing.T) {
	dir := t.TempDir()
	grokHome := filepath.Join(dir, "grok")
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(grokHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_HOME", grokHome)
	configPath := filepath.Join(grokHome, "config.toml")
	original := "[models]\ndefault = \"one\"\n\n[model.one]\n" +
		"base_url = \"https://one.example/v1\"\napi_key = \"test-key\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	server := proxy.New(log.New(io.Discard, "", 0))
	server.PathAddr = "127.0.0.1:0"
	app := &App{logger: log.New(io.Discard, "", 0), dataDir: dataDir, server: server}
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	patched, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	ccSwitchConfig := strings.Replace(string(patched),
		`base_url = "http://127.0.0.1:18787/c/one"`,
		`base_url = "http://127.0.0.1:15721/grokbuild/v1"`, 1)
	ccSwitchConfig = strings.Replace(ccSwitchConfig, `api_key = "test-key"`, `api_key = "PROXY_MANAGED"`, 1)
	if err := os.WriteFile(configPath, []byte(ccSwitchConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := app.Stop(); err == nil || !strings.Contains(err.Error(), "CC Switch") {
		t.Fatalf("stop during CC Switch takeover error = %v", err)
	}
	if !app.IsRunning() {
		t.Fatal("proxy stopped before CC Switch restored its hellogrok backup")
	}
	if _, err := os.Stat(cfgpatch.StatePath(dataDir)); err != nil {
		t.Fatalf("recovery state was lost during CC Switch takeover: %v", err)
	}
	current, _ := os.ReadFile(configPath)
	if string(current) != ccSwitchConfig {
		t.Fatal("stop attempt overwrote the active CC Switch config")
	}

	// CC Switch stops first and restores the live snapshot it captured while
	// hellogrok was active. hellogrok can then restore the true original.
	if err := os.WriteFile(configPath, patched, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := app.Stop(); err != nil {
		t.Fatal(err)
	}
	if app.IsRunning() {
		t.Fatal("app remained running after the safe stop order")
	}
	restored, _ := os.ReadFile(configPath)
	if string(restored) != original {
		t.Fatalf("safe stop order did not restore original\nwant: %q\ngot:  %q", original, restored)
	}
}

func TestAppStopRelinquishesCompleteExternalProviderReplacement(t *testing.T) {
	dir := t.TempDir()
	grokHome := filepath.Join(dir, "grok")
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(grokHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_HOME", grokHome)
	configPath := filepath.Join(grokHome, "config.toml")
	original := "[model.one]\nbase_url = \"https://one.example/v1\"\napi_key = \"test-key\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	var refreshTargets []map[string]string
	refresh := func(_ context.Context, selections map[string]string) (groksync.Result, error) {
		refreshTargets = append(refreshTargets, cloneStringMap(selections))
		return groksync.Result{GrokFound: true, ReachableLeaders: 1}, nil
	}
	server := proxy.New(log.New(io.Discard, "", 0))
	server.PathAddr = "127.0.0.1:0"
	app := &App{
		logger:              log.New(io.Discard, "", 0),
		dataDir:             dataDir,
		server:              server,
		refreshGrokSessions: refresh,
	}
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	external := "[model.two]\nbase_url = \"https://two.example/v1\"\napi_key = \"new-key\"\n"
	if err := os.WriteFile(configPath, []byte(external), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := app.Stop(); err != nil {
		t.Fatal(err)
	}
	if app.IsRunning() {
		t.Fatal("app remained running after a complete external provider replacement")
	}
	current, _ := os.ReadFile(configPath)
	if string(current) != external {
		t.Fatalf("external provider config was overwritten: %q", current)
	}
	if _, err := os.Stat(cfgpatch.StatePath(dataDir)); !os.IsNotExist(err) {
		t.Fatalf("obsolete recovery state remains: %v", err)
	}
	if len(refreshTargets) != 2 || len(refreshTargets[1]) != 0 {
		t.Fatalf("external replacement selected obsolete models: %v", refreshTargets)
	}
}

func TestAppStopPreservesDeletedChannelAndRefreshesOnlyRemainingRoutes(t *testing.T) {
	dir := t.TempDir()
	grokHome := filepath.Join(dir, "grok")
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(grokHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_HOME", grokHome)
	configPath := filepath.Join(grokHome, "config.toml")
	original := "[model.one]\nbase_url = \"https://one.example/v1\"\napi_key = \"one-key\"\n\n" +
		"[model.two]\nbase_url = \"https://two.example/v1\"\napi_key = \"two-key\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	var refreshTargets []map[string]string
	refresh := func(_ context.Context, selections map[string]string) (groksync.Result, error) {
		refreshTargets = append(refreshTargets, cloneStringMap(selections))
		return groksync.Result{GrokFound: true, ReachableLeaders: 1}, nil
	}
	server := proxy.New(log.New(io.Discard, "", 0))
	server.PathAddr = "127.0.0.1:0"
	app := &App{logger: log.New(io.Discard, "", 0), dataDir: dataDir, server: server, refreshGrokSessions: refresh}
	if err := app.Start(); err != nil {
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

	if err := app.Stop(); err != nil {
		t.Fatal(err)
	}
	current, _ := os.ReadFile(configPath)
	expectedStart := strings.Index(original, "[model.one]\n")
	expectedEndOffset := strings.Index(original[expectedStart:], "[model.two]\n")
	expected := original[:expectedStart] + original[expectedStart+expectedEndOffset:]
	if string(current) != expected {
		t.Fatalf("deleted channel was reintroduced or remaining channel was not restored\nwant: %q\ngot:  %q", expected, current)
	}
	if len(refreshTargets) != 2 || len(refreshTargets[1]) != 1 || refreshTargets[1]["two"] != "two" {
		t.Fatalf("stop refreshed deleted or missing routes: %v", refreshTargets)
	}
	if _, exists := refreshTargets[1]["one"]; exists {
		t.Fatalf("deleted channel was selected during stop: %v", refreshTargets[1])
	}
}

func TestAppStopPreservesManagedConfigEdit(t *testing.T) {
	dir := t.TempDir()
	grokHome := filepath.Join(dir, "grok")
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(grokHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_HOME", grokHome)
	configPath := filepath.Join(grokHome, "config.toml")
	original := "[model.one]\nbase_url = \"https://one.example/v1\"\napi_key = \"test-key\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	server := proxy.New(log.New(io.Discard, "", 0))
	server.PathAddr = "127.0.0.1:0"
	app := &App{logger: log.New(io.Discard, "", 0), dataDir: dataDir, server: server}
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	patched, _ := os.ReadFile(configPath)
	conflicted := strings.Replace(string(patched), "supports_backend_search = false", "supports_backend_search = true", 1)
	if err := os.WriteFile(configPath, []byte(conflicted), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := app.Stop(); err != nil {
		t.Fatal(err)
	}
	if app.IsRunning() {
		t.Fatal("proxy remained active after its route was safely restored")
	}
	current, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	want := original + "supports_backend_search = true\n"
	if string(current) != want {
		t.Fatalf("managed edit was not preserved while stopping\nwant: %q\ngot:  %q", want, current)
	}
}

func TestAppStopKeepsServingWhenRenamedModelStillReferencesHellogrok(t *testing.T) {
	dir := t.TempDir()
	grokHome := filepath.Join(dir, "grok")
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(grokHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_HOME", grokHome)
	configPath := filepath.Join(grokHome, "config.toml")
	original := "[model.one]\nbase_url = \"https://one.example/v1\"\napi_key = \"test-key\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	server := proxy.New(log.New(io.Discard, "", 0))
	server.PathAddr = "127.0.0.1:0"
	app := &App{logger: log.New(io.Discard, "", 0), dataDir: dataDir, server: server}
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	patched, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	renamed := strings.Replace(string(patched), "[model.one]", "[model.renamed]", 1)
	if err := os.WriteFile(configPath, []byte(renamed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := app.Stop(); err == nil {
		t.Fatal("stop succeeded while a renamed model still referenced hellogrok")
	}
	if !app.IsRunning() {
		t.Fatal("proxy stopped while config still referenced it")
	}
	if err := os.WriteFile(configPath, patched, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := app.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureFacadeIdleRejectsOccupiedAddress(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := ensureFacadeIdle(address); err == nil {
		_ = listener.Close()
		t.Fatal("occupied facade address was treated as idle")
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ensureFacadeIdle(address); err != nil {
		t.Fatalf("released facade address remained busy: %v", err)
	}
}

func TestAppStartReportsOccupiedFacadeAddress(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	server := proxy.New(log.New(io.Discard, "", 0))
	server.PathAddr = listener.Addr().String()
	app := &App{
		logger:  log.New(io.Discard, "", 0),
		dataDir: t.TempDir(),
		server:  server,
	}
	err = app.Start()
	if err == nil || !strings.Contains(err.Error(), "本地代理端口 "+server.PathAddr+" 已被占用") {
		t.Fatalf("occupied facade error = %v", err)
	}
	if app.IsRunning() {
		t.Fatal("app remained running after occupied facade address")
	}
}

func TestResolveSearchRoutesPreservesEveryConfiguredNativeSearchCapability(t *testing.T) {
	app := &App{logger: log.New(io.Discard, "", 0)}
	routes := []config.Route{
		{ChannelID: "grok-custom", WireModel: "grok-4.5", APIBackend: "responses", SupportsBackendSearch: true},
		{ChannelID: "deepseek-v4-flash", WireModel: "deepseek-v4-flash", APIBackend: "chat_completions", SupportsBackendSearch: true},
		{ChannelID: "plain", WireModel: "plain", APIBackend: "chat_completions"},
	}
	effective := app.resolveSearchRoutes(routes, config.WebSearchSelection{
		Model: "deepseek-v4-flash", Explicit: true, Source: "config",
	})
	if !effective[0].SupportsBackendSearch || !effective[1].SupportsBackendSearch || effective[2].SupportsBackendSearch {
		t.Fatalf("configured capabilities were not preserved: %+v", effective)
	}
	if !routes[0].SupportsBackendSearch || !routes[1].SupportsBackendSearch {
		t.Fatalf("search routing mutated its input: %+v", routes)
	}
}

func TestBuildSupportsBackendSearchMaterialization(t *testing.T) {
	for _, test := range []struct {
		backend    string
		configured bool
		want       bool
	}{
		{backend: "responses", configured: true, want: true},
		{backend: "responses", configured: false, want: false},
		{backend: "messages", configured: true, want: true},
		{backend: "chat_completions", configured: true, want: true},
	} {
		route := config.Route{APIBackend: test.backend, SupportsBackendSearch: test.configured}
		if got := buildSupportsBackendSearch(route); got != test.want {
			t.Fatalf("backend=%s configured=%t materialized=%t want=%t", test.backend, test.configured, got, test.want)
		}
	}
}

func TestBuildAPIBackendUsesResponsesOnlyForCapableChannels(t *testing.T) {
	for _, test := range []struct {
		backend    string
		configured bool
		want       string
	}{
		{backend: "responses", configured: true, want: "responses"},
		{backend: "messages", configured: true, want: "responses"},
		{backend: "chat_completions", configured: true, want: "responses"},
		{backend: "messages", configured: false, want: "messages"},
		{backend: "chat_completions", configured: false, want: "chat_completions"},
	} {
		route := config.Route{APIBackend: test.backend, SupportsBackendSearch: test.configured}
		if got := buildAPIBackend(route); got != test.want {
			t.Fatalf("backend=%s configured=%t build=%q want=%q", test.backend, test.configured, got, test.want)
		}
	}
}

func TestResolveSearchRoutesTrustsOnlyConfiguredBackendSearch(t *testing.T) {
	app := &App{logger: log.New(io.Discard, "", 0)}
	routes := []config.Route{
		{ChannelID: "grok-missing", WireModel: "grok-4.5", APIBackend: "responses"},
		{ChannelID: "gpt-false", WireModel: "gpt-5.6", APIBackend: "responses"},
		{ChannelID: "grok-true", WireModel: "grok-4.5", APIBackend: "responses", SupportsBackendSearch: true},
	}
	effective := app.resolveSearchRoutes(routes, config.WebSearchSelection{})
	byID := map[string]config.Route{}
	for _, route := range effective {
		byID[route.ChannelID] = route
	}
	if byID["grok-missing"].SupportsBackendSearch || byID["gpt-false"].SupportsBackendSearch ||
		!byID["grok-true"].SupportsBackendSearch {
		t.Fatalf("backend-search configuration was not preserved: %+v", byID)
	}
}

func TestAppStartDoesNotProbeUpstreamSearchCapability(t *testing.T) {
	for _, test := range []struct {
		name       string
		modelID    string
		modelsTOML string
		capability string
	}{
		{name: "omitted backend search", modelID: "grok-missing-search-flag"},
		{
			name:       "explicit client search model",
			modelID:    "grok-explicit-search-model",
			modelsTOML: "[models]\nweb_search = \"grok-explicit-search-model\"\n\n",
			capability: "supports_backend_search = true\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var upstreamRequests atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				upstreamRequests.Add(1)
			}))
			defer upstream.Close()

			dir := t.TempDir()
			grokHome := filepath.Join(dir, "grok")
			if err := os.MkdirAll(grokHome, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GROK_HOME", grokHome)
			original := test.modelsTOML + "[model." + test.modelID + "]\n" +
				"model = \"grok-4.5\"\n" +
				"base_url = \"" + upstream.URL + "/v1\"\n" +
				"api_key = \"test-key\"\n" + test.capability
			if err := os.WriteFile(filepath.Join(grokHome, "config.toml"), []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}

			server := proxy.New(log.New(io.Discard, "", 0))
			server.PathAddr = "127.0.0.1:0"
			app := &App{logger: log.New(io.Discard, "", 0), dataDir: filepath.Join(dir, "data"), server: server}
			if err := app.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if app.IsRunning() {
					_ = app.Stop()
				}
			})
			if got := upstreamRequests.Load(); got != 0 {
				t.Fatalf("startup contacted upstream %d time(s), want 0", got)
			}
			if err := app.Stop(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
