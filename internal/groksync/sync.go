package groksync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/hellowind777/hellogrok/internal/appinfo"
)

const (
	initializeMethod   = "initialize"
	reloadModelsMethod = "_x.ai/internal/reload_models"
	listSessionsMethod = "_x.ai/sessions/list"
	setModelMethod     = "session/setModel"
	legacySetModel     = "session/set_model"
)

// Result describes the shared-leader sessions affected by a config reload.
// Non-resident and non-target sessions are deliberately excluded.
type Result struct {
	GrokFound             bool
	ReachableLeaders      int
	TargetSessions        int
	RefreshedSessions     int
	SkippedActiveSessions int
	FailedSessions        int
}

// Refresh reloads Grok Build's model catalog and makes idle shared-leader
// sessions reselect their current custom model. Reselecting is required because
// the config watcher updates the catalog but does not replace a live session's
// sampling URL, backend, or credentials.
func Refresh(ctx context.Context, modelSelections map[string]string) (Result, error) {
	result := Result{}
	grokPath, err := findGrok()
	if err != nil {
		return result, err
	}
	result.GrokFound = true

	raw, err := runCommand(ctx, grokPath, "leader", "list", "--json")
	if err != nil {
		return result, fmt.Errorf("list Grok leaders: %w", err)
	}
	leaders, err := parseReachableLeaders(raw)
	if err != nil {
		return result, err
	}
	result.ReachableLeaders = len(leaders)
	if len(leaders) == 0 {
		return result, nil
	}

	targets := make(map[string]string, len(modelSelections))
	for currentID, desiredID := range modelSelections {
		currentID = strings.TrimSpace(currentID)
		desiredID = strings.TrimSpace(desiredID)
		if currentID != "" && desiredID != "" {
			targets[currentID] = desiredID
		}
	}

	var refreshErrors []error
	for _, leader := range leaders {
		client, err := openProcessClient(ctx, grokPath, leader)
		if err != nil {
			refreshErrors = append(refreshErrors, err)
			continue
		}
		leaderResult, refreshErr := refreshClient(ctx, client, targets)
		closeErr := client.Close()
		result.TargetSessions += leaderResult.TargetSessions
		result.RefreshedSessions += leaderResult.RefreshedSessions
		result.SkippedActiveSessions += leaderResult.SkippedActiveSessions
		result.FailedSessions += leaderResult.FailedSessions
		if refreshErr != nil {
			refreshErrors = append(refreshErrors, refreshErr)
		}
		if closeErr != nil && ctx.Err() == nil {
			refreshErrors = append(refreshErrors, closeErr)
		}
	}
	return result, errors.Join(refreshErrors...)
}

type leaderInfo struct {
	PID            int     `json:"pid"`
	Classification string  `json:"classification"`
	SocketPath     *string `json:"socketPath"`
	LockPath       *string `json:"lockPath"`
}

func parseReachableLeaders(raw []byte) ([]leaderInfo, error) {
	var all []leaderInfo
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil, fmt.Errorf("decode Grok leader list: %w", err)
	}
	candidates := make([]leaderInfo, 0, len(all))
	for _, leader := range all {
		if strings.EqualFold(strings.TrimSpace(leader.Classification), "reachable") {
			candidates = append(candidates, leader)
		}
	}
	candidates = append(candidates, supplementalLeaderCandidates(all)...)

	seen := map[string]bool{}
	var reachable []leaderInfo
	for _, leader := range candidates {
		key := ""
		if leader.SocketPath != nil {
			key = strings.TrimSpace(*leader.SocketPath)
		}
		if key == "" && leader.LockPath != nil {
			key = "lock:" + strings.TrimSpace(*leader.LockPath)
		}
		if key == "" {
			// No stable identifier to deduplicate on; collapsing unidentified
			// leaders into one would silently skip reachable sessions.
			reachable = append(reachable, leader)
			continue
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		reachable = append(reachable, leader)
	}
	return reachable, nil
}

type session struct {
	SessionID string `json:"sessionId"`
	ModelID   string `json:"modelId"`
	Activity  string `json:"activity"`
	Resident  bool   `json:"resident"`
}

type rosterResult struct {
	Sessions []session `json:"sessions"`
}

type extensionResult[T any] struct {
	Result *T              `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func (r extensionResult[T]) value(method string) (*T, error) {
	if len(r.Error) > 0 && string(r.Error) != "null" {
		var message string
		if err := json.Unmarshal(r.Error, &message); err != nil {
			var detail struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}
			if json.Unmarshal(r.Error, &detail) == nil && strings.TrimSpace(detail.Message) != "" {
				message = strings.TrimSpace(detail.Message)
				if strings.TrimSpace(detail.Code) != "" {
					message = strings.TrimSpace(detail.Code) + ": " + message
				}
			} else {
				message = string(r.Error)
			}
		}
		return nil, fmt.Errorf("%s extension error: %s", method, message)
	}
	if r.Result == nil {
		return nil, fmt.Errorf("%s extension response has no result", method)
	}
	return r.Result, nil
}

type requester interface {
	Request(context.Context, string, string, any, any) error
}

func refreshClient(ctx context.Context, client requester, targets map[string]string) (Result, error) {
	result := Result{GrokFound: true, ReachableLeaders: 1}
	initialize := map[string]any{
		"protocolVersion": 1,
		"clientCapabilities": map[string]any{
			"fs": map[string]bool{
				"readTextFile":  false,
				"writeTextFile": false,
			},
			"terminal": false,
		},
		"_meta": map[string]any{
			"startupHints": map[string]bool{
				"nonInteractive":    true,
				"skipGitStatus":     true,
				"skipProjectLayout": true,
			},
			"clientType":    "hellogrok",
			"clientVersion": appinfo.Version,
		},
	}
	if err := client.Request(ctx, "hellogrok-init", initializeMethod, initialize, nil); err != nil {
		return result, fmt.Errorf("initialize Grok leader bridge: %w", err)
	}
	var reload extensionResult[struct {
		Models int `json:"models"`
	}]
	if err := client.Request(ctx, "hellogrok-reload", reloadModelsMethod, map[string]any{}, &reload); err != nil {
		return result, fmt.Errorf("reload Grok model catalog: %w", err)
	}
	if _, err := reload.value(reloadModelsMethod); err != nil {
		return result, fmt.Errorf("reload Grok model catalog: %w", err)
	}
	var rosterEnvelope extensionResult[rosterResult]
	if err := client.Request(ctx, "hellogrok-list", listSessionsMethod, map[string]any{}, &rosterEnvelope); err != nil {
		return result, fmt.Errorf("list Grok sessions: %w", err)
	}
	roster, err := rosterEnvelope.value(listSessionsMethod)
	if err != nil {
		return result, fmt.Errorf("list Grok sessions: %w", err)
	}

	var sessionErrors []error
	requestIndex := 0
	for _, current := range roster.Sessions {
		if !current.Resident {
			continue
		}
		modelID := strings.TrimSpace(current.ModelID)
		desiredModelID, target := targets[modelID]
		if !target {
			continue
		}
		result.TargetSessions++
		if !strings.EqualFold(strings.TrimSpace(current.Activity), "idle") {
			result.SkippedActiveSessions++
			continue
		}
		requestIndex++
		params := map[string]string{"sessionId": current.SessionID, "modelId": desiredModelID}
		id := fmt.Sprintf("hellogrok-set-%d", requestIndex)
		err := client.Request(ctx, id, setModelMethod, params, nil)
		if isMethodNotFound(err) {
			err = client.Request(ctx, id+"-legacy", legacySetModel, params, nil)
		}
		if err != nil {
			result.FailedSessions++
			sessionErrors = append(sessionErrors, fmt.Errorf("refresh session %q model %q as %q: %w", current.SessionID, modelID, desiredModelID, err))
			continue
		}
		result.RefreshedSessions++
	}
	return result, errors.Join(sessionErrors...)
}

func findGrok() (string, error) {
	if path, err := exec.LookPath("grok"); err == nil {
		return path, nil
	}
	var candidates []string
	name := "grok"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if grokHome := strings.TrimSpace(os.Getenv("GROK_HOME")); grokHome != "" {
		candidates = append(candidates, filepath.Join(grokHome, "bin", name))
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".grok", "bin", name))
	}
	return findGrokCandidate(candidates)
}

func findGrokCandidate(candidates []string) (string, error) {
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("locate Grok executable: %w", exec.ErrNotFound)
}

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	configureCommand(cmd)
	output, err := cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(output))
		if detail != "" {
			return nil, fmt.Errorf("%w: %s", err, detail)
		}
		return nil, err
	}
	return output, nil
}

type processClient struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	encoder *json.Encoder
	decoder *json.Decoder
	stderr  bytes.Buffer
}

func openProcessClient(ctx context.Context, grokPath string, leader leaderInfo) (*processClient, error) {
	args := []string{"agent", "--leader"}
	if leader.SocketPath != nil && strings.TrimSpace(*leader.SocketPath) != "" {
		args = append(args, "--leader-socket", strings.TrimSpace(*leader.SocketPath))
	}
	args = append(args, "stdio")
	cmd := exec.CommandContext(ctx, grokPath, args...)
	configureCommand(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open Grok bridge stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("open Grok bridge stdout: %w", err)
	}
	client := &processClient{cmd: cmd, stdin: stdin}
	client.encoder = json.NewEncoder(stdin)
	client.decoder = json.NewDecoder(stdout)
	cmd.Stderr = &client.stderr
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("start Grok leader bridge: %w", err)
	}
	return client, nil
}

type rpcResponse struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func isMethodNotFound(err error) bool {
	var rpcErr *rpcError
	return errors.As(err, &rpcErr) && rpcErr.Code == -32601
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("JSON-RPC error %d: %s", e.Code, strings.TrimSpace(e.Message))
}

func (c *processClient) Request(ctx context.Context, id, method string, params, result any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	request := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}
	if err := c.encoder.Encode(request); err != nil {
		return fmt.Errorf("write %s request: %w", method, err)
	}
	for {
		var response rpcResponse
		if err := c.decoder.Decode(&response); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("read %s response: %w", method, err)
		}
		var responseID string
		if json.Unmarshal(response.ID, &responseID) != nil || responseID != id {
			continue
		}
		if response.Error != nil {
			return response.Error
		}
		if len(response.Result) == 0 {
			return fmt.Errorf("%s response has no result", method)
		}
		if result != nil && string(response.Result) != "null" {
			if err := json.Unmarshal(response.Result, result); err != nil {
				return fmt.Errorf("decode %s result: %w", method, err)
			}
		}
		return nil
	}
}

func (c *processClient) Close() error {
	_ = c.stdin.Close()
	done := make(chan error, 1)
	go func() { done <- c.cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			return nil
		}
		detail := strings.TrimSpace(c.stderr.String())
		if detail != "" {
			return fmt.Errorf("Grok leader bridge exited: %w: %s", err, detail)
		}
		return fmt.Errorf("Grok leader bridge exited: %w", err)
	case <-time.After(time.Second):
		_ = c.cmd.Process.Kill()
		<-done
		return nil
	}
}
