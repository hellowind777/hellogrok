package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hellowind777/hellogrok/internal/config"
)

type grokHeadlessResult struct {
	Text      string `json:"text"`
	SessionID string `json:"sessionId"`
}

// This test launches the installed Grok Build executable with an isolated home
// and a local provider. Keep it opt-in so normal repository tests do not depend
// on a separately installed binary.
func TestGrokBuildProcessRetriesContextBudget(t *testing.T) {
	if os.Getenv("HELLOGROK_GROK_E2E") != "1" {
		t.Skip("set HELLOGROK_GROK_E2E=1 to run the installed Grok Build process test")
	}
	grokPath, err := exec.LookPath("grok")
	if err != nil {
		t.Skipf("grok executable is not installed: %v", err)
	}

	var mu sync.Mutex
	limits := make([]uint64, 0, 4)
	rejected := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		defer request.Body.Close()
		body, _ := io.ReadAll(request.Body)
		root, decodeErr := decodeRequestObject(body)
		if decodeErr != nil {
			t.Errorf("decode Grok upstream request: %v", decodeErr)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		limit, _ := positiveJSONUint64(root["max_output_tokens"])
		mu.Lock()
		limits = append(limits, limit)
		shouldReject := limit == 384000 && !rejected
		if shouldReject {
			rejected = true
		}
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if shouldReject {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `{"error":{"message":"This model's maximum context length is 1048576 tokens. However, you requested 1048712 tokens (664712 in the messages, 384000 in the completion). Please reduce the length of the messages or completion.","type":"invalid_request_error"}}`)
			return
		}
		_, _ = fmt.Fprint(w, nativeSuccessBody("responses", "wire-model", "OK"))
	}))
	defer upstream.Close()

	parsedUpstream, _ := url.Parse(upstream.URL)
	route := config.Route{
		ChannelID:            "context-e2e",
		Host:                 parsedUpstream.Host,
		OriginBase:           upstream.URL,
		APIBackend:           "responses",
		APIBackendConfigured: true,
		WireModel:            "wire-model",
		APIKey:               "test-key",
		AuthScheme:           "bearer",
		IncomingAuthScheme:   "bearer",
	}
	server := New(log.New(io.Discard, "", 0))
	server.SetRoutes([]config.Route{route})
	startPathTestServer(t, server)

	grokHome := filepath.Join(t.TempDir(), "grok-home")
	if err := os.MkdirAll(grokHome, 0o755); err != nil {
		t.Fatal(err)
	}
	configText := fmt.Sprintf(`[models]
default = "context-e2e"

[model.context-e2e]
model = "context-e2e"
base_url = "http://%s/c/context-e2e"
api_key = "test-key"
api_backend = "responses"
context_window = 1048576
max_completion_tokens = 384000
reasoning_efforts = ["none", "low", "high", "max"]
reasoning_effort = "high"
`, server.PathAddr)
	if err := os.WriteFile(filepath.Join(grokHome, "config.toml"), []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, grokPath,
		"-p", "Reply exactly OK", "-m", "context-e2e", "--output-format", "json",
		"--no-subagents", "--disable-web-search", "--max-turns", "1",
		"--permission-mode", "bypassPermissions", "--verbatim")
	command.Dir = t.TempDir()
	command.Env = append(os.Environ(),
		"GROK_HOME="+grokHome,
		"HOME="+grokHome,
		"USERPROFILE="+grokHome,
	)
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("Grok Build process timed out: %v\n%s", ctx.Err(), output)
	}
	if err != nil {
		t.Fatalf("Grok Build process failed: %v\n%s", err, output)
	}
	mu.Lock()
	gotLimits := append([]uint64(nil), limits...)
	mu.Unlock()
	foundRetry := false
	for index := 0; index+1 < len(gotLimits); index++ {
		if gotLimits[index] == 384000 && gotLimits[index+1] == 383864 {
			foundRetry = true
			break
		}
	}
	if !foundRetry {
		t.Fatalf("provider completion limits = %v, output=%s", gotLimits, output)
	}
	if !strings.Contains(string(output), "OK") || strings.Contains(string(output), "maximum context length") {
		t.Fatalf("unexpected Grok Build output: %s", output)
	}
}

func TestGrokBuildProcessCompactsAtNextPromptAllProtocols(t *testing.T) {
	if os.Getenv("HELLOGROK_GROK_E2E") != "1" {
		t.Skip("set HELLOGROK_GROK_E2E=1 to run the installed Grok Build process test")
	}
	grokPath, err := exec.LookPath("grok")
	if err != nil {
		t.Skipf("grok executable is not installed: %v", err)
	}

	for _, backend := range []string{"responses", "chat_completions", "messages"} {
		t.Run(backend, func(t *testing.T) {
			var mu sync.Mutex
			requests := make([]string, 0, 4)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				defer request.Body.Close()
				body, _ := io.ReadAll(request.Body)
				if bytes.Contains(body, []byte("generating the session title")) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprint(w, grokProcessUsageBody(backend, "wire-model", "Context threshold test", 10, 1))
					return
				}
				mu.Lock()
				requests = append(requests, string(body))
				call := len(requests)
				mu.Unlock()

				text := "SECOND"
				inputTokens := uint64(100)
				if call == 1 {
					text = "FIRST"
					inputTokens = 90_000
				} else if call == 2 {
					text = "COMPACTED SUMMARY"
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, grokProcessUsageBody(backend, "wire-model", text, inputTokens, 1))
			}))
			defer upstream.Close()

			parsedUpstream, _ := url.Parse(upstream.URL)
			route := config.Route{
				ChannelID:            "compact-e2e-" + backend,
				Host:                 parsedUpstream.Host,
				OriginBase:           upstream.URL,
				APIBackend:           backend,
				APIBackendConfigured: true,
				WireModel:            "wire-model",
				APIKey:               "test-key",
				AuthScheme:           "bearer",
				IncomingAuthScheme:   "bearer",
			}
			server := New(log.New(io.Discard, "", 0))
			server.SetRoutes([]config.Route{route})
			startPathTestServer(t, server)

			grokHome := filepath.Join(t.TempDir(), "grok-home")
			if err := os.MkdirAll(grokHome, 0o755); err != nil {
				t.Fatal(err)
			}
			configText := fmt.Sprintf(`[models]
default = %q

[model.%s]
model = %q
base_url = "http://%s/c/%s"
api_key = "test-key"
api_backend = %q
context_window = 100000
max_completion_tokens = 4096
supports_backend_search = false
supports_reasoning_effort = false
`, route.ChannelID, route.ChannelID, route.ChannelID, server.PathAddr, route.ChannelID, backend)
			if err := os.WriteFile(filepath.Join(grokHome, "config.toml"), []byte(configText), 0o600); err != nil {
				t.Fatal(err)
			}
			workDir := t.TempDir()
			env := append(os.Environ(),
				"GROK_HOME="+grokHome,
				"HOME="+grokHome,
				"USERPROFILE="+grokHome,
			)

			first := runGrokHeadless(t, grokPath, workDir, env, "", "Reply exactly FIRST")
			if first.SessionID == "" {
				t.Fatal("first Grok process did not return a session ID")
			}
			if first.Text != "FIRST" {
				t.Fatalf("first Grok response = %q, want FIRST", first.Text)
			}

			second := runGrokHeadless(t, grokPath, workDir, env, first.SessionID, "Reply exactly SECOND")
			if second.SessionID != first.SessionID {
				t.Fatalf("resumed session ID = %q, want %q", second.SessionID, first.SessionID)
			}

			mu.Lock()
			gotRequests := append([]string(nil), requests...)
			mu.Unlock()
			if len(gotRequests) < 3 {
				t.Fatalf("upstream requests = %d, want first turn + compaction + resumed turn; second response=%q",
					len(gotRequests), second.Text)
			}
			if second.Text != "SECOND" {
				t.Fatalf("resumed Grok response = %q, want SECOND", second.Text)
			}
			resumedRequest := ""
			for _, requestBody := range gotRequests[1:] {
				if strings.Contains(requestBody, "Reply exactly SECOND") {
					resumedRequest = requestBody
					break
				}
			}
			if resumedRequest == "" {
				t.Fatalf("no post-compaction request contained resumed user input: %#v", gotRequests)
			}
		})
	}
}

func TestGrokBuildProcessResumePreservesCustomChannelIdentity(t *testing.T) {
	if os.Getenv("HELLOGROK_GROK_E2E") != "1" {
		t.Skip("set HELLOGROK_GROK_E2E=1 to run the installed Grok Build process test")
	}
	grokPath, err := exec.LookPath("grok")
	if err != nil {
		t.Skipf("grok executable is not installed: %v", err)
	}

	var mu sync.Mutex
	paths := make([]string, 0, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		mu.Lock()
		paths = append(paths, request.URL.Path)
		mu.Unlock()
		text := "FIRST"
		if bytes.Contains(body, []byte("Reply exactly SECOND")) {
			text = "SECOND"
		} else if bytes.Contains(body, []byte("generating the session title")) {
			text = "Resume identity"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, nativeSuccessBody("responses", "grok-4.6", text))
	}))
	defer upstream.Close()

	parsedUpstream, _ := url.Parse(upstream.URL)
	routes := []config.Route{
		{
			ChannelID: "grok-resume-a", Host: parsedUpstream.Host, OriginBase: upstream.URL + "/a/v1",
			APIBackend: "responses", APIBackendConfigured: true, WireModel: "grok-4.6",
			APIKey: "test-key-a", AuthScheme: "bearer", IncomingAuthScheme: "bearer",
		},
		{
			ChannelID: "grok-resume-b", Host: parsedUpstream.Host, OriginBase: upstream.URL + "/b/v1",
			APIBackend: "responses", APIBackendConfigured: true, WireModel: "grok-4.6",
			APIKey: "test-key-b", AuthScheme: "bearer", IncomingAuthScheme: "bearer",
		},
	}
	server := New(log.New(io.Discard, "", 0))
	server.SetRoutes(routes)
	startPathTestServer(t, server)

	grokHome := filepath.Join(t.TempDir(), "grok-home")
	if err := os.MkdirAll(grokHome, 0o755); err != nil {
		t.Fatal(err)
	}
	configText := fmt.Sprintf(`[models]
default = "grok-resume-b"

[model.grok-4.6]
model = "grok-4.6"
base_url = "http://127.0.0.1:1"
api_key = "unused-test-key"
api_backend = "responses"

[model.grok-resume-a]
model = "grok-resume-a"
base_url = "http://%s/c/grok-resume-a"
api_key = "test-key-a"
api_backend = "responses"

[model.grok-resume-b]
model = "grok-resume-b"
base_url = "http://%s/c/grok-resume-b"
api_key = "test-key-b"
api_backend = "responses"
`, server.PathAddr, server.PathAddr)
	if err := os.WriteFile(filepath.Join(grokHome, "config.toml"), []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(),
		"GROK_HOME="+grokHome,
		"HOME="+grokHome,
		"USERPROFILE="+grokHome,
	)
	workDir := t.TempDir()
	first := runGrokHeadlessWithModel(t, grokPath, workDir, env, "", "grok-resume-b", "Reply exactly FIRST")
	if first.SessionID == "" || first.Text != "FIRST" {
		t.Fatalf("first result = %+v", first)
	}
	second := runGrokHeadlessWithModel(t, grokPath, workDir, env, first.SessionID, "", "Reply exactly SECOND")
	if second.SessionID != first.SessionID || second.Text != "SECOND" {
		t.Fatalf("resumed result = %+v, first = %+v", second, first)
	}

	mu.Lock()
	gotPaths := append([]string(nil), paths...)
	mu.Unlock()
	if len(gotPaths) < 2 {
		t.Fatalf("upstream paths = %v", gotPaths)
	}
	for _, path := range gotPaths {
		if !strings.HasPrefix(path, "/b/v1/") {
			t.Fatalf("resumed session left the selected channel: %v", gotPaths)
		}
	}
}

func runGrokHeadless(t *testing.T, grokPath, workDir string, env []string, sessionID, prompt string) grokHeadlessResult {
	return runGrokHeadlessWithModel(t, grokPath, workDir, env, sessionID, "", prompt)
}

func runGrokHeadlessWithModel(t *testing.T, grokPath, workDir string, env []string, sessionID, model, prompt string) grokHeadlessResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	args := []string{
		"-p", prompt, "--output-format", "json", "--no-subagents", "--disable-web-search",
		"--max-turns", "1", "--permission-mode", "bypassPermissions", "--verbatim",
	}
	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	} else if model != "" {
		args = append(args, "-m", model)
	}
	command := exec.CommandContext(ctx, grokPath, args...)
	command.Dir = workDir
	command.Env = env
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("Grok Build process timed out: %v\n%s", ctx.Err(), output)
	}
	if err != nil {
		t.Fatalf("Grok Build process failed: %v\n%s", err, output)
	}
	var result grokHeadlessResult
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode Grok Build output: %v\n%s", err, output)
	}
	return result
}

func grokProcessUsageBody(backend, model, text string, inputTokens, outputTokens uint64) string {
	total := inputTokens + outputTokens
	switch backend {
	case "responses":
		return fmt.Sprintf(`{"id":"resp_e2e","object":"response","status":"completed","model":%q,"output":[{"type":"message","id":"msg_e2e","status":"completed","role":"assistant","content":[{"type":"output_text","text":%q,"annotations":[]}]}],"usage":{"input_tokens":%d,"output_tokens":%d,"total_tokens":%d,"context_details":{"input_tokens":%d,"output_tokens":%d}}}`, model, text, inputTokens, outputTokens, total, inputTokens, outputTokens)
	case "messages":
		return fmt.Sprintf(`{"id":"msg_e2e","type":"message","role":"assistant","content":[{"type":"text","text":%q}],"model":%q,"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":%d,"output_tokens":%d}}`, text, model, inputTokens, outputTokens)
	default:
		return fmt.Sprintf(`{"id":"chat_e2e","object":"chat.completion","created":1,"model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`, model, text, inputTokens, outputTokens, total)
	}
}
