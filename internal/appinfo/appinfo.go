package appinfo

import (
	"os"
	"path/filepath"
	"runtime"
)

const (
	Name        = "hellogrok"
	Version     = "0.1.6"
	LogFileName = "hellogrok.log"
)

// DataDir is the single runtime-data location used by every package.
func DataDir() string {
	if runtime.GOOS == "windows" {
		if dir := os.Getenv("LOCALAPPDATA"); dir != "" {
			return filepath.Join(dir, Name)
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, "."+Name)
	}
	return filepath.Join(os.TempDir(), Name)
}

func LogPath() string {
	return filepath.Join(DataDir(), LogFileName)
}
