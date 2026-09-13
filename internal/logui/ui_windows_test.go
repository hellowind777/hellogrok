//go:build windows

package logui

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestEditStyleWrapsStatusButKeepsLogLinesUnwrapped(t *testing.T) {
	statusStyle := editStyle(true)
	if statusStyle&wsHScroll != 0 || statusStyle&esAutohscroll != 0 {
		t.Fatalf("wrapped status style enables horizontal scrolling: %#x", statusStyle)
	}
	logStyle := editStyle(false)
	if logStyle&wsHScroll == 0 || logStyle&esAutohscroll == 0 {
		t.Fatalf("unwrapped log style omits horizontal scrolling: %#x", logStyle)
	}
}

func TestUTF16LengthCountsSurrogatePairsAsTwo(t *testing.T) {
	if got := utf16Length("a😀b"); got != 4 {
		t.Fatalf("surrogate-pair selection offset = %d, want 4", got)
	}
}

func TestStatusPanelHeightPrioritizesLogArea(t *testing.T) {
	if got := statusPanelHeight(500); got != 170 {
		t.Fatalf("default status height = %d, want 170", got)
	}
	if got := statusPanelHeight(240); got != 80 {
		t.Fatalf("compact status height = %d, want 80", got)
	}
}

func TestMigrateLegacyWindowSize(t *testing.T) {
	legacy := migrateLegacyWindowSize(geometryJSON{W: legacyDefaultWinW, H: legacyDefaultWinH})
	if legacy.W != defaultWinW || legacy.H != defaultWinH {
		t.Fatalf("legacy window size = %dx%d, want %dx%d", legacy.W, legacy.H, defaultWinW, defaultWinH)
	}

	custom := geometryJSON{W: 960, H: 720}
	if got := migrateLegacyWindowSize(custom); got != custom {
		t.Fatalf("custom window size changed from %+v to %+v", custom, got)
	}
}

// stubOpenSeams replaces window construction and the build timeout so Open can
// be driven without a real window, and restores the shared open state after.
func stubOpenSeams(t *testing.T) {
	t.Helper()
	oldBuild, oldTimeout := buildWindowFn, openTimeout
	openMu.Lock()
	building = false
	openHWND = 0
	openMu.Unlock()
	t.Cleanup(func() {
		buildWindowFn = oldBuild
		openTimeout = oldTimeout
		openMu.Lock()
		building = false
		openHWND = 0
		openMu.Unlock()
	})
}

func TestOpenIgnoresSecondClickWhileWindowBuilds(t *testing.T) {
	stubOpenSeams(t)
	logPath := filepath.Join(t.TempDir(), "proxy.log")
	started := make(chan struct{})
	var startOnce sync.Once
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseBuild := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseBuild)
	var calls atomic.Int32
	buildWindowFn = func(string, StatusFunc, func() (int, error), func(int) error) (uintptr, *winState, error) {
		calls.Add(1)
		startOnce.Do(func() { close(started) })
		<-release
		return 0, nil, fmt.Errorf("test build aborted")
	}
	openTimeout = 10 * time.Second

	first := make(chan error, 1)
	go func() { first <- Open(logPath, nil, nil, nil) }()
	<-started

	done := make(chan error, 1)
	go func() { done <- Open(logPath, nil, nil, nil) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("second Open during build = %v, want nil", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("second Open during build blocked instead of being ignored")
	}
	if calls.Load() != 1 {
		t.Fatalf("build calls during debounce = %d, want 1", calls.Load())
	}

	releaseBuild()
	if err := <-first; err == nil || !strings.Contains(err.Error(), "test build aborted") {
		t.Fatalf("first Open error = %v, want test build aborted", err)
	}
}

func TestOpenRetriesAfterBuildFailure(t *testing.T) {
	stubOpenSeams(t)
	logPath := filepath.Join(t.TempDir(), "proxy.log")
	const testHWND = uintptr(0x1234)
	var calls atomic.Int32
	buildWindowFn = func(string, StatusFunc, func() (int, error), func(int) error) (uintptr, *winState, error) {
		if calls.Add(1) == 1 {
			return 0, nil, fmt.Errorf("test build failed")
		}
		openMu.Lock()
		openHWND = testHWND
		openMu.Unlock()
		return testHWND, &winState{}, nil
	}

	if err := Open(logPath, nil, nil, nil); err == nil || !strings.Contains(err.Error(), "test build failed") {
		t.Fatalf("first Open error = %v, want test build failed", err)
	}
	if err := Open(logPath, nil, nil, nil); err != nil {
		t.Fatalf("second Open after failure = %v, want nil", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("build calls after failure = %d, want 2", calls.Load())
	}
}

func TestOpenRetriesAfterBuildTimeout(t *testing.T) {
	stubOpenSeams(t)
	logPath := filepath.Join(t.TempDir(), "proxy.log")
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var calls atomic.Int32
	buildWindowFn = func(string, StatusFunc, func() (int, error), func(int) error) (uintptr, *winState, error) {
		calls.Add(1)
		<-release
		return 0, nil, fmt.Errorf("test build aborted")
	}
	openTimeout = 50 * time.Millisecond

	for attempt := 1; attempt <= 2; attempt++ {
		if err := Open(logPath, nil, nil, nil); err == nil || !strings.Contains(err.Error(), "超时") {
			t.Fatalf("Open attempt %d error = %v, want timeout", attempt, err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("build calls after timeout = %d, want 2", calls.Load())
	}
}
