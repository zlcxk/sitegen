package output

import (
	"fmt"
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
	GeneratedAt     time.Time
	Checks          []Check
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
	b.WriteString("\n检查结果\n--------\n")
	if len(r.Checks) == 0 {
		b.WriteString("(无)\n")
	}
	for _, check := range r.Checks {
		detail := strings.TrimSpace(check.Detail)
		if detail != "" {
			fmt.Fprintf(&b, "[%-4s] %s / %s: %s\n", check.Status, check.Category, check.Name, detail)
		} else {
			fmt.Fprintf(&b, "[%-4s] %s / %s\n", check.Status, check.Category, check.Name)
		}
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
