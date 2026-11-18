package dashboard

import (
	"net/url"
	"strings"

	"site-observability/internal/config"
)

func buildDashboardLinks(site *config.Site, links config.Links) []DashboardLink {
	var out []DashboardLink
	add := func(title, raw string, keepTime bool) {
		if raw == "" {
			return
		}
		out = append(out, DashboardLink{
			AsDropdown:  false,
			Icon:        "external link",
			IncludeVars: false,
			KeepTime:    keepTime,
			Tags:        []string{},
			TargetBlank: true,
			Title:       title,
			Tooltip:     title,
			Type:        "link",
			URL:         raw,
		})
	}
	add("SkyWalking", links.Skywalking, false)
	add("Prometheus", links.Prometheus, false)
	add("Grafana", links.Grafana, false)
	if siteHasSkywalking(site) && links.SkywalkingServiceDashboard != "" {
		raw := grafanaQueryLink(links.SkywalkingServiceDashboard, map[string]string{
			"var-service":       "${service:queryparam}",
			"var-layer":         "${layer}",
			"var-DS_Prometheus": "${skywalking_prometheus}",
			"var-DS_Skywalking": "${skywalking_datasource}",
		}, "")
		add("SkyWalking 服务详情", raw, true)
	}
	for _, dashboard := range links.Dashboards {
		add(dashboard.Title, dashboard.URL, true)
	}
	return out
}

// dataLinks builds per-row SkyWalking drill-down links. Grafana field tokens
// (${__field.labels.*}) and variable tokens are preserved; concrete values are
// encoded with net/url. Missing dashboard bases produce no link, never a fake
// URL.
func (b *builder) dataLinks(targets []string) []DataLink {
	var out []DataLink
	for _, target := range targets {
		switch target {
		case "instance":
			if raw := grafanaQueryLink(b.links.SkywalkingInstanceDashboard, map[string]string{
				"var-service":          "${service:queryparam}",
				"var-service_instance": "${__field.labels.service_instance}",
			}, ""); raw != "" {
				out = append(out, DataLink{Title: "实例详情", URL: raw})
			}
		case "endpoint":
			if raw := grafanaQueryLink(b.links.SkywalkingEndpointDashboard, map[string]string{
				"var-service":  "${service:queryparam}",
				"var-endpoint": "${__field.labels.endpoint}",
			}, "${__url_time_range}"); raw != "" {
				out = append(out, DataLink{Title: "Endpoint 详情", URL: raw})
			}
		case "trace":
			if raw := grafanaQueryLink(b.links.SkywalkingTraceDashboard, map[string]string{
				"var-service":  "${service:queryparam}",
				"var-endpoint": "${__field.labels.endpoint}",
			}, "${__url_time_range}"); raw != "" {
				out = append(out, DataLink{Title: "Trace 查询", URL: raw, TargetBlank: true})
			}
		}
	}
	return out
}

// grafanaQueryLink encodes static query parameters with net/url while keeping
// Grafana variable tokens intact. rawSuffix is appended as a raw query fragment
// (for example ${__url_time_range}).
func grafanaQueryLink(base string, params map[string]string, rawSuffix string) string {
	if strings.TrimSpace(base) == "" {
		return ""
	}
	u, err := url.Parse(base)
	if err != nil || u.Path == "" {
		return ""
	}
	query := u.Query()
	replacements := map[string]string{}
	for key, value := range params {
		if strings.Contains(value, "${") {
			marker := "__SITEGEN_" + key + "__"
			query.Set(key, marker)
			replacements[marker] = value
			continue
		}
		query.Set(key, value)
	}
	u.RawQuery = query.Encode()
	out := u.String()
	for marker, value := range replacements {
		out = strings.ReplaceAll(out, marker, value)
	}
	if rawSuffix != "" {
		if strings.Contains(out, "?") {
			out += "&" + rawSuffix
		} else {
			out += "?" + rawSuffix
		}
	}
	return out
}

func componentDashboardURL(links config.Links, component config.Component) string {
	if links.Grafana == "" {
		return ""
	}
	uid := links.ComponentDashboards[component.Type]
	if uid == "" {
		return ""
	}
	base := strings.TrimRight(links.Grafana, "/")
	values := url.Values{}
	params := links.ComponentDashboardParams[component.Type]
	if len(params) > 0 {
		replacer := strings.NewReplacer(
			"{instance}", component.Prometheus.Instance,
			"{host}", componentHost(component.Prometheus.Instance),
			"{job}", component.Prometheus.Job,
			"{component}", component.Name,
			"{type}", component.Type,
		)
		for key, template := range params {
			values.Set(key, replacer.Replace(template))
		}
	} else {
		values.Set("var-component", component.Name)
		if component.Prometheus.Job != "" {
			values.Set("var-job", component.Prometheus.Job)
		}
		if component.Prometheus.Instance != "" {
			values.Set("var-instance", component.Prometheus.Instance)
		}
	}
	return base + "/d/" + url.PathEscape(uid) + "?" + values.Encode()
}

func componentHost(instance string) string {
	value := instance
	if index := strings.Index(value, "://"); index >= 0 {
		value = value[index+3:]
	}
	if index := strings.LastIndex(value, ":"); index > 0 {
		value = value[:index]
	}
	return value
}

func markdownCode(value string) string {
	value = strings.ReplaceAll(value, "`", "'")
	value = strings.ReplaceAll(value, "\n", " ")
	return value
}

func markdownText(value string) string {
	value = markdownCode(value)
	value = strings.ReplaceAll(value, "[", "\\[")
	value = strings.ReplaceAll(value, "]", "\\]")
	return value
}
