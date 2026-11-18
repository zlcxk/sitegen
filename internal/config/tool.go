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
	Prometheus  PrometheusToolConfig `yaml:"prometheus"`
	Links       Links                `yaml:"links"`
	Datasources DatasourcesConfig    `yaml:"datasources"`
}

// PrometheusToolConfig configures the validation endpoint. TLS skip verify is
// deliberately false by default.
type PrometheusToolConfig struct {
	URL string `yaml:"url"`
	// SkywalkingURL optionally points at the Prometheus instance that scrapes
	// SkyWalking OAP. It is deliberately separate from the business endpoint.
	SkywalkingURL         string `yaml:"skywalkingUrl"`
	Timeout               string `yaml:"timeout"`
	TLSCAFile             string `yaml:"tlsCAFile"`
	TLSInsecureSkipVerify bool   `yaml:"tlsInsecureSkipVerify"`
}

// DatasourceConfig maps a logical datasource to a Grafana data source. UID may
// be empty; the generated dashboard then asks the user to select it at import
// time. UIDs are never hard-coded into templates.
type DatasourceConfig struct {
	UID  string `yaml:"uid"`
	Name string `yaml:"name,omitempty"`
	Type string `yaml:"type,omitempty"`
}

// DatasourcesConfig separates business metrics, SkyWalking's Prometheus, and
// the SkyWalking native datasource.
type DatasourcesConfig struct {
	BusinessPrometheus   DatasourceConfig `yaml:"businessPrometheus"`
	SkywalkingPrometheus DatasourceConfig `yaml:"skywalkingPrometheus"`
	Skywalking           DatasourceConfig `yaml:"skywalking"`
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
	cfg.Prometheus.SkywalkingURL = strings.TrimSpace(cfg.Prometheus.SkywalkingURL)
	cfg.Prometheus.Timeout = strings.TrimSpace(cfg.Prometheus.Timeout)
	cfg.Prometheus.TLSCAFile = strings.TrimSpace(cfg.Prometheus.TLSCAFile)
	cfg.Links.Skywalking = strings.TrimSpace(cfg.Links.Skywalking)
	cfg.Links.Prometheus = strings.TrimSpace(cfg.Links.Prometheus)
	cfg.Links.Grafana = strings.TrimSpace(cfg.Links.Grafana)
	cfg.Links.SkywalkingServiceURLTemplate = strings.TrimSpace(cfg.Links.SkywalkingServiceURLTemplate)
	cfg.Links.SkywalkingEndpointURLTemplate = strings.TrimSpace(cfg.Links.SkywalkingEndpointURLTemplate)
	cfg.Links.SkywalkingInstanceDashboard = strings.TrimSpace(cfg.Links.SkywalkingInstanceDashboard)
	cfg.Links.SkywalkingEndpointDashboard = strings.TrimSpace(cfg.Links.SkywalkingEndpointDashboard)
	cfg.Links.SkywalkingTraceDashboard = strings.TrimSpace(cfg.Links.SkywalkingTraceDashboard)
	cfg.Links.SkywalkingServiceDashboard = strings.TrimSpace(cfg.Links.SkywalkingServiceDashboard)
	for i := range cfg.Links.Dashboards {
		cfg.Links.Dashboards[i].Title = strings.TrimSpace(cfg.Links.Dashboards[i].Title)
		cfg.Links.Dashboards[i].URL = strings.TrimSpace(cfg.Links.Dashboards[i].URL)
	}
	cfg.Datasources.BusinessPrometheus = normalizeDatasource(cfg.Datasources.BusinessPrometheus)
	cfg.Datasources.SkywalkingPrometheus = normalizeDatasource(cfg.Datasources.SkywalkingPrometheus)
	cfg.Datasources.Skywalking = normalizeDatasource(cfg.Datasources.Skywalking)
}

func normalizeDatasource(ds DatasourceConfig) DatasourceConfig {
	ds.UID = strings.TrimSpace(ds.UID)
	ds.Name = strings.TrimSpace(ds.Name)
	ds.Type = strings.TrimSpace(ds.Type)
	return ds
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
	if cfg.Prometheus.SkywalkingURL != "" {
		if err := validateHTTPURL(cfg.Prometheus.SkywalkingURL, true); err != nil {
			add(SeverityError, "prometheus.skywalkingUrl", "%v", err)
		}
	}
	validateDatasource := func(path string, ds DatasourceConfig, expectedType string) {
		if ds.Type != "" && ds.Type != expectedType {
			add(SeverityError, path+".type", "必须为 %q", expectedType)
		}
		if ds.UID != "" && !uidRE.MatchString(ds.UID) {
			add(SeverityError, path+".uid", "Grafana 数据源 UID 非法（1-40 位字母、数字、下划线或连字符）")
		}
	}
	validateDatasource("datasources.businessPrometheus", cfg.Datasources.BusinessPrometheus, "prometheus")
	validateDatasource("datasources.skywalkingPrometheus", cfg.Datasources.SkywalkingPrometheus, "prometheus")
	validateDatasource("datasources.skywalking", cfg.Datasources.Skywalking, "apache-skywalking-datasource")
	for _, item := range []struct {
		path string
		raw  string
	}{
		{"links.skywalkingInstanceDashboard", cfg.Links.SkywalkingInstanceDashboard},
		{"links.skywalkingEndpointDashboard", cfg.Links.SkywalkingEndpointDashboard},
		{"links.skywalkingTraceDashboard", cfg.Links.SkywalkingTraceDashboard},
		{"links.skywalkingServiceDashboard", cfg.Links.SkywalkingServiceDashboard},
	} {
		if item.raw == "" {
			continue
		}
		if strings.HasPrefix(item.raw, "d/") || strings.HasPrefix(item.raw, "/d/") {
			continue
		}
		if err := validateHTTPURL(item.raw, true); err != nil {
			add(SeverityError, item.path, "%v", err)
		}
	}
	for index, dashboard := range cfg.Links.Dashboards {
		path := fmt.Sprintf("links.dashboards[%d]", index)
		if dashboard.Title == "" {
			add(SeverityError, path+".title", "不能为空")
		}
		if dashboard.URL == "" {
			add(SeverityError, path+".url", "不能为空")
			continue
		}
		if strings.HasPrefix(dashboard.URL, "d/") || strings.HasPrefix(dashboard.URL, "/d/") {
			continue
		}
		if err := validateHTTPURL(dashboard.URL, true); err != nil {
			add(SeverityError, path+".url", "%v", err)
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
