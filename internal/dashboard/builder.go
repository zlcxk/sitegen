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
	site     *config.Site
	registry *metrics.Registry
	links    config.Links
	sections map[string][]Panel
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
	b.addApplicationPanels()
	b.addComponentPanels()
	b.addServicePanels()

	d.Templating = Templating{List: buildVariables(site)}
	d.Panels = b.layout()

	if err := Validate(&d); err != nil {
		return nil, fmt.Errorf("生成 Dashboard 校验失败: %w", err)
	}
	return &d, nil
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
			b.addTemplatePanel(panel, domain.ID+" / "+panel.Name, query, domain.ID, nil)
		}
	}
}

func (b *builder) addApplicationPanels() {
	active := b.activeApplicationProfiles()
	for _, profile := range active {
		selector := ""
		switch profile.Scope {
		case metrics.ScopeApplication:
			selector = metrics.ApplicationSelector(b.site)
		case metrics.ScopeNamespace:
			selector = metrics.NamespaceSelector(b.site)
		default:
			continue
		}
		for _, panel := range profile.Panels {
			query := metrics.RenderQuery(panel.Query, selector)
			b.addTemplatePanel(panel, panel.Name, query, "", nil)
		}
	}
	if len(b.site.Spec.Applications) > 0 {
		if auto, ok := b.registry.AutoProfile("platform"); ok {
			selector := `job=~` + metrics.QuoteLabelValue(metrics.AppJobsRegex(b.site))
			for _, panel := range auto.Panels {
				b.addTemplatePanel(panel, panel.Name, metrics.RenderQuery(panel.Query, selector), "", nil)
			}
		}
	}
}

func (b *builder) addComponentPanels() {
	var linkLines []string
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
		if len(profiles) == 0 && linkURL == "" {
			unconfigured = append(unconfigured, fmt.Sprintf("- `%s`（%s）：未启用指标模板，也未配置专业 Dashboard 链接",
				markdownCode(component.ID), markdownCode(component.Type)))
		}
		for _, profile := range profiles {
			selector := metrics.ComponentSelector(component)
			for _, panel := range profile.Panels {
				title := component.Name + " / " + panel.Name
				query := metrics.RenderQuery(panel.Query, selector)
				b.addTemplatePanel(panel, title, query, component.Name, panelLinks)
			}
		}
	}
	if len(linkLines) > 0 {
		content := "### 依赖组件专业 Dashboard\n\n" + strings.Join(linkLines, "\n")
		b.sections[metrics.SectionComponent] = append(b.sections[metrics.SectionComponent],
			textPanel("组件 Dashboard 下钻", content))
	}
	if len(unconfigured) > 0 {
		content := "### 未接入指标的组件\n\n" + strings.Join(unconfigured, "\n")
		b.sections[metrics.SectionComponent] = append(b.sections[metrics.SectionComponent],
			textPanel("组件说明", content))
	}
}

func (b *builder) addServicePanels() {
	panels := buildServicePanels(b.site, b.links)
	b.sections[metrics.SectionService] = append(b.sections[metrics.SectionService], panels...)
}

func (b *builder) activeApplicationProfiles() []*metrics.Profile {
	enabled := map[string]bool{}
	for _, app := range b.site.Spec.Applications {
		for _, id := range app.Metrics {
			enabled[id] = true
		}
	}
	var out []*metrics.Profile
	for _, id := range b.registry.Order {
		profile := b.registry.Profiles[id]
		if profile.Auto || profile.Target != config.TargetApplication || !enabled[id] {
			continue
		}
		out = append(out, profile)
	}
	return out
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

func (b *builder) addTemplatePanel(template metrics.Panel, title, query, suffix string, links []PanelLink) {
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
		collapsed := false
		row := Panel{
			Collapsed: &collapsed,
			ID:        nextID,
			Title:     metrics.SectionTitle[section],
			Type:      "row",
			GridPos:   GridPos{X: 0, Y: y, W: 24, H: 1},
			Panels:    &[]Panel{},
		}
		nextID++
		panels = append(panels, row)
		y++

		x, rowHeight := 0, 0
		for i := range sectionPanels {
			panel := sectionPanels[i]
			width, height := panelSize(section, panel.Type)
			if x+width > 24 {
				x = 0
				y += rowHeight
				rowHeight = 0
			}
			panel.GridPos = GridPos{X: x, Y: y, W: width, H: height}
			panel.ID = nextID
			nextID++
			panels = append(panels, panel)
			x += width
			if height > rowHeight {
				rowHeight = height
			}
		}
		y += rowHeight
	}
	return panels
}

func panelSize(section, panelType string) (int, int) {
	switch section {
	case metrics.SectionOverview:
		if panelType == "stat" {
			return 6, 5
		}
		return 8, 6
	case metrics.SectionHTTP:
		return 12, 8
	case metrics.SectionRuntime:
		return 12, 8
	case metrics.SectionService:
		return 12, 6
	case metrics.SectionComponent:
		return 8, 6
	case metrics.SectionEnvironment:
		return 12, 8
	default:
		return 12, 8
	}
}

func contains(items []string, value string) bool {
	for _, item := range items {
		if item == value {
			return true
		}
	}
	return false
}
