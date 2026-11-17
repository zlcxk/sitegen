package config

import (
	"bytes"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"site-observability/internal/output"
)

// Marshal renders a site configuration with stable two-space indentation.
func Marshal(site *Site) ([]byte, error) {
	if site == nil {
		return nil, fmt.Errorf("站点配置为空")
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(site); err != nil {
		_ = enc.Close()
		return nil, fmt.Errorf("序列化站点配置: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("序列化站点配置: %w", err)
	}
	return buf.Bytes(), nil
}

// Write serializes the config and replaces the target atomically, retaining a
// <path>.bak copy when an existing file is present.
func Write(path string, site *Site, backup bool) error {
	data, err := Marshal(site)
	if err != nil {
		return err
	}
	if err := output.WriteAtomic(path, data, 0o644, backup); err != nil {
		return fmt.Errorf("写入站点配置 %s: %w", path, err)
	}
	return nil
}

// BackupPath returns the companion backup file path.
func BackupPath(path string) string { return path + ".bak" }

// Exists reports whether path exists.
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
