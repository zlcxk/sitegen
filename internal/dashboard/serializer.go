package dashboard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"site-observability/internal/config"
	"site-observability/internal/metrics"
	"site-observability/internal/output"
)

var uidRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)

// BuildJSON builds and serializes a dashboard with deterministic formatting.
func BuildJSON(site *config.Site, registry *metrics.Registry, links config.Links) ([]byte, error) {
	d, err := Build(site, registry, links)
	if err != nil {
		return nil, err
	}
	return Marshal(d)
}

// Marshal validates and serializes the dashboard, then re-parses the JSON to
// guarantee the emitted document is syntactically valid.
func Marshal(d *Dashboard) ([]byte, error) {
	if err := Validate(d); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(d); err != nil {
		return nil, fmt.Errorf("序列化 Dashboard: %w", err)
	}
	data := buf.Bytes()
	var check any
	if err := json.Unmarshal(data, &check); err != nil {
		return nil, fmt.Errorf("Dashboard JSON 重新解析失败: %w", err)
	}
	return data, nil
}

// Write validates, serializes and atomically writes a dashboard. The previous
// file is preserved on any failure and copied to <path>.bak on success.
func Write(d *Dashboard, path string) error {
	data, err := Marshal(d)
	if err != nil {
		return err
	}
	if err := output.WriteAtomic(path, data, 0o644, true); err != nil {
		return fmt.Errorf("写入 Dashboard %s: %w", path, err)
	}
	return nil
}

var dashboardVarRE = regexp.MustCompile(`\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?`)

// Validate performs structural and layout checks required before a dashboard
// can be written or imported.
func Validate(d *Dashboard) error {
	if d == nil {
		return fmt.Errorf("Dashboard 为空")
	}
	if strings.TrimSpace(d.Title) == "" {
		return fmt.Errorf("Dashboard title 不能为空")
	}
	if !uidRE.MatchString(d.UID) {
		return fmt.Errorf("Dashboard UID %q 非法（1-40 位字母、数字、下划线或连字符）", d.UID)
	}
	if d.Time.From == "" || d.Time.To == "" {
		return fmt.Errorf("Dashboard 时间范围不能为空")
	}
	if d.Refresh == "" {
		return fmt.Errorf("Dashboard refresh 不能为空")
	}
	if len(d.Panels) == 0 {
		return fmt.Errorf("Dashboard 至少需要一个面板或 Row")
	}

	ids := map[int]bool{}
	if err := validatePanelList(d.Panels, ids); err != nil {
		return err
	}
	return validateVariables(d)
}

func validatePanelList(panels []Panel, ids map[int]bool) error {
	occupied := map[[2]int]int{}
	for _, panel := range panels {
		if panel.ID <= 0 {
			return fmt.Errorf("面板 %q 的 ID 必须大于 0", panel.Title)
		}
		if ids[panel.ID] {
			return fmt.Errorf("面板 ID %d 重复", panel.ID)
		}
		ids[panel.ID] = true
		if strings.TrimSpace(panel.Title) == "" {
			return fmt.Errorf("面板 %d 的标题不能为空", panel.ID)
		}
		if err := validateGrid(panel); err != nil {
			return fmt.Errorf("面板 %q: %w", panel.Title, err)
		}
		for y := panel.GridPos.Y; y < panel.GridPos.Y+panel.GridPos.H; y++ {
			for x := panel.GridPos.X; x < panel.GridPos.X+panel.GridPos.W; x++ {
				cell := [2]int{x, y}
				if other, ok := occupied[cell]; ok {
					return fmt.Errorf("面板 %d 与面板 %d 在网格位置 (%d,%d) 重叠", panel.ID, other, x, y)
				}
				occupied[cell] = panel.ID
			}
		}

		switch panel.Type {
		case "row":
			if panel.Panels == nil {
				return fmt.Errorf("Row %q 缺少 panels 字段", panel.Title)
			}
			if panel.Collapsed == nil {
				return fmt.Errorf("Row %q 缺少 collapsed 字段", panel.Title)
			}
			if err := validatePanelList(*panel.Panels, ids); err != nil {
				return fmt.Errorf("Row %q: %w", panel.Title, err)
			}
		case "text":
			content, _ := panel.Options["content"].(string)
			if strings.TrimSpace(content) == "" {
				return fmt.Errorf("文本面板 %q 内容为空", panel.Title)
			}
		case "timeseries", "stat", "table", "bargauge", "gauge":
			if panel.Datasource == nil || panel.Datasource.UID != datasourceUID {
				return fmt.Errorf("数据面板 %q 未绑定 %s 数据源", panel.Title, datasourceVarName)
			}
			if len(panel.Targets) == 0 {
				return fmt.Errorf("数据面板 %q 没有查询", panel.Title)
			}
			for _, target := range panel.Targets {
				if strings.TrimSpace(target.Expr) == "" {
					return fmt.Errorf("面板 %q 存在空查询", panel.Title)
				}
				if strings.Contains(target.Expr, "%SELECTOR%") {
					return fmt.Errorf("面板 %q 存在未替换的 %%SELECTOR%% 占位符", panel.Title)
				}
				if strings.Contains(target.Expr, "{{") {
					return fmt.Errorf("面板 %q 存在未替换的模板占位符", panel.Title)
				}
			}
		default:
			return fmt.Errorf("面板 %q 使用了不支持的类型 %q", panel.Title, panel.Type)
		}
	}
	return nil
}

func validateVariables(d *Dashboard) error {
	defined := map[string]bool{}
	queries := map[string]string{}
	hasDatasourceVar := false
	for _, variable := range d.Templating.List {
		if variable.Name == "" {
			return fmt.Errorf("Dashboard 变量名不能为空")
		}
		if defined[variable.Name] {
			return fmt.Errorf("Dashboard 变量 %q 重复", variable.Name)
		}
		defined[variable.Name] = true
		if variable.Name == datasourceVarName {
			hasDatasourceVar = true
		}
		if variable.Query == nil {
			return fmt.Errorf("Dashboard 变量 %q 的 query 不能为空", variable.Name)
		}
		if query, ok := variable.Query.(string); ok {
			queries[variable.Name] = query
		}
	}
	if !hasDatasourceVar {
		return fmt.Errorf("Dashboard 缺少 %s 数据源变量", datasourceVarName)
	}

	used := map[string]bool{datasourceVarName: true}
	checkRefs := func(expr string) error {
		for _, match := range dashboardVarRE.FindAllStringSubmatch(expr, -1) {
			name := match[1]
			if strings.HasPrefix(name, "__") {
				continue
			}
			if !defined[name] {
				return fmt.Errorf("查询引用了未定义的 Dashboard 变量 $%s", name)
			}
			used[name] = true
		}
		return nil
	}
	for name, query := range queries {
		if err := checkRefs(query); err != nil {
			return fmt.Errorf("变量 %q: %w", name, err)
		}
	}
	if err := walkPanelQueries(d.Panels, checkRefs); err != nil {
		return err
	}
	for name := range defined {
		if !used[name] {
			return fmt.Errorf("Dashboard 变量 %q 没有被任何面板或变量查询使用；不允许保留装饰性变量", name)
		}
	}
	if defined["pod"] && !defined["app_selector"] {
		return fmt.Errorf("变量 pod 依赖 app_selector，但 app_selector 未定义")
	}
	if defined["pod"] {
		query := queries["pod"]
		if !dashboardVarRE.MatchString(query) || !strings.Contains(query, "app_selector") {
			return fmt.Errorf("变量 pod 必须依赖 app_selector，确保只显示当前应用关联的 Pod")
		}
	}
	return nil
}

func walkPanelQueries(panels []Panel, fn func(string) error) error {
	for _, panel := range panels {
		for _, target := range panel.Targets {
			if err := fn(target.Expr); err != nil {
				return fmt.Errorf("面板 %q: %w", panel.Title, err)
			}
		}
		if panel.Panels != nil {
			if err := walkPanelQueries(*panel.Panels, fn); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateGrid(panel Panel) error {
	pos := panel.GridPos
	if pos.W <= 0 || pos.H <= 0 {
		return fmt.Errorf("网格宽高必须大于 0")
	}
	if pos.X < 0 || pos.Y < 0 {
		return fmt.Errorf("网格坐标不能为负数")
	}
	if pos.X+pos.W > 24 {
		return fmt.Errorf("网格宽度超出 24 列（x=%d, w=%d）", pos.X, pos.W)
	}
	return nil
}

// ValidateJSON parses serialized JSON and validates the resulting dashboard.
func ValidateJSON(data []byte) error {
	var d Dashboard
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&d); err != nil {
		return fmt.Errorf("Dashboard JSON 解析失败: %w", err)
	}
	return Validate(&d)
}
