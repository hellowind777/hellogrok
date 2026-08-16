package main

import (
	"bytes"
	"context"
	"fmt"
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
	"time"

	"github.com/hellowind777/hellogrok/internal/appinfo"
	"github.com/hellowind777/hellogrok/internal/capacity"
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

func TestConfiguredAutoCompactEnvironment(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    uint8
		wantNil bool
		wantErr bool
	}{
		{name: "empty", value: "", wantNil: true},
		{name: "zero", value: "0", want: 0},
		{name: "upper boundary", value: "100", want: 100},
		{name: "out of range", value: "101", wantErr: true},
		{name: "not an integer", value: "85.5", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("GROK_AUTO_COMPACT_THRESHOLD_PERCENT", test.value)
			got, err := configuredAutoCompactEnvironment()
			if (err != nil) != test.wantErr {
				t.Fatalf("configuredAutoCompactEnvironment() err=%v", err)
			}
			if test.wantErr {
				return
			}
			if test.wantNil {
				if got != nil {
					t.Fatalf("configuredAutoCompactEnvironment() = %d, want nil", *got)
				}
				return
			}
			if got == nil || *got != test.want {
				t.Fatalf("configuredAutoCompactEnvironment() = %v, want %d", got, test.want)
			}
		})
	}
}

func TestAppProjectsIndependentSafeAutoCompactThresholdsAndRestores(t *testing.T) {
	dir := t.TempDir()
	grokHome := filepath.Join(dir, "grok")
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(grokHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_HOME", grokHome)
	t.Setenv("GROK_AUTO_COMPACT_THRESHOLD_PERCENT", "")
	configPath := filepath.Join(grokHome, "config.toml")
	original := "[session]\nauto_compact_threshold_percent = 85\n\n" +
		"[model.one]\nbase_url = \"https://one.example/v1\"\napi_key = \"test-key\"\ncontext_window = 1048576\nmax_completion_tokens = 384000\nauto_compact_threshold_percent = 90\n\n" +
		"[model.two]\nbase_url = \"https://two.example/v1\"\napi_key = \"test-key\"\ncontext_window = 393216\nmax_completion_tokens = 65536\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	server := proxy.New(log.New(io.Discard, "", 0))
	server.PathAddr = "127.0.0.1:0"
	app := &App{logger: log.New(io.Discard, "", 0), dataDir: dataDir, server: server}
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if app.IsRunning() {
			_ = app.Stop()
		}
	})
	patched, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(patched)
	oneStart := strings.Index(text, "[model.one]")
	twoStart := strings.Index(text, "[model.two]")
	if oneStart < 0 || twoStart <= oneStart ||
		!strings.Contains(text[oneStart:twoStart], "auto_compact_threshold_percent = 58") ||
		!strings.Contains(text[twoStart:], "auto_compact_threshold_percent = 78") {
		t.Fatalf("per-model safe thresholds were not applied:\n%s", text)
	}
	if err := app.Stop(); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != original {
		t.Fatalf("config was not restored exactly\nwant: %q\ngot:  %q", original, restored)
	}
}

func TestAppLeavesThresholdUnchangedUntilCapacityIsKnown(t *testing.T) {
	dir := t.TempDir()
	grokHome := filepath.Join(dir, "grok")
	if err := os.MkdirAll(grokHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_HOME", grokHome)
	t.Setenv("GROK_AUTO_COMPACT_THRESHOLD_PERCENT", "")
	configPath := filepath.Join(grokHome, "config.toml")
	original := "[model.one]\nbase_url = \"https://one.example/v1\"\napi_key = \"test-key\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	server := proxy.New(log.New(io.Discard, "", 0))
	server.PathAddr = "127.0.0.1:0"
	app := &App{logger: log.New(io.Discard, "", 0), dataDir: filepath.Join(dir, "data"), server: server}
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	patched, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(patched), "auto_compact_threshold_percent") {
		t.Fatalf("unknown capacity produced a guessed threshold:\n%s", patched)
	}
	if err := app.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestAppLearnsRuntimeCapacityAndUpdatesThreshold(t *testing.T) {
	dir := t.TempDir()
	grokHome := filepath.Join(dir, "grok")
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(grokHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_HOME", grokHome)
	t.Setenv("GROK_AUTO_COMPACT_THRESHOLD_PERCENT", "")
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
	app.queueCapacityObservation("one", capacity.Observation{
		ContextWindow: 1_048_576, ContextSource: capacity.SourceResponseHeader,
		MaxCompletionTokens: 384_000, CompletionSource: capacity.SourceResponseHeader,
	})
	deadline := time.Now().Add(3 * time.Second)
	for {
		patched, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(patched), "auto_compact_threshold_percent = 58") &&
			strings.Contains(string(patched), "context_window = 1048576") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("runtime capacity did not update threshold:\n%s", patched)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := app.Stop(); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != original {
		t.Fatalf("runtime threshold was not restored exactly\nwant: %q\ngot:  %q", original, restored)
	}
}

func TestAppUsesTrustedCapacityCacheAtStartup(t *testing.T) {
	dir := t.TempDir()
	grokHome := filepath.Join(dir, "grok")
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(grokHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_HOME", grokHome)
	t.Setenv("GROK_AUTO_COMPACT_THRESHOLD_PERCENT", "")
	configPath := filepath.Join(grokHome, "config.toml")
	original := "[model.one]\nbase_url = \"https://one.example/v1\"\napi_key = \"test-key\"\nmax_completion_tokens = 384000\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	models, err := config.LoadModels(configPath)
	if err != nil {
		t.Fatal(err)
	}
	routes, err := config.BuildRoutes(models)
	if err != nil || len(routes) != 1 {
		t.Fatalf("BuildRoutes() routes=%d err=%v", len(routes), err)
	}
	cache, err := capacity.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	key := capacity.RouteKey(routes[0].OriginBase, routes[0].WireModel, routes[0].APIBackend)
	if _, changed, err := cache.Observe(key, capacity.Observation{
		ContextWindow: 1_048_576, ContextSource: capacity.SourceResponseHeader,
	}); err != nil || !changed {
		t.Fatalf("seed capacity cache changed=%t err=%v", changed, err)
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
	if !strings.Contains(string(patched), "context_window = 1048576") ||
		!strings.Contains(string(patched), "auto_compact_threshold_percent = 58") {
		t.Fatalf("cached capacity was not projected at startup:\n%s", patched)
	}
	if err := app.Stop(); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != original {
		t.Fatalf("cached startup projection was not restored exactly\nwant: %q\ngot:  %q", original, restored)
	}
}

func TestAppUpdatesManagedCapacityWithoutLosingOriginalBytes(t *testing.T) {
	dir := t.TempDir()
	grokHome := filepath.Join(dir, "grok")
	if err := os.MkdirAll(grokHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_HOME", grokHome)
	t.Setenv("GROK_AUTO_COMPACT_THRESHOLD_PERCENT", "")
	configPath := filepath.Join(grokHome, "config.toml")
	original := "[model.one]\r\nbase_url = \"https://one.example/v1\"\r\napi_key = \"test-key\"\r\nauto_compact_threshold_percent = 92 # personal\r\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	server := proxy.New(log.New(io.Discard, "", 0))
	server.PathAddr = "127.0.0.1:0"
	app := &App{logger: log.New(io.Discard, "", 0), dataDir: filepath.Join(dir, "data"), server: server}
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor := func(contextWindow, threshold string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for {
			current, err := os.ReadFile(configPath)
			if err != nil {
				if time.Now().After(deadline) {
					t.Fatal(err)
				}
				time.Sleep(10 * time.Millisecond)
				continue
			}
			text := string(current)
			if strings.Contains(text, contextWindow) && strings.Contains(text, threshold) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("capacity update missing %q or %q:\n%s", contextWindow, threshold, text)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	app.queueCapacityObservation("one", capacity.Observation{
		ContextWindow: 1_048_576, ContextSource: capacity.SourceResponseHeader,
		MaxCompletionTokens: 384_000, CompletionSource: capacity.SourceResponseHeader,
	})
	waitFor("context_window = 1048576", "auto_compact_threshold_percent = 58")
	app.queueCapacityObservation("one", capacity.Observation{
		ContextWindow: 524_288, ContextSource: capacity.SourceResponseHeader,
		MaxCompletionTokens: 65_536, CompletionSource: capacity.SourceResponseHeader,
	})
	waitFor("context_window = 524288", "auto_compact_threshold_percent = 82")
	if err := app.Stop(); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != original {
		t.Fatalf("repeated capacity updates lost original bytes\nwant: %q\ngot:  %q", original, restored)
	}
}

func TestAppPreservesRuntimeUserThresholdBeforeCapacityLearning(t *testing.T) {
	dir := t.TempDir()
	grokHome := filepath.Join(dir, "grok")
	if err := os.MkdirAll(grokHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_HOME", grokHome)
	t.Setenv("GROK_AUTO_COMPACT_THRESHOLD_PERCENT", "")
	configPath := filepath.Join(grokHome, "config.toml")
	original := "[model.one]\nbase_url = \"https://one.example/v1\"\napi_key = \"test-key\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	server := proxy.New(log.New(io.Discard, "", 0))
	server.PathAddr = "127.0.0.1:0"
	app := &App{logger: log.New(io.Discard, "", 0), dataDir: filepath.Join(dir, "data"), server: server}
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	patched, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	userEdited := strings.Replace(string(patched), "api_key = \"test-key\"", "api_key = \"test-key\"\nauto_compact_threshold_percent = 40", 1)
	if err := os.WriteFile(configPath, []byte(userEdited), 0o600); err != nil {
		t.Fatal(err)
	}
	app.queueCapacityObservation("one", capacity.Observation{
		ContextWindow: 1_048_576, ContextSource: capacity.SourceResponseHeader,
		MaxCompletionTokens: 384_000, CompletionSource: capacity.SourceResponseHeader,
	})
	time.Sleep(100 * time.Millisecond)
	current, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(current), "auto_compact_threshold_percent = 40") ||
		strings.Contains(string(current), "auto_compact_threshold_percent = 58") {
		t.Fatalf("runtime user threshold was overwritten:\n%s", current)
	}
	if err := app.Stop(); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(restored), "base_url = \"https://one.example/v1\"") ||
		!strings.Contains(string(restored), "auto_compact_threshold_percent = 40") {
		t.Fatalf("runtime user threshold did not survive shutdown:\n%s", restored)
	}
}

func TestAppUsesRequestOutputForBudgetWithoutCreatingOutputCap(t *testing.T) {
	dir := t.TempDir()
	grokHome := filepath.Join(dir, "grok")
	if err := os.MkdirAll(grokHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_HOME", grokHome)
	t.Setenv("GROK_AUTO_COMPACT_THRESHOLD_PERCENT", "")
	configPath := filepath.Join(grokHome, "config.toml")
	original := "[model.one]\nbase_url = \"https://one.example/v1\"\napi_key = \"test-key\"\ncontext_window = 1048576\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	server := proxy.New(log.New(io.Discard, "", 0))
	server.PathAddr = "127.0.0.1:0"
	app := &App{logger: log.New(io.Discard, "", 0), dataDir: filepath.Join(dir, "data"), server: server}
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	app.queueCapacityObservation("one", capacity.Observation{
		MaxCompletionTokens: 384_000,
		CompletionSource:    capacity.SourceRequest,
	})
	deadline := time.Now().Add(3 * time.Second)
	for {
		patched, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatal(err)
		}
		text := string(patched)
		if strings.Contains(text, "auto_compact_threshold_percent = 58") {
			if strings.Contains(text, "max_completion_tokens") {
				t.Fatalf("request-derived output was turned into a model cap:\n%s", text)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("request output did not produce a safe threshold:\n%s", text)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := app.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestAppStopCancelsActiveSessionRefreshRetry(t *testing.T) {
	dir := t.TempDir()
	grokHome := filepath.Join(dir, "grok")
	if err := os.MkdirAll(grokHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_HOME", grokHome)
	t.Setenv("GROK_AUTO_COMPACT_THRESHOLD_PERCENT", "")
	configPath := filepath.Join(grokHome, "config.toml")
	if err := os.WriteFile(configPath, []byte("[model.one]\nbase_url = \"https://one.example/v1\"\napi_key = \"test-key\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	retryStarted := make(chan struct{})
	refresh := func(ctx context.Context, _ map[string]string) (groksync.Result, error) {
		switch calls.Add(1) {
		case 1:
			return groksync.Result{GrokFound: true, ReachableLeaders: 1, TargetSessions: 1, SkippedActiveSessions: 1}, nil
		case 2:
			close(retryStarted)
			<-ctx.Done()
			return groksync.Result{GrokFound: true, ReachableLeaders: 1, TargetSessions: 1, SkippedActiveSessions: 1}, ctx.Err()
		default:
			return groksync.Result{GrokFound: true, ReachableLeaders: 1}, nil
		}
	}
	server := proxy.New(log.New(io.Discard, "", 0))
	server.PathAddr = "127.0.0.1:0"
	app := &App{
		logger: log.New(io.Discard, "", 0), dataDir: filepath.Join(dir, "data"), server: server,
		refreshGrokSessions: refresh, refreshRetryDelay: func(int) time.Duration { return time.Millisecond },
	}
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-retryStarted:
	case <-time.After(time.Second):
		t.Fatal("active-session refresh retry did not start")
	}
	started := time.Now()
	if err := app.Stop(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Stop waited too long for refresh cancellation: %s", elapsed)
	}
}

func TestQueueGrokRefreshCoalescesPendingSelections(t *testing.T) {
	app := &App{
		backgroundCtx: context.Background(),
		refreshSignal: make(chan struct{}, 1),
	}
	app.queueGrokRefresh("first", map[string]string{"one": "one"})
	app.queueGrokRefresh("latest", map[string]string{"two": "two"})
	request := app.takeGrokRefresh()
	if request == nil || request.phase != "latest" ||
		request.selections["one"] != "one" || request.selections["two"] != "two" {
		t.Fatalf("coalesced request = %+v", request)
	}
	if len(app.refreshSignal) != 1 {
		t.Fatalf("refresh signals = %d, want one coalesced signal", len(app.refreshSignal))
	}
}

func TestGrokRefreshRetriesAreBounded(t *testing.T) {
	var calls atomic.Int32
	server := proxy.New(log.New(io.Discard, "", 0))
	app := &App{
		logger: log.New(io.Discard, "", 0), server: server, running: true,
		refreshRetryDelay: func(int) time.Duration { return 0 },
		refreshGrokSessions: func(context.Context, map[string]string) (groksync.Result, error) {
			calls.Add(1)
			return groksync.Result{
				GrokFound: true, ReachableLeaders: 1, TargetSessions: 1, SkippedActiveSessions: 1,
			}, nil
		},
	}
	app.startBackgroundWorkersLocked()
	t.Cleanup(app.stopBackgroundWorkers)
	app.queueGrokRefresh("capacity", map[string]string{"one": "one"})
	deadline := time.Now().Add(3 * time.Second)
	for {
		app.mu.Lock()
		exhausted := strings.Contains(app.grokSyncStatus, "已停止本轮自动刷新")
		app.mu.Unlock()
		if exhausted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("refresh retries did not finish: calls=%d", calls.Load())
		}
		time.Sleep(time.Millisecond)
	}
	if got := calls.Load(); got != maxGrokRefreshRetries {
		t.Fatalf("refresh calls = %d, want %d", got, maxGrokRefreshRetries)
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
	original := "[model.one]\nbase_url = \"https://one.example/v1\"\napi_key = \"test-key\"\nsupports_backend_search = false\n"
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

func TestAppDoesNotInventDeepSeekLimits(t *testing.T) {
	dir := t.TempDir()
	grokHome := filepath.Join(dir, "grok")
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(grokHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_HOME", grokHome)
	configPath := filepath.Join(grokHome, "config.toml")
	original := "[model.deepseek]\n" +
		"model = \"deepseek-v4-pro\"\n" +
		"base_url = \"https://api.deepseek.com\"\n" +
		"api_key = \"test-key\"\n" +
		"api_backend = \"responses\"\n"
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
	for _, expected := range []string{
		`reasoning_efforts = ["none", "low", "high", "max"]`,
		`reasoning_effort = "high"`,
	} {
		if !strings.Contains(string(patched), expected) {
			t.Fatalf("pre-response DeepSeek config missing %q:\n%s", expected, patched)
		}
	}
	for _, unexpected := range []string{"context_window", "max_completion_tokens", "inference_idle_timeout_secs"} {
		if strings.Contains(string(patched), unexpected) {
			t.Fatalf("DeepSeek capacity was invented for %q:\n%s", unexpected, patched)
		}
	}
	if err := app.Stop(); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != original {
		t.Fatalf("DeepSeek config was not restored exactly\nwant: %q\ngot:  %q", original, restored)
	}
}

func TestAppPreservesExplicitReasoningMenusWithoutAddingDefaults(t *testing.T) {
	tests := []struct {
		name string
		menu string
	}{
		{"full compact list", `reasoning_efforts = ["none", "low", "high", "max"]`},
		{"compact list without none", `reasoning_efforts = ["low", "high", "max"]`},
		{"empty list", `reasoning_efforts = []`},
		{"custom object menu", `reasoning_efforts = [{ value = "max", label = "Only Max", default = true }]`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			grokHome := filepath.Join(dir, "grok")
			dataDir := filepath.Join(dir, "data")
			if err := os.MkdirAll(grokHome, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GROK_HOME", grokHome)
			configPath := filepath.Join(grokHome, "config.toml")
			original := "[model.deepseek]\n" +
				"model = \"deepseek-v4-pro\"\n" +
				"base_url = \"https://api.deepseek.com\"\n" +
				"api_key = \"test-key\"\n" + test.menu + "\n"
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
			if !strings.Contains(string(patched), test.menu) ||
				strings.Contains(string(patched), `reasoning_effort = "high"`) ||
				strings.Count(string(patched), "reasoning_efforts") != 1 {
				t.Fatalf("explicit menu was not authoritative:\n%s", patched)
			}
			if err := app.Stop(); err != nil {
				t.Fatal(err)
			}
			restored, _ := os.ReadFile(configPath)
			if string(restored) != original {
				t.Fatalf("explicit menu lifecycle was not byte-exact\nwant: %q\ngot:  %q", original, restored)
			}
		})
	}
}

func TestAppMigratesLegacyReasoningArrayTablesOnce(t *testing.T) {
	dir := t.TempDir()
	grokHome := filepath.Join(dir, "grok")
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(grokHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_HOME", grokHome)
	configPath := filepath.Join(grokHome, "config.toml")
	var original strings.Builder
	for _, id := range []string{"deepseek-v4-pro", "deepseek-v4-flash", "deepseek-v4-chat"} {
		fmt.Fprintf(&original, "[model.%s]\nmodel = %q\nbase_url = \"https://api.deepseek.com\"\napi_key = \"test-key\"\nreasoning_effort = \"max\"\n", id, id)
		for index, value := range []string{"none", "low", "high", "max"} {
			fmt.Fprintf(&original, "[[model.%s.reasoning_efforts]]\nvalue = %q\nlabel = %q\n", id, value, strings.ToUpper(value[:1])+value[1:])
			if index == 2 {
				original.WriteString("default = true\n")
			}
		}
		original.WriteString("\n")
	}
	if err := os.WriteFile(configPath, []byte(original.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	runLifecycle := func() string {
		t.Helper()
		server := proxy.New(log.New(io.Discard, "", 0))
		server.PathAddr = "127.0.0.1:0"
		app := &App{logger: log.New(io.Discard, "", 0), dataDir: dataDir, server: server}
		if err := app.Start(); err != nil {
			t.Fatal(err)
		}
		if err := app.Stop(); err != nil {
			t.Fatal(err)
		}
		current, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatal(err)
		}
		return string(current)
	}

	first := runLifecycle()
	if strings.Contains(first, "[[model.") || strings.Contains(first, "label =") ||
		strings.Count(first, `reasoning_efforts = ["none", "low", "high", "max"]`) != 3 ||
		strings.Count(first, `reasoning_effort = "max"`) != 3 {
		t.Fatalf("legacy menus were not compacted exactly once:\n%s", first)
	}
	second := runLifecycle()
	if second != first {
		t.Fatalf("second lifecycle changed migrated config\nfirst:  %q\nsecond: %q", first, second)
	}
}

func TestAppPreservesConfiguredDeepSeekLimits(t *testing.T) {
	dir := t.TempDir()
	grokHome := filepath.Join(dir, "grok")
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(grokHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_HOME", grokHome)
	configPath := filepath.Join(grokHome, "config.toml")
	original := "[model.deepseek]\n" +
		"model = \"deepseek-v4-pro\"\n" +
		"base_url = \"https://api.deepseek.com\"\n" +
		"api_key = \"test-key\"\n" +
		"api_backend = \"responses\"\n" +
		"context_window = 131072 # selected billing tier\n" +
		"max_completion_tokens = 32768 # selected output cap\n" +
		"inference_idle_timeout_secs = 900 # selected idle policy\n" +
		"supports_reasoning_effort = true # user capability\n" +
		"reasoning_efforts = [{ value = \"max\", label = \"Only Max\", default = true }] # user menu\n"
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
	if !strings.Contains(string(patched), "context_window = 131072 # selected billing tier") ||
		!strings.Contains(string(patched), "max_completion_tokens = 32768 # selected output cap") ||
		!strings.Contains(string(patched), "inference_idle_timeout_secs = 900 # selected idle policy") ||
		!strings.Contains(string(patched), "supports_reasoning_effort = true # user capability") ||
		!strings.Contains(string(patched), `reasoning_efforts = [{ value = "max", label = "Only Max", default = true }] # user menu`) ||
		strings.Contains(string(patched), `label = "Low"`) ||
		strings.Contains(string(patched), "context_window = 1000000") ||
		strings.Contains(string(patched), "max_completion_tokens = 384000") ||
		strings.Contains(string(patched), "inference_idle_timeout_secs = 660") {
		t.Fatalf("configured DeepSeek limits were replaced:\n%s", patched)
	}
	if err := app.Stop(); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != original {
		t.Fatalf("configured DeepSeek limits were not restored byte-exactly\nwant: %q\ngot:  %q", original, restored)
	}
}

func TestConfiguredMaxCompletionTokensProjectsOnlyConfiguredLimits(t *testing.T) {
	if got := configuredMaxCompletionTokens(config.Route{}); got != 0 {
		t.Fatalf("unconfigured max completion tokens = %d", got)
	}
	if got := configuredMaxCompletionTokens(config.Route{
		MaxCompletionTokens: 32768, MaxCompletionTokensConfigured: true,
	}); got != 32768 {
		t.Fatalf("configured max completion tokens = %d", got)
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
	want := strings.Replace(original, "supports_backend_search = false", "supports_backend_search = true", 1)
	if string(current) != want {
		t.Fatalf("managed edit was not preserved while stopping\nwant: %q\ngot:  %q", want, current)
	}
}

func TestAppStopRestoresRoutesWhenUserConfigIsInvalid(t *testing.T) {
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
	userEdit := "\n[user]\nunfinished = \"\n"
	if err := os.WriteFile(configPath, append(patched, []byte(userEdit)...), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := app.Stop(); err != nil {
		t.Fatal(err)
	}
	if app.IsRunning() {
		t.Fatal("proxy remained active after invalid config recovery")
	}
	current, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	want := original + userEdit
	if string(current) != want {
		t.Fatalf("stop did not restore the proxy route while preserving the invalid user edit\nwant: %q\ngot:  %q", want, current)
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

func TestResolveSearchRoutesProjectsSelectedCustomModelAsBackendSearch(t *testing.T) {
	app := &App{logger: log.New(io.Discard, "", 0)}
	routes := []config.Route{
		{
			ChannelID: "selected", APIBackend: "messages", WireModel: "wire-selected",
			SupportsBackendSearchConfigured: true,
		},
		{ChannelID: "ordinary", APIBackend: "chat_completions", WireModel: "wire-ordinary"},
	}
	effective := app.resolveSearchRoutes(routes, config.WebSearchSelection{
		Model: "selected", Explicit: true, Source: "config",
	})
	if !effective[0].DefaultSearchModel || !effective[0].SupportsBackendSearch {
		t.Fatalf("selected route was not projected as backend-search capable: %+v", effective[0])
	}
	if buildAPIBackend(effective[0]) != "responses" || !projectBackendSearch(effective[0]) {
		t.Fatalf("selected route was not exposed through Grok Build's Responses backend: %+v", effective[0])
	}
	if effective[1].DefaultSearchModel || effective[1].SupportsBackendSearch {
		t.Fatalf("unselected route capability changed: %+v", effective[1])
	}
	if routes[0].DefaultSearchModel || routes[0].SupportsBackendSearch {
		t.Fatalf("search routing mutated its input: %+v", routes[0])
	}
}

func TestResolveSearchRoutesDefaultsOfficialDeepSeekSearchUnlessExplicitlyDisabled(t *testing.T) {
	app := &App{logger: log.New(io.Discard, "", 0)}
	routes := []config.Route{
		{ChannelID: "deepseek-default", Host: "api.deepseek.com", WireModel: "deepseek-v4-pro", APIBackend: "chat_completions"},
		{ChannelID: "deepseek-disabled", Host: "api.deepseek.com", WireModel: "deepseek-v4-flash", APIBackend: "responses", SupportsBackendSearchConfigured: true},
		{ChannelID: "deepseek-future", Host: "api.deepseek.com", WireModel: "deepseek-future-model", APIBackend: "responses"},
		{ChannelID: "relay", Host: "relay.example", WireModel: "deepseek-v4-pro", APIBackend: "responses"},
	}
	effective := app.resolveSearchRoutes(routes, config.WebSearchSelection{})
	if !effective[0].SupportsBackendSearch || effective[1].SupportsBackendSearch ||
		!effective[2].SupportsBackendSearch || effective[3].SupportsBackendSearch {
		t.Fatalf("DeepSeek provider search defaults were not scoped correctly: %+v", effective)
	}
	for index := range routes {
		if routes[index].SupportsBackendSearch {
			t.Fatalf("search default mutated input route %d: %+v", index, routes[index])
		}
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

func TestProjectBackendSearchOnlyForExplicitOrDocumentedDefaults(t *testing.T) {
	tests := []struct {
		name  string
		route config.Route
		want  bool
	}{
		{
			name:  "explicit generic capability",
			route: config.Route{SupportsBackendSearch: true, SupportsBackendSearchConfigured: true},
			want:  true,
		},
		{
			name:  "explicit generic disable",
			route: config.Route{SupportsBackendSearchConfigured: true},
			want:  true,
		},
		{
			name:  "documented V4 default",
			route: config.Route{Host: "api.deepseek.com", WireModel: "deepseek-v4-pro"},
			want:  true,
		},
		{
			name:  "future DeepSeek model gets endpoint default",
			route: config.Route{Host: "api.deepseek.com", WireModel: "deepseek-future-model"},
			want:  true,
		},
		{
			name:  "unconfigured generic model stays catalog owned",
			route: config.Route{SupportsBackendSearch: true},
		},
		{
			name:  "selected default search model",
			route: config.Route{SupportsBackendSearch: true, DefaultSearchModel: true},
			want:  true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := projectBackendSearch(test.route); got != test.want {
				t.Fatalf("project backend search = %t, want %t", got, test.want)
			}
		})
	}
}

func TestBuildDeepSeekReasoningEffortsFollowOfficialEndpointProtocol(t *testing.T) {
	tests := []struct {
		name  string
		route config.Route
		want  []string
	}{
		{name: "Responses", route: config.Route{APIBackend: "responses"}, want: []string{"none", "low", "high", "max"}},
		{name: "Chat", route: config.Route{APIBackend: "chat_completions"}, want: []string{"none", "low", "high", "max"}},
		{name: "Messages", route: config.Route{APIBackend: "messages"}, want: []string{"none", "low", "high", "max"}},
		{name: "DeepSeek default Chat search bridge", route: config.Route{APIBackend: "chat_completions", SupportsBackendSearch: true}, want: []string{"none", "low", "high", "max"}},
		{name: "explicit native Chat search", route: config.Route{APIBackend: "chat_completions", SupportsBackendSearch: true, ChatSearchDialect: config.ChatSearchDialectWebSearchOptions}, want: []string{"none", "low", "high", "max"}},
		{name: "explicit Messages search bridge", route: config.Route{APIBackend: "chat_completions", SupportsBackendSearch: true, ChatSearchDialect: config.ChatSearchDialectMessages}, want: []string{"none", "low", "high", "max"}},
		{name: "catalog resolved protocol", route: config.Route{}, want: []string{"none", "low", "high", "max"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.route.Host = "api.deepseek.com"
			test.route.WireModel = "deepseek-future-model"
			efforts := buildDeepSeekReasoningEfforts(test.route)
			if len(efforts) != len(test.want) {
				t.Fatalf("DeepSeek effort menu = %+v", efforts)
			}
			for index, want := range test.want {
				if efforts[index].Value != want || efforts[index].Label != strings.ToUpper(want[:1])+want[1:] {
					t.Fatalf("DeepSeek effort menu = %+v", efforts)
				}
			}
			if !efforts[len(efforts)-2].Default || efforts[len(efforts)-1].Default {
				t.Fatalf("DeepSeek effort default = %+v", efforts)
			}
		})
	}
	for _, route := range []config.Route{
		{Host: "relay.example", WireModel: "deepseek-v4-pro"},
		{Host: "api.deepseek.com", WireModel: "deepseek-v4-pro", ReasoningEffortConfigured: true},
	} {
		if efforts := buildDeepSeekReasoningEfforts(route); efforts != nil {
			t.Fatalf("non-first-party or explicitly configured route received an inferred effort menu: route=%+v efforts=%+v", route, efforts)
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
		{backend: "", configured: false, want: ""},
	} {
		route := config.Route{APIBackend: test.backend, SupportsBackendSearch: test.configured}
		if got := buildAPIBackend(route); got != test.want {
			t.Fatalf("backend=%s configured=%t build=%q want=%q", test.backend, test.configured, got, test.want)
		}
	}
}

func TestAppLeavesUnconfiguredAPIBackendToGrokBuildCatalog(t *testing.T) {
	dir := t.TempDir()
	grokHome := filepath.Join(dir, "grok")
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(grokHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_HOME", grokHome)
	configPath := filepath.Join(grokHome, "config.toml")
	original := "[model.future]\n" +
		"model = \"future-catalog-model\"\n" +
		"base_url = \"https://future.example/v1\"\n" +
		"api_key = \"test-key\"\n"
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
	model := string(patched)
	if !strings.Contains(model, `base_url = "http://127.0.0.1:18787/c/future"`) {
		t.Fatalf("future model was not routed through the facade:\n%s", model)
	}
	if strings.Contains(model, "api_backend") {
		t.Fatalf("unconfigured api_backend was pinned instead of remaining catalog-owned:\n%s", model)
	}
	if err := app.Stop(); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != original {
		t.Fatalf("catalog-owned config was not restored exactly\nwant: %q\ngot:  %q", original, restored)
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
		name                 string
		modelID              string
		modelsTOML           string
		environmentModel     string
		capability           string
		wantSearchProjection bool
	}{
		{name: "omitted backend search", modelID: "grok-missing-search-flag"},
		{
			name:                 "config-selected search model overrides explicit false",
			modelID:              "grok-config-search-model",
			modelsTOML:           "[models]\nweb_search = \"grok-config-search-model\"\n\n",
			capability:           "supports_backend_search = false\n",
			wantSearchProjection: true,
		},
		{
			name:                 "environment-selected search model overrides explicit false",
			modelID:              "grok-environment-search-model",
			environmentModel:     "grok-environment-search-model",
			capability:           "supports_backend_search = false\n",
			wantSearchProjection: true,
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
			t.Setenv("GROK_WEB_SEARCH_MODEL", test.environmentModel)
			configPath := filepath.Join(grokHome, "config.toml")
			original := test.modelsTOML + "[model." + test.modelID + "]\n" +
				"model = \"grok-4.5\"\n" +
				"base_url = \"" + upstream.URL + "/v1\"\n" +
				"api_key = \"test-key\"\n" + test.capability
			if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
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
			patched, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			projected := strings.Contains(string(patched), "supports_backend_search = true") &&
				strings.Contains(string(patched), `api_backend = "responses"`)
			if projected != test.wantSearchProjection {
				t.Fatalf("search projection=%t want=%t config:\n%s", projected, test.wantSearchProjection, patched)
			}
			if err := app.Stop(); err != nil {
				t.Fatal(err)
			}
			restored, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(restored) != original {
				t.Fatalf("config was not restored exactly\nwant: %q\ngot:  %q", original, restored)
			}
		})
	}
}
