//go:build windows || tray

package tray

import (
	"errors"
	"testing"
)

func TestStopAndQuitAlwaysQuitsWhenCleanupFails(t *testing.T) {
	want := errors.New("cleanup failed")
	quit := false
	err := stopAndQuit(func() error { return want }, func() { quit = true })
	if !errors.Is(err, want) {
		t.Fatalf("stop error = %v, want %v", err, want)
	}
	if !quit {
		t.Fatal("quit was not called after cleanup failed")
	}
}

func TestStopAndQuitQuitsAfterSuccessfulCleanup(t *testing.T) {
	quit := false
	if err := stopAndQuit(func() error { return nil }, func() { quit = true }); err != nil {
		t.Fatal(err)
	}
	if !quit {
		t.Fatal("quit was not called after cleanup succeeded")
	}
}
