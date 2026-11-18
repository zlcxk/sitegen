package dashboard

import (
	"fmt"
	"strings"

	"site-observability/internal/config"
	"site-observability/internal/metrics"
)

func buildVariables(site *config.Site, registry *metrics.Registry, datasources config.DatasourcesConfig) ([]Variable, error) {
	vars := []Variable{datasourceVariable(datasourceVarName, "业务 Prometheus", datasources.BusinessPrometheus)}
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
		// JVM is application-scoped and no longer exposes a Pod variable.
		if profile.ID == "kubernetes" {
			podVar, err := buildPodVariable(site, profile.ID, selectorVar)
			if err != nil {
				return nil, err
			}
			if podVar != nil {
				vars = append(vars, *podVar)
			}
		}
	}
	if siteHasSkywalking(site) {
		skProm := datasourceVariable(skywalkingPrometheusVarName, "SkyWalking Prometheus", datasources.SkywalkingPrometheus)
		skNative := datasourceVariable(skywalkingDatasourceVarName, "SkyWalking 数据源", datasources.Skywalking)
		skNative.Query = "apache-skywalking-datasource"
		vars = append(vars, skProm, skNative, skywalkingServiceVariable(site), skywalkingLayerVariable())
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

func datasourceVariable(name, label string, config config.DatasourceConfig) Variable {
	current := VariableCurrent{Selected: false, Text: "", Value: ""}
	if config.UID != "" {
		text := config.Name
		if text == "" {
			text = config.UID
		}
		current = VariableCurrent{Selected: true, Text: text, Value: config.UID}
	}
	return Variable{
		Current:   current,
		Hide:      0,
		Label:     label,
		Name:      name,
		Query:     "prometheus",
		QueryType: VariableQueryTypeDatasource,
		Refresh:   1,
		Type:      "datasource",
	}
}

func skywalkingServiceVariable(site *config.Site) Variable {
	var options []VariableOption
	var values []string
	for _, app := range site.Spec.Applications {
		if !app.SkywalkingEnabled() || app.Skywalking.Service == "" {
			continue
		}
		if contains(values, app.Skywalking.Service) {
			continue
		}
		values = append(values, app.Skywalking.Service)
		options = append(options, VariableOption{
			Selected: len(options) == 0,
			Text:     app.Application + " / " + app.Namespace + "（" + app.Skywalking.Service + "）",
			Value:    app.Skywalking.Service,
		})
	}
	current := VariableCurrent{Selected: false, Text: "", Value: ""}
	if len(options) > 0 {
		current = VariableCurrent{Selected: true, Text: options[0].Text, Value: options[0].Value}
	}
	return Variable{
		Current:      current,
		Hide:         0,
		Label:        "APM 服务",
		Multi:        false,
		Name:         skywalkingServiceVarName,
		Options:      options,
		Query:        strings.Join(values, ","),
		QueryType:    VariableQueryTypeCustom,
		Type:         "custom",
		ValuesFormat: "csv",
	}
}

func skywalkingLayerVariable() Variable {
	return Variable{
		Current:      VariableCurrent{Selected: true, Text: "GENERAL", Value: "GENERAL"},
		Hide:         2,
		Multi:        false,
		Name:         skywalkingLayerVarName,
		Options:      []VariableOption{{Selected: true, Text: "GENERAL", Value: "GENERAL"}},
		Query:        "GENERAL",
		QueryType:    VariableQueryTypeCustom,
		Type:         "custom",
		ValuesFormat: "csv",
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
		Current:      current,
		Hide:         0,
		Label:        label,
		Multi:        false,
		Name:         name,
		Options:      options,
		Query:        strings.Join(values, ","),
		QueryType:    VariableQueryTypeCustom,
		Type:         "custom",
		ValuesFormat: "csv",
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

// Grafana 11/13 Prometheus variable query types. A plain query string may be
// interpreted as a Series query and sent to /api/v1/series match[], which
// fails with `unexpected "("`. The object form makes the type explicit.
const (
	promVariableQueryRefID = "PrometheusVariableQueryEditor-VariableQuery"
	promVarLabelValues     = 1
	promVarQueryResult     = 3
)

func promQueryVariable(query string, qryType int) map[string]any {
	return map[string]any{
		"qryType": qryType,
		"query":   query,
		"refId":   promVariableQueryRefID,
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
	//
	// Two Grafana details matter here:
	//   1. qryType=3 (VarQueryResult) keeps the query on /api/v1/query.
	//   2. The Prometheus variable parser matches `query_result(...)` with a
	//      regex that does NOT use the `s` flag, so a multi-line expression is
	//      treated as a Series query and sent to /api/v1/series match[].
	//      Remove CR/LF (the editor does the same) while preserving spaces.
	query := "query_result(" + expression + ")"
	query = strings.ReplaceAll(query, "\n", "")
	query = strings.ReplaceAll(query, "\r", "")
	return &Variable{
		AllValue:     ".*",
		Current:      allCurrent(),
		Datasource:   &Datasource{Type: "prometheus", UID: datasourceUID},
		Definition:   query,
		Hide:         0,
		IncludeAll:   true,
		Label:        podVariableLabel(profileID),
		Multi:        false,
		Name:         metrics.ProfilePodVar(profileID),
		Query:        promQueryVariable(query, promVarQueryResult),
		QueryType:    VariableQueryTypeQueryResult,
		Regex:        `/pod="([^"]+)"/`,
		RegexApplyTo: "value",
		Refresh:      2,
		Sort:         1,
		Type:         "query",
	}, nil
}

func allCurrent() VariableCurrent {
	return VariableCurrent{Selected: true, Text: "All", Value: "$__all"}
}
