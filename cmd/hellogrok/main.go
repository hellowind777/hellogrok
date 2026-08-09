package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hellowind777/hellogrok/internal/appinfo"
	"github.com/hellowind777/hellogrok/internal/autostart"
	"github.com/hellowind777/hellogrok/internal/cfgpatch"
	"github.com/hellowind777/hellogrok/internal/config"
	"github.com/hellowind777/hellogrok/internal/console"
	"github.com/hellowind777/hellogrok/internal/dialog"
	"github.com/hellowind777/hellogrok/internal/groksync"
	"github.com/hellowind777/hellogrok/internal/logretention"
	"github.com/hellowind777/hellogrok/internal/logui"
	"github.com/hellowind777/hellogrok/internal/logview"
	"github.com/hellowind777/hellogrok/internal/openpath"
	"github.com/hellowind777/hellogrok/internal/prefs"
	"github.com/hellowind777/hellogrok/internal/proxy"
)

func main() {
	dataDir := appinfo.DataDir()
	logPath := appinfo.LogPath()

	if len(os.Args) > 1 && os.Args[1] == "logview" {
		_ = os.MkdirAll(dataDir, 0o700)
		_ = console.Show("hellogrok 日志")
		f, _ := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND, 0o600)
		if f != nil {
			_ = f.Close()
		}
		if err := logview.Run(logPath); err != nil {
			fmt.Fprintln(os.Stderr, "打开日志失败:", err)
			fmt.Println("按 Enter 退出...")
			_, _ = fmt.Scanln()
			os.Exit(1)
		}
		return
	}

	if len(os.Args) > 1 && os.Args[1] != "start" {
		runUtilityCommand(os.Args[1:], dataDir, logPath)
		return
	}
	if len(os.Args) == 1 && !hasDefaultUI {
		runDefault(nil, nil)
		return
	}
	if len(os.Args) == 1 {
		release, alreadyRunning, err := acquireDefaultInstance(dataDir)
		if err != nil {
			message := "无法确认 hellogrok 是否已在运行：\n" + err.Error()
			fmt.Fprintln(os.Stderr, message)
			dialog.Info("hellogrok 启动失败", message)
			return
		}
		if alreadyRunning {
			fmt.Fprintln(os.Stderr, "hellogrok is already running")
			return
		}
		defer release()
	}

	cli := len(os.Args) > 1
	_ = os.MkdirAll(dataDir, 0o700)
	retentionNotice := ""
	if days, retentionErr := prefs.LogRetentionUsageDays(prefs.Path(dataDir)); retentionErr != nil {
		retentionNotice = "load log retention: " + retentionErr.Error()
	} else if removed, retentionErr := logretention.Prune(logPath, days, time.Now()); retentionErr != nil {
		retentionNotice = "prune log history: " + retentionErr.Error()
	} else if removed > 0 {
		retentionNotice = fmt.Sprintf("log retention removed %d older usage day(s); keeping %d", removed, days)
	}

	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		if cli {
			log.Fatal(err)
		}
		lf = nil
	}

	var logWriter io.Writer
	if cli {
		if lf != nil {
			logWriter = io.MultiWriter(os.Stderr, lf)
		} else {
			logWriter = os.Stderr
		}
	} else {
		if lf != nil {
			logWriter = lf
		} else {
			logWriter = io.Discard
		}
		console.Hide()
	}
	if lf != nil {
		defer lf.Close()
	}

	logger := log.New(logWriter, "", log.LstdFlags|log.Lmsgprefix)
	logger.SetPrefix("[hellogrok] ")
	if retentionNotice != "" {
		logger.Printf("%s", retentionNotice)
	}

	app := &App{
		logger:              logger,
		logFile:             lf,
		dataDir:             dataDir,
		logPath:             logPath,
		server:              proxy.NewPersistent(logger, dataDir),
		refreshGrokSessions: groksync.Refresh,
	}

	logger.Printf("application ready")

	if cli {
		if err := runForeground(app, logger); err != nil {
			logger.Printf("foreground run failed: %v", err)
			os.Exit(1)
		}
		return
	}

	runDefaultWithSignals(app, logger)
}

func runDefaultWithSignals(app *App, logger *log.Logger) {
	sigc := make(chan os.Signal, 2)
	done := make(chan struct{})
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)
	go func() {
		for {
			select {
			case sig := <-sigc:
				logger.Printf("signal %s received; restoring config and stopping", sig)
				if err := app.Stop(); err != nil {
					logger.Printf("signal stop deferred: %v", err)
					continue
				}
				requestDefaultExit()
				return
			case <-done:
				return
			}
		}
	}()
	runDefault(app, logger)
	close(done)
	signal.Stop(sigc)
}

func runUtilityCommand(args []string, dataDir, logPath string) {
	switch args[0] {
	case "version", "-v", "--version":
		fmt.Printf("%s %s\n", appinfo.Name, appinfo.Version)
	case "restore":
		if err := ensureFacadeIdle(net.JoinHostPort(cfgpatch.ProxyHost, cfgpatch.ProxyPort)); err != nil {
			fmt.Fprintln(os.Stderr, "restore config:", err)
			os.Exit(1)
		}
		n, err := cfgpatch.Restore(config.ConfigPath(), cfgpatch.StatePath(dataDir))
		if err != nil {
			fmt.Fprintln(os.Stderr, "restore config:", err)
			os.Exit(1)
		}
		fmt.Printf("restored %d proxy-managed setting(s)\n", n)
	case "routes":
		models, err := config.LoadModels(config.ConfigPath())
		if err != nil {
			fmt.Fprintln(os.Stderr, "load config:", err)
			os.Exit(1)
		}
		routes, err := config.BuildRoutes(models)
		if err != nil {
			fmt.Fprintln(os.Stderr, "build routes:", err)
			os.Exit(1)
		}
		for _, route := range routes {
			fmt.Printf("channel=%s host=%s backend=%s model=%s backend_search=%t auth=%s\n",
				route.ChannelID, route.Host, route.APIBackend, route.WireModel, route.SupportsBackendSearch, routeAuthStatus(route))
		}
	case "log":
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			fmt.Fprintln(os.Stderr, "create data directory:", err)
			os.Exit(1)
		}
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			fmt.Fprintln(os.Stderr, "create log:", err)
			os.Exit(1)
		}
		_ = f.Close()
		fmt.Println(logPath)
		if err := openpath.Open(logPath); err != nil {
			fmt.Fprintln(os.Stderr, "open log:", err)
			os.Exit(1)
		}
	case "autostart":
		runAutostartCommand(args[1:])
	case "help", "-h", "--help":
		printUsage(os.Stdout)
	default:
		printUsage(os.Stderr)
		os.Exit(2)
	}
}

func ensureFacadeIdle(address string) error {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("local facade %s is active; stop it before restoring", address)
	}
	return listener.Close()
}

func routeAuthStatus(route config.Route) string {
	if strings.TrimSpace(route.APIKey) != "" {
		return "channel-owned"
	}
	for name, value := range route.ExtraHeaders {
		if (strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "X-Api-Key")) &&
			strings.TrimSpace(value) != "" {
			return "channel-owned"
		}
	}
	if route.DynamicAuth {
		return "auth-provider"
	}
	return "missing"
}

func runForeground(app *App, logger *log.Logger) error {
	if err := app.Start(); err != nil {
		return err
	}
	logger.Printf("running; status: %s", app.StatusDetail())

	// SIGINT/SIGTERM must restore base_url (no orphaned proxy URLs).
	sigc := make(chan os.Signal, 2)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigc)
	for {
		<-sigc
		logger.Printf("signal received; restoring config and stopping")
		if err := app.Stop(); err != nil {
			logger.Printf("stop deferred; resolve the configuration conflict and signal again: %v", err)
			continue
		}
		return nil
	}
}

func runAutostartCommand(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: hellogrok autostart [enable|disable|status]")
		os.Exit(2)
	}
	switch args[0] {
	case "enable":
		if err := setAutostart(true); err != nil {
			fmt.Fprintln(os.Stderr, "enable autostart:", err)
			os.Exit(1)
		}
		fmt.Println("autostart enabled")
	case "disable":
		if err := setAutostart(false); err != nil {
			fmt.Fprintln(os.Stderr, "disable autostart:", err)
			os.Exit(1)
		}
		fmt.Println("autostart disabled")
	case "status":
		if autostart.Enabled() {
			fmt.Println("enabled")
		} else {
			fmt.Println("disabled")
		}
	default:
		fmt.Fprintln(os.Stderr, "usage: hellogrok autostart [enable|disable|status]")
		os.Exit(2)
	}
}

func setAutostart(enabled bool) error {
	if hasDefaultUI {
		return autostart.SetUI(enabled)
	}
	return autostart.Set(enabled)
}

func printUsage(w io.Writer) {
	fmt.Fprintf(w, "hellogrok %s\n", appinfo.Version)
	fmt.Fprintln(w, "usage: hellogrok <command>")
	fmt.Fprintln(w, "  version               print the application version")
	fmt.Fprintln(w, "  start                 run proxy in foreground; Ctrl+C/SIGTERM restores config")
	fmt.Fprintln(w, "  restore               recover proxy-managed config after an unclean exit")
	fmt.Fprintln(w, "  routes                list configured upstream routes without credentials")
	fmt.Fprintln(w, "  autostart <action>    enable, disable, or inspect login autostart")
	fmt.Fprintln(w, "  log                   print and open the log file")
	fmt.Fprintln(w, "  logview               follow the log in the current terminal")
}

// App implements tray.Controller.
type App struct {
	logger              *log.Logger
	logFile             *os.File
	dataDir             string
	logPath             string
	server              *proxy.Server
	refreshGrokSessions func(context.Context, map[string]string) (groksync.Result, error)

	mu             sync.Mutex
	running        bool
	lastError      string
	patchedIDs     []string
	modelAliases   map[string]string
	grokSyncStatus string

	cfgMu  sync.Mutex
	prefMu sync.Mutex
}

func (a *App) beginSessionLog() {
	if a.logger != nil {
		a.logger.Printf("======== session start %s ========", time.Now().Format("2006-01-02 15:04:05"))
	}
}

func (a *App) IsRunning() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.running
}

func (a *App) StatusText() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.running {
		return "已停止"
	}
	return "运行中"
}

func (a *App) StatusDetail() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.running {
		if a.lastError != "" {
			return "【代理】 状态：已停止\n配置：未改写\n本地端口：未监听\n\n【上次错误】 " + a.lastError
		}
		return "【代理】 状态：已停止\n配置：未改写\n本地端口：未监听"
	}
	patched := append([]string(nil), a.patchedIDs...)
	sort.Strings(patched)
	list := "(无)"
	if len(patched) > 0 {
		list = strings.Join(patched, ", ")
	}
	hotSwitch := "未报告"
	if a.grokSyncStatus != "" {
		hotSwitch = a.grokSyncStatus
	}
	detail := "【代理】 本地入口：http://" + a.server.PathAddr + "/c/<渠道>/responses\n\n" +
		fmt.Sprintf("【渠道】 配置校验：已通过；数量：%d 个\n", len(patched)) +
		"列表：" + list + "\n\n" +
		"【Grok 会话】 热切换：" + hotSwitch + "\n\n" +
		"【协议与搜索】 Grok 消费：搜索开启时投影为 Responses\n" +
		"上游协议：保持渠道真实格式\n" +
		"搜索分流：开启走当前渠道，关闭走客户端搜索\n\n" +
		fmt.Sprintf("【配置恢复】 临时改写：%d 个渠道，停止代理：恢复原值", len(patched))
	if a.lastError != "" {
		detail += "\n\n【当前警告】 " + a.lastError
	}
	return detail
}

func (a *App) OpenMonitor() error {
	if a.logPath != "" {
		f, err := os.OpenFile(a.logPath, os.O_CREATE|os.O_APPEND, 0o600)
		if err == nil {
			_ = f.Close()
		}
	}
	return logui.Open(a.logPath, func() (string, string) {
		return a.StatusText(), a.StatusDetail()
	}, a.LogRetentionUsageDays, a.SetLogRetentionUsageDays)
}

func (a *App) Start() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running {
		return nil
	}
	a.lastError = ""

	cfgPath := config.ConfigPath()
	stPath := cfgpatch.StatePath(a.dataDir)

	// Own the facade address before touching config. A second instance must not
	// mistake the active instance's rewrite state for an orphan and restore it.
	if err := a.server.ReservePath(); err != nil {
		if addressInUse(err) {
			return a.abortStart(fmt.Errorf("本地代理端口 %s 已被占用，请关闭占用该端口的程序后重试", a.server.PathAddr))
		}
		return a.abortStart(fmt.Errorf("reserve local facade: %w", err))
	}
	a.beginSessionLog()

	if takeover, err := cfgpatch.DetectCCSwitchTakeover(cfgPath); err != nil {
		return a.abortStart(fmt.Errorf("inspect config ownership before start: %w", err))
	} else if takeover.Active() {
		return a.abortStart(ccSwitchConflictError(takeover, "启动 hellogrok"))
	}

	// Orphan recovery: a previous unclean exit may leave proxy URLs in config.
	// Recovery must succeed before loading routes or applying new changes.
	a.cfgMu.Lock()
	n, err := cfgpatch.Restore(cfgPath, stPath)
	a.cfgMu.Unlock()
	if err != nil {
		return a.abortStart(fmt.Errorf("restore config before start: %w", err))
	}
	if n > 0 {
		a.logger.Printf("orphan restore: %d proxy-managed setting(s) recovered before start", n)
	}

	// Load models AFTER orphan restore for auth/routes.
	models, err := config.LoadModels(cfgPath)
	if err != nil {
		return a.abortStart(fmt.Errorf("load models: %w", err))
	}
	for _, model := range models {
		if cfgpatch.IsProxyURL(model.BaseURL) || cfgpatch.IsProxyURL(model.APIBaseURL) {
			return a.abortStart(fmt.Errorf("model %q still points to the local facade but no restorable origin is available; restore the original custom URL before starting", model.ID))
		}
	}
	routes, err := config.BuildRoutes(models)
	if err != nil {
		return a.abortStart(fmt.Errorf("build routes: %w", err))
	}
	if len(routes) == 0 {
		return a.abortStart(fmt.Errorf("no explicit custom model endpoints found"))
	}
	searchSelection, err := config.LoadWebSearchSelection(cfgPath)
	if err != nil {
		return a.abortStart(fmt.Errorf("load web search model: %w", err))
	}
	routes = a.resolveSearchRoutes(routes, searchSelection)
	if takeover, err := cfgpatch.DetectCCSwitchTakeover(cfgPath); err != nil {
		return a.abortStart(fmt.Errorf("recheck config ownership before rewrite: %w", err))
	} else if takeover.Active() {
		return a.abortStart(ccSwitchConflictError(takeover, "启动 hellogrok"))
	}
	a.server.SetRoutes(routes)
	if err := a.server.ServePath(); err != nil {
		return a.abortStart(fmt.Errorf("start local facade: %w", err))
	}
	a.logger.Printf("channel facade on http://%s/c/<channel>/{responses|messages|chat/completions}", a.server.PathAddr)

	// Rewrite every explicit endpoint before Grok can load a direct URL. Waiting
	// for session discovery races the first request after a model switch.
	a.cfgMu.Lock()
	effectiveRoutes := make(map[string]config.Route, len(routes))
	for _, route := range routes {
		effectiveRoutes[route.ChannelID] = route
	}
	targets := make([]cfgpatch.Target, 0, len(models))
	for _, model := range models {
		if strings.TrimSpace(model.BaseURL) == "" && strings.TrimSpace(model.APIBaseURL) == "" {
			continue
		}
		route, ok := effectiveRoutes[model.ID]
		if !ok {
			continue
		}
		targets = append(targets, cfgpatch.Target{
			ID:                    model.ID,
			APIBaseURL:            strings.TrimSpace(model.APIBaseURL) != "",
			APIBackend:            route.APIBackend,
			BuildAPIBackend:       buildAPIBackend(route),
			SupportsBackendSearch: buildSupportsBackendSearch(route),
		})
	}
	res, err := cfgpatch.ApplyTargets(cfgPath, stPath, targets)
	a.cfgMu.Unlock()
	if err != nil {
		// ApplyTargets rolls back failures after its state file is committed.
		// Retry restoration here as a lifecycle-level fallback.
		a.cfgMu.Lock()
		_, restoreErr := cfgpatch.Restore(cfgPath, stPath)
		a.cfgMu.Unlock()
		if restoreErr != nil {
			err = fmt.Errorf("%w; fallback config rollback failed: %v", err, restoreErr)
		}
		return a.abortStart(fmt.Errorf("rewrite config: %w", err))
	}
	a.patchedIDs = append([]string(nil), res.Targets...)
	sort.Strings(a.patchedIDs)
	a.modelAliases = cloneStringMap(res.LegacyModelAliases)
	a.logger.Printf("config rewrite all: model_sections=%d base=%d api_base=%d api_backend=%d backend_search=%d backend_tools=%d web_fetch=%d subagents_enabled=%d targets=%v",
		res.ModelSections, res.BaseURLs, res.APIBaseURLs, res.APIBackends, res.BackendSearch, res.BackendTools, res.WebFetch, res.SubagentsEnabled, res.Targets)
	a.logger.Printf("config validation passed: backend_protocols=capability-projected backend_tools=true web_fetch=true backend_search=materialized subagent_defaults=repaired-if-needed targets=%d", res.ValidatedTargets)
	a.refreshOpenGrokSessions("enable", enableGrokSessionSelections(a.patchedIDs, a.modelAliases))
	for _, route := range routes {
		backend := strings.TrimSpace(route.APIBackend)
		a.logger.Printf("channel facade: model=%s build_backend=%s upstream_backend=%s", route.ChannelID, buildAPIBackend(route), backend)
		if route.SupportsBackendSearch {
			a.logger.Printf("channel search: model=%s supports_backend_search=true mode=hosted-current-channel source=config", route.ChannelID)
		} else {
			a.logger.Printf("channel search: model=%s supports_backend_search=false mode=client-web_search configured-model-or-authenticated-official-default", route.ChannelID)
		}
	}

	a.running = true
	a.logger.Printf("started path=%s mode=capability-projected-facade+search-adapter+auth-isolate", a.server.PathAddr)
	return nil
}

func addressInUse(err error) bool {
	// Windows reports WSAEADDRINUSE (10048), which is not represented by
	// syscall.EADDRINUSE in every Go Windows toolchain.
	return errors.Is(err, syscall.EADDRINUSE) || errors.Is(err, syscall.Errno(10048))
}

func (a *App) resolveSearchRoutes(
	routes []config.Route,
	selection config.WebSearchSelection,
) []config.Route {
	effective := append([]config.Route(nil), routes...)
	proxiedSearchModel := false
	for index := range effective {
		if effective[index].ChannelID == selection.Model {
			proxiedSearchModel = true
		}
	}
	if !selection.Explicit {
		a.logger.Printf("search routing: channel capabilities preserved; Messages/Chat compatibility uses a client web_search declaration promoted by the facade; startup_probe=disabled")
		return effective
	}
	a.logger.Printf("search routing: explicit client model=%q source=%s; channel capabilities preserved; startup_probe=disabled", selection.Model, selection.Source)
	switch {
	case selection.Model == "":
		a.logger.Printf("search model routing: explicit empty model disables a usable custom client-search route")
	case proxiedSearchModel:
		a.logger.Printf("search model routing: model=%s uses the local facade without startup validation", selection.Model)
	default:
		a.logger.Printf("search model routing: model=%s is not a proxied custom route; Build will resolve it directly", selection.Model)
	}
	return effective
}

// Grok Build serializes hosted_tools and consumes structured search results only
// on Responses. Capable channels therefore use the Responses facade while the
// route retains the provider's real upstream protocol.
func buildAPIBackend(route config.Route) string {
	if route.SupportsBackendSearch {
		return "responses"
	}
	backend := strings.ToLower(strings.TrimSpace(route.APIBackend))
	if backend == "" {
		return "chat_completions"
	}
	return backend
}

func buildSupportsBackendSearch(route config.Route) bool {
	return route.SupportsBackendSearch
}

func (a *App) abortStart(err error) error {
	a.server.Stop()
	a.server = proxy.NewPersistent(a.logger, a.dataDir)
	a.lastError = err.Error()
	return err
}

func (a *App) refreshOpenGrokSessions(phase string, selections map[string]string) {
	if a.refreshGrokSessions == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	result, err := a.refreshGrokSessions(ctx, selections)

	switch {
	case !result.GrokFound:
		a.grokSyncStatus = "未找到 grok 可执行文件；新窗口仍会读取配置，已打开窗口需在 /model 中重选当前模型"
	case result.ReachableLeaders == 0:
		a.grokSyncStatus = "未发现可连接的共享 leader；新窗口自动生效，--no-leader 窗口需在 /model 中重选当前模型"
	default:
		a.grokSyncStatus = fmt.Sprintf("已刷新 %d/%d 个空闲自定义模型会话", result.RefreshedSessions, result.TargetSessions)
		if result.SkippedActiveSessions > 0 {
			a.grokSyncStatus += fmt.Sprintf("，跳过 %d 个活动会话", result.SkippedActiveSessions)
		}
		if result.FailedSessions > 0 {
			a.grokSyncStatus += fmt.Sprintf("，%d 个会话刷新失败", result.FailedSessions)
		}
	}
	if err != nil {
		a.grokSyncStatus += "；刷新不完整: " + err.Error()
		a.logger.Printf("grok session hot reload phase=%s leaders=%d targets=%d refreshed=%d active_skipped=%d failed=%d error=%v",
			phase, result.ReachableLeaders, result.TargetSessions, result.RefreshedSessions, result.SkippedActiveSessions, result.FailedSessions, err)
		return
	}
	a.logger.Printf("grok session hot reload phase=%s leaders=%d targets=%d refreshed=%d active_skipped=%d failed=%d",
		phase, result.ReachableLeaders, result.TargetSessions, result.RefreshedSessions, result.SkippedActiveSessions, result.FailedSessions)
}

func enableGrokSessionSelections(targetIDs []string, legacyAliases map[string]string) map[string]string {
	selections := make(map[string]string, len(targetIDs)+len(legacyAliases))
	for _, id := range targetIDs {
		if id = strings.TrimSpace(id); id != "" {
			selections[id] = id
		}
	}
	for legacyID, targetID := range legacyAliases {
		legacyID = strings.TrimSpace(legacyID)
		targetID = strings.TrimSpace(targetID)
		if legacyID != "" && targetID != "" {
			selections[legacyID] = targetID
		}
	}
	return selections
}

func disableGrokSessionSelections(targetIDs []string, legacyAliases map[string]string) map[string]string {
	selections := make(map[string]string, len(targetIDs)+len(legacyAliases))
	for _, id := range targetIDs {
		if id = strings.TrimSpace(id); id != "" {
			selections[id] = id
		}
	}
	for legacyID, targetID := range legacyAliases {
		legacyID = strings.TrimSpace(legacyID)
		targetID = strings.TrimSpace(targetID)
		if legacyID == "" || targetID == "" {
			continue
		}
		selections[targetID] = legacyID
		selections[legacyID] = legacyID
	}
	return selections
}

func disableGrokSessionSelectionsForReferences(references []string, legacyAliases map[string]string) map[string]string {
	active := make(map[string]struct{}, len(references))
	for _, reference := range references {
		for _, suffix := range []string{".base_url", ".api_base_url"} {
			if id := strings.TrimSuffix(reference, suffix); id != reference && strings.TrimSpace(id) != "" {
				active[id] = struct{}{}
				break
			}
		}
	}
	targetIDs := make([]string, 0, len(active))
	for id := range active {
		targetIDs = append(targetIDs, id)
	}
	aliases := make(map[string]string)
	for legacyID, targetID := range legacyAliases {
		_, legacyActive := active[legacyID]
		_, targetActive := active[targetID]
		if legacyActive || targetActive {
			aliases[legacyID] = targetID
		}
	}
	return disableGrokSessionSelections(targetIDs, aliases)
}

func cloneStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	clone := make(map[string]string, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}

func (a *App) Stop() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.running {
		return nil
	}

	cfgPath := config.ConfigPath()
	stPath := cfgpatch.StatePath(a.dataDir)
	a.cfgMu.Lock()
	takeover, err := cfgpatch.DetectCCSwitchTakeover(cfgPath)
	if err == nil && takeover.Active() {
		err = ccSwitchConflictError(takeover, "停止 hellogrok")
	}
	if err != nil {
		a.cfgMu.Unlock()
		a.lastError = err.Error()
		a.logger.Printf("stop deferred while config has another owner: %v", err)
		return err
	}

	activeReferences, err := cfgpatch.ActiveProxyReferences(cfgPath)
	var n int
	if err == nil {
		n, err = cfgpatch.Restore(cfgPath, stPath)
	}
	relinquished := err == nil && len(activeReferences) == 0
	if err != nil {
		restoreErr := err
		// A provider manager may replace the whole live config while hellogrok is
		// running. If none of our local URLs survived, preserve that external
		// configuration and discard only the obsolete recovery transaction.
		var relinquishErr error
		relinquished, relinquishErr = cfgpatch.Relinquish(cfgPath, stPath)
		switch {
		case relinquishErr != nil:
			err = fmt.Errorf("%w; inspect remaining hellogrok routes: %v", restoreErr, relinquishErr)
		case !relinquished:
			err = restoreErr
		default:
			err = nil
		}
	}
	a.cfgMu.Unlock()
	if err != nil {
		a.lastError = err.Error()
		a.logger.Printf("config restore deferred; proxy remains active: %v", err)
		return err
	}
	if relinquished {
		a.logger.Printf("config ownership changed externally; no hellogrok routes remain, recovery state relinquished")
		a.refreshOpenGrokSessions("disable", map[string]string{})
	} else {
		a.logger.Printf("config restore: %d proxy-managed setting(s) restored", n)
		a.refreshOpenGrokSessions("disable", disableGrokSessionSelectionsForReferences(activeReferences, a.modelAliases))
	}

	a.server.Stop()
	a.server = proxy.NewPersistent(a.logger, a.dataDir)
	a.running = false
	a.patchedIDs = nil
	a.modelAliases = nil
	a.grokSyncStatus = ""
	a.lastError = ""
	a.logger.Printf("stopped")
	return nil
}

func ccSwitchConflictError(takeover cfgpatch.CCSwitchTakeover, action string) error {
	return fmt.Errorf(
		"检测到 CC Switch 正在接管 Grok Build（模型 %q，地址 %s）；两个工具会同时改写 config.toml。请先在 CC Switch 中关闭 Grok Build 的代理接管，再%s",
		takeover.ModelID,
		takeover.BaseURL,
		action,
	)
}

func (a *App) IsAutostart() bool         { return autostart.Enabled() }
func (a *App) SetAutostart(v bool) error { return autostart.SetUI(v) }

func (a *App) ProxyEnabledOnLaunch() (bool, error) {
	a.prefMu.Lock()
	defer a.prefMu.Unlock()
	return prefs.ProxyEnabled(prefs.Path(a.dataDir))
}

func (a *App) SetProxyEnabledOnLaunch(enabled bool) error {
	a.prefMu.Lock()
	defer a.prefMu.Unlock()
	return prefs.SetProxyEnabled(prefs.Path(a.dataDir), enabled)
}

func (a *App) LogRetentionUsageDays() (int, error) {
	a.prefMu.Lock()
	defer a.prefMu.Unlock()
	return prefs.LogRetentionUsageDays(prefs.Path(a.dataDir))
}

func (a *App) SetLogRetentionUsageDays(days int) error {
	a.prefMu.Lock()
	defer a.prefMu.Unlock()
	return prefs.SetLogRetentionUsageDays(prefs.Path(a.dataDir), days)
}
