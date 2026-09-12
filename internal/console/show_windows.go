//go:build windows

package console

import (
	"os"
	"syscall"
	"unsafe"
)

// Show allocates a console window and binds stdio (for log viewer).
func Show(title string) error {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	alloc := kernel32.NewProc("AllocConsole")
	setTitle := kernel32.NewProc("SetConsoleTitleW")

	if _, _, err := alloc.Call(); err != nil && err != syscall.ERROR_ACCESS_DENIED {
		// ERROR_ACCESS_DENIED means a console already exists, which is fine.
		return err
	}
	if title != "" {
		t, err := syscall.UTF16PtrFromString(title)
		if err == nil {
			_, _, _ = setTitle.Call(uintptr(unsafe.Pointer(t)))
		}
	}

	// Bind Go stdio to the new console devices
	out, err := os.OpenFile("CONOUT$", os.O_RDWR, 0)
	if err != nil {
		return err
	}
	os.Stdout = out
	os.Stderr = out
	in, err := os.OpenFile("CONIN$", os.O_RDWR, 0)
	if err != nil {
		return err
	}
	os.Stdin = in
	return nil
}
