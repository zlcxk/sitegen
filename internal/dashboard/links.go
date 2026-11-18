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
	if len(site.Spec.Applications) == 0 {
		return nil
	}
	lines := []string{"### SkyWalking 服务映射", ""}
	anyBase, anyServiceTemplate, anyEndpointTemplate := false, false, false
	for _, app := range site.Spec.Applications {
		resolved := resolveSkywalkingLinks(links, app)
		if resolved.baseURL != "" {
			anyBase = true
		}
		if resolved.serviceTemplate != "" {
			anyServiceTemplate = true
		}
		if resolved.endpointTemplate != "" {
			anyEndpointTemplate = true
		}
		line := fmt.Sprintf("- `%s/%s` → SkyWalking Service `%s`",
			markdownCode(app.Application), markdownCode(app.Namespace), markdownCode(app.Skywalking.Service))
		if resolved.serviceTemplate != "" {
			if serviceURL := expandSkywalkingURL(resolved.serviceTemplate, site, app); serviceURL != "" {
				line += " · " + markdownLink("Service 页面", serviceURL)
			}
		} else if resolved.baseURL != "" {
			line += " · " + markdownLink("SkyWalking UI", resolved.baseURL) + "（未配置 Service URL 模板）"
		}
		lines = append(lines, line)
		if resolved.endpointTemplate != "" {
			if endpointURL := expandSkywalkingURL(resolved.endpointTemplate, site, app); endpointURL != "" {
				lines = append(lines, "  - "+markdownLink("Endpoint 页面", endpointURL))
			}
		}
	}
	if !anyBase && !anyServiceTemplate && !anyEndpointTemplate {
		lines = append(lines,
			"未配置 SkyWalking UI 地址。以下是 YAML 中声明的 Prometheus 应用与 SkyWalking Service 映射关系；配置 `spec.links.skywalking` 或应用的 `skywalking.ui` 后可从这里下钻。", "")
	}
	if !anyServiceTemplate {
		lines = append(lines, "", "未配置 Service URL 模板（`skywalkingServiceURLTemplate` 或应用级 `skywalking.ui.serviceURLTemplate`）；不会把 SkyWalking 首页伪装成具体 Service 页面。")
	}
	if !anyEndpointTemplate {
		lines = append(lines, "", "未配置 Endpoint URL 模板（`skywalkingEndpointURLTemplate` 或应用级 `skywalking.ui.endpointURLTemplate`）；不会生成 Endpoint 专属链接，请从 SkyWalking Service 页面进入 Endpoint 排行。")
	}
	if format := defaultEndpointFormat(site); format != "" {
		lines = append(lines, "", "YAML 声明的 Endpoint 维度格式：`"+markdownCode(format)+"`")
	}
	panels := []Panel{textPanel("SkyWalking 服务映射与下钻", strings.Join(lines, "\n"))}

	if depLines := explicitDependencyLines(site); len(depLines) > 0 {
		content := "### YAML 显式依赖（配置关系，非 SkyWalking 实测拓扑）\n\n" +
			strings.Join(depLines, "\n") +
			"\n\n本 Dashboard 不伪造调用拓扑；实际调用关系请以 SkyWalking 服务拓扑和 Trace 为准。"
		panels = append(panels, textPanel("YAML 配置依赖", content))
	}
	return panels
}

type resolvedSkywalkingLinks struct {
	baseURL          string
	serviceTemplate  string
	endpointTemplate string
}

// resolveSkywalkingLinks prefers application-specific UI configuration and
// falls back to the global tool configuration.
func resolveSkywalkingLinks(global config.Links, app config.Application) resolvedSkywalkingLinks {
	out := resolvedSkywalkingLinks{
		baseURL:          global.Skywalking,
		serviceTemplate:  global.SkywalkingServiceURLTemplate,
		endpointTemplate: global.SkywalkingEndpointURLTemplate,
	}
	ui := app.Skywalking.UI
	if ui.BaseURL != "" {
		out.baseURL = ui.BaseURL
	}
	if ui.ServiceURLTemplate != "" {
		out.serviceTemplate = ui.ServiceURLTemplate
	}
	if ui.EndpointURLTemplate != "" {
		out.endpointTemplate = ui.EndpointURLTemplate
	}
	return out
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
