package output

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

type Status string

const (
	Pass Status = "PASS"
	Warn Status = "WARN"
	Fail Status = "FAIL"
	Skip Status = "SKIP"
)

// Check is one validation outcome shown in a report and on the terminal.
type Check struct {
	Category string
	Name     string
	Status   Status
	Detail   string
}

// Report contains all validation and generation metadata for one site.
type Report struct {
	SiteID          string
	SiteName        string
	SiteFile        string
	DashboardPath   string
	TemplateVersion string
	PrometheusCheck bool
	// DeferredEnvironmentCheck marks a non-strict build whose live data
	// acceptance is intentionally left to `sitegen check`.
	DeferredEnvironmentCheck bool
	GeneratedAt              time.Time
	Checks                   []Check
}

func (r *Report) Add(category, name string, status Status, detail string) {
	r.Checks = append(r.Checks, Check{
		Category: category,
		Name:     name,
		Status:   status,
		Detail:   detail,
	})
}

func (r *Report) AddAll(checks []Check) {
	r.Checks = append(r.Checks, checks...)
}

// HasFailures reports whether any validation failed.
func (r *Report) HasFailures() bool {
	for _, check := range r.Checks {
		if check.Status == Fail {
			return true
		}
	}
	return false
}

// Counts returns status totals.
func (r *Report) Counts() map[Status]int {
	counts := map[Status]int{}
	for _, check := range r.Checks {
		counts[check.Status]++
	}
	return counts
}

// RootCauses condenses FAIL/WARN findings into the small number of upstream
// problems a user should fix first. It is heuristic by design: it only groups
// findings that already carry an explicit root-cause phrase.
func (r *Report) RootCauses() []string {
	var causes []string
	seen := map[string]bool{}
	add := func(text string) {
		if text == "" || seen[text] || len(causes) >= 8 {
			return
		}
		seen[text] = true
		causes = append(causes, text)
	}
	for _, check := range r.Checks {
		if check.Status != Fail && check.Status != Warn {
			continue
		}
		switch {
		case check.Category == "配置" && check.Status == Fail:
			add("配置错误：" + check.Name)
		case strings.Contains(check.Name, "Pod 归属") &&
			(strings.Contains(check.Detail, "不存在") || strings.Contains(check.Detail, "没有匹配") || strings.Contains(check.Detail, "交集")):
			add("Kubernetes Pod 归属未建立：" + causeTarget(check.Name))
		case strings.Contains(check.Name, "容器过滤") && check.Status == Fail:
			add("Container Matcher 会过滤掉全部应用序列：" + causeTarget(check.Name))
		case strings.Contains(check.Detail, "指标不存在"):
			add("指标不存在：" + causeTarget(check.Name))
		case strings.Contains(check.Detail, "未匹配到序列") || strings.Contains(check.Detail, "没有匹配序列"):
			add("Selector 未匹配到数据：" + causeTarget(check.Name))
		case strings.Contains(check.Detail, "必需标签不完整"):
			add("指标标签不完整：" + causeTarget(check.Name))
		case check.Category == "PromQL" && check.Status == Fail:
			add("PromQL 执行失败：" + causeTarget(check.Name))
		case check.Category == "Dashboard 变量" && check.Status == Fail:
			add("Dashboard 变量查询失败：" + causeTarget(check.Name))
		case check.Category == "SkyWalking" && check.Status == Warn && strings.Contains(check.Detail, "没有"):
			add("SkyWalking APM 当前无数据：" + causeTarget(check.Name))
		case check.Category == "SkyWalking" && strings.Contains(check.Name, "拓扑"):
			add("SkyWalking 原生拓扑尚未在 Grafana 验证")
		}
	}
	return causes
}

// causeTarget condenses per-metric check names into the object that owns the
// root cause, so one broken App Selector does not produce 11 summary entries.
func causeTarget(name string) string {
	if strings.HasPrefix(name, "容器过滤/") {
		return name
	}
	if strings.HasPrefix(name, "Pod 归属/") {
		parts := strings.SplitN(name, "/", 3)
		if len(parts) >= 2 {
			return "Pod 归属/" + parts[1]
		}
	}
	if strings.HasPrefix(name, "应用/") || strings.HasPrefix(name, "站点/") || strings.HasPrefix(name, "组件/") {
		parts := strings.SplitN(name, "/", 3)
		if len(parts) >= 2 {
			return parts[0] + "/" + parts[1]
		}
	}
	if index := strings.IndexByte(name, '/'); index > 0 {
		return name[:index]
	}
	return name
}

// DownstreamCounts counts checks skipped because an upstream check failed.
func (r *Report) DownstreamCounts() map[string]int {
	counts := map[string]int{}
	for _, check := range r.Checks {
		if check.Status != Skip {
			continue
		}
		if !strings.Contains(check.Detail, "依赖") && !strings.Contains(check.Detail, "根因") {
			continue
		}
		counts[check.Category]++
	}
	return counts
}

// SummaryLines renders the root-cause and downstream summary used by both the
// text report and the terminal.
func (r *Report) SummaryLines() []string {
	var lines []string
	lines = append(lines, "检查摘要")
	lines = append(lines, "--------")
	causes := r.RootCauses()
	if len(causes) == 0 {
		lines = append(lines, "未发现需要优先处理的根因。")
	} else {
		lines = append(lines, "需要处理的主要问题：")
		for i, cause := range causes {
			lines = append(lines, fmt.Sprintf("%d. %s", i+1, cause))
		}
	}
	downstream := r.DownstreamCounts()
	if len(downstream) > 0 {
		lines = append(lines, "", "受影响的下游检查：")
		for _, category := range sortedCountKeys(downstream) {
			lines = append(lines, fmt.Sprintf("- %s：%d 项", downstreamLabel(category), downstream[category]))
		}
	}
	lines = append(lines, "")
	return lines
}

func downstreamLabel(category string) string {
	switch category {
	case "PromQL":
		return "面板查询（PromQL）"
	case "Prometheus":
		return "指标与资源检查"
	case "Dashboard 变量":
		return "Dashboard 变量"
	default:
		return category
	}
}

func sortedCountKeys(counts map[string]int) []string {
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// WriteText writes a credential-free report atomically.
func (r *Report) WriteText(path string) error {
	var b strings.Builder
	b.WriteString("Site Observability 校验报告\n")
	b.WriteString("==========================\n\n")
	fmt.Fprintf(&b, "站点: %s (%s)\n", r.SiteName, r.SiteID)
	fmt.Fprintf(&b, "配置文件: %s\n", r.SiteFile)
	fmt.Fprintf(&b, "模板版本: %s\n", r.TemplateVersion)
	generated := r.GeneratedAt
	if generated.IsZero() {
		generated = time.Now()
	}
	fmt.Fprintf(&b, "生成时间: %s\n", generated.Format(time.RFC3339))
	if r.DashboardPath != "" {
		fmt.Fprintf(&b, "Dashboard: %s\n", r.DashboardPath)
	} else {
		b.WriteString("Dashboard: 未生成\n")
	}
	b.WriteString("\n")
	for _, line := range r.SummaryLines() {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteString("检查结果\n--------\n")
	if len(r.Checks) == 0 {
		b.WriteString("(无)\n")
	}
	for _, check := range r.Checks {
		writeCheck(&b, check)
	}
	counts := r.Counts()
	fmt.Fprintf(&b, "\n汇总: PASS=%d WARN=%d FAIL=%d SKIP=%d\n",
		counts[Pass], counts[Warn], counts[Fail], counts[Skip])
	if r.HasFailures() {
		b.WriteString("结果: FAILED\n")
	} else {
		b.WriteString("结果: OK\n")
	}
	if err := WriteAtomic(path, []byte(b.String()), 0o644, false); err != nil {
		return fmt.Errorf("写入校验报告 %s: %w", path, err)
	}
	return nil
}

func writeCheck(b *strings.Builder, check Check) {
	detail := strings.TrimSpace(check.Detail)
	if detail == "" {
		fmt.Fprintf(b, "[%-4s] %s / %s\n", check.Status, check.Category, check.Name)
		return
	}
	lines := strings.Split(detail, "\n")
	fmt.Fprintf(b, "[%-4s] %s / %s: %s\n", check.Status, check.Category, check.Name, lines[0])
	for _, line := range lines[1:] {
		fmt.Fprintf(b, "       %s\n", line)
	}
}

// SummaryLine is a compact one-line summary for batch output.
func (r *Report) SummaryLine() string {
	counts := r.Counts()
	state := "OK"
	if r.HasFailures() {
		state = "FAILED"
	}
	return fmt.Sprintf("%s: %s (PASS=%d WARN=%d FAIL=%d SKIP=%d)",
		r.SiteID, state, counts[Pass], counts[Warn], counts[Fail], counts[Skip])
}
