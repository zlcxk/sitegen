package output

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReportSummaryGroupsRootCausesAndDownstream(t *testing.T) {
	report := &Report{
		SiteID:          "site",
		SiteName:        "Site",
		SiteFile:        "site.yaml",
		TemplateVersion: "1.0.0+abc",
		GeneratedAt:     time.Unix(1700000000, 0),
	}
	report.Add("配置", "spec.applications[0].namespace", Fail, "不能为空")
	report.Add("Prometheus", "应用/app/http_server_requests_seconds_count", Warn,
		`指标存在（近 7 天），但最近 1h 内未匹配到序列。

实际 Selector:
application="app"`)
	report.Add("Prometheus", "Pod 归属/app/kube_pod_labels", Fail, "Pod 归属指标 kube_pod_labels 不存在（近 7 天无序列）")
	report.Add("PromQL", "应用/app/http/http-rps", Skip, "依赖的基础指标在近 1h 内没有匹配序列；根因已在上方指标检查中报告，跳过该下游查询")
	report.Add("PromQL", "应用/app/http/http-p95", Skip, "依赖的基础指标在近 1h 内没有匹配序列；根因已在上方指标检查中报告，跳过该下游查询")
	report.Add("Kubernetes 指标", "应用/app/container_memory_working_set_bytes", Skip, "Pod 归属未通过，根因已在 Pod 归属检查中报告")

	lines := report.SummaryLines()
	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"检查摘要",
		"需要处理的主要问题",
		"配置错误",
		"Selector 未匹配到数据",
		"Kubernetes Pod 归属未建立",
		"受影响的下游检查",
		"面板查询（PromQL）：2 项",
		"Kubernetes 指标：1 项",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("summary missing %q:\n%s", want, joined)
		}
	}

	path := filepath.Join(t.TempDir(), "report.txt")
	if err := report.WriteText(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "检查摘要") {
		t.Fatal("report file is missing the summary")
	}
	if !strings.Contains(text, "       application=\"app\"") && !strings.Contains(text, "       \n") {
		t.Fatalf("multi-line detail must be indented in the report:\n%s", text)
	}
	if !strings.Contains(text, "实际 Selector:") {
		t.Fatal("report must include the actual selector")
	}
}
