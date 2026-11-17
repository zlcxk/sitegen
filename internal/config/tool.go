package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ToolConfig holds process-level settings that are independent of any site.
// Credentials are intentionally absent and must come from environment
// variables so they cannot be committed alongside site YAML.
type ToolConfig struct {
	Prometheus PrometheusToolConfig `yaml:"prometheus"`
	Links      Links                `yaml:"links"`
}

// PrometheusToolConfig configures the validation endpoint. TLS skip verify is
// deliberately false by default.
type PrometheusToolConfig struct {
	URL                   string `yaml:"url"`
	Timeout               string `yaml:"timeout"`
	TLSCAFile             string `yaml:"tlsCAFile"`
	TLSInsecureSkipVerify bool   `yaml:"tlsInsecureSkipVerify"`
}

// LoadToolConfig loads an explicit config path, or discovers one from
// SITEGEN_CONFIG, ./sitegen.yaml, or ~/.sitegen/config.yaml. A missing
// discovered file is not an error; an explicitly supplied missing file is.
func LoadToolConfig(explicitPath string) (*ToolConfig, string, error) {
	path, required := explicitPath, explicitPath != ""
	if path == "" {
		path = os.Getenv("SITEGEN_CONFIG")
		required = path != ""
	}
	if path == "" {
		if Exists("sitegen.yaml") {
			path = "sitegen.yaml"
		}
	}
	if path == "" {
		if home, err := os.UserHomeDir(); err == nil {
			candidate := filepath.Join(home, ".sitegen", "config.yaml")
			if Exists(candidate) {
				path = candidate
			}
		}
	}
	if path == "" {
		return &ToolConfig{}, "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !required && errors.Is(err, os.ErrNotExist) {
			return &ToolConfig{}, "", nil
		}
		return nil, path, fmt.Errorf("读取工具配置 %s: %w", path, err)
	}
	var cfg ToolConfig
	if err := DecodeStrict(data, path, &cfg); err != nil {
		return nil, path, err
	}
	NormalizeToolConfig(&cfg)
	return &cfg, path, nil
}

// NormalizeToolConfig trims scalar settings.
func NormalizeToolConfig(cfg *ToolConfig) {
	if cfg == nil {
		return
	}
	cfg.Prometheus.URL = strings.TrimSpace(cfg.Prometheus.URL)
	cfg.Prometheus.Timeout = strings.TrimSpace(cfg.Prometheus.Timeout)
	cfg.Prometheus.TLSCAFile = strings.TrimSpace(cfg.Prometheus.TLSCAFile)
	cfg.Links.Skywalking = strings.TrimSpace(cfg.Links.Skywalking)
	cfg.Links.Prometheus = strings.TrimSpace(cfg.Links.Prometheus)
	cfg.Links.Grafana = strings.TrimSpace(cfg.Links.Grafana)
	cfg.Links.SkywalkingServiceURLTemplate = strings.TrimSpace(cfg.Links.SkywalkingServiceURLTemplate)
	cfg.Links.SkywalkingEndpointURLTemplate = strings.TrimSpace(cfg.Links.SkywalkingEndpointURLTemplate)
}

// ValidateToolConfig validates non-secret settings. It is called before checks
// so invalid URLs fail with a field path rather than an opaque HTTP error.
func ValidateToolConfig(cfg *ToolConfig) Issues {
	var issues Issues
	if cfg == nil {
		return issues
	}
	add := func(severity Severity, path, format string, args ...any) {
		issues = append(issues, Issue{Severity: severity, Path: path, Message: fmt.Sprintf(format, args...)})
	}
	cfg.Links.Validate("links", add)
	if cfg.Prometheus.URL != "" {
		if err := validateHTTPURL(cfg.Prometheus.URL, true); err != nil {
			add(SeverityError, "prometheus.url", "%v", err)
		}
	}
	if cfg.Prometheus.Timeout != "" {
		if _, err := ParseTimeout(cfg.Prometheus.Timeout); err != nil {
			add(SeverityError, "prometheus.timeout", "%v", err)
		}
	}
	if cfg.Prometheus.TLSCAFile != "" {
		if _, err := os.Stat(cfg.Prometheus.TLSCAFile); err != nil {
			add(SeverityError, "prometheus.tlsCAFile", "无法读取 CA 文件: %v", err)
		}
	}
	return issues
}

// ParseTimeout parses a duration and applies the 15 second default.
func ParseTimeout(raw string) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return 15 * time.Second, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("必须使用 Go duration 格式（例如 15s、1m）")
	}
	if d <= 0 {
		return 0, fmt.Errorf("必须大于 0")
	}
	if d > 5*time.Minute {
		return 0, fmt.Errorf("不能超过 5 分钟")
	}
	return d, nil
}
