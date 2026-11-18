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

// TargetSelector renders the selector used for the Prometheus `up` metric.
// It contains only the scrape job and explicitly configured targetLabels.
// Application identity labels are deliberately NOT added.
func TargetSelector(app config.Application) string {
	parts := make([]string, 0, 1+len(app.Prometheus.TargetLabels))
	if app.Prometheus.Job != "" {
		parts = append(parts, `job=`+QuoteLabelValue(app.Prometheus.Job))
	}
	for _, key := range sortedKeys(app.Prometheus.TargetLabels) {
		parts = append(parts, key+"="+QuoteLabelValue(app.Prometheus.TargetLabels[key]))
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

// RenderCheckQuery substitutes Grafana variables with deterministic values for
// Prometheus validation. It must not be used to generate dashboards.
func RenderCheckQuery(query string, site *config.Site, selector string) string {
	out := RenderQuery(query, selector)
	firstApp := ""
	if site != nil && len(site.Spec.Applications) > 0 {
		firstApp = site.Spec.Applications[0].ID
	}
	out = grafanaJSONVarRE.ReplaceAllStringFunc(out, func(token string) string {
		if strings.Contains(token, "pod") {
			return QuoteLabelValue(".+")
		}
		return QuoteLabelValue(firstApp)
	})
	out = grafanaVarRE.ReplaceAllStringFunc(out, func(token string) string {
		if strings.Contains(token, "pod") {
			return ".+"
		}
		return firstApp
	})
	out = grafanaBareVarRE.ReplaceAllStringFunc(out, func(token string) string {
		if strings.Contains(token, "pod") {
			return ".+"
		}
		return firstApp
	})
	return out
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
