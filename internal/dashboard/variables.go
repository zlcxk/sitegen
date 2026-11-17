package dashboard

import (
	"strings"

	"site-observability/internal/config"
	"site-observability/internal/metrics"
)

func buildVariables(site *config.Site, registry *metrics.Registry) []Variable {
	vars := []Variable{datasourceVariable()}
	if len(site.Spec.Applications) == 0 || !hasApplicationPanels(site, registry) {
		return vars
	}
	vars = append(vars, appSelectorVariable(site))
	if podVariable := buildPodVariable(site); podVariable != nil {
		vars = append(vars, *podVariable)
	}
	return vars
}

// hasApplicationPanels reports whether at least one application-scope metric
// panel will be generated. Variables that would not affect any panel are never
// emitted (no decorative controls).
func hasApplicationPanels(site *config.Site, registry *metrics.Registry) bool {
	for _, id := range registry.Order {
		profile := registry.Profiles[id]
		if profile.Auto || profile.Target != config.TargetApplication {
			continue
		}
		apps := site.ApplicationsWithMetric(profile.ID)
		if len(apps) == 0 {
			continue
		}
		if profile.Scope == metrics.ScopeNamespace {
			for _, app := range apps {
				if app.Kubernetes.PodSelector != nil {
					return true
				}
			}
			continue
		}
		for _, panel := range profile.Panels {
			if panel.Aggregation != metrics.AggregationSite {
				return true
			}
		}
	}
	return false
}

func datasourceVariable() Variable {
	return Variable{
		Current:    VariableCurrent{Selected: false, Text: "", Value: ""},
		Definition: "prometheus",
		Hide:       0,
		Label:      "数据源",
		Name:       datasourceVarName,
		Query:      "prometheus",
		Refresh:    1,
		Type:       "datasource",
	}
}

// appSelectorVariable is a single-select custom variable. Each option value is
// the stable configuration ID of exactly one Application object. Queries never
// interpolate a user-controlled PromQL fragment; they compile every configured
// application's exact selector at generation time and filter by this ID.
func appSelectorVariable(site *config.Site) Variable {
	options := make([]VariableOption, 0, len(site.Spec.Applications))
	values := make([]string, 0, len(site.Spec.Applications))
	for i, app := range site.Spec.Applications {
		values = append(values, app.ID)
		options = append(options, VariableOption{
			Selected: i == 0,
			Text:     app.Application + " / " + app.Namespace,
			Value:    app.ID,
		})
	}
	current := VariableCurrent{Selected: true, Text: site.Spec.Applications[0].Application + " / " + site.Spec.Applications[0].Namespace, Value: site.Spec.Applications[0].ID}
	return Variable{
		Current:    current,
		Definition: strings.Join(values, ","),
		Hide:       0,
		Label:      "应用实例",
		Multi:      false,
		Name:       "app_selector",
		Options:    options,
		Query:      strings.Join(values, ","),
		Type:       "custom",
	}
}

func buildPodVariable(site *config.Site) *Variable {
	expression := metrics.BuildPodVariableExpression(site)
	if expression == "" {
		return nil
	}
	// query_result executes arbitrary PromQL and returns the series text. A
	// regex extracts the pod label values. label_values() only accepts series
	// selectors, while the application guard is a binary expression.
	query := "query_result(" + expression + ")"
	return &Variable{
		AllValue:   ".*",
		Current:    allCurrent(),
		Datasource: &Datasource{Type: "prometheus", UID: datasourceUID},
		Definition: query,
		Hide:       0,
		IncludeAll: true,
		Label:      "Pod",
		Multi:      false,
		Name:       "pod",
		Query:      query,
		Regex:      `/pod="([^"]+)"/`,
		Refresh:    2,
		Sort:       1,
		Type:       "query",
	}
}

func allCurrent() VariableCurrent {
	return VariableCurrent{Selected: true, Text: "All", Value: "$__all"}
}
