package dashboard

import (
	"strings"

	"site-observability/internal/config"
	"site-observability/internal/metrics"
)

func buildVariables(site *config.Site) []Variable {
	vars := []Variable{datasourceVariable()}
	apps := site.Spec.Applications
	if len(apps) > 0 {
		vars = append(vars, applicationVariable(site))
		vars = append(vars, namespaceVariable(site))
		if podVariable := buildPodVariable(site); podVariable != nil {
			vars = append(vars, *podVariable)
		}
	}
	if len(site.Spec.Components) > 0 {
		vars = append(vars, componentVariable(site))
		vars = append(vars, instanceVariable(site))
	}
	return vars
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

func applicationVariable(site *config.Site) Variable {
	names := uniqueStrings(func() []string {
		out := make([]string, 0, len(site.Spec.Applications))
		for _, app := range site.Spec.Applications {
			out = append(out, app.Application)
		}
		return out
	}())
	options := make([]VariableOption, 0, len(names))
	for _, name := range names {
		options = append(options, VariableOption{Text: name, Value: name})
	}
	return Variable{
		AllValue:   ".*",
		Current:    allCurrent(),
		Definition: strings.Join(names, ","),
		Hide:       0,
		IncludeAll: true,
		Label:      "应用",
		Multi:      true,
		Name:       "application",
		Options:    options,
		Query:      strings.Join(names, ","),
		Type:       "custom",
	}
}

func namespaceVariable(site *config.Site) Variable {
	metric := ""
	switch {
	case anyApplicationProfile(site, "http"):
		metric = "http_server_requests_seconds_count"
	case anyApplicationProfile(site, "jvm"):
		metric = "jvm_memory_used_bytes"
	}
	appGuard := metrics.QuoteLabelValue(metrics.SiteApplicationsRegex(site))
	nsGuard := metrics.QuoteLabelValue(metrics.SiteNamespacesRegex(site))
	selector := `application=~"$application", application=~` + appGuard + `, namespace=~` + nsGuard
	query := ""
	switch {
	case metric != "":
		query = "label_values(" + metric + "{" + selector + "}, namespace)"
	case anyApplicationProfile(site, "kubernetes"):
		query = "label_values(kube_pod_labels{namespace=~" + nsGuard + ", label_application=~\"$application\", label_application=~" + appGuard + "}, namespace)"
	default:
		query = "label_values({application=~\"$application\", application=~" + appGuard + ", namespace=~" + nsGuard + "}, namespace)"
	}
	return queryVariable("namespace", "Namespace", query, 2)
}

func buildPodVariable(site *config.Site) *Variable {
	appGuard := metrics.QuoteLabelValue(metrics.SiteApplicationsRegex(site))
	nsGuard := metrics.QuoteLabelValue(metrics.SiteNamespacesRegex(site))
	appFilter := `application=~"$application", application=~` + appGuard
	nsFilter := `namespace=~"$namespace", namespace=~` + nsGuard
	var query string
	switch {
	case anyApplicationProfile(site, "jvm"):
		query = "label_values(jvm_memory_used_bytes{" + appFilter + ", " + nsFilter + "}, pod)"
	case anyApplicationProfile(site, "kubernetes"):
		// cAdvisor container metrics do not carry the application label. When
		// JVM is disabled, use kube-state-metrics pod labels as the safest
		// available application correlation; the validation report warns about it.
		query = "label_values(kube_pod_labels{" + nsFilter + ", label_application=~\"$application\", label_application=~" + appGuard + "}, pod)"
	default:
		return nil
	}
	v := queryVariable("pod", "Pod", query, 2)
	return &v
}

func queryVariable(name, label, query string, refresh int) Variable {
	return Variable{
		AllValue:   ".*",
		Current:    allCurrent(),
		Datasource: &Datasource{Type: "prometheus", UID: datasourceUID},
		Definition: query,
		Hide:       0,
		IncludeAll: true,
		Label:      label,
		Multi:      true,
		Name:       name,
		Query:      query,
		Refresh:    refresh,
		Sort:       1,
		Type:       "query",
	}
}

func componentVariable(site *config.Site) Variable {
	options := make([]VariableOption, 0, len(site.Spec.Components))
	values := make([]string, 0, len(site.Spec.Components))
	for _, component := range site.Spec.Components {
		options = append(options, VariableOption{Text: component.Name, Value: component.ID})
		values = append(values, component.ID)
	}
	return Variable{
		AllValue:   ".*",
		Current:    allCurrent(),
		Definition: strings.Join(values, ","),
		Hide:       0,
		IncludeAll: true,
		Label:      "组件",
		Multi:      true,
		Name:       "component",
		Options:    options,
		Query:      strings.Join(values, ","),
		Type:       "custom",
	}
}

func instanceVariable(site *config.Site) Variable {
	seen := map[string]bool{}
	var options []VariableOption
	var values []string
	for _, component := range site.Spec.Components {
		if component.Prometheus.Instance == "" || seen[component.Prometheus.Instance] {
			continue
		}
		seen[component.Prometheus.Instance] = true
		values = append(values, component.Prometheus.Instance)
		options = append(options, VariableOption{
			Text:  component.Name + " / " + component.Prometheus.Instance,
			Value: component.Prometheus.Instance,
		})
	}
	return Variable{
		AllValue:   ".*",
		Current:    allCurrent(),
		Definition: strings.Join(values, ","),
		Hide:       0,
		IncludeAll: true,
		Label:      "Instance",
		Multi:      true,
		Name:       "instance",
		Options:    options,
		Query:      strings.Join(values, ","),
		Type:       "custom",
	}
}

func anyApplicationProfile(site *config.Site, profileID string) bool {
	for _, app := range site.Spec.Applications {
		for _, id := range app.Metrics {
			if id == profileID {
				return true
			}
		}
	}
	return false
}

func allCurrent() VariableCurrent {
	return VariableCurrent{Selected: true, Text: "All", Value: "$__all"}
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}
