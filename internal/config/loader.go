package config

import (
	"fmt"
	"os"
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
	var site Site
	if err := DecodeStrict(data, filename, &site); err != nil {
		return nil, err
	}
	Normalize(&site)
	return &site, nil
}
