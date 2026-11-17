package metrics

import (
	"regexp"
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

// RegexAlternation returns an RE2 alternation of literal values.
func RegexAlternation(values []string) string {
	unique := uniqueSorted(values)
	if len(unique) == 0 {
		return "^$"
	}
	quoted := make([]string, 0, len(unique))
	for _, value := range unique {
		quoted = append(quoted, regexp.QuoteMeta(value))
	}
	return strings.Join(quoted, "|")
}

// AppJobsRegex returns a regex matching all configured application jobs.
func AppJobsRegex(site *config.Site) string {
	return RegexAlternation(applicationJobValues(site))
}

// SiteNamespacesRegex returns a regex matching all site namespaces.
func SiteNamespacesRegex(site *config.Site) string {
	return RegexAlternation(applicationNamespaceValues(site))
}

// PromRegexContent escapes a regex alternation for placement inside a quoted
// PromQL string literal.
func PromRegexContent(values []string) string {
	quoted := QuoteLabelValue(RegexAlternation(values))
	return quoted[1 : len(quoted)-1]
}

// ApplicationSelector renders the selector used by dashboard application
// panels. The static site guards are ANDed with the Dashboard variables so an
// arbitrary URL-supplied variable value cannot pull in another site's series.
func ApplicationSelector(site *config.Site) string {
	parts := []string{
		`application=~"$application"`,
		`application=~` + QuoteLabelValue(SiteApplicationsRegex(site)),
		`namespace=~"$namespace"`,
		`namespace=~` + QuoteLabelValue(SiteNamespacesRegex(site)),
		`job=~` + QuoteLabelValue(AppJobsRegex(site)),
	}
	for _, label := range commonApplicationLabels(site) {
		value := site.Spec.Applications[0].Prometheus.Labels[label]
		parts = append(parts, label+"="+QuoteLabelValue(value))
	}
	return strings.Join(parts, ", ")
}

// SiteApplicationsRegex returns a regex matching configured application names.
func SiteApplicationsRegex(site *config.Site) string {
	return RegexAlternation(applicationNames(site))
}

// ApplicationSelectorConcrete is used by Prometheus checks and aggregates all
// configured applications without Grafana variables.
func ApplicationSelectorConcrete(site *config.Site) string {
	parts := []string{
		`application=~` + QuoteLabelValue(RegexAlternation(applicationNames(site))),
		`namespace=~` + QuoteLabelValue(SiteNamespacesRegex(site)),
		`job=~` + QuoteLabelValue(AppJobsRegex(site)),
	}
	for _, label := range commonApplicationLabels(site) {
		value := site.Spec.Applications[0].Prometheus.Labels[label]
		parts = append(parts, label+"="+QuoteLabelValue(value))
	}
	return strings.Join(parts, ", ")
}

// ApplicationTargetSelector renders an exact selector for a single app,
// including every configured extra Prometheus label.
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

// NamespaceSelector renders the selector used by Kubernetes panels. The static
// namespace guard prevents URL-overridden variables from leaking other sites.
func NamespaceSelector(site *config.Site) string {
	return `namespace=~"$namespace", namespace=~` + QuoteLabelValue(SiteNamespacesRegex(site)) + `, pod=~"$pod"`
}

// NamespaceSelectorConcrete renders a Kubernetes check selector.
func NamespaceSelectorConcrete(site *config.Site) string {
	return `namespace=~` + QuoteLabelValue(SiteNamespacesRegex(site)) + `, pod=~".+"`
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

// AddFilter appends a matcher to the %SELECTOR% placeholder.
func AddFilter(query, matcher string) string {
	return strings.ReplaceAll(query, selectorToken, selectorToken+", "+matcher)
}

// RenderCheckQuery substitutes Grafana variables with deterministic values for
// Prometheus validation. It must not be used to generate dashboards.
func RenderCheckQuery(query string, site *config.Site, selector string) string {
	out := RenderQuery(query, selector)
	appRegex := PromRegexContent(applicationNames(site))
	namespaceRegex := PromRegexContent(applicationNamespaceValues(site))
	jobRegex := PromRegexContent(applicationJobValues(site))
	replacements := map[string]string{
		"${application}":     appRegex,
		"$application":       appRegex,
		"${namespace}":       namespaceRegex,
		"$namespace":         namespaceRegex,
		"${pod}":             ".+",
		"$pod":               ".+",
		"${app_jobs}":        jobRegex,
		"$app_jobs":          jobRegex,
		"${site_namespaces}": namespaceRegex,
		"$site_namespaces":   namespaceRegex,
	}
	for token, value := range replacements {
		out = strings.ReplaceAll(out, token, value)
	}
	return out
}

func applicationJobValues(site *config.Site) []string {
	jobs := make([]string, 0, len(site.Spec.Applications))
	for _, app := range site.Spec.Applications {
		if app.Prometheus.Job != "" {
			jobs = append(jobs, app.Prometheus.Job)
		}
	}
	return jobs
}

func applicationNamespaceValues(site *config.Site) []string {
	namespaces := make([]string, 0, len(site.Spec.Applications))
	for _, app := range site.Spec.Applications {
		if app.Namespace != "" {
			namespaces = append(namespaces, app.Namespace)
		}
	}
	return namespaces
}

func applicationNames(site *config.Site) []string {
	names := make([]string, 0, len(site.Spec.Applications))
	for _, app := range site.Spec.Applications {
		names = append(names, app.Application)
	}
	return names
}

func commonApplicationLabels(site *config.Site) []string {
	if len(site.Spec.Applications) == 0 {
		return nil
	}
	var candidates []string
	for key := range site.Spec.Applications[0].Prometheus.Labels {
		if !reservedAppLabel(key) {
			candidates = append(candidates, key)
		}
	}
	sort.Strings(candidates)
	var common []string
	for _, key := range candidates {
		value := site.Spec.Applications[0].Prometheus.Labels[key]
		all := true
		for _, app := range site.Spec.Applications[1:] {
			v, ok := app.Prometheus.Labels[key]
			if !ok || v != value {
				all = false
				break
			}
		}
		if all {
			common = append(common, key)
		}
	}
	return common
}

func reservedAppLabel(label string) bool {
	switch label {
	case "application", "namespace", "job", "instance":
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

func uniqueSorted(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
