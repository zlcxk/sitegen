package config

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
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
	slugRE       = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,33}[a-z0-9])?$`)
	labelNameRE  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	metricNameRE = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	uidRE        = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)
	placeholder  = regexp.MustCompile(`\{([a-zA-Z]+)\}`)
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

	configuredApps := map[string]Application{}
	for _, app := range site.Spec.Applications {
		if app.ID != "" {
			configuredApps[app.ID] = app
		}
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
		} else if err := validateUTF8(d.Blackbox.Job); err != nil {
			add(SeverityError, base+".blackbox.job", "%v", err)
		}
		if d.Blackbox.Instance == "" {
			add(SeverityError, base+".blackbox.instance", "不能为空")
		} else if err := validateUTF8(d.Blackbox.Instance); err != nil {
			add(SeverityError, base+".blackbox.instance", "%v", err)
		}
		seenEntry := map[string]bool{}
		for j, entryID := range d.EntryApplications {
			path := fmt.Sprintf("%s.entryApplications[%d]", base, j)
			if entryID == "" {
				add(SeverityError, path, "入口应用 ID 不能为空")
				continue
			}
			if seenEntry[entryID] {
				add(SeverityError, path, "入口应用 %q 重复", entryID)
				continue
			}
			seenEntry[entryID] = true
			app, ok := configuredApps[entryID]
			if !ok {
				add(SeverityError, path, "入口应用 %q 不存在于 spec.applications", entryID)
				continue
			}
			if !app.HasMetric("http") {
				add(SeverityWarning, path, "入口应用 %q 未启用 HTTP 指标模板；不会计入站点外部请求汇总", entryID)
			}
		}
	}
	if len(configuredApps) > 0 && len(site.ApplicationsWithMetric("http")) > 0 {
		hasHTTPEntry := false
		for _, app := range site.EntryApplicationsWithMetric("http") {
			if app.ID != "" {
				hasHTTPEntry = true
				break
			}
		}
		if !hasHTTPEntry {
			add(SeverityWarning, "spec.domains", "站点启用了 HTTP 指标但没有配置启用 HTTP 的 entryApplications；将不会生成站点级外部请求汇总面板。请在域名下显式声明入口应用。")
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
		} else if err := validateUTF8(app.Application); err != nil {
			add(SeverityError, base+".application", "%v", err)
		} else if err := validateVariableLabelValue(app.Application); err != nil {
			add(SeverityError, base+".application", "%v", err)
		}
		if app.Namespace == "" {
			add(SeverityError, base+".namespace", "不能为空")
		} else if err := validateUTF8(app.Namespace); err != nil {
			add(SeverityError, base+".namespace", "%v", err)
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
		} else if err := validateUTF8(app.Prometheus.Job); err != nil {
			add(SeverityError, base+".prometheus.job", "%v", err)
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
		validateTargetLabels(app.Prometheus.TargetLabels, base+".prometheus.targetLabels", add)
		validateSkywalkingUI(app.Skywalking.UI, base+".skywalking.ui", add)
		validateApplicationKubernetes(app, base+".kubernetes", add)
		if app.Skywalking.Service == "" {
			add(SeverityError, base+".skywalking.service", "不能为空；留空时会自动生成 {application}_{namespace}")
		} else if err := validateUTF8(app.Skywalking.Service); err != nil {
			add(SeverityError, base+".skywalking.service", "%v", err)
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
		} else if err := validateUTF8(component.Prometheus.Job); err != nil {
			add(SeverityError, base+".prometheus.job", "%v", err)
		}
		if component.Prometheus.Instance == "" {
			add(SeverityError, base+".prometheus.instance", "不能为空")
		} else if err := validateUTF8(component.Prometheus.Instance); err != nil {
			add(SeverityError, base+".prometheus.instance", "%v", err)
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

// validateUTF8 rejects strings that cannot be encoded into a PromQL label
// value. UTF-8 validity is checked before any query is generated so the error
// points at the configuration field instead of surfacing as a Prometheus API
// parse failure.
func validateUTF8(value string) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("包含非法 UTF-8 字节，无法安全生成 PromQL；请重新填写该字段")
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
		if key == SitegenAppLabel {
			add(SeverityError, labelPath, "sitegen_app 为生成器保留标签，不允许配置")
		}
		if value == "" {
			add(SeverityError, labelPath, "标签值不能为空")
		} else if err := validateUTF8(value); err != nil {
			add(SeverityError, labelPath, "%v", err)
		}
	}
}

func validateApplicationKubernetes(app Application, path string, add func(Severity, string, string, ...any)) {
	k8sEnabled := app.HasMetric("kubernetes")
	if !k8sEnabled {
		if app.Kubernetes.PodSelector != nil {
			add(SeverityWarning, path+".podSelector", "应用未启用 kubernetes 指标模板，podSelector 不会生效")
		}
		if len(app.Kubernetes.ContainerMatchers) > 0 {
			add(SeverityWarning, path+".containerMatchers", "应用未启用 kubernetes 指标模板，containerMatchers 不会生效")
		}
		return
	}
	if app.Kubernetes.PodSelector == nil {
		// Do not also warn about container matchers: no application-level
		// Kubernetes panels will be generated at all.
		add(SeverityWarning, path+".podSelector",
			"未配置 Pod 归属规则；应用级 Kubernetes 面板不会生成。请补充 podSelector 后重新执行 build")
		if len(app.Kubernetes.ContainerMatchers) > 0 {
			add(SeverityWarning, path+".containerMatchers",
				"containerMatchers 已配置，但缺少 podSelector，当前不会生效")
		}
		return
	}
	ps := app.Kubernetes.PodSelector
	switch ps.Mode {
	case PodSelectorLabels:
		if ps.Metric != "" && !metricNameRE.MatchString(ps.Metric) {
			add(SeverityError, path+".podSelector.metric", "不是合法的 Prometheus 指标名")
		}
		if len(ps.Matchers) == 0 {
			add(SeverityError, path+".podSelector.matchers", "labels 模式必须提供至少一个标签匹配条件")
		}
		for _, key := range sortedMapKeys(ps.Matchers) {
			if !labelNameRE.MatchString(key) {
				add(SeverityError, path+".podSelector.matchers."+key, "不是合法的 Prometheus 标签名")
			}
			if key == SitegenAppLabel {
				add(SeverityError, path+".podSelector.matchers."+key, "sitegen_app 为生成器保留标签，不允许配置")
			}
			if ps.Matchers[key] == "" {
				add(SeverityError, path+".podSelector.matchers."+key, "标签值不能为空")
			} else if err := validateUTF8(ps.Matchers[key]); err != nil {
				add(SeverityError, path+".podSelector.matchers."+key, "%v", err)
			}
		}
	case PodSelectorPodRegex:
		if ps.Pod == "" {
			add(SeverityError, path+".podSelector.pod", "pod-regex 模式必须提供 pod 正则")
		} else if err := validateUTF8(ps.Pod); err != nil {
			add(SeverityError, path+".podSelector.pod", "%v", err)
		} else if _, err := regexp.Compile(ps.Pod); err != nil {
			add(SeverityError, path+".podSelector.pod", "正则非法: %v", err)
		} else {
			add(SeverityWarning, path+".podSelector",
				"pod-regex 兼容模式依赖 Pod 名称；Deployment 滚动更新后名称会变化，请确保正则能覆盖新实例")
		}
	default:
		add(SeverityError, path+".podSelector.mode", "必须为 labels 或 pod-regex")
	}

	if len(app.Kubernetes.ContainerMatchers) == 0 {
		add(SeverityWarning, path+".containerMatchers",
			"已配置 Pod 归属，但未配置容器过滤条件；Pod CPU/内存可能包含 Pause 容器或 Pod 父级 cgroup 序列，请用 sitegen check 核对")
		return
	}
	for index, matcher := range app.Kubernetes.ContainerMatchers {
		matcherPath := fmt.Sprintf("%s.containerMatchers[%d]", path, index)
		if !labelNameRE.MatchString(matcher.Name) {
			add(SeverityError, matcherPath+".name", "不是合法的 Prometheus 标签名")
		}
		if matcher.Name == SitegenAppLabel {
			add(SeverityError, matcherPath+".name", "sitegen_app 为生成器保留标签，不允许配置")
		}
		switch matcher.Name {
		case "job", "instance":
			add(SeverityError, matcherPath+".name",
				"容器过滤只作用于 cAdvisor 容器序列，不能用 %q 过滤；应用 Prometheus Job 不等于 cAdvisor Job。请改用 container/image 等容器标签，或跳过", matcher.Name)
		case "namespace", "pod":
			add(SeverityError, matcherPath+".name",
				"%q 由应用配置或 Pod 归属规则管理，不能作为容器过滤条件；请使用 container/image 等容器标签", matcher.Name)
		}
		if err := validateUTF8(matcher.Value); err != nil {
			add(SeverityError, matcherPath+".value", "%v", err)
		}
		switch matcher.EffectiveOperator() {
		case MatchEqual:
			// An empty equality value matches every series that is missing
			// the label; it is almost never what the user means.
			if matcher.Value == "" {
				add(SeverityError, matcherPath+".value",
					"equal 操作符不允许空字符串；如需排除空标签，请使用 not_equal \"\"")
			}
		case MatchNotEqual:
			// Explicitly allowed: excludes series that do not carry the label.
		case MatchRegex:
			if matcher.Value == "" {
				add(SeverityError, matcherPath+".value", "regex 操作符不允许空正则")
			} else if _, err := regexp.Compile(matcher.Value); err != nil {
				add(SeverityError, matcherPath+".value", "正则非法: %v", err)
			}
		case MatchNotRegex:
			if matcher.Value == "" {
				add(SeverityError, matcherPath+".value",
					"not_regex 不允许空正则；如需排除空标签，请使用 not_equal \"\"")
			} else if _, err := regexp.Compile(matcher.Value); err != nil {
				add(SeverityError, matcherPath+".value", "正则非法: %v", err)
			}
		default:
			add(SeverityError, matcherPath+".operator",
				"必须为 equal、not_equal、regex 或 not_regex")
		}
	}
}

func validateTargetLabels(labels map[string]string, path string, add func(Severity, string, string, ...any)) {
	for _, key := range sortedMapKeys(labels) {
		labelPath := path + "." + key
		if !labelNameRE.MatchString(key) {
			add(SeverityError, labelPath, "不是合法的 Prometheus 标签名")
		}
		if key == "job" || key == "instance" {
			add(SeverityError, labelPath, "job/instance 使用专用字段配置，不允许重复出现在 targetLabels 中")
		}
		if key == SitegenAppLabel {
			add(SeverityError, labelPath, "sitegen_app 为生成器保留标签，不允许配置")
		}
		if strings.TrimSpace(labels[key]) == "" {
			add(SeverityError, labelPath, "标签值不能为空")
		} else if err := validateUTF8(labels[key]); err != nil {
			add(SeverityError, labelPath, "%v", err)
		}
	}
}

func validateSkywalkingUI(ui SkywalkingUI, path string, add func(Severity, string, string, ...any)) {
	if ui.BaseURL != "" {
		if err := validateHTTPURL(ui.BaseURL, true); err != nil {
			add(SeverityError, path+".baseURL", "%v", err)
		}
	}
	for _, item := range []struct {
		name string
		raw  string
	}{
		{"serviceURLTemplate", ui.ServiceURLTemplate},
		{"endpointURLTemplate", ui.EndpointURLTemplate},
	} {
		if item.raw == "" {
			continue
		}
		if err := validateURLTemplate(item.raw); err != nil {
			add(SeverityError, path+"."+item.name, "%v", err)
			continue
		}
		if !strings.Contains(item.raw, "{service}") && !strings.Contains(item.raw, "{application}") {
			add(SeverityWarning, path+"."+item.name,
				"模板未包含 {service} 或 {application}；无法定位到具体应用页面")
		}
	}
	if ui.BaseURL == "" && ui.ServiceURLTemplate == "" && ui.EndpointURLTemplate == "" {
		return
	}
	if ui.ServiceURLTemplate != "" || ui.EndpointURLTemplate != "" {
		add(SeverityWarning, path,
			"SkyWalking UI URL 模板只做了语法校验；必须使用目标 SkyWalking 版本实际打开 Service/Endpoint 页面验证")
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
		if !strings.Contains(tmpl.raw, "{service}") && !strings.Contains(tmpl.raw, "{application}") {
			add(SeverityWarning, path+"."+tmpl.name,
				"模板未包含 {service} 或 {application}；无法定位到具体应用的页面，将只显示通用 UI 入口")
			anyIssue = true
		}
	}
	if l.SkywalkingServiceURLTemplate != "" || l.SkywalkingEndpointURLTemplate != "" {
		add(SeverityWarning, path+".skywalkingServiceURLTemplate",
			"SkyWalking URL 模板只做了语法校验；必须使用目标 SkyWalking UI 版本（如 10.4）实际打开 Service/Endpoint 页面验证")
		anyIssue = true
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

// ValidateURLTemplate validates a SkyWalking URL template and its placeholders.
func ValidateURLTemplate(raw string) error {
	return validateURLTemplate(raw)
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
