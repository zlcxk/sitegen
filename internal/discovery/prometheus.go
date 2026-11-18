package discovery

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/promql/parser"
)

// Config describes a Prometheus connection. Credentials must be supplied by
// the caller from environment variables, never from site YAML.
type Config struct {
	URL                   string
	Timeout               time.Duration
	Username              string
	Password              string
	BearerToken           string
	TLSCAFile             string
	TLSInsecureSkipVerify bool
	// AllowPath permits a non-root base path, e.g. a Grafana datasource proxy
	// (/api/datasources/uid/<uid>/resources). The business endpoint keeps the
	// strict root-only policy.
	AllowPath bool
}

// Client is a small, timeout-aware Prometheus API wrapper.
type Client struct {
	api     promv1.API
	timeout time.Duration
}

// NewClient validates the endpoint and builds an authenticated client. Requests
// to any host other than the configured endpoint are rejected, so credentials
// cannot be forwarded through a cross-host redirect.
func NewClient(cfg Config) (*Client, error) {
	if err := validateEndpoint(cfg.URL, cfg.AllowPath); err != nil {
		return nil, err
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 15 * time.Second
	}
	if cfg.Username != "" || cfg.Password != "" {
		if cfg.Username == "" || cfg.Password == "" {
			return nil, fmt.Errorf("Basic Auth 必须同时设置用户名和密码")
		}
		if cfg.BearerToken != "" {
			return nil, fmt.Errorf("Basic Auth 与 Bearer Token 不能同时配置")
		}
	}

	tlsConfig := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: cfg.TLSInsecureSkipVerify, // explicitly configured, default false
	}
	if cfg.TLSCAFile != "" {
		caPEM, err := os.ReadFile(cfg.TLSCAFile)
		if err != nil {
			return nil, fmt.Errorf("读取 TLS CA 文件失败: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("TLS CA 文件不包含有效的 PEM 证书")
		}
		tlsConfig.RootCAs = pool
	}

	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		base = &http.Transport{}
	}
	transport := base.Clone()
	transport.TLSClientConfig = tlsConfig
	transport.MaxIdleConnsPerHost = 8

	parsed, _ := url.Parse(cfg.URL)
	authRT := &authTransport{
		base:        transport,
		user:        cfg.Username,
		pass:        cfg.Password,
		bearer:      cfg.BearerToken,
		allowedHost: parsed.Host,
	}

	client, err := api.NewClient(api.Config{
		Address:      strings.TrimRight(cfg.URL, "/"),
		RoundTripper: authRT,
	})
	if err != nil {
		return nil, fmt.Errorf("创建 Prometheus 客户端: %w", err)
	}
	return &Client{api: promv1.NewAPI(client), timeout: cfg.Timeout}, nil
}

// Ping confirms that the query API is reachable and authorized.
func (c *Client) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	value, warnings, _, err := c.api.Query(ctx, "vector(1)", time.Now(), promv1.WithTimeout(c.timeout))
	if err != nil {
		return fmt.Errorf("Prometheus 查询失败: %w", err)
	}
	if len(warnings) > 0 {
		// Warnings are intentionally not echoed: they can contain query text.
	}
	vector, ok := value.(model.Vector)
	if !ok || len(vector) == 0 {
		return fmt.Errorf("Prometheus 连通性检查返回了意外结果")
	}
	return nil
}

// Series returns series matching a selector within the given window.
func (c *Client) Series(ctx context.Context, match string, start, end time.Time) ([]model.LabelSet, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	series, _, _, err := c.api.Series(ctx, []string{match}, start, end, promv1.WithTimeout(c.timeout))
	if err != nil {
		return nil, fmt.Errorf("Prometheus series 查询失败: %w", err)
	}
	return series, nil
}

// MetricExists checks the metric name without loading samples. It uses the
// label-values API and falls back to the series API for older Prometheus
// versions that do not support match[] on label values.
func (c *Client) MetricExists(ctx context.Context, metric string, start, end time.Time) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	values, _, _, err := c.api.LabelValues(ctx, "__name__", []string{metric}, start, end, promv1.WithTimeout(c.timeout))
	if err == nil {
		for _, value := range values {
			if string(value) == metric {
				return true, nil
			}
		}
		return false, nil
	}

	series, _, _, seriesErr := c.api.Series(ctx, []string{metric}, start, end, promv1.WithTimeout(c.timeout))
	if seriesErr != nil {
		return false, fmt.Errorf("Prometheus 指标存在性查询失败: %v；series 回退失败: %w", err, seriesErr)
	}
	for _, labels := range series {
		if string(labels[model.LabelName("__name__")]) == metric {
			return true, nil
		}
	}
	return false, nil
}

// Query executes an instant query. Empty results are not errors.
func (c *Client) Query(ctx context.Context, expr string) (model.Value, error) {
	return c.QueryAt(ctx, expr, time.Now())
}

// QueryAt executes an instant query at a deterministic timestamp.
func (c *Client) QueryAt(ctx context.Context, expr string, at time.Time) (model.Value, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	value, _, _, err := c.api.Query(ctx, expr, at, promv1.WithTimeout(c.timeout))
	if err != nil {
		return nil, fmt.Errorf("PromQL 执行失败: %w", err)
	}
	return value, nil
}

// QueryRange executes a range query. Discovery uses it only to distinguish a
// metric that exists historically from one that is currently being scraped.
func (c *Client) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) (model.Value, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	value, _, _, err := c.api.QueryRange(ctx, expr, promv1.Range{
		Start: start,
		End:   end,
		Step:  step,
	}, promv1.WithTimeout(c.timeout))
	if err != nil {
		return nil, fmt.Errorf("PromQL range 执行失败: %w", err)
	}
	return value, nil
}

// MetricNames returns the metric names known in the window.
func (c *Client) MetricNames(ctx context.Context, start, end time.Time) ([]string, error) {
	return c.LabelValues(ctx, "__name__", "", start, end)
}

// LabelNames returns label names for a legal metric selector. An empty match
// asks Prometheus for all label names in the window.
func (c *Client) LabelNames(ctx context.Context, match string, start, end time.Time) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	var matches []string
	if strings.TrimSpace(match) != "" {
		matches = []string{match}
	}
	values, _, _, err := c.api.LabelNames(ctx, matches, start, end, promv1.WithTimeout(c.timeout))
	if err != nil {
		return nil, fmt.Errorf("Prometheus 标签名查询失败: %w", err)
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, string(value))
	}
	return out, nil
}

// LabelValues returns values for one label, optionally constrained by a legal
// Series Selector (for example `up{job="prometheus"}`).
func (c *Client) LabelValues(ctx context.Context, label, match string, start, end time.Time) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	var matches []string
	if strings.TrimSpace(match) != "" {
		if !looksLikeSelector(match) {
			return nil, fmt.Errorf("标签值查询的 match 必须是合法的 Series Selector，不能是 PromQL 表达式: %q", match)
		}
		matches = []string{match}
	}
	values, _, _, err := c.api.LabelValues(ctx, label, matches, start, end, promv1.WithTimeout(c.timeout))
	if err != nil {
		return nil, fmt.Errorf("Prometheus 标签值查询失败: %w", err)
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, string(value))
	}
	return out, nil
}

var metricSelectorParser = parser.NewParser(parser.Options{})

// looksLikeSelector rejects arbitrary PromQL being passed to match[]. The
// Prometheus parser is the authority: match[] accepts a metric name with label
// matchers, not an aggregation or arithmetic expression.
func looksLikeSelector(match string) bool {
	match = strings.TrimSpace(match)
	if match == "" {
		return true
	}
	_, err := metricSelectorParser.ParseMetricSelector(match)
	return err == nil
}

// IsEmpty reports whether a query returned no series.
func IsEmpty(value model.Value) bool {
	switch v := value.(type) {
	case model.Vector:
		return len(v) == 0
	case model.Matrix:
		return len(v) == 0
	case *model.Scalar:
		return v == nil
	case *model.String:
		return v == nil || v.Value == ""
	default:
		return value == nil
	}
}

func validateEndpoint(raw string, allowPath bool) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("Prometheus URL 不能为空（可通过 --prometheus-url 或 SITEGEN_PROMETHEUS_URL 设置）")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("Prometheus URL 非法: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("Prometheus URL 必须以 http:// 或 https:// 开头")
	}
	if u.Host == "" {
		return fmt.Errorf("Prometheus URL 缺少主机名")
	}
	if u.User != nil {
		return fmt.Errorf("Prometheus URL 不允许包含用户名或密码，请使用环境变量")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("Prometheus URL 不能包含 query 或 fragment")
	}
	if !allowPath && u.Path != "" && u.Path != "/" {
		return fmt.Errorf("Prometheus URL 不能包含路径，请填写服务根地址")
	}
	return nil
}

type authTransport struct {
	base        http.RoundTripper
	user        string
	pass        string
	bearer      string
	allowedHost string
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host != t.allowedHost {
		return nil, fmt.Errorf("拒绝访问非配置的 Prometheus 主机 %q", req.URL.Host)
	}
	clone := req.Clone(req.Context())
	if t.bearer != "" {
		clone.Header.Set("Authorization", "Bearer "+t.bearer)
	} else if t.user != "" {
		clone.SetBasicAuth(t.user, t.pass)
	}
	clone.Header.Set("User-Agent", "sitegen/1")
	return t.base.RoundTrip(clone)
}
