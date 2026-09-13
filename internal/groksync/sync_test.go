package groksync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type fakeCall struct {
	Method string
	Params any
}

type fakeRequester struct {
	responses map[string]json.RawMessage
	errors    map[string]error
	calls     []fakeCall
}

func (f *fakeRequester) Request(_ context.Context, _ string, method string, params, result any) error {
	f.calls = append(f.calls, fakeCall{Method: method, Params: params})
	if err := f.errors[method]; err != nil {
		return err
	}
	if result != nil {
		raw := f.responses[method]
		if err := json.Unmarshal(raw, result); err != nil {
			return err
		}
	}
	return nil
}

func TestRefreshClientReloadsOnlyIdleResidentTargetSessions(t *testing.T) {
	client := &fakeRequester{responses: map[string]json.RawMessage{
		reloadModelsMethod: json.RawMessage(`{"result":{"models":3}}`),
		listSessionsMethod: json.RawMessage(`{"result":{"sessions":[
			{"sessionId":"idle-target","modelId":"provider.v1-beta","activity":"idle","resident":true},
			{"sessionId":"working-target","modelId":"provider.v1-beta","activity":"working","resident":true},
			{"sessionId":"input-target","modelId":"provider-v2","activity":"needs_input","resident":true},
			{"sessionId":"dormant-target","modelId":"provider-v2","activity":"dormant","resident":false},
			{"sessionId":"idle-official","modelId":"grok-4.5","activity":"idle","resident":true}
		]}}`),
	}}
	targets := map[string]string{"provider.v1-beta": "provider.v1-beta", "provider-v2": "provider-v2"}
	result, err := refreshClient(context.Background(), client, targets)
	if err != nil {
		t.Fatal(err)
	}
	if result.TargetSessions != 3 || result.RefreshedSessions != 1 || result.SkippedActiveSessions != 2 || result.FailedSessions != 0 {
		t.Fatalf("result = %+v", result)
	}
	wantMethods := []string{initializeMethod, reloadModelsMethod, listSessionsMethod, setModelMethod}
	if len(client.calls) != len(wantMethods) {
		t.Fatalf("calls = %+v", client.calls)
	}
	for index, want := range wantMethods {
		if client.calls[index].Method != want {
			t.Fatalf("call %d method = %q, want %q", index, client.calls[index].Method, want)
		}
	}
	params, ok := client.calls[3].Params.(map[string]string)
	if !ok || params["sessionId"] != "idle-target" || params["modelId"] != "provider.v1-beta" {
		t.Fatalf("setModel params = %#v", client.calls[3].Params)
	}
}

func TestRefreshClientReportsMalformedRoster(t *testing.T) {
	client := &fakeRequester{responses: map[string]json.RawMessage{
		reloadModelsMethod: json.RawMessage(`{"result":{"models":1}}`),
		listSessionsMethod: json.RawMessage(`{"result":{"sessions":"invalid"}}`),
	}}
	_, err := refreshClient(context.Background(), client, map[string]string{"one": "one"})
	if err == nil || !strings.Contains(err.Error(), "list Grok sessions") {
		t.Fatalf("malformed roster error = %v", err)
	}
}

func TestRefreshClientMigratesLegacyUnquotedDottedModelID(t *testing.T) {
	client := &fakeRequester{responses: map[string]json.RawMessage{
		reloadModelsMethod: json.RawMessage(`{"result":{"models":1}}`),
		listSessionsMethod: json.RawMessage(`{"result":{"sessions":[{"sessionId":"legacy","modelId":"provider","activity":"idle","resident":true}]}}`),
	}}
	result, err := refreshClient(context.Background(), client, map[string]string{
		"provider":         "provider.v1-beta",
		"provider.v1-beta": "provider.v1-beta",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.RefreshedSessions != 1 || len(client.calls) != 4 {
		t.Fatalf("result=%+v calls=%+v", result, client.calls)
	}
	params, ok := client.calls[3].Params.(map[string]string)
	if !ok || params["modelId"] != "provider.v1-beta" {
		t.Fatalf("legacy setModel params = %#v", client.calls[3].Params)
	}
}

func TestRefreshClientContinuesAfterSessionFailure(t *testing.T) {
	client := &fakeRequester{
		responses: map[string]json.RawMessage{
			reloadModelsMethod: json.RawMessage(`{"result":{"models":1}}`),
			listSessionsMethod: json.RawMessage(`{"result":{"sessions":[{"sessionId":"one","modelId":"target","activity":"idle","resident":true}]}}`),
		},
		errors: map[string]error{setModelMethod: errors.New("session became active")},
	}
	result, err := refreshClient(context.Background(), client, map[string]string{"target": "target"})
	if err == nil || !strings.Contains(err.Error(), "session became active") {
		t.Fatalf("setModel error = %v", err)
	}
	if result.TargetSessions != 1 || result.FailedSessions != 1 || result.RefreshedSessions != 0 {
		t.Fatalf("result = %+v", result)
	}
}

func TestRefreshClientFallsBackToLegacySetModelMethod(t *testing.T) {
	client := &fakeRequester{
		responses: map[string]json.RawMessage{
			reloadModelsMethod: json.RawMessage(`{"result":{"models":1}}`),
			listSessionsMethod: json.RawMessage(`{"result":{"sessions":[{"sessionId":"one","modelId":"target","activity":"idle","resident":true}]}}`),
		},
		errors: map[string]error{setModelMethod: &rpcError{Code: -32601, Message: "Method not found"}},
	}
	result, err := refreshClient(context.Background(), client, map[string]string{"target": "target"})
	if err != nil {
		t.Fatal(err)
	}
	if result.RefreshedSessions != 1 || result.FailedSessions != 0 {
		t.Fatalf("result = %+v", result)
	}
	if len(client.calls) != 5 || client.calls[3].Method != setModelMethod || client.calls[4].Method != legacySetModel {
		t.Fatalf("calls = %+v", client.calls)
	}
}

func TestParseReachableLeadersFiltersStaleAndDeduplicatesSockets(t *testing.T) {
	raw := []byte(`[
		{"pid":1,"classification":"Stale","socketPath":null},
		{"pid":2,"classification":"Reachable","socketPath":"leader-a"},
		{"pid":3,"classification":"reachable","socketPath":"leader-a"},
		{"pid":4,"classification":"Reachable","socketPath":"leader-b"}
	]`)
	leaders, err := parseReachableLeaders(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(leaders) != 2 || leaders[0].PID != 2 || leaders[1].PID != 4 {
		t.Fatalf("leaders = %+v", leaders)
	}
	if _, err := parseReachableLeaders([]byte(`{"not":"an array"}`)); err == nil {
		t.Fatal("malformed leader list was accepted")
	}
}

func TestParseReachableLeadersKeepsLeadersWithoutStableKey(t *testing.T) {
	raw := []byte(`[
		{"pid":1,"classification":"Reachable","socketPath":null,"lockPath":null},
		{"pid":2,"classification":"Reachable","socketPath":null,"lockPath":null}
	]`)
	leaders, err := parseReachableLeaders(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(leaders) != 2 || leaders[0].PID != 1 || leaders[1].PID != 2 {
		t.Fatalf("leaders without socket path were collapsed: %+v", leaders)
	}
}

func TestParseReachableLeadersDeduplicatesLockPathFallback(t *testing.T) {
	raw := []byte(`[
		{"pid":1,"classification":"Reachable","lockPath":"lock-a"},
		{"pid":2,"classification":"Reachable","lockPath":"lock-a"},
		{"pid":3,"classification":"Reachable","lockPath":"lock-b"}
	]`)
	leaders, err := parseReachableLeaders(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(leaders) != 2 || leaders[0].PID != 1 || leaders[1].PID != 3 {
		t.Fatalf("leaders = %+v", leaders)
	}
}

func TestFindGrokCandidateSupportsFallbackAndMissingExecutable(t *testing.T) {
	dir := t.TempDir()
	candidate := filepath.Join(dir, "grok-test")
	if err := os.WriteFile(candidate, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	path, err := findGrokCandidate([]string{filepath.Join(dir, "missing"), candidate})
	if err != nil || path != candidate {
		t.Fatalf("path=%q err=%v", path, err)
	}
	_, err = findGrokCandidate([]string{filepath.Join(dir, "still-missing")})
	if !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("missing executable error = %v", err)
	}
}

func TestProcessClientMatchesResponseIDAndIgnoresNotifications(t *testing.T) {
	responses := strings.NewReader(
		`{"jsonrpc":"2.0","method":"_x.ai/models/update","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","id":"wanted","result":{"ok":true}}` + "\n",
	)
	var requests bytes.Buffer
	client := &processClient{
		encoder: json.NewEncoder(&requests),
		decoder: json.NewDecoder(responses),
	}
	var result struct {
		OK bool `json:"ok"`
	}
	if err := client.Request(context.Background(), "wanted", "test/method", map[string]any{}, &result); err != nil {
		t.Fatal(err)
	}
	if !result.OK || !strings.Contains(requests.String(), `"method":"test/method"`) {
		t.Fatalf("result=%+v request=%s", result, requests.String())
	}
}

func TestRefreshClientDecodesRealJSONRPCResultShape(t *testing.T) {
	responses := strings.NewReader(
		`{"jsonrpc":"2.0","id":"hellogrok-init","result":{}}` + "\n" +
			`{"jsonrpc":"2.0","id":"hellogrok-reload","result":{"result":{"models":1}}}` + "\n" +
			`{"jsonrpc":"2.0","id":"hellogrok-list","result":{"result":{"sessions":[{"sessionId":"live","modelId":"provider.v1-beta","activity":"idle","resident":true}]}}}` + "\n" +
			`{"jsonrpc":"2.0","id":"hellogrok-set-1","result":{}}` + "\n",
	)
	var requests bytes.Buffer
	client := &processClient{
		encoder: json.NewEncoder(&requests),
		decoder: json.NewDecoder(responses),
	}
	result, err := refreshClient(context.Background(), client, map[string]string{
		"provider.v1-beta": "provider.v1-beta",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.TargetSessions != 1 || result.RefreshedSessions != 1 {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(requests.String(), `"method":"session/setModel"`) {
		t.Fatalf("setModel request missing: %s", requests.String())
	}
}

func TestRefreshClientReportsExtensionEnvelopeErrors(t *testing.T) {
	client := &fakeRequester{responses: map[string]json.RawMessage{
		reloadModelsMethod: json.RawMessage(`{"result":null,"error":{"code":"reload_failed","message":"bad config"}}`),
	}}
	_, err := refreshClient(context.Background(), client, map[string]string{"one": "one"})
	if err == nil || !strings.Contains(err.Error(), "reload_failed: bad config") {
		t.Fatalf("reload extension error = %v", err)
	}

	client = &fakeRequester{responses: map[string]json.RawMessage{
		reloadModelsMethod: json.RawMessage(`{"result":{"models":1}}`),
		listSessionsMethod: json.RawMessage(`{"result":null,"error":"roster unavailable"}`),
	}}
	_, err = refreshClient(context.Background(), client, map[string]string{"one": "one"})
	if err == nil || !strings.Contains(err.Error(), "roster unavailable") {
		t.Fatalf("roster extension error = %v", err)
	}
}

func TestProcessClientRejectsMatchingResponseWithoutResult(t *testing.T) {
	var requests bytes.Buffer
	client := &processClient{
		encoder: json.NewEncoder(&requests),
		decoder: json.NewDecoder(strings.NewReader(`{"jsonrpc":"2.0","id":"wanted"}`)),
	}
	err := client.Request(context.Background(), "wanted", "test/method", map[string]any{}, nil)
	if err == nil || !strings.Contains(err.Error(), "has no result") {
		t.Fatalf("malformed response error = %v", err)
	}
}
