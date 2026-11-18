package config

import (
	"fmt"
	"os"
	"unicode/utf8"
)

// Load reads and validates the YAML structure of a site file. Semantic
// validation is performed separately by Validate so callers can combine it
// with template checks and present a complete report.
func Load(path string) (*Site, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件 %s: %w", path, err)
	}
	site, err := Decode(data, path)
	if err != nil {
		return nil, err
	}
	return site, nil
}

// Decode parses strict YAML and normalizes derived fields.
func Decode(data []byte, filename string) (*Site, error) {
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("%s: 配置文件包含非法 UTF-8 字节（第 %d 行附近），无法安全解析 YAML/PromQL；请用 UTF-8 编辑器重新保存",
			filename, firstInvalidUTF8Line(data))
	}
	var site Site
	if err := DecodeStrict(data, filename, &site); err != nil {
		return nil, err
	}
	Normalize(&site)
	return &site, nil
}

func firstInvalidUTF8Line(data []byte) int {
	line := 1
	for index := 0; index < len(data); {
		r, size := utf8.DecodeRune(data[index:])
		if r == utf8.RuneError && size == 1 {
			return line
		}
		if r == '\n' {
			line++
		}
		index += size
	}
	return line
}
