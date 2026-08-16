//go:build windows

package cfgpatch

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestFailedRuntimeApplyKeepsPreviousActiveTransaction(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := []byte("[model.one]\nbase_url = \"https://one.example/v1\"\nauto_compact_threshold_percent = 90\n")
	if err := os.WriteFile(configPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	firstThreshold := uint8(58)
	if _, err := ApplyTargets(configPath, statePath, []Target{{
		ID: "one", ContextWindow: 1_048_576, AutoCompactThresholdPercent: &firstThreshold,
	}}); err != nil {
		t.Fatal(err)
	}
	firstConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	firstState, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}

	path, err := windows.UTF16PtrFromString(configPath)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(
		path,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = windows.CloseHandle(handle)
		}
	})

	secondThreshold := uint8(82)
	if _, err := ApplyTargets(configPath, statePath, []Target{{
		ID: "one", ContextWindow: 524_288, AutoCompactThresholdPercent: &secondThreshold,
	}}); err == nil {
		t.Fatal("runtime update succeeded while Windows denied atomic replacement")
	}
	currentConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	currentState, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(currentConfig, firstConfig) {
		t.Fatalf("failed update changed the active config\nwant: %q\ngot:  %q", firstConfig, currentConfig)
	}
	if !bytes.Equal(currentState, firstState) {
		t.Fatalf("failed update changed the recovery state\nwant: %q\ngot:  %q", firstState, currentState)
	}
	if err := windows.CloseHandle(handle); err != nil {
		t.Fatal(err)
	}
	closed = true
	if _, err := Restore(configPath, statePath); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored, original) {
		t.Fatalf("original config was not restored\nwant: %q\ngot:  %q", original, restored)
	}
}
