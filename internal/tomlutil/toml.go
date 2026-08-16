package tomlutil

import (
	"bytes"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/pelletier/go-toml/v2"
)

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// StripUTF8BOM returns TOML content without an optional UTF-8 byte-order mark.
func StripUTF8BOM(raw []byte) []byte {
	return bytes.TrimPrefix(raw, utf8BOM)
}

// PreserveUTF8BOM restores the source file's optional UTF-8 byte-order mark.
func PreserveUTF8BOM(source, content []byte) []byte {
	if !bytes.HasPrefix(source, utf8BOM) {
		return content
	}
	withBOM := make([]byte, 0, len(utf8BOM)+len(content))
	withBOM = append(withBOM, utf8BOM...)
	return append(withBOM, content...)
}

// Unmarshal accepts both ordinary UTF-8 and UTF-8 with a byte-order mark.
func Unmarshal(raw []byte, target any) error {
	raw = StripUTF8BOM(raw)
	if offset := firstInvalidUTF8(raw); offset >= 0 {
		line, column := bytePosition(raw, offset)
		return fmt.Errorf("内容不是有效的 UTF-8（第 %d 行，第 %d 列，字节偏移 %d）", line, column, offset)
	}
	return toml.Unmarshal(raw, target)
}

// UnmarshalFile includes the file and parser position in user-facing errors.
func UnmarshalFile(path string, raw []byte, target any) error {
	err := Unmarshal(raw, target)
	if err == nil {
		return nil
	}
	var decodeErr *toml.DecodeError
	if errors.As(err, &decodeErr) {
		line, column := decodeErr.Position()
		return fmt.Errorf("TOML 配置文件 %s 解析失败（第 %d 行，第 %d 列）：%w", path, line, column, err)
	}
	return fmt.Errorf("TOML 配置文件 %s 解析失败：%w", path, err)
}

func firstInvalidUTF8(raw []byte) int {
	for offset := 0; offset < len(raw); {
		_, size := utf8.DecodeRune(raw[offset:])
		if size == 1 && raw[offset] >= utf8.RuneSelf {
			return offset
		}
		offset += size
	}
	return -1
}

func bytePosition(raw []byte, offset int) (int, int) {
	line, column := 1, 1
	for index := 0; index < offset; {
		if raw[index] == '\n' {
			line++
			column = 1
			index++
			continue
		}
		_, size := utf8.DecodeRune(raw[index:])
		if size == 0 {
			size = 1
		}
		index += size
		column++
	}
	return line, column
}
