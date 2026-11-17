package config

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// Issue is one semantic validation finding.
type Issue struct {
	Severity Severity
	Path     string
	Message  string
}

func (i Issue) String() string {
	return fmt.Sprintf("%s: %s", i.Path, i.Message)
}

// Issues is a deterministic list of validation findings.
type Issues []Issue

func (is Issues) HasErrors() bool {
	for _, issue := range is {
		if issue.Severity == SeverityError {
			return true
		}
	}
	return false
}

func (is Issues) ErrorStrings() []string {
	out := make([]string, 0, len(is))
	for _, issue := range is {
		out = append(out, issue.String())
	}
	return out
}

// TemplateInfo is the minimal template metadata needed by configuration
// validation. It deliberately avoids coupling config to the metrics package.
type TemplateInfo struct {
	Target         string
	ComponentTypes []string
	Auto           bool
}

// Resolver returns template metadata for a profile ID.
type Resolver func(profileID string) (TemplateInfo, bool)

var (
	slugRE      = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,33}[a-z0-9])?$`)
	labelNameRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	uidRE       = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)
	placeholder = regexp.MustCompile(`\{([a-zA-Z]+)\}`)
)

// ValidateSlug reports whether an object ID is safe and stable.
func ValidateSlug(id string, max int) error {
	if id == "" {
		return fmt.Errorf("不能为空")
	}
	if len(id) > max {
		return fmt.Errorf("长度不能超过 %d 个字符", max)
	}
	if !slugRE.MatchString(id) {
		return fmt.Errorf("只能包含小写字母、数字和连字符，且必须以字母或数字开头和结尾")
	}
	return nil
}

// Validate runs all semantic checks. The resolver may be nil to skip template
// existence checks (useful for focused unit tests).
func Validate(site *Site, resolve Resolver) Issues {
	var issues Issues
	add := func(severity Severity, path, format string, args ...any) {
		issues = append(issues, Issue{Severity: severity, Path: path, Message: fmt.Sprintf(format, args...)})
	}

	if site.APIVersion != APIVersion {
		add(SeverityError, "apiVersion", "必须为 %q", APIVersion)
	}
	if site.Kind != KindSite {
		add(SeverityError, "kind", "必须为 %q", KindSite)
	}
	if err := ValidateSlug(site.Metadata.ID, 35); err != nil {
		add(SeverityError, "metadata.id", "%v", err)
	}
	if site.Metadata.Name == "" {
		add(SeverityError, "metadata.name", "不能为空")
	}
	if site.Metadata.Environment == "" {
		add(SeverityError, "metadata.environment", "不能为空")
	}
	site.Spec.Links.Validate("spec.links", add)

	idPaths := map[string]string{}
	if site.Metadata.ID != "" {
		idPaths[site.Metadata.ID] = "metadata.id"
	}
	registerID := func(id, path string) {
		if id == "" {
			return
		}
		if first, ok := idPaths[id]; ok {
			add(SeverityError, path, "ID %q 与 %s 重复；所有对象 ID 必须全局唯一", id, first)
			return
		}
		idPaths[id] = path
	}

	if len(site.Spec.Domains) == 0 {
		add(SeverityError, "spec.domains", "至少需要配置一个域名")
	}
	for i, d := range site.Spec.Domains {
		base := fmt.Sprintf("spec.domains[%d]", i)
		if err := ValidateSlug(d.ID, 35); err != nil {
			add(SeverityError, base+".id", "%v", err)
		}
		registerID(d.ID, base+".id")
		if err := validateHTTPURL(d.URL, false); err != nil {
			add(SeverityError, base+".url", "%v", err)
		}
		if d.Blackbox.Job == "" {
			add(SeverityError, base+".blackbox.job", "不能为空")
		}
		if d.Blackbox.Instance == "" {
			add(SeverityError, base+".blackbox.instance", "不能为空")
		}
	}

	appPairs := map[string]string{}
	services := map[string]string{}
	dependencyRefs := map[string]bool{}
	for _, app := range site.Spec.Applications {
		dependencyRefs[app.ID] = true
	}
	for _, component := range site.Spec.Components {
		dependencyRefs[component.ID] = true
	}

	for i, app := range site.Spec.Applications {
		base := fmt.Sprintf("spec.applications[%d]", i)
		if err := ValidateSlug(app.ID, 35); err != nil {
			add(SeverityError, base+".id", "%v", err)
		}
		registerID(app.ID, base+".id")
		if app.Application == "" {
			add(SeverityError, base+".application", "不能为空")
		} else if err := validateVariableLabelValue(app.Application); err != nil {
			add(SeverityError, base+".application", "%v", err)
		}
		if app.Namespace == "" {
			add(SeverityError, base+".namespace", "不能为空")
		} else if err := validateVariableLabelValue(app.Namespace); err != nil {
			add(SeverityError, base+".namespace", "%v", err)
		}
		pair := app.Application + "\x00" + app.Namespace
		if app.Application != "" && app.Namespace != "" {
			if first, ok := appPairs[pair]; ok {
				add(SeverityError, base, "Application+Namespace（%s/%s）与 %s 重复", app.Application, app.Namespace, first)
			} else {
				appPairs[pair] = base
			}
		}
		if app.Prometheus.Job == "" {
			add(SeverityError, base+".prometheus.job", "不能为空")
		}
		if app.Prometheus.Labels != nil {
			if v, ok := app.Prometheus.Labels["application"]; ok && v != app.Application {
				add(SeverityError, base+".prometheus.labels.application", "必须与 application 字段一致（%q）", app.Application)
			}
			if v, ok := app.Prometheus.Labels["namespace"]; ok && v != app.Namespace {
				add(SeverityError, base+".prometheus.labels.namespace", "必须与 namespace 字段一致（%q）", app.Namespace)
			}
			validateExtraLabels(app.Prometheus.Labels, base+".prometheus.labels", add)
		}
		if app.Skywalking.Service == "" {
			add(SeverityError, base+".skywalking.service", "不能为空；留空时会自动生成 {application}_{namespace}")
		} else if first, ok := services[app.Skywalking.Service]; ok {
			add(SeverityError, base+".skywalking.service", "SkyWalking Service %q 已映射到 %s；不允许重复映射", app.Skywalking.Service, first)
		} else {
			services[app.Skywalking.Service] = base
		}
		if app.Skywalking.EndpointFormat == "" {
			add(SeverityError, base+".skywalking.endpointFormat", "不能为空")
		}
		seenMetrics := map[string]bool{}
		for j, metric := range app.Metrics {
			path := fmt.Sprintf("%s.metrics[%d]", base, j)
			if strings.TrimSpace(metric) == "" {
				add(SeverityError, path, "指标模板 ID 不能为空")
				continue
			}
			if seenMetrics[metric] {
				add(SeverityError, path, "指标模板 %q 重复启用", metric)
				continue
			}
			seenMetrics[metric] = true
			if resolve != nil {
				info, ok := resolve(metric)
				if !ok {
					add(SeverityError, path, "指标模板 %q 不存在于模板注册表", metric)
				} else {
					if info.Auto {
						add(SeverityError, path, "指标模板 %q 为自动模板，不能在站点配置中引用", metric)
					}
					if info.Target != TargetApplication {
						add(SeverityError, path, "指标模板 %q 适用于 %s，不适用于应用", metric, info.Target)
					}
				}
			}
		}
		if len(app.Metrics) == 0 {
			add(SeverityWarning, base+".metrics", "未启用任何指标模板；不会生成应用指标面板")
		}
		seenDeps := map[string]bool{}
		for j, dep := range app.Dependencies {
			path := fmt.Sprintf("%s.dependencies[%d]", base, j)
			if dep == app.ID {
				add(SeverityError, path, "应用不能依赖自身")
				continue
			}
			if seenDeps[dep] {
				add(SeverityError, path, "依赖 %q 重复", dep)
				continue
			}
			seenDeps[dep] = true
			if !dependencyRefs[dep] {
				add(SeverityError, path, "依赖对象 %q 不存在", dep)
			}
		}
	}

	componentSelectors := map[string]string{}
	for i, component := range site.Spec.Components {
		base := fmt.Sprintf("spec.components[%d]", i)
		if err := ValidateSlug(component.ID, 35); err != nil {
			add(SeverityError, base+".id", "%v", err)
		}
		registerID(component.ID, base+".id")
		if component.Name == "" {
			add(SeverityError, base+".name", "不能为空")
		}
		if err := ValidateSlug(component.Type, 35); err != nil {
			add(SeverityError, base+".type", "%v", err)
		}
		if component.Prometheus.Job == "" {
			add(SeverityError, base+".prometheus.job", "不能为空")
		}
		if component.Prometheus.Instance == "" {
			add(SeverityError, base+".prometheus.instance", "不能为空")
		}
		validateExtraLabels(component.Prometheus.Labels, base+".prometheus.labels", add)
		selectorKey := component.Type + "\x00" + component.Prometheus.Job + "\x00" + component.Prometheus.Instance
		for _, key := range sortedMapKeys(component.Prometheus.Labels) {
			selectorKey += "\x00" + key + "=" + component.Prometheus.Labels[key]
		}
		if first, ok := componentSelectors[selectorKey]; ok {
			add(SeverityError, base, "组件定义与 %s 重复（type/job/instance/labels 相同）；多个应用应复用同一个组件实体", first)
		} else {
			componentSelectors[selectorKey] = base
		}
		seenMetrics := map[string]bool{}
		for j, metric := range component.Metrics {
			path := fmt.Sprintf("%s.metrics[%d]", base, j)
			if strings.TrimSpace(metric) == "" {
				add(SeverityError, path, "指标模板 ID 不能为空")
				continue
			}
			if seenMetrics[metric] {
				add(SeverityError, path, "指标模板 %q 重复启用", metric)
				continue
			}
			seenMetrics[metric] = true
			if resolve == nil {
				continue
			}
			info, ok := resolve(metric)
			if !ok {
				add(SeverityError, path, "指标模板 %q 不存在于模板注册表", metric)
				continue
			}
			if info.Auto {
				add(SeverityError, path, "指标模板 %q 为自动模板，不能在站点配置中引用", metric)
			}
			if info.Target != TargetComponent {
				add(SeverityError, path, "指标模板 %q 适用于 %s，不适用于依赖组件", metric, info.Target)
				continue
			}
			if len(info.ComponentTypes) > 0 && !contains(info.ComponentTypes, component.Type) {
				add(SeverityError, path, "指标模板 %q 不支持组件类型 %q", metric, component.Type)
			}
		}
		if len(component.Metrics) == 0 {
			add(SeverityWarning, base+".metrics", "未启用任何指标模板；不会生成组件指标面板")
		}
	}

	return issues
}

func validateVariableLabelValue(value string) error {
	if strings.ContainsAny(value, "\"\\|,$\r\n\t") {
		return fmt.Errorf("不能包含双引号、反斜杠、竖线、逗号、美元符号、制表符或换行；该值会进入 Dashboard 正则/自定义变量")
	}
	return nil
}

func validateExtraLabels(labels map[string]string, path string, add func(Severity, string, string, ...any)) {
	for _, key := range sortedMapKeys(labels) {
		value := labels[key]
		labelPath := path + "." + key
		if !labelNameRE.MatchString(key) {
			add(SeverityError, labelPath, "不是合法的 Prometheus 标签名")
		}
		if key == "job" || key == "instance" {
			add(SeverityError, labelPath, "job/instance 使用专用字段配置，不允许重复出现在 labels 中")
		}
		if value == "" {
			add(SeverityError, labelPath, "标签值不能为空")
		}
	}
}

// Validate checks and reports URL/link configuration. It returns true when any
// finding was appended.
func (l Links) Validate(path string, add func(Severity, string, string, ...any)) bool {
	anyIssue := false
	baseLinks := []struct {
		name string
		raw  string
	}{
		{"skywalking", l.Skywalking},
		{"prometheus", l.Prometheus},
		{"grafana", l.Grafana},
	}
	for _, link := range baseLinks {
		if link.raw == "" {
			continue
		}
		if err := validateHTTPURL(link.raw, true); err != nil {
			add(SeverityError, path+"."+link.name, "%v", err)
			anyIssue = true
		}
	}
	templates := []struct {
		name string
		raw  string
	}{
		{"skywalkingServiceURLTemplate", l.SkywalkingServiceURLTemplate},
		{"skywalkingEndpointURLTemplate", l.SkywalkingEndpointURLTemplate},
	}
	for _, tmpl := range templates {
		if tmpl.raw == "" {
			continue
		}
		if err := validateURLTemplate(tmpl.raw); err != nil {
			add(SeverityError, path+"."+tmpl.name, "%v", err)
			anyIssue = true
		}
	}
	for _, typ := range sortedMapKeys(l.ComponentDashboards) {
		uid := l.ComponentDashboards[typ]
		if uid == "" {
			continue
		}
		if typ == "" || !slugRE.MatchString(strings.ToLower(typ)) {
			add(SeverityError, path+".componentDashboards."+typ, "组件类型必须是稳定的小写标识")
			anyIssue = true
		}
		if !uidRE.MatchString(uid) {
			add(SeverityError, path+".componentDashboards."+typ, "Grafana Dashboard UID 非法（1-40 位字母、数字、下划线或连字符）")
			anyIssue = true
		}
	}
	return anyIssue
}

func sortedMapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func validateURLTemplate(raw string) error {
	for _, m := range placeholder.FindAllStringSubmatch(raw, -1) {
		switch m[1] {
		case "service", "application", "namespace", "site":
		default:
			return fmt.Errorf("不支持的占位符 %q（仅支持 service/application/namespace/site）", m[0])
		}
	}
	expanded := raw
	for _, token := range []string{"{service}", "{application}", "{namespace}", "{site}"} {
		expanded = strings.ReplaceAll(expanded, token, "x")
	}
	if strings.ContainsAny(expanded, "{}") {
		return fmt.Errorf("包含未闭合或不支持的占位符")
	}
	return validateHTTPURL(expanded, true)
}

// ValidateHTTPURL validates a user-facing HTTP(S) URL.
func ValidateHTTPURL(raw string) error {
	return validateHTTPURL(raw, false)
}

func validateHTTPURL(raw string, allowPath bool) error {
	if raw == "" {
		return fmt.Errorf("不能为空")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("URL 非法: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("URL 必须以 http:// 或 https:// 开头")
	}
	if u.Host == "" {
		return fmt.Errorf("URL 缺少主机名")
	}
	if u.User != nil {
		return fmt.Errorf("URL 不允许包含用户名或密码")
	}
	if !allowPath && u.Fragment != "" {
		return fmt.Errorf("站点 URL 不允许包含 fragment")
	}
	return nil
}

func contains(items []string, value string) bool {
	for _, item := range items {
		if item == value {
			return true
		}
	}
	return false
}
