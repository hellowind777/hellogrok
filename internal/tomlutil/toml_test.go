package tomlutil

import (
	"strings"
	"testing"
)

func TestUnmarshalAcceptsUTF8BOM(t *testing.T) {
	raw := append([]byte{0xEF, 0xBB, 0xBF}, []byte("[model.one]\nvalue = 1\n")...)
	var root map[string]any
	if err := Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	model, _ := root["model"].(map[string]any)
	one, _ := model["one"].(map[string]any)
	if got := one["value"]; got != int64(1) {
		t.Fatalf("value = %#v, want 1", got)
	}
}

func TestUnmarshalFileReportsPathAndPosition(t *testing.T) {
	path := `C:\Users\<user>\.grok\config.toml`
	var root map[string]any
	err := UnmarshalFile(path, []byte("[model.one]\nvalue =\n"), &root)
	if err == nil {
		t.Fatal("invalid TOML was accepted")
	}
	message := err.Error()
	for _, want := range []string{path, "第 2 行", "第 8 列"} {
		if !strings.Contains(message, want) {
			t.Fatalf("error %q does not contain %q", message, want)
		}
	}
}

func TestUnmarshalFileRejectsInvalidUTF8WithLocation(t *testing.T) {
	path := `C:\Users\<user>\.grok\config.toml`
	raw := append([]byte("[model.one]\nname = \""), 0xFF)
	raw = append(raw, []byte("\"\n")...)
	var root map[string]any
	err := UnmarshalFile(path, raw, &root)
	if err == nil {
		t.Fatal("invalid UTF-8 was accepted")
	}
	for _, want := range []string{path, "不是有效的 UTF-8", "第 2 行", "第 9 列"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
}
