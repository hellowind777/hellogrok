package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hellowind777/hellogrok/internal/config"
)

// Opt-in: uses the configured provider and may incur inference charges.
func TestLiveProviderReadTool(t *testing.T) {
	channel := os.Getenv("HELLOGROK_LIVE_CHANNEL")
	if channel == "" {
		t.Skip("set HELLOGROK_LIVE_CHANNEL to test a configured provider")
	}
	configPath := os.Getenv("HELLOGROK_LIVE_CONFIG")
	if configPath == "" {
		t.Fatal("set HELLOGROK_LIVE_CONFIG to the provider configuration")
	}
	models, err := config.LoadModels(configPath)
	if err != nil {
		t.Fatal("cannot load model configuration")
	}
	routes, err := config.BuildRoutes(models)
	if err != nil {
		t.Fatal("cannot build routes")
	}
	var selected *config.Route
	for i := range routes {
		if routes[i].ChannelID == channel {
			selected = &routes[i]
			break
		}
	}
	if selected == nil {
		t.Fatal("requested channel not found")
	}
	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{*selected})
	startPathTestServer(t, s)
	home := t.TempDir()
	work := t.TempDir()
	token := "ACCEPTANCE_" + compatID("read")
	if err := os.WriteFile(filepath.Join(work, "probe.txt"), []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("[models]\ndefault = %q\n[model.%q]\nmodel = %q\nbase_url = %q\napi_key = \"test-key\"\napi_backend = %q\n", channel, channel, channel, "http://"+s.PathAddr+"/c/"+channel, selected.APIBackend)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "grok", "-p", "Read probe.txt using the read_file tool. Return exactly the file contents. Do not guess.", "-m", channel, "--output-format", "json", "--no-subagents", "--disable-web-search", "--tools", "read_file", "--disallowed-tools", "search_tool,use_tool,workflow,run_terminal_cmd", "--permission-mode", "bypassPermissions", "--max-turns", "3", "--verbatim")
	cmd.Dir = work
	cmd.Env = append(os.Environ(), "GROK_HOME="+home, "HOME="+home, "USERPROFILE="+home)
	output, err := cmd.CombinedOutput()
	if err != nil {
		detail := string(output)
		if selected.APIKey != "" {
			detail = strings.ReplaceAll(detail, selected.APIKey, "[redacted]")
		}
		detail = regexp.MustCompile(`https?://[^\s"<>]+`).ReplaceAllString(detail, "[endpoint]")
		detail = regexp.MustCompile(`(?i)(bearer\s+|sk-)[a-z0-9._-]+`).ReplaceAllString(detail, "[redacted]")
		t.Fatalf("Grok process failed: %v\n%s", err, detail)
	}
	var result grokHeadlessResult
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatal("invalid Grok JSON output")
	}
	if !strings.Contains(result.Text, token) {
		t.Fatal("model did not return the unpredictable file contents")
	}
	t.Log("real provider read the isolated file and returned its unpredictable contents through the proxy")
}
