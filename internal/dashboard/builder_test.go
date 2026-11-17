package dashboard

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"site-observability/internal/config"
	"site-observability/internal/metrics"
)

func loadExample(t *testing.T) (*config.Site, *metrics.Registry) {
	t.Helper()
	reg, err := metrics.LoadDefault()
	if err != nil {
		t.Fatalf("LoadDefault() error = %v", err)
	}
	site, err := config.Load("../../sites/examples/order-admin-test.yaml")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	issues := config.Validate(site, reg.Resolve)
	if issues.HasErrors() {
		t.Fatalf("config validation errors: %v", issues.ErrorStrings())
	}
	return site, reg
}

func buildDashboard(t *testing.T, site *config.Site, reg *metrics.Registry, links config.Links) *Dashboard {
	t.Helper()
	data, err := BuildJSON(site, reg, links)
	if err != nil {
		t.Fatalf("BuildJSON() error = %v", err)
	}
	var d Dashboard
	if err := json.Unmarshal(data, &d); err != nil {
		t.Fatal(err)
	}
	return &d
}

func walkPanels(panels []Panel, fn func(Panel)) {
	for _, panel := range panels {
		fn(panel)
		if panel.Panels != nil {
			walkPanels(*panel.Panels, fn)
		}
	}
}

func findByTitle(d *Dashboard, title string) []Panel {
	var out []Panel
	walkPanels(d.Panels, func(panel Panel) {
		if panel.Title == title {
			out = append(out, panel)
		}
	})
	return out
}

func exprs(d *Dashboard) []string {
	var out []string
	walkPanels(d.Panels, func(panel Panel) {
		for _, target := range panel.Targets {
			out = append(out, target.Expr)
		}
	})
	return out
}

func TestBuildExampleDeterministic(t *testing.T) {
	site, reg := loadExample(t)
	first, err := BuildJSON(site, reg, config.Links{})
	if err != nil {
		t.Fatalf("BuildJSON() error = %v", err)
	}
	second, err := BuildJSON(site, reg, config.Links{})
	if err != nil {
		t.Fatalf("BuildJSON() second error = %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("same site and templates must produce identical JSON")
	}
	if err := ValidateJSON(first); err != nil {
		t.Fatalf("ValidateJSON() error = %v", err)
	}
	if strings.Contains(string(first), "%SELECTOR%") {
		t.Fatal("serialized dashboard contains unresolved %SELECTOR%")
	}

	var dashboard Dashboard
	if err := json.Unmarshal(first, &dashboard); err != nil {
		t.Fatal(err)
	}
	if dashboard.UID != "site-order-admin-test" {
		t.Errorf("UID = %q", dashboard.UID)
	}
	if dashboard.Refresh != "30s" || dashboard.Time.From != "now-1h" {
		t.Errorf("unexpected refresh/time: %q/%q", dashboard.Refresh, dashboard.Time.From)
	}
	vars := map[string]Variable{}
	for _, v := range dashboard.Templating.List {
		vars[v.Name] = v
	}
	for _, name := range []string{"datasource", "app_selector", "pod"} {
		if _, ok := vars[name]; !ok {
			t.Errorf("missing variable %q", name)
		}
	}
	for _, removed := range []string{"application", "namespace", "component", "instance"} {
		if _, ok := vars[removed]; ok {
			t.Errorf("legacy/decorative variable %q must not be emitted", removed)
		}
	}
	selector := vars["app_selector"]
	if selector.Multi || selector.IncludeAll {
		t.Error("app_selector must be single-select so application objects cannot be recombined")
	}
	if len(selector.Options) != 2 {
		t.Errorf("app_selector options = %d", len(selector.Options))
	}
	podVar := vars["pod"]
	if !strings.Contains(podVar.Query.(string), "app_selector") {
		t.Error("pod variable must depend on app_selector")
	}

	if strings.Contains(string(first), "SkyWalking 拓扑图") {
		t.Fatal("dashboard must not fake a SkyWalking topology")
	}
	if !strings.Contains(string(first), "YAML 显式依赖") {
		t.Fatal("explicit YAML dependency view missing")
	}

	// Row collapse strategy: only the health overview is expanded.
	for _, panel := range dashboard.Panels {
		if panel.Type != "row" {
			continue
		}
		if panel.Title == metrics.SectionTitle[metrics.SectionOverview] {
			if panel.Collapsed == nil || *panel.Collapsed {
				t.Error("overview row must be expanded")
			}
		} else if panel.Collapsed == nil || !*panel.Collapsed {
			t.Errorf("row %q must be collapsed", panel.Title)
		}
	}
}

func TestBuildWithoutHTTPDoesNotGenerateHTTPPanels(t *testing.T) {
	site, reg := loadExample(t)
	for i := range site.Spec.Applications {
		site.Spec.Applications[i].Metrics = []string{"jvm"}
	}
	d := buildDashboard(t, site, reg, config.Links{})
	for _, expr := range exprs(d) {
		if strings.Contains(expr, "http_server_requests") {
			t.Fatal("HTTP panels generated for a site with HTTP disabled")
		}
	}
}

func TestSiteAggregateUsesOnlyEntryApplications(t *testing.T) {
	site, reg := loadExample(t)
	// Enable HTTP for user-center too, but do NOT declare it as an entry app.
	for i := range site.Spec.Applications {
		if site.Spec.Applications[i].ID == "user-center-test" {
			site.Spec.Applications[i].Metrics = append(site.Spec.Applications[i].Metrics, "http")
		}
	}
	d := buildDashboard(t, site, reg, config.Links{})
	panels := findByTitle(d, "入口应用请求量")
	if len(panels) != 1 {
		t.Fatalf("entry request panel count = %d", len(panels))
	}
	expr := panels[0].Targets[0].Expr
	if !strings.Contains(expr, `application="rtd-admin"`) {
		t.Fatalf("entry aggregate missing entry app: %s", expr)
	}
	if strings.Contains(expr, "user-center-application") {
		t.Fatalf("entry aggregate leaked a downstream application: %s", expr)
	}
	if strings.Contains(expr, "app_selector") {
		t.Fatalf("site-level aggregate must not depend on the application variable: %s", expr)
	}

	// HTTP detail panels are application-scoped and keep only HTTP-enabled apps
	// in the generated union, so a downstream app enabled later cannot appear
	// unless the variable selects it explicitly.
	for _, panel := range findByTitle(d, "请求量趋势") {
		expr := panel.Targets[0].Expr
		if !strings.Contains(expr, "user-center-application") {
			t.Fatal("HTTP detail union should include the newly enabled app")
		}
		if !strings.Contains(expr, "app_selector") {
			t.Fatal("HTTP detail query must be filtered by $app_selector")
		}
	}
}

func TestHTTPOnlySiteHasNoDecorativePodVariable(t *testing.T) {
	site, reg := loadExample(t)
	for i := range site.Spec.Applications {
		site.Spec.Applications[i].Metrics = []string{"http"}
		site.Spec.Applications[i].Kubernetes = config.Kubernetes{}
	}
	d := buildDashboard(t, site, reg, config.Links{})
	for _, variable := range d.Templating.List {
		if variable.Name == "pod" {
			t.Fatal("HTTP-only site must not emit an unused $pod variable")
		}
	}
}

func TestProfileScopeFollowsPerApplicationEnablement(t *testing.T) {
	site, reg := loadExample(t)
	site.Spec.Applications[0].Metrics = []string{"http", "jvm"}
	site.Spec.Applications[1].Metrics = []string{"jvm"}
	d := buildDashboard(t, site, reg, config.Links{})

	for _, panel := range findByTitle(d, "请求量趋势") {
		if strings.Contains(panel.Targets[0].Expr, "user-center-application") {
			t.Fatal("HTTP query must not include an application that did not enable HTTP")
		}
	}
	jvmExpr := findByTitle(d, "JVM Heap（按 Pod）")[0].Targets[0].Expr
	if !strings.Contains(jvmExpr, "rtd-admin") || !strings.Contains(jvmExpr, "user-center-application") {
		t.Fatal("JVM query must include every application that enabled JVM")
	}
}

func TestMissingEntryApplicationsSkipsSiteHTTPSummary(t *testing.T) {
	site, reg := loadExample(t)
	for i := range site.Spec.Domains {
		site.Spec.Domains[i].EntryApplications = nil
	}
	d := buildDashboard(t, site, reg, config.Links{})
	for _, title := range []string{"入口应用请求量", "入口应用 P95 延迟", "入口应用 5xx 比例"} {
		if len(findByTitle(d, title)) != 0 {
			t.Fatalf("site-level HTTP panel %q generated without entry applications", title)
		}
	}
	warnings := findByTitle(d, "HTTP 入口应用未配置")
	if len(warnings) != 1 {
		t.Fatal("missing entryApplications must produce an explicit dashboard warning")
	}
}

func TestHTTPStatusMetricsAndMillisecondUnits(t *testing.T) {
	site, reg := loadExample(t)
	d := buildDashboard(t, site, reg, config.Links{})
	for _, title := range []string{"HTTP 2xx 比例", "HTTP 3xx 比例", "HTTP 4xx 比例", "HTTP 5xx 比例"} {
		if len(findByTitle(d, title)) == 0 {
			t.Errorf("missing status metric panel %q", title)
		}
	}
	if len(findByTitle(d, "HTTP 成功率")) != 0 {
		t.Fatal("misleading HTTP success-rate panel must not exist")
	}
	for _, title := range []string{"平均响应时间", "HTTP P95 延迟", "HTTP P99 延迟"} {
		panels := findByTitle(d, title)
		if len(panels) == 0 {
			t.Fatalf("missing latency panel %q", title)
		}
		for _, panel := range panels {
			if panel.FieldConfig == nil || panel.FieldConfig.Defaults.Unit != "ms" {
				t.Errorf("%s unit must be ms", title)
			}
			if !strings.Contains(panel.Targets[0].Expr, "1000") {
				t.Errorf("%s query must convert seconds to milliseconds", title)
			}
		}
	}
}

func TestEndpointQueriesRetainApplicationIdentity(t *testing.T) {
	site, reg := loadExample(t)
	d := buildDashboard(t, site, reg, config.Links{})
	slow := findByTitle(d, "慢接口 Top 10")
	if len(slow) != 1 {
		t.Fatal("slow endpoint panel missing")
	}
	slowExpr := slow[0].Targets[0].Expr
	for _, label := range []string{"application", "namespace", "method", "uri"} {
		if !strings.Contains(slowExpr, label) {
			t.Errorf("slow endpoint aggregation drops %s", label)
		}
	}
	if !strings.Contains(slowExpr, "and on (application, namespace, method, uri)") {
		t.Error("traffic threshold must match the same identity dimensions")
	}
	top := findByTitle(d, "高请求量 Endpoint")
	if len(top) != 1 || !strings.Contains(top[0].Targets[0].Expr, "sum by (application, namespace, method, uri)") {
		t.Error("top endpoint aggregation must retain application identity")
	}
}

func TestKubernetesQueriesUseDeclaredPodOwnership(t *testing.T) {
	site, reg := loadExample(t)
	d := buildDashboard(t, site, reg, config.Links{})
	for _, title := range []string{"Pod CPU", "Pod 内存 Working Set", "CPU Throttling"} {
		panels := findByTitle(d, title)
		if len(panels) != 1 {
			t.Fatalf("%s count = %d", title, len(panels))
		}
		expr := panels[0].Targets[0].Expr
		if !strings.Contains(expr, "kube_pod_labels") || !strings.Contains(expr, `label_app="rtd-admin"`) {
			t.Errorf("%s is not constrained by kube_pod_labels ownership: %s", title, expr)
		}
		if strings.Contains(expr, `container!="POD"`) || strings.Contains(expr, `container!=""`) {
			t.Errorf("%s must not hard-code unverified container filters", title)
		}
	}
	// JVM heap belongs to the runtime row only.
	if len(findByTitle(d, "JVM Heap（按 Pod）")) != 1 {
		t.Fatal("JVM heap should be generated exactly once")
	}
	for _, panel := range findByTitle(d, "Pod CPU") {
		if panel.FieldConfig == nil {
			t.Fatal("missing field config")
		}
	}
}

func TestNoDuplicateDetailedPanels(t *testing.T) {
	site, reg := loadExample(t)
	d := buildDashboard(t, site, reg, config.Links{})
	counts := map[string]int{}
	walkPanels(d.Panels, func(panel Panel) {
		if panel.Type != "row" && panel.Type != "text" {
			counts[panel.Title]++
		}
	})
	for title, count := range counts {
		if count > 1 {
			t.Errorf("data panel %q appears %d times", title, count)
		}
	}
}

func TestMissingPodSelectorSkipsApplicationKubernetesPanels(t *testing.T) {
	site, reg := loadExample(t)
	for i := range site.Spec.Applications {
		if site.Spec.Applications[i].ID == "user-center-test" {
			site.Spec.Applications[i].Kubernetes.PodSelector = nil
		}
	}
	d := buildDashboard(t, site, reg, config.Links{})
	if len(findByTitle(d, "Kubernetes Pod 归属缺失")) != 1 {
		t.Fatal("missing podSelector must produce an explicit warning panel")
	}
	for _, panel := range findByTitle(d, "Pod CPU") {
		if strings.Contains(panel.Targets[0].Expr, "user-center-application") {
			t.Fatal("unconstrained app must not be included in Kubernetes queries")
		}
	}
}

func TestSameApplicationDifferentNamespacesNotCombined(t *testing.T) {
	site, reg := loadExample(t)
	second := site.Spec.Applications[0]
	second.ID = "rtd-admin-dev"
	second.Namespace = "dev"
	second.Prometheus.Labels = map[string]string{
		"application": "rtd-admin",
		"namespace":   "dev",
	}
	second.Skywalking.Service = "rtd-admin_dev"
	second.Dependencies = nil
	site.Spec.Applications = append(site.Spec.Applications, second)
	if issues := config.Validate(site, reg.Resolve); issues.HasErrors() {
		t.Fatalf("config validation errors: %v", issues.ErrorStrings())
	}
	d := buildDashboard(t, site, reg, config.Links{})
	appVar := Variable{}
	for _, v := range d.Templating.List {
		if v.Name == "app_selector" {
			appVar = v
		}
	}
	values := map[string]bool{}
	for _, option := range appVar.Options {
		values[option.Value] = true
	}
	if !values["rtd-admin-test"] || !values["rtd-admin-dev"] {
		t.Fatalf("app_selector options = %#v", appVar.Options)
	}
	joined := strings.Join(exprs(d), "\n")
	if !strings.Contains(joined, `namespace="test"`) || !strings.Contains(joined, `namespace="dev"`) {
		t.Fatal("queries must use exact namespace selectors for both instances")
	}
	if strings.Contains(joined, `namespace=~"test|dev"`) {
		t.Fatal("namespace alternation would allow cross-object matching")
	}
	if !strings.Contains(joined, "app_selector") {
		t.Fatal("application panels must be restricted by app_selector")
	}
}

func TestBuildLinksAndDrillDowns(t *testing.T) {
	site, reg := loadExample(t)
	links := config.Links{
		Skywalking:                    "https://skywalking.example.com",
		Grafana:                       "https://grafana.example.com",
		SkywalkingServiceURLTemplate:  "https://skywalking.example.com/service?name={service}",
		SkywalkingEndpointURLTemplate: "https://skywalking.example.com/endpoint?name={service}",
		ComponentDashboards:           map[string]string{"mysql": "mysql-overview"},
	}
	data, err := BuildJSON(site, reg, links)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "https://skywalking.example.com/service?name=rtd-admin_test") {
		t.Fatal("SkyWalking service drill-down link missing")
	}
	if !strings.Contains(text, "https://skywalking.example.com/endpoint?name=rtd-admin_test") {
		t.Fatal("SkyWalking endpoint drill-down link missing")
	}
	if !strings.Contains(text, "https://grafana.example.com/d/mysql-overview") {
		t.Fatal("component Grafana drill-down link missing")
	}
	if !strings.Contains(text, `application=\"rtd-admin\"`) {
		t.Fatal("exact application selector missing")
	}
	if strings.Contains(text, `application=~"$application"`) || strings.Contains(text, `namespace=~"$namespace"`) {
		t.Fatal("legacy independent variable matching must not be emitted")
	}
	for _, hidden := range []string{"site_namespaces", "app_jobs"} {
		if strings.Contains(text, `"name": "`+hidden+`"`) {
			t.Fatalf("unexpected hidden variable %q", hidden)
		}
	}
}

func TestNoSkywalkingEndpointTemplateDoesNotFakeEndpointLink(t *testing.T) {
	site, reg := loadExample(t)
	links := config.Links{Skywalking: "https://skywalking.example.com"}
	data, err := BuildJSON(site, reg, links)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, "Endpoint 页面") {
		t.Fatal("endpoint link must not be faked without an endpoint URL template")
	}
	if !strings.Contains(text, "未配置 `skywalkingEndpointURLTemplate`") {
		t.Fatal("missing endpoint template should be explained explicitly")
	}
}
