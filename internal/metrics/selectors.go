package metrics

import (
	"sort"
	"strings"

	"site-observability/internal/config"
)

var selectorToken = "%SELECTOR%"

// QuoteLabelValue returns a PromQL-safe double-quoted string literal.
func QuoteLabelValue(value string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		"\n", `\n`,
		"\r", `\r`,
		"\t", `\t`,
	)
	return `"` + replacer.Replace(value) + `"`
}

// ApplicationTargetSelector renders an exact selector for a single app,
// including every configured extra Prometheus label. This is the atomic unit
// of application correlation: application, namespace, job and extra labels
// are always kept together as one object.
func ApplicationTargetSelector(app config.Application) string {
	parts := []string{
		`application=` + QuoteLabelValue(app.Application),
		`namespace=` + QuoteLabelValue(app.Namespace),
	}
	if app.Prometheus.Job != "" {
		parts = append(parts, `job=`+QuoteLabelValue(app.Prometheus.Job))
	}
	for _, key := range sortedKeys(app.Prometheus.Labels) {
		if reservedAppLabel(key) {
			continue
		}
		parts = append(parts, key+"="+QuoteLabelValue(app.Prometheus.Labels[key]))
	}
	return strings.Join(parts, ", ")
}

// ComponentSelector renders an exact component selector.
func ComponentSelector(component config.Component) string {
	parts := []string{`job=` + QuoteLabelValue(component.Prometheus.Job)}
	if component.Prometheus.Instance != "" {
		parts = append(parts, `instance=`+QuoteLabelValue(component.Prometheus.Instance))
	}
	for _, key := range sortedKeys(component.Prometheus.Labels) {
		parts = append(parts, key+"="+QuoteLabelValue(component.Prometheus.Labels[key]))
	}
	return strings.Join(parts, ", ")
}

// DomainSelector renders an exact Blackbox selector.
func DomainSelector(domain config.Domain) string {
	return `job=` + QuoteLabelValue(domain.Blackbox.Job) + `, instance=` + QuoteLabelValue(domain.Blackbox.Instance)
}

// RenderQuery injects a selector into a metric query template.
func RenderQuery(query, selector string) string {
	return strings.ReplaceAll(query, selectorToken, selector)
}

// RenderApplicationQuery compiles an application-scope query template for a
// set of application objects.
//
// Each configured application is compiled with its complete exact selector.
// When more than one application is involved the per-application series are
// unioned with `or`; literal label alternations are never used to simulate an
// object relationship.
//
// filterByApp=true additionally restricts the result to the application chosen
// in the $app_selector variable. A synthetic sitegen_app label is attached to
// each series before the union, and filtered with a scalar match. Overriding
// the variable to a value outside the site therefore yields no data instead of
// pulling in another site's series.
func RenderApplicationQuery(query string, apps []config.Application, filterByApp bool) string {
	if len(apps) == 0 {
		return query
	}
	if len(apps) == 1 && !filterByApp {
		return RenderQuery(query, ApplicationTargetSelector(apps[0]))
	}
	return renderUnion(query, apps, filterByApp, func(metric, extra string, app config.Application) string {
		selector := ApplicationTargetSelector(app)
		if extra != "" {
			selector += ", " + extra
		}
		return labelReplaceSeries(metric, selector, app.ID)
	})
}

// RenderNamespaceQuery compiles a Kubernetes query template for applications
// that declared an explicit pod ownership rule.
func RenderNamespaceQuery(query string, apps []config.Application, filterByApp bool) string {
	if len(apps) == 0 {
		return query
	}
	return renderUnion(query, apps, filterByApp, func(metric, extra string, app config.Application) string {
		return namespaceSeries(metric, extra, app)
	})
}

// RenderJobQuery compiles a job-scope query (the `up` metric) for a set of
// applications. Only the job label is used here because scrape targets are not
// required to carry application or namespace labels.
func RenderJobQuery(query string, apps []config.Application) string {
	var withJob []config.Application
	for _, app := range apps {
		if app.Prometheus.Job != "" {
			withJob = append(withJob, app)
		}
	}
	if len(withJob) == 0 {
		return query
	}
	if len(withJob) == 1 {
		return RenderQuery(query, `job=`+QuoteLabelValue(withJob[0].Prometheus.Job))
	}
	return renderUnion(query, withJob, false, func(metric, extra string, app config.Application) string {
		selector := `job=` + QuoteLabelValue(app.Prometheus.Job)
		if extra != "" {
			selector += ", " + extra
		}
		return metric + "{" + selector + "}"
	})
}

// BuildPodVariableExpression returns a PromQL expression whose pod label
// values are restricted to the application selected by $app_selector. It is
// used inside a Grafana label_values() variable query.
func BuildPodVariableExpression(site *config.Site) string {
	if site == nil {
		return ""
	}
	var exprs []string
	for _, app := range site.Spec.Applications {
		// The pod variable is only used by JVM and Kubernetes panels. HTTP-only
		// applications do not reference it, so they must not create a
		// decorative variable.
		if app.HasMetric("jvm") {
			exprs = append(exprs, labelReplaceSeries("jvm_memory_used_bytes", ApplicationTargetSelector(app), app.ID))
		}
		if !app.HasMetric("kubernetes") || app.Kubernetes.PodSelector == nil {
			continue
		}
		ps := app.Kubernetes.PodSelector
		switch ps.Mode {
		case config.PodSelectorLabels:
			selector := `namespace=` + QuoteLabelValue(app.Namespace)
			for _, key := range sortedKeys(ps.Matchers) {
				selector += ", " + key + "=" + QuoteLabelValue(ps.Matchers[key])
			}
			exprs = append(exprs, labelReplaceSeries(ps.PodLabelMetric(), selector, app.ID))
		case config.PodSelectorPodRegex:
			if ps.Pod == "" {
				continue
			}
			selector := `namespace=` + QuoteLabelValue(app.Namespace) + `, pod=~` + QuoteLabelValue(ps.Pod)
			exprs = append(exprs, labelReplaceSeries("container_memory_working_set_bytes", selector, app.ID))
		}
	}
	if len(exprs) == 0 {
		return ""
	}
	union := exprs[0]
	if len(exprs) > 1 {
		union = "(" + strings.Join(exprs, " or ") + ")"
	}
	return filterByAppSelector(union)
}

// RenderCheckQuery substitutes Grafana variables with deterministic values for
// Prometheus validation. It must not be used to generate dashboards.
func RenderCheckQuery(query string, site *config.Site, selector string) string {
	out := RenderQuery(query, selector)
	firstApp := ""
	if site != nil && len(site.Spec.Applications) > 0 {
		firstApp = site.Spec.Applications[0].ID
	}
	replacements := map[string]string{
		"${pod:json}":          QuoteLabelValue(".+"),
		"${pod}":               ".+",
		"$pod":                 ".+",
		"${app_selector:json}": QuoteLabelValue(firstApp),
		"${app_selector}":      firstApp,
		"$app_selector":        firstApp,
	}
	for token, value := range replacements {
		out = strings.ReplaceAll(out, token, value)
	}
	return out
}

// renderUnion rewrites every metric{...}[range] region that contains
// %SELECTOR% into a union of per-application series expressions.
func renderUnion(query string, apps []config.Application, filterByApp bool, build func(metric, extra string, app config.Application) string) string {
	for {
		idx := strings.Index(query, selectorToken)
		if idx < 0 {
			return query
		}
		left := strings.LastIndex(query[:idx], "{")
		if left < 0 || strings.Contains(query[left:idx], "}") {
			return query
		}
		// Find the metric selector's closing brace, skipping braces that belong
		// to Grafana variable expressions such as ${pod:json}. A naive
		// strings.Index(query[idx:], "}") would stop at the variable's brace and
		// produce malformed PromQL.
		right := selectorClosingBrace(query, idx)
		if right < 0 {
			return query
		}
		mstart := left
		for mstart > 0 && isMetricNameChar(query[mstart-1]) {
			mstart--
		}
		metric := query[mstart:left]
		if metric == "" {
			return query
		}
		inside := strings.ReplaceAll(query[left+1:right], selectorToken, "")
		extra := strings.TrimSpace(inside)
		extra = strings.TrimPrefix(extra, ",")
		extra = strings.TrimSpace(extra)

		end := right + 1
		rangeText := ""
		if end < len(query) && query[end] == '[' {
			if close := strings.Index(query[end:], "]"); close >= 0 {
				rangeText = query[end : end+close+1]
				end += close + 1
			}
		}

		parts := make([]string, 0, len(apps))
		for _, app := range apps {
			parts = append(parts, build(metric, extra, app))
		}
		if len(parts) == 0 {
			return query
		}
		union := parts[0]
		if len(parts) > 1 {
			union = "(" + strings.Join(parts, " or ") + ")"
		}
		if filterByApp {
			union = filterByAppSelector(union)
		}

		var replacement string
		if rangeText != "" {
			base := strings.TrimSuffix(rangeText, "]")
			if !strings.Contains(base, ":") {
				base += ":"
			}
			replacement = "(" + union + ")" + base + "]"
		} else {
			replacement = "(" + union + ")"
		}
		query = query[:mstart] + replacement + query[end:]
	}
}

func namespaceSeries(metric, extra string, app config.Application) string {
	selector := `namespace=` + QuoteLabelValue(app.Namespace) + `, pod=~${pod:json}`
	if extra != "" {
		selector += ", " + extra
	}
	for _, key := range sortedKeys(app.Kubernetes.ContainerMatchers) {
		selector += ", " + key + "=" + QuoteLabelValue(app.Kubernetes.ContainerMatchers[key])
	}
	if app.Kubernetes.PodSelector == nil {
		return labelReplaceSeries(metric, selector, app.ID)
	}
	switch app.Kubernetes.PodSelector.Mode {
	case config.PodSelectorPodRegex:
		if app.Kubernetes.PodSelector.Pod != "" {
			selector += `, pod=~` + QuoteLabelValue(app.Kubernetes.PodSelector.Pod)
		}
		return labelReplaceSeries(metric, selector, app.ID)
	default:
		ownerSelector := `namespace=` + QuoteLabelValue(app.Namespace)
		for _, key := range sortedKeys(app.Kubernetes.PodSelector.Matchers) {
			ownerSelector += ", " + key + "=" + QuoteLabelValue(app.Kubernetes.PodSelector.Matchers[key])
		}
		owner := labelReplaceSeries(app.Kubernetes.PodSelector.PodLabelMetric(), ownerSelector, app.ID)
		// kube_pod_labels can expose more than one series per pod. Reduce the
		// owner set to exactly one series per pod before the join; otherwise
		// the multiplication would inflate CPU/memory by the number of owner
		// series. `and` cannot carry group_left labels, so use * with value 1.
		owner = "max by (namespace, pod, sitegen_app) (" + owner + ")"
		return "(" + metric + "{" + selector + "} * on (namespace, pod) group_left(sitegen_app) " + owner + ")"
	}
}

func labelReplaceSeries(metric, selector, appID string) string {
	return "label_replace(" + metric + "{" + selector + "}, " + QuoteLabelValue(config.SitegenAppLabel) +
		", " + QuoteLabelValue(appID) + ", \"\", \"\")"
}

func filterByAppSelector(expr string) string {
	// ${app_selector:json} is Grafana's JSON escaping format. It emits a
	// properly quoted, escaped PromQL string literal, so a URL-overridden
	// variable value cannot break out of the string and inject PromQL. The
	// value is still matched against generated sitegen_app IDs, so an unknown
	// value simply yields no data.
	return "(" + expr + ") * on (" + config.SitegenAppLabel + ") group_left " +
		"label_replace(vector(1), " + QuoteLabelValue(config.SitegenAppLabel) + ", ${app_selector:json}, \"\", \"\")"
}

// selectorClosingBrace returns the index of the metric selector's closing
// brace, skipping quoted strings and ${...} Grafana variable expressions.
func selectorClosingBrace(query string, start int) int {
	for i := start; i < len(query); i++ {
		switch query[i] {
		case '"':
			for i++; i < len(query); i++ {
				if query[i] == '\\' {
					i++
					continue
				}
				if query[i] == '"' {
					break
				}
			}
		case '$':
			if i+1 < len(query) && query[i+1] == '{' {
				depth := 0
				for j := i + 1; j < len(query); j++ {
					switch query[j] {
					case '{':
						depth++
					case '}':
						depth--
						if depth == 0 {
							i = j
							j = len(query)
						}
					}
				}
			}
		case '}':
			return i
		}
	}
	return -1
}

func isMetricNameChar(ch byte) bool {
	return ch == '_' || ch == ':' ||
		(ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9')
}

func reservedAppLabel(label string) bool {
	switch label {
	case "application", "namespace", "job", "instance", config.SitegenAppLabel:
		return true
	default:
		return false
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
