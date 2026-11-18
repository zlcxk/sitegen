package dashboard

import (
	"fmt"
	"strings"

	"site-observability/internal/config"
	"site-observability/internal/metrics"
)

func buildVariables(site *config.Site, registry *metrics.Registry) ([]Variable, error) {
	vars := []Variable{datasourceVariable()}
	if len(site.Spec.Applications) == 0 {
		return vars, nil
	}
	for _, id := range registry.Order {
		profile := registry.Profiles[id]
		if profile.Auto || profile.Target != config.TargetApplication {
			continue
		}
		apps := appsForProfile(site, profile)
		if len(apps) == 0 || !profileHasPanelVariable(profile) {
			continue
		}
		selectorVar := metrics.ProfileSelectorVar(profile.ID)
		vars = append(vars, appSelectorVariable(profile.ID, selectorVar, apps))
		if profile.ID == "jvm" || profile.ID == "kubernetes" {
			podVar, err := buildPodVariable(site, profile.ID, selectorVar)
			if err != nil {
				return nil, err
			}
			if podVar != nil {
				vars = append(vars, *podVar)
			}
		}
	}
	return vars, nil
}

func appsForProfile(site *config.Site, profile *metrics.Profile) []config.Application {
	apps := site.ApplicationsWithMetric(profile.ID)
	if profile.Scope != metrics.ScopeNamespace {
		return apps
	}
	out := make([]config.Application, 0, len(apps))
	for _, app := range apps {
		if app.Kubernetes.PodSelector != nil {
			out = append(out, app)
		}
	}
	return out
}

func profileHasPanelVariable(profile *metrics.Profile) bool {
	if profile.Scope == metrics.ScopeNamespace {
		return true
	}
	for _, panel := range profile.Panels {
		if panel.Aggregation != metrics.AggregationSite {
			return true
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

// appSelectorVariable is a single-select custom variable scoped to one
// profile. Its options contain only applications that enabled that profile.
func appSelectorVariable(profileID, name string, apps []config.Application) Variable {
	options := make([]VariableOption, 0, len(apps))
	values := make([]string, 0, len(apps))
	for i, app := range apps {
		values = append(values, app.ID)
		options = append(options, VariableOption{
			Selected: i == 0,
			Text:     app.Application + " / " + app.Namespace,
			Value:    app.ID,
		})
	}
	label := profileVariableLabel(profileID)
	current := VariableCurrent{
		Selected: true,
		Text:     apps[0].Application + " / " + apps[0].Namespace,
		Value:    apps[0].ID,
	}
	return Variable{
		Current:    current,
		Definition: strings.Join(values, ","),
		Hide:       0,
		Label:      label,
		Multi:      false,
		Name:       name,
		Options:    options,
		Query:      strings.Join(values, ","),
		Type:       "custom",
	}
}

func profileVariableLabel(profileID string) string {
	switch profileID {
	case "http":
		return "应用实例（HTTP）"
	case "jvm":
		return "应用实例（JVM）"
	case "kubernetes":
		return "应用实例（Kubernetes）"
	default:
		return "应用实例（" + profileID + "）"
	}
}

func podVariableLabel(profileID string) string {
	switch profileID {
	case "jvm":
		return "Pod（JVM）"
	case "kubernetes":
		return "Pod（Kubernetes）"
	default:
		return "Pod（" + profileID + "）"
	}
}

func buildPodVariable(site *config.Site, profileID, selectorVar string) (*Variable, error) {
	expression, err := metrics.BuildPodVariableExpression(site, profileID, selectorVar)
	if err != nil {
		return nil, fmt.Errorf("生成 %s Pod 变量: %w", profileID, err)
	}
	if expression == "" {
		return nil, nil
	}
	// query_result executes arbitrary PromQL and returns the series text. A
	// regex extracts pod label values. label_values() cannot express the
	// application identity filter.
	query := "query_result(" + expression + ")"
	return &Variable{
		AllValue:   ".*",
		Current:    allCurrent(),
		Datasource: &Datasource{Type: "prometheus", UID: datasourceUID},
		Definition: query,
		Hide:       0,
		IncludeAll: true,
		Label:      podVariableLabel(profileID),
		Multi:      false,
		Name:       metrics.ProfilePodVar(profileID),
		Query:      query,
		Regex:      `/pod="([^"]+)"/`,
		Refresh:    2,
		Sort:       1,
		Type:       "query",
	}, nil
}

func allCurrent() VariableCurrent {
	return VariableCurrent{Selected: true, Text: "All", Value: "$__all"}
}
