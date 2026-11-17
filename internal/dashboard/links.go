package dashboard

import (
	"fmt"
	"net/url"
	"strings"

	"site-observability/internal/config"
)

func buildDashboardLinks(links config.Links) []DashboardLink {
	var out []DashboardLink
	add := func(title, raw string) {
		if raw == "" {
			return
		}
		out = append(out, DashboardLink{
			AsDropdown:  false,
			Icon:        "external link",
			IncludeVars: false,
			KeepTime:    false,
			Tags:        []string{},
			TargetBlank: true,
			Title:       title,
			Tooltip:     title,
			Type:        "link",
			URL:         raw,
		})
	}
	add("SkyWalking", links.Skywalking)
	add("Prometheus", links.Prometheus)
	add("Grafana", links.Grafana)
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
	values.Set("var-component", component.Name)
	if component.Prometheus.Job != "" {
		values.Set("var-job", component.Prometheus.Job)
	}
	if component.Prometheus.Instance != "" {
		values.Set("var-instance", component.Prometheus.Instance)
	}
	return base + "/d/" + url.PathEscape(uid) + "?" + values.Encode()
}

func buildServicePanels(site *config.Site, links config.Links) []Panel {
	base := markdownLink("打开 SkyWalking UI", links.Skywalking)
	serviceTemplate := links.SkywalkingServiceURLTemplate
	endpointTemplate := links.SkywalkingEndpointURLTemplate

	var serviceLines []string
	for _, app := range site.Spec.Applications {
		serviceURL := expandSkywalkingURL(serviceTemplate, site, app)
		if serviceURL == "" {
			serviceURL = links.Skywalking
		}
		line := fmt.Sprintf("- `%s` (%s) → `%s`",
			markdownCode(app.Application), markdownCode(app.Namespace), markdownCode(app.Skywalking.Service))
		if serviceURL != "" {
			line += " · " + markdownLink("Service 页面", serviceURL)
		}
		serviceLines = append(serviceLines, line)
	}

	topology := []string{"### SkyWalking 服务拓扑", ""}
	if base != "" {
		topology = append(topology, base, "")
	} else {
		topology = append(topology, "未配置 SkyWalking UI 地址。请在 `sitegen.yaml` 或站点 `spec.links.skywalking` 中配置。", "")
	}
	topology = append(topology, "SkyWalking 拓扑不在本 Dashboard 中伪造；此处只提供页面入口与 YAML 声明的服务映射。")
	if len(serviceLines) > 0 {
		topology = append(topology, "", "**站点服务映射**", "")
		topology = append(topology, serviceLines...)
	}
	if depLines := explicitDependencyLines(site); len(depLines) > 0 {
		topology = append(topology, "", "**YAML 显式依赖（配置关系，非 SkyWalking 实测拓扑）**", "")
		topology = append(topology, depLines...)
	}

	endpoint := []string{"### SkyWalking Endpoint 排行", ""}
	if endpointTemplate != "" {
		var lines []string
		for _, app := range site.Spec.Applications {
			u := expandSkywalkingURL(endpointTemplate, site, app)
			if u != "" {
				lines = append(lines, "- "+markdownCode(app.Skywalking.Service)+" · "+markdownLink("Endpoint 页面", u))
			}
		}
		if len(lines) == 0 {
			endpoint = append(endpoint, "未配置可用的 Endpoint URL 模板。")
		} else {
			endpoint = append(endpoint, lines...)
		}
	} else if base != "" {
		endpoint = append(endpoint, base, "", "未配置 `skywalkingEndpointURLTemplate`，请从 SkyWalking UI 中进入 Endpoint 排行。")
	} else {
		endpoint = append(endpoint, "未配置 SkyWalking UI 地址。")
	}
	if format := defaultEndpointFormat(site); format != "" {
		endpoint = append(endpoint, "", "YAML 声明的 Endpoint 维度格式：`"+markdownCode(format)+"`")
	}

	trace := []string{"### SkyWalking 慢 Trace", ""}
	if base != "" {
		trace = append(trace, base, "")
	}
	trace = append(trace, "本版本不自动构造复杂 Trace URL；请从 SkyWalking 服务或 Endpoint 页面进入慢 Trace 查询。")

	calls := []string{"### SkyWalking 服务调用量与成功率", ""}
	if base != "" {
		calls = append(calls, base, "")
	}
	calls = append(calls, "SkyWalking 调用指标没有在本 Dashboard 中伪装成 Prometheus 数据；请使用 SkyWalking 服务拓扑和指标页面查看。")

	return []Panel{
		textPanel("SkyWalking 服务拓扑", strings.Join(topology, "\n")),
		textPanel("SkyWalking Endpoint 排行", strings.Join(endpoint, "\n")),
		textPanel("SkyWalking 慢 Trace", strings.Join(trace, "\n")),
		textPanel("SkyWalking 服务调用", strings.Join(calls, "\n")),
	}
}

func explicitDependencyLines(site *config.Site) []string {
	labels := map[string]string{}
	for _, app := range site.Spec.Applications {
		labels[app.ID] = app.Application + "/" + app.Namespace
	}
	for _, component := range site.Spec.Components {
		labels[component.ID] = component.Name
	}
	var lines []string
	for _, app := range site.Spec.Applications {
		if len(app.Dependencies) == 0 {
			continue
		}
		var deps []string
		for _, dep := range app.Dependencies {
			label := labels[dep]
			if label == "" {
				label = dep
			}
			deps = append(deps, markdownCode(label))
		}
		lines = append(lines, "- `"+markdownCode(app.Application+"/"+app.Namespace)+"` → "+strings.Join(deps, ", "))
	}
	return lines
}

func defaultEndpointFormat(site *config.Site) string {
	if len(site.Spec.Applications) == 0 {
		return ""
	}
	format := site.Spec.Applications[0].Skywalking.EndpointFormat
	for _, app := range site.Spec.Applications[1:] {
		if app.Skywalking.EndpointFormat != format {
			return ""
		}
	}
	return format
}

func expandSkywalkingURL(template string, site *config.Site, app config.Application) string {
	if template == "" {
		return ""
	}
	replacer := strings.NewReplacer(
		"{service}", url.QueryEscape(app.Skywalking.Service),
		"{application}", url.QueryEscape(app.Application),
		"{namespace}", url.QueryEscape(app.Namespace),
		"{site}", url.QueryEscape(site.Metadata.ID),
	)
	return replacer.Replace(template)
}

func markdownLink(label, raw string) string {
	if raw == "" {
		return ""
	}
	safe := strings.NewReplacer(" ", "%20", "<", "%3C", ">", "%3E").Replace(raw)
	return "[" + label + "](<" + safe + ">)"
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
