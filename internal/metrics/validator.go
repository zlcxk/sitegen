package metrics

import (
	"fmt"
	"strings"

	"site-observability/internal/config"
)

const (
	targetApplication = config.TargetApplication
	targetComponent   = config.TargetComponent
	targetDomain      = config.TargetDomain
)

func validateProfile(p *Profile) error {
	if !templateSlugRE.MatchString(p.ID) {
		return fmt.Errorf("profile ID %q 非法", p.ID)
	}
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("profile %s: name 不能为空", p.ID)
	}
	switch p.Target {
	case targetApplication, targetComponent, targetDomain:
	default:
		return fmt.Errorf("profile %s: target %q 非法（application/component/domain）", p.ID, p.Target)
	}
	switch p.Scope {
	case ScopeApplication, ScopeNamespace, ScopeComponent, ScopeDomain, ScopeJob, ScopeSkywalking:
	default:
		return fmt.Errorf("profile %s: scope %q 非法", p.ID, p.Scope)
	}
	switch p.Datasource {
	case "", "business", "skywalking_prometheus", "skywalking_datasource":
	default:
		return fmt.Errorf("profile %s: datasource %q 非法", p.ID, p.Datasource)
	}
	if p.Target != targetComponent && len(p.ComponentTypes) > 0 {
		return fmt.Errorf("profile %s: 只有 component profile 可以配置 componentTypes", p.ID)
	}
	if p.Target == targetComponent && len(p.ComponentTypes) == 0 {
		return fmt.Errorf("profile %s: component profile 必须配置 componentTypes", p.ID)
	}
	if p.Auto && p.Target == targetComponent {
		return fmt.Errorf("profile %s: component profile 不能是自动模板", p.ID)
	}
	if len(p.Panels) == 0 {
		return fmt.Errorf("profile %s: panels 不能为空", p.ID)
	}

	panelIDs := map[string]bool{}
	for i := range p.Panels {
		pn := &p.Panels[i]
		path := fmt.Sprintf("profile %s panel %s", p.ID, pn.ID)
		if !templateSlugRE.MatchString(pn.ID) {
			return fmt.Errorf("%s: panel ID 非法", path)
		}
		if panelIDs[pn.ID] {
			return fmt.Errorf("profile %s: panel ID %q 重复", p.ID, pn.ID)
		}
		panelIDs[pn.ID] = true
		if strings.TrimSpace(pn.Name) == "" {
			return fmt.Errorf("%s: name 不能为空", path)
		}
		if pn.Panel == "nodeGraph" {
			if pn.NodeGraph == nil {
				return fmt.Errorf("%s: nodeGraph 面板必须配置 nodeGraph.nodeMetrics/edgeMetrics", path)
			}
			if strings.TrimSpace(pn.NodeGraph.NodeMetrics) == "" || strings.TrimSpace(pn.NodeGraph.EdgeMetrics) == "" {
				return fmt.Errorf("%s: nodeGraph.nodeMetrics/edgeMetrics 不能为空", path)
			}
		} else if strings.TrimSpace(pn.Query) == "" {
			return fmt.Errorf("%s: query 不能为空", path)
		}
		if strings.TrimSpace(pn.Unit) == "" {
			return fmt.Errorf("%s: unit 不能为空", path)
		}
		if !allowedPanelTypes[pn.Panel] {
			return fmt.Errorf("%s: panel 类型 %q 非法", path, pn.Panel)
		}
		switch pn.Datasource {
		case "", "business", "skywalking_prometheus", "skywalking_datasource":
		default:
			return fmt.Errorf("%s: datasource %q 非法", path, pn.Datasource)
		}
		for _, target := range pn.LinkTargets {
			switch target {
			case "instance", "endpoint", "trace":
			default:
				return fmt.Errorf("%s: linkTargets %q 非法（instance/endpoint/trace）", path, target)
			}
		}
		switch pn.Aggregation {
		case "", AggregationApplication:
		case AggregationSite:
			if p.Scope != ScopeApplication {
				return fmt.Errorf("%s: aggregation=site 仅支持 application scope", path)
			}
		default:
			return fmt.Errorf("%s: aggregation %q 非法（application/site）", path, pn.Aggregation)
		}
		if len(pn.Sections) == 0 {
			return fmt.Errorf("%s: sections 不能为空", path)
		}
		seenSections := map[string]bool{}
		for _, section := range pn.Sections {
			if _, ok := SectionTitle[section]; !ok {
				return fmt.Errorf("%s: section %q 非法", path, section)
			}
			if seenSections[section] {
				return fmt.Errorf("%s: section %q 重复", path, section)
			}
			seenSections[section] = true
		}
		if pn.Panel != "nodeGraph" && p.Scope != ScopeJob && p.Scope != ScopeSkywalking &&
			!strings.Contains(pn.Query, "%SELECTOR%") {
			return fmt.Errorf("%s: query 缺少 %%SELECTOR%% 占位符", path)
		}
		for _, token := range tokenRE.FindAllString(pn.Query, -1) {
			if token != "%SELECTOR%" {
				return fmt.Errorf("%s: 不支持的模板占位符 %s", path, token)
			}
		}
		if strings.Contains(pn.Query, ":json") {
			return fmt.Errorf("%s: Grafana :json 变量不能用于 PromQL；字符串参数使用 \"${var}\"，正则 Matcher 使用 \"${var:regex}\"", path)
		}
		if unquotedRegexVarRE.MatchString(pn.Query) {
			return fmt.Errorf("%s: 正则 Matcher 中的变量必须放在双引号内并使用 :regex，例如 pod=~\"${pod:regex}\"", path)
		}
		for _, match := range varRE.FindAllStringSubmatch(pn.Query, -1) {
			name := match[1]
			if allowedVars[name] || strings.HasSuffix(name, "_pod") || strings.HasSuffix(name, "_app_selector") {
				continue
			}
			return fmt.Errorf("%s: 不支持或拼写错误的 Dashboard 变量 $%s", path, name)
		}
		if len(pn.RequiredMetrics) == 0 {
			return fmt.Errorf("%s: requiredMetrics 不能为空", path)
		}
		if err := validateUniqueNames(pn.RequiredMetrics, "requiredMetrics", metricNameRE, path); err != nil {
			return err
		}
		if err := validateUniqueNames(pn.RequiredLabels, "requiredLabels", labelNameRE, path); err != nil {
			return err
		}
		if err := validateUniqueNames(pn.OptionalLabels, "optionalLabels", labelNameRE, path); err != nil {
			return err
		}
		for _, label := range pn.RequiredLabels {
			if contains(pn.OptionalLabels, label) {
				return fmt.Errorf("%s: label %q 同时出现在 requiredLabels 和 optionalLabels", path, label)
			}
		}
		if err := validateThresholds(pn.Thresholds); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	if p.Discovery != nil {
		if !metricNameRE.MatchString(p.Discovery.Metric) {
			return fmt.Errorf("profile %s: discovery.metric %q 非法", p.ID, p.Discovery.Metric)
		}
		if err := validateUniqueNames(p.Discovery.GroupBy, "discovery.groupBy", labelNameRE, "profile "+p.ID); err != nil {
			return err
		}
		if err := validateUniqueNames(p.Discovery.Labels, "discovery.labels", labelNameRE, "profile "+p.ID); err != nil {
			return err
		}
		required := map[string]bool{}
		for _, panel := range p.Panels {
			for _, metric := range panel.RequiredMetrics {
				required[metric] = true
			}
		}
		if !required[p.Discovery.Metric] {
			return fmt.Errorf("profile %s: discovery.metric %q 必须出现在某个面板的 requiredMetrics 中", p.ID, p.Discovery.Metric)
		}
	}
	return nil
}

func validateUniqueNames(items []string, field string, re interface{ MatchString(string) bool }, path string) error {
	seen := map[string]bool{}
	for _, item := range items {
		if item == "" {
			return fmt.Errorf("%s: %s 包含空值", path, field)
		}
		if !re.MatchString(item) {
			return fmt.Errorf("%s: %s 包含非法名称 %q", path, field, item)
		}
		if seen[item] {
			return fmt.Errorf("%s: %s 包含重复项 %q", path, field, item)
		}
		seen[item] = true
	}
	return nil
}

func validateThresholds(steps []Threshold) error {
	if len(steps) == 0 {
		return nil
	}
	allowedColors := map[string]bool{
		"red": true, "green": true, "yellow": true, "orange": true,
		"blue": true, "purple": true,
	}
	last := -1e308
	for i, step := range steps {
		if !allowedColors[step.Color] {
			return fmt.Errorf("thresholds[%d]: 颜色 %q 非法", i, step.Color)
		}
		if i == 0 {
			if step.Value != nil {
				return fmt.Errorf("thresholds[0]: 基础阈值不能设置 value")
			}
			continue
		}
		if step.Value == nil {
			return fmt.Errorf("thresholds[%d]: 必须设置 value", i)
		}
		if *step.Value <= last {
			return fmt.Errorf("thresholds[%d]: value 必须严格递增", i)
		}
		last = *step.Value
	}
	return nil
}

func contains(items []string, value string) bool {
	for _, item := range items {
		if item == value {
			return true
		}
	}
	return false
}
