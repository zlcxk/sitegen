package metrics

import (
	"regexp"

	"site-observability/internal/config"
)

// Section keys map to the fixed row order of the dashboard.
const (
	SectionOverview    = "overview"
	SectionHTTP        = "http"
	SectionRuntime     = "runtime"
	SectionService     = "service"
	SectionComponent   = "component"
	SectionEnvironment = "environment"
)

// SectionOrder is the only dashboard row order.
var SectionOrder = []string{
	SectionOverview,
	SectionHTTP,
	SectionRuntime,
	SectionService,
	SectionComponent,
	SectionEnvironment,
}

// SectionTitle is the Chinese dashboard row title.
var SectionTitle = map[string]string{
	SectionOverview:    "站点健康总览",
	SectionHTTP:        "HTTP 请求分析",
	SectionRuntime:     "应用与 JVM",
	SectionService:     "服务调用关系",
	SectionComponent:   "依赖组件",
	SectionEnvironment: "Kubernetes 运行环境",
}

// Scope determines how %SELECTOR% is rendered.
const (
	ScopeApplication = "application"
	ScopeNamespace   = "namespace"
	ScopeComponent   = "component"
	ScopeDomain      = "domain"
	ScopeJob         = "job"
)

// Profile is a user-selectable bundle of metric panels.
type Profile struct {
	ID             string   `yaml:"id"`
	Name           string   `yaml:"name"`
	Target         string   `yaml:"target"`
	Scope          string   `yaml:"scope"`
	ComponentTypes []string `yaml:"componentTypes,omitempty"`
	Auto           bool     `yaml:"auto,omitempty"`
	Panels         []Panel  `yaml:"panels"`
}

// Panel aggregation scopes.
const (
	// AggregationApplication renders a query for the currently selected
	// application (or the exact application under validation).
	AggregationApplication = "application"
	// AggregationSite aggregates only configured entry applications. It must
	// never aggregate all applications, because that double counts internal
	// service calls as external site traffic.
	AggregationSite = "site"
)

// Panel describes one Grafana panel and its PromQL template.
type Panel struct {
	ID              string      `yaml:"id"`
	Name            string      `yaml:"name"`
	Description     string      `yaml:"description,omitempty"`
	Query           string      `yaml:"query"`
	Unit            string      `yaml:"unit"`
	Panel           string      `yaml:"panel"`
	LegendFormat    string      `yaml:"legendFormat,omitempty"`
	Sections        []string    `yaml:"sections"`
	Aggregation     string      `yaml:"aggregation,omitempty"`
	Instant         bool        `yaml:"instant,omitempty"`
	Thresholds      []Threshold `yaml:"thresholds,omitempty"`
	RequiredMetrics []string    `yaml:"requiredMetrics"`
	RequiredLabels  []string    `yaml:"requiredLabels,omitempty"`
	OptionalLabels  []string    `yaml:"optionalLabels,omitempty"`
}

// Threshold is a Grafana threshold step. A nil value is the base color.
type Threshold struct {
	Color string   `yaml:"color"`
	Value *float64 `yaml:"value,omitempty"`
}

type templateFile struct {
	Version  string    `yaml:"version"`
	Profiles []Profile `yaml:"profiles"`
}

var (
	templateSlugRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,33}[a-z0-9])?$`)
	metricNameRE   = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	labelNameRE    = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	versionRE      = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	varRE          = regexp.MustCompile(`\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?`)
	tokenRE        = regexp.MustCompile(`%[A-Z][A-Z0-9_]*%`)
)

var allowedVars = map[string]bool{
	"pod":          true,
	"app_selector": true,
}

var allowedPanelTypes = map[string]bool{
	"timeseries": true,
	"stat":       true,
	"table":      true,
	"bargauge":   true,
	"gauge":      true,
}

// ResolveAdapter lets the config package validate template references without
// importing metrics.
func (r *Registry) Resolve(profileID string) (config.TemplateInfo, bool) {
	p, ok := r.Profiles[profileID]
	if !ok {
		return config.TemplateInfo{}, false
	}
	return config.TemplateInfo{Target: p.Target, ComponentTypes: p.ComponentTypes, Auto: p.Auto}, true
}
