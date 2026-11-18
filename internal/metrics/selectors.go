package metrics

import (
	"sort"
	"strconv"
	"strings"

	"site-observability/internal/config"
)

var selectorToken = "%SELECTOR%"

// QuoteLabelValue returns a PromQL-safe double-quoted string literal. The
// PromQL parser accepts Go string escapes, so strconv.Quote covers quotes,
// backslashes, newlines and every other control byte consistently.
func QuoteLabelValue(value string) string {
	return strconv.Quote(value)
}

// LabelCondition is one equality condition in a selector. Regex conditions are
// reserved for future use; configured selectors are exact equality so that
// independent label alternation can never cross-match objects.
type LabelCondition struct {
	Name  string
	Value string
	Regex bool
}

// String renders one selector condition.
func (c LabelCondition) String() string {
	operator := "="
	if c.Regex {
		operator = "=~"
	}
	return c.Name + operator + QuoteLabelValue(c.Value)
}

// Selector is the single object used both to generate PromQL and to explain a
// failed check. Reporting cannot drift from the query that was actually run.
type Selector struct {
	Conditions []LabelCondition
}

// NewSelector builds a selector from ordered conditions.
func NewSelector(conditions ...LabelCondition) Selector {
	return Selector{Conditions: conditions}
}

// String renders the inner matcher list without braces.
func (s Selector) String() string {
	parts := make([]string, 0, len(s.Conditions))
	for _, condition := range s.Conditions {
		parts = append(parts, condition.String())
	}
	return strings.Join(parts, ", ")
}

// Append returns a copy with extra equality conditions.
func (s Selector) Append(conditions ...LabelCondition) Selector {
	out := Selector{Conditions: make([]LabelCondition, 0, len(s.Conditions)+len(conditions))}
	out.Conditions = append(out.Conditions, s.Conditions...)
	out.Conditions = append(out.Conditions, conditions...)
	return out
}

// Empty reports whether the selector has no conditions.
func (s Selector) Empty() bool { return len(s.Conditions) == 0 }

// ApplicationSelector renders an exact selector for a single app, including
// every configured extra Prometheus label. This is the atomic unit of
// application correlation: application, namespace, job and extra labels are
// always kept together as one object.
func ApplicationSelector(app config.Application) Selector {
	conditions := []LabelCondition{
		{Name: "application", Value: app.Application},
		{Name: "namespace", Value: app.Namespace},
	}
	if app.Prometheus.Job != "" {
		conditions = append(conditions, LabelCondition{Name: "job", Value: app.Prometheus.Job})
	}
	for _, key := range sortedKeys(app.Prometheus.Labels) {
		if reservedAppLabel(key) {
			continue
		}
		conditions = append(conditions, LabelCondition{Name: key, Value: app.Prometheus.Labels[key]})
	}
	return NewSelector(conditions...)
}

// ApplicationTargetSelector is the string form used by templates.
func ApplicationTargetSelector(app config.Application) string {
	return ApplicationSelector(app).String()
}

// TargetSelectorFor renders the selector used for the Prometheus `up` metric.
// It contains only the scrape job and explicitly configured targetLabels.
// Application identity labels are deliberately NOT added.
func TargetSelectorFor(app config.Application) Selector {
	conditions := make([]LabelCondition, 0, 1+len(app.Prometheus.TargetLabels))
	if app.Prometheus.Job != "" {
		conditions = append(conditions, LabelCondition{Name: "job", Value: app.Prometheus.Job})
	}
	for _, key := range sortedKeys(app.Prometheus.TargetLabels) {
		conditions = append(conditions, LabelCondition{Name: key, Value: app.Prometheus.TargetLabels[key]})
	}
	return NewSelector(conditions...)
}

// TargetSelector is the string form used by templates.
func TargetSelector(app config.Application) string {
	return TargetSelectorFor(app).String()
}

// ComponentSelectorFor renders an exact component selector.
func ComponentSelectorFor(component config.Component) Selector {
	conditions := []LabelCondition{{Name: "job", Value: component.Prometheus.Job}}
	if component.Prometheus.Instance != "" {
		conditions = append(conditions, LabelCondition{Name: "instance", Value: component.Prometheus.Instance})
	}
	for _, key := range sortedKeys(component.Prometheus.Labels) {
		conditions = append(conditions, LabelCondition{Name: key, Value: component.Prometheus.Labels[key]})
	}
	return NewSelector(conditions...)
}

// ComponentSelector is the string form used by templates.
func ComponentSelector(component config.Component) string {
	return ComponentSelectorFor(component).String()
}

// DomainSelectorFor renders an exact Blackbox selector.
func DomainSelectorFor(domain config.Domain) Selector {
	return NewSelector(
		LabelCondition{Name: "job", Value: domain.Blackbox.Job},
		LabelCondition{Name: "instance", Value: domain.Blackbox.Instance},
	)
}

// DomainSelector is the string form used by templates.
func DomainSelector(domain config.Domain) string {
	return DomainSelectorFor(domain).String()
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
	// Quoted tokens are the modern, context-correct form. Replace the whole
	// quoted literal so regex and string contexts both stay valid PromQL.
	out = quotedGrafanaVarRE.ReplaceAllStringFunc(out, func(match string) string {
		token := match[1 : len(match)-1]
		if isPodVariable(token) {
			return QuoteLabelValue(".+")
		}
		return QuoteLabelValue(firstApp)
	})
	out = quotedGrafanaBareVarRE.ReplaceAllStringFunc(out, func(match string) string {
		token := match[1 : len(match)-1]
		if isPodVariable(token) {
			return QuoteLabelValue(".+")
		}
		return QuoteLabelValue(firstApp)
	})
	// Legacy unquoted tokens. Quote the replacement because the token is
	// normally used as a matcher value or string argument.
	out = grafanaVarTokenRE.ReplaceAllStringFunc(out, func(token string) string {
		if isPodVariable(token) {
			return QuoteLabelValue(".+")
		}
		return QuoteLabelValue(firstApp)
	})
	out = grafanaVarRE.ReplaceAllStringFunc(out, func(token string) string {
		if isPodVariable(token) {
			return QuoteLabelValue(".+")
		}
		return QuoteLabelValue(firstApp)
	})
	out = grafanaBareVarRE.ReplaceAllStringFunc(out, func(token string) string {
		if isPodVariable(token) {
			return QuoteLabelValue(".+")
		}
		return QuoteLabelValue(firstApp)
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
