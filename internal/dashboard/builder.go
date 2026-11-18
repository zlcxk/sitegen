package dashboard

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"strings"

	"site-observability/internal/config"
	"site-observability/internal/metrics"
	templateassets "site-observability/templates"
)

const datasourceVarName = "datasource"
const datasourceUID = "${datasource}"

type builder struct {
	site           *config.Site
	registry       *metrics.Registry
	links          config.Links
	sections       map[string][]Panel
	entryHTTPWarn  bool
	k8sMissingWarn bool
	containerWarn  bool
}

// Build creates a deterministic dashboard from a validated site configuration.
// The embedded base template supplies Grafana metadata and visual defaults;
// rows, panels, variables and links are generated from the site graph.
func Build(site *config.Site, registry *metrics.Registry, links config.Links) (*Dashboard, error) {
	if site == nil {
		return nil, fmt.Errorf("站点配置为空")
	}
	if registry == nil {
		return nil, fmt.Errorf("指标模板注册表为空")
	}
	base, baseDigest, err := loadBaseDashboard(templateassets.FS, "dashboard/site-overview.json")
	if err != nil {
		return nil, err
	}

	d := base
	d.Editable = true
	d.SchemaVersion = 39
	d.Refresh = "30s"
	d.Time = TimeRange{From: "now-1h", To: "now"}
	d.Timezone = "browser"
	d.Version = 1
	d.ID = nil
	d.UID = DashboardUID(site.Metadata.ID)
	d.Title = site.Metadata.Name + " · 站点全链路观测"
	description := strings.TrimSpace(site.Metadata.Description)
	if description != "" {
		description += "\n\n"
	}
	description += "由 sitegen 根据站点 YAML 生成，请勿手工修改。"
	d.Description = description
	d.Tags = []string{
		"sitegen",
		"site:" + site.Metadata.ID,
		"environment:" + site.Metadata.Environment,
		registry.TemplateVersionTag(),
		"sitegen-dashboard-template:" + baseDigest,
	}
	d.Links = buildDashboardLinks(links)

	b := &builder{
		site:     site,
		registry: registry,
		links:    links,
		sections: map[string][]Panel{},
	}
	b.addDomainPanels()
	if err := b.addApplicationPanels(); err != nil {
		return nil, fmt.Errorf("站点 %s: %w", site.Metadata.ID, err)
	}
	b.addComponentPanels()
	b.addServicePanels()

	variables, err := buildVariables(site, registry)
	if err != nil {
		return nil, err
	}
	d.Templating = Templating{List: variables}
	d.Panels = b.layout()

	if err := validatePanelQueries(d.Panels); err != nil {
		return nil, err
	}
	if err := Validate(&d); err != nil {
		return nil, fmt.Errorf("生成 Dashboard 校验失败: %w", err)
	}
	return &d, nil
}

func validatePanelQueries(panels []Panel) error {
	for _, panel := range panels {
		for _, target := range panel.Targets {
			if err := metrics.ValidateQuery(target.Expr); err != nil {
				return fmt.Errorf("面板 %q 的 PromQL 无效: %w\n原始表达式:\n%s", panel.Title, err, target.Expr)
			}
		}
		if panel.Panels != nil {
			if err := validatePanelQueries(*panel.Panels); err != nil {
				return err
			}
		}
	}
	return nil
}

// DashboardUID returns a stable, Grafana-safe UID (maximum 40 characters).
func DashboardUID(siteID string) string {
	return "site-" + siteID
}

func loadBaseDashboard(fsys fs.FS, file string) (Dashboard, string, error) {
	data, err := fs.ReadFile(fsys, file)
	if err != nil {
		return Dashboard{}, "", fmt.Errorf("读取 Dashboard 基础模板: %w", err)
	}
	var d Dashboard
	if err := json.Unmarshal(data, &d); err != nil {
		return Dashboard{}, "", fmt.Errorf("解析 Dashboard 基础模板: %w", err)
	}
	if d.SchemaVersion == 0 {
		return Dashboard{}, "", fmt.Errorf("Dashboard 基础模板缺少 schemaVersion")
	}
	sum := sha256.Sum256(data)
	return d, fmt.Sprintf("%x", sum[:8]), nil
}

func (b *builder) addDomainPanels() {
	profile, ok := b.registry.AutoProfile("blackbox")
	if !ok {
		return
	}
	for _, domain := range b.site.Spec.Domains {
		selector := metrics.DomainSelector(domain)
		for _, panel := range profile.Panels {
			query := metrics.RenderQuery(panel.Query, selector)
			b.addTemplatePanel(panel, domain.ID+" / "+panel.Name, query, nil)
		}
	}
}

func (b *builder) addApplicationPanels() error {
	for _, id := range b.registry.Order {
		profile := b.registry.Profiles[id]
		if profile.Auto || profile.Target != config.TargetApplication {
			continue
		}
		apps := b.site.ApplicationsWithMetric(profile.ID)
		if len(apps) == 0 {
			continue
		}
		for _, panel := range profile.Panels {
			switch profile.Scope {
			case metrics.ScopeApplication:
				if panel.Aggregation == metrics.AggregationSite {
					entry := b.site.EntryApplicationsWithMetric(profile.ID)
					if len(entry) == 0 {
						if profile.ID == "http" {
							b.addEntryApplicationWarning()
						}
						continue
					}
					query, err := metrics.RenderApplicationQuery(panel.Query, entry, false, "")
					if err != nil {
						return fmt.Errorf("生成面板 %q（入口应用 %s）: %w", panel.Name, appIDs(entry), err)
					}
					b.addTemplatePanel(panel, panel.Name, query, nil)
					continue
				}
				query, err := metrics.RenderApplicationQuery(panel.Query, apps, true, metrics.ProfileSelectorVar(profile.ID))
				if err != nil {
					return fmt.Errorf("生成面板 %q（应用 %s）: %w", panel.Name, appIDs(apps), err)
				}
				b.addTemplatePanel(panel, panel.Name, query, nil)
			case metrics.ScopeNamespace:
				k8sApps, missing := splitKubernetesApps(apps)
				if len(missing) > 0 {
					b.addKubernetesMissingWarning(missing)
				}
				if len(k8sApps) == 0 {
					continue
				}
				b.addContainerMatcherWarning(k8sApps)
				query, err := metrics.RenderNamespaceQuery(panel.Query, k8sApps, true, metrics.ProfileSelectorVar(profile.ID))
				if err != nil {
					return fmt.Errorf("生成面板 %q（应用 %s）: %w", panel.Name, appIDs(k8sApps), err)
				}
				b.addTemplatePanel(panel, panel.Name, query, nil)
			}
		}
	}

	if len(b.site.Spec.Applications) > 0 {
		if auto, ok := b.registry.AutoProfile("platform"); ok {
			for _, panel := range auto.Panels {
				query, err := metrics.RenderJobQuery(panel.Query, b.site.Spec.Applications)
				if err != nil {
					return fmt.Errorf("生成面板 %q（应用 %s）: %w", panel.Name, appIDs(b.site.Spec.Applications), err)
				}
				b.addTemplatePanel(panel, panel.Name, query, nil)
			}
		}
	}
	return nil
}

func (b *builder) addEntryApplicationWarning() {
	if b.entryHTTPWarn {
		return
	}
	b.entryHTTPWarn = true
	content := "### HTTP 入口应用未配置\n\n" +
		"站点启用了 HTTP 指标，但没有任何域名通过 `entryApplications` 声明入口应用。\n\n" +
		"为避免把下游应用的内部调用重复计入站点外部请求量，站点级 HTTP 汇总面板不会生成。\n\n" +
		"请在域名下显式声明入口应用（示例：`entryApplications: [rtd-admin-test]`），然后重新生成。"
	b.sections[metrics.SectionHTTP] = append(b.sections[metrics.SectionHTTP],
		textPanel("HTTP 入口应用未配置", content))
}

func (b *builder) addKubernetesMissingWarning(apps []config.Application) {
	if b.k8sMissingWarn {
		return
	}
	b.k8sMissingWarn = true
	var lines []string
	for _, app := range apps {
		lines = append(lines, fmt.Sprintf("- `%s`（%s/%s）：未配置 `kubernetes.podSelector`",
			markdownCode(app.ID), markdownCode(app.Application), markdownCode(app.Namespace)))
	}
	content := "### 未生成应用级 Kubernetes 面板\n\n" +
		"以下应用启用了 kubernetes 指标，但没有可用于识别 Pod 归属的标签或正则。\n" +
		"为避免把同一 Namespace 中其他应用的 Pod 计入统计，相关面板已被跳过。\n\n" +
		strings.Join(lines, "\n")
	b.sections[metrics.SectionEnvironment] = append(b.sections[metrics.SectionEnvironment],
		textPanel("Kubernetes Pod 归属缺失", content))
}

func (b *builder) addContainerMatcherWarning(apps []config.Application) {
	if b.containerWarn {
		return
	}
	var missing []string
	for _, app := range apps {
		if len(app.Kubernetes.ContainerMatchers) == 0 {
			missing = append(missing, "`"+markdownCode(app.ID)+"`")
		}
	}
	if len(missing) == 0 {
		return
	}
	b.containerWarn = true
	content := "### 容器过滤条件未配置\n\n" +
		"以下应用未配置 `kubernetes.containerMatchers`：" + strings.Join(missing, "、") + "。\n\n" +
		"Pod CPU/内存可能包含 Pause 容器或 Pod 父级 cgroup 序列。请通过 `sitegen check` 核对真实标签结构后，在应用配置中补充容器过滤条件（例如 image 或 container 标签）。"
	b.sections[metrics.SectionEnvironment] = append(b.sections[metrics.SectionEnvironment],
		textPanel("容器过滤条件提示", content))
}

func splitKubernetesApps(apps []config.Application) (withSelector, missing []config.Application) {
	for _, app := range apps {
		if app.Kubernetes.PodSelector == nil {
			missing = append(missing, app)
			continue
		}
		withSelector = append(withSelector, app)
	}
	return withSelector, missing
}

func (b *builder) addComponentPanels() {
	if len(b.site.Spec.Components) == 0 {
		return
	}
	var linkLines []string
	var inventory []string
	var unconfigured []string
	for _, component := range b.site.Spec.Components {
		profiles := b.activeComponentProfiles(component)
		linkURL := componentDashboardURL(b.links, component)
		var panelLinks []PanelLink
		if linkURL != "" {
			panelLinks = []PanelLink{{
				Title:       "打开 " + component.Name + " 专业 Dashboard",
				URL:         linkURL,
				TargetBlank: true,
			}}
			linkLines = append(linkLines, fmt.Sprintf("- [%s](<%s>)", markdownText(component.Name), linkURL))
		}
		inventory = append(inventory, fmt.Sprintf("- `%s` %s（类型 `%s`，job `%s`）",
			markdownCode(component.ID), markdownText(component.Name), markdownCode(component.Type), markdownCode(component.Prometheus.Job)))
		if len(profiles) == 0 && linkURL == "" {
			unconfigured = append(unconfigured, fmt.Sprintf("- `%s`（%s）：未启用指标模板，也未配置专业 Dashboard 链接",
				markdownCode(component.ID), markdownCode(component.Type)))
		}
		for _, profile := range profiles {
			selector := metrics.ComponentSelector(component)
			for _, panel := range profile.Panels {
				title := component.Name + " / " + panel.Name
				query := metrics.RenderQuery(panel.Query, selector)
				b.addTemplatePanel(panel, title, query, panelLinks)
			}
		}
	}
	var content []string
	content = append(content, "### 站点依赖组件", "")
	if len(inventory) > 0 {
		content = append(content, inventory...)
	} else {
		content = append(content, "站点未配置依赖组件。")
	}
	if len(linkLines) > 0 {
		content = append(content, "", "### 专业 Dashboard 下钻", "")
		content = append(content, linkLines...)
	}
	if len(unconfigured) > 0 {
		content = append(content, "", "### 未接入指标的组件", "")
		content = append(content, unconfigured...)
	}
	content = append(content, "", "组件状态面板只在站点健康总览中生成一次，依赖组件的性能指标请进入专业 Dashboard 查看。")
	b.sections[metrics.SectionComponent] = append(b.sections[metrics.SectionComponent],
		textPanel("依赖组件与下钻", strings.Join(content, "\n")))
}

func (b *builder) addServicePanels() {
	panels := buildServicePanels(b.site, b.links)
	b.sections[metrics.SectionService] = append(b.sections[metrics.SectionService], panels...)
}

func (b *builder) activeComponentProfiles(component config.Component) []*metrics.Profile {
	enabled := map[string]bool{}
	for _, id := range component.Metrics {
		enabled[id] = true
	}
	var out []*metrics.Profile
	for _, id := range b.registry.Order {
		profile := b.registry.Profiles[id]
		if profile.Auto || profile.Target != config.TargetComponent || !enabled[id] {
			continue
		}
		if len(profile.ComponentTypes) > 0 && !contains(profile.ComponentTypes, component.Type) {
			continue
		}
		out = append(out, profile)
	}
	return out
}

func (b *builder) addTemplatePanel(template metrics.Panel, title, query string, links []PanelLink) {
	for _, section := range template.Sections {
		panel := renderPanel(template, title, query)
		panel.Links = links
		b.sections[section] = append(b.sections[section], panel)
	}
}

func (b *builder) layout() []Panel {
	var panels []Panel
	nextID := 1
	y := 0
	for _, section := range metrics.SectionOrder {
		sectionPanels := b.sections[section]
		if len(sectionPanels) == 0 {
			continue
		}
		collapsed := section != metrics.SectionOverview
		row := Panel{
			Collapsed: &collapsed,
			ID:        nextID,
			Title:     metrics.SectionTitle[section],
			Type:      "row",
			GridPos:   GridPos{X: 0, Y: y, W: 24, H: 1},
		}
		nextID++
		if collapsed {
			children, _ := packPanels(sectionPanels, section, &nextID, 1)
			row.Panels = &children
			panels = append(panels, row)
			y++
			continue
		}
		empty := []Panel{}
		row.Panels = &empty
		panels = append(panels, row)
		y++

		top, bottom := packPanels(sectionPanels, section, &nextID, y)
		panels = append(panels, top...)
		if bottom > y {
			y = bottom
		}
	}
	return panels
}

func packPanels(items []Panel, section string, nextID *int, startY int) ([]Panel, int) {
	out := make([]Panel, 0, len(items))
	x, y, rowHeight := 0, startY, 0
	for i := range items {
		panel := items[i]
		width, height := panelSize(section, panel.Type)
		if x+width > 24 {
			x = 0
			y += rowHeight
			rowHeight = 0
		}
		panel.GridPos = GridPos{X: x, Y: y, W: width, H: height}
		panel.ID = *nextID
		*nextID = *nextID + 1
		out = append(out, panel)
		x += width
		if height > rowHeight {
			rowHeight = height
		}
	}
	return out, y + rowHeight
}

func panelSize(section, panelType string) (int, int) {
	switch section {
	case metrics.SectionOverview:
		if panelType == "stat" {
			return 6, 5
		}
		return 8, 6
	case metrics.SectionHTTP:
		if panelType == "table" {
			return 12, 9
		}
		return 12, 8
	case metrics.SectionRuntime:
		return 12, 8
	case metrics.SectionService:
		return 12, 6
	case metrics.SectionComponent:
		return 12, 6
	case metrics.SectionEnvironment:
		return 12, 8
	default:
		return 12, 8
	}
}

func appIDs(apps []config.Application) string {
	ids := make([]string, 0, len(apps))
	for _, app := range apps {
		ids = append(ids, app.ID)
	}
	return strings.Join(ids, ", ")
}

func contains(items []string, value string) bool {
	for _, item := range items {
		if item == value {
			return true
		}
	}
	return false
}
