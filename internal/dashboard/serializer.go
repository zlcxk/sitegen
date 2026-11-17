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

	varNames := map[string]bool{}
	hasDatasourceVar := false
	for _, variable := range d.Templating.List {
		if variable.Name == "" {
			return fmt.Errorf("Dashboard 变量名不能为空")
		}
		if varNames[variable.Name] {
			return fmt.Errorf("Dashboard 变量 %q 重复", variable.Name)
		}
		varNames[variable.Name] = true
		if variable.Name == datasourceVarName {
			hasDatasourceVar = true
		}
		if variable.Query == nil {
			return fmt.Errorf("Dashboard 变量 %q 的 query 不能为空", variable.Name)
		}
	}
	if !hasDatasourceVar {
		return fmt.Errorf("Dashboard 缺少 %s 数据源变量", datasourceVarName)
	}

	ids := map[int]bool{}
	occupied := map[[2]int]int{}
	for _, panel := range d.Panels {
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
