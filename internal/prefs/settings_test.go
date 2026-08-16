package prefs

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf8"
)

func TestProxyEnabledDefaultsTrueAndPersistsExplicitChoice(t *testing.T) {
	path := Path(t.TempDir())
	if enabled, err := ProxyEnabled(path); err != nil || !enabled {
		t.Fatalf("default enabled=%v err=%v", enabled, err)
	}
	if days, err := LogRetentionUsageDays(path); err != nil || days != DefaultLogRetentionUsageDays {
		t.Fatalf("default retention=%v err=%v", days, err)
	}
	if err := SetProxyEnabled(path, false); err != nil {
		t.Fatal(err)
	}
	if enabled, err := ProxyEnabled(path); err != nil || enabled {
		t.Fatalf("saved false enabled=%v err=%v", enabled, err)
	}
	if err := SetProxyEnabled(path, true); err != nil {
		t.Fatal(err)
	}
	if enabled, err := ProxyEnabled(path); err != nil || !enabled {
		t.Fatalf("saved true enabled=%v err=%v", enabled, err)
	}
	if err := SetLogRetentionUsageDays(path, 14); err != nil {
		t.Fatal(err)
	}
	if enabled, err := ProxyEnabled(path); err != nil || !enabled {
		t.Fatalf("retention update lost proxy state: enabled=%v err=%v", enabled, err)
	}
	if days, err := LogRetentionUsageDays(path); err != nil || days != 14 {
		t.Fatalf("saved retention=%v err=%v", days, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.Valid(raw) || bytes.HasPrefix(raw, []byte{0xEF, 0xBB, 0xBF}) {
		t.Fatal("settings file is not UTF-8 without BOM")
	}
}

func TestProxyEnabledRejectsCorruptSettings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, settingsFileName)
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ProxyEnabled(path); err == nil {
		t.Fatal("corrupt settings must not be ignored")
	}
}

func TestSettingsUpgradeKeepsLegacyProxyChoiceAndAddsRetentionDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), settingsFileName)
	if err := os.WriteFile(path, []byte("{\"proxy_enabled\":false}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if enabled, err := ProxyEnabled(path); err != nil || enabled {
		t.Fatalf("legacy proxy enabled=%v err=%v", enabled, err)
	}
	if days, err := LogRetentionUsageDays(path); err != nil || days != DefaultLogRetentionUsageDays {
		t.Fatalf("legacy retention=%v err=%v", days, err)
	}
	if err := SetLogRetentionUsageDays(path, 3); err != nil {
		t.Fatal(err)
	}
	if enabled, err := ProxyEnabled(path); err != nil || enabled {
		t.Fatalf("legacy false was not preserved: enabled=%v err=%v", enabled, err)
	}
}
