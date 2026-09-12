package prefs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const settingsFileName = "settings.json"

const DefaultLogRetentionUsageDays = 7

type settings struct {
	ProxyEnabled          bool `json:"proxy_enabled"`
	LogRetentionUsageDays int  `json:"log_retention_usage_days"`
}

type storedSettings struct {
	ProxyEnabled          *bool `json:"proxy_enabled"`
	LogRetentionUsageDays *int  `json:"log_retention_usage_days"`
}

func Path(dataDir string) string {
	return filepath.Join(dataDir, settingsFileName)
}

// backupCorrupt preserves a settings file that failed to parse so the user can
// inspect it, while allowing defaults to take over.
func backupCorrupt(path string, raw []byte) {
	_ = os.WriteFile(path+".bad", raw, 0o600)
}

func ProxyEnabled(path string) (bool, error) {
	current, err := load(path)
	if err != nil {
		return false, err
	}
	return current.ProxyEnabled, nil
}

func SetProxyEnabled(path string, enabled bool) error {
	current, err := load(path)
	if err != nil {
		return err
	}
	current.ProxyEnabled = enabled
	return write(path, current)
}

func LogRetentionUsageDays(path string) (int, error) {
	current, err := load(path)
	if err != nil {
		return 0, err
	}
	return current.LogRetentionUsageDays, nil
}

func SetLogRetentionUsageDays(path string, days int) error {
	if !ValidLogRetentionUsageDays(days) {
		return fmt.Errorf("unsupported log retention usage days %d", days)
	}
	current, err := load(path)
	if err != nil {
		return err
	}
	current.LogRetentionUsageDays = days
	return write(path, current)
}

func ValidLogRetentionUsageDays(days int) bool {
	switch days {
	case 0, 3, 7, 14, 30:
		return true
	default:
		return false
	}
}

func load(path string) (settings, error) {
	current := settings{
		ProxyEnabled:          true,
		LogRetentionUsageDays: DefaultLogRetentionUsageDays,
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return current, nil
	}
	if err != nil {
		return settings{}, err
	}
	var stored storedSettings
	if err := json.Unmarshal(raw, &stored); err != nil {
		// A corrupt settings file must not deadlock every read and write:
		// back it up for inspection and fall back to defaults so the next
		// write rebuilds a valid file.
		backupCorrupt(path, raw)
		return current, nil
	}
	if stored.ProxyEnabled != nil {
		current.ProxyEnabled = *stored.ProxyEnabled
	}
	if stored.LogRetentionUsageDays != nil {
		if !ValidLogRetentionUsageDays(*stored.LogRetentionUsageDays) {
			return settings{}, fmt.Errorf("decode settings: unsupported log retention usage days %d", *stored.LogRetentionUsageDays)
		}
		current.LogRetentionUsageDays = *stored.LogRetentionUsageDays
	}
	return current, nil
}

func write(path string, current settings) error {
	raw, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".hellogrok-settings-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	committed = true
	return nil
}
