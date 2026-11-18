package dashboard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
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

func variableQueryString(query any) string {
	if object, ok := query.(map[string]any); ok {
		text, _ := object["query"].(string)
		return text
	}
	text, _ := query.(string)
	return text
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
	for _, name := range []string{
		"datasource",
		"http_app_selector", "jvm_app_selector", "k8s_app_selector",
		"k8s_pod",
	} {
		if _, ok := vars[name]; !ok {
			t.Errorf("missing variable %q", name)
		}
	}
	if _, ok := vars["jvm_pod"]; ok {
		t.Error("JVM must not expose a Pod variable")
	}
	for _, removed := range []string{"application", "namespace", "component", "instance", "app_selector", "pod", "jvm_pod"} {
		if _, ok := vars[removed]; ok {
			t.Errorf("legacy/decorative variable %q must not be emitted", removed)
		}
	}
	httpSelector := vars["http_app_selector"]
	if httpSelector.Multi || httpSelector.IncludeAll {
		t.Error("profile selectors must be single-select")
	}
	// Only rtd-admin enables HTTP in the example; user-center only enables JVM
	// and Kubernetes.
	if len(httpSelector.Options) != 1 {
		t.Errorf("http_app_selector options = %d", len(httpSelector.Options))
	}
	if len(vars["jvm_app_selector"].Options) != 2 {
		t.Errorf("jvm_app_selector options = %d", len(vars["jvm_app_selector"].Options))
	}
	if len(vars["k8s_app_selector"].Options) != 2 {
		t.Errorf("k8s_app_selector options = %d", len(vars["k8s_app_selector"].Options))
	}
	if !strings.Contains(variableQueryString(vars["k8s_pod"].Query), "k8s_app_selector") {
		t.Error("k8s_pod must depend on k8s_app_selector")
	}
	if _, ok := vars["service"]; !ok {
		t.Error("SkyWalking-enabled site must expose the APM service variable")
	}
	if _, ok := vars["skywalking_prometheus"]; !ok {
		t.Error("SkyWalking-enabled site must expose the SkyWalking Prometheus datasource variable")
	}
	if _, ok := vars["skywalking_datasource"]; !ok {
		t.Error("SkyWalking-enabled site must expose the SkyWalking native datasource variable")
	}

	if strings.Contains(string(first), "YAML 显式依赖") {
		t.Fatal("static YAML dependency text panel must not remain")
	}
	if !strings.Contains(string(first), `"type": "nodeGraph"`) ||
		!strings.Contains(string(first), "SkyWalking 服务拓扑") {
		t.Fatal("real SkyWalking Node Graph topology missing")
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
		if !strings.Contains(expr, "http_app_selector") {
			t.Fatal("HTTP detail query must be filtered by the HTTP profile selector")
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
		if strings.HasSuffix(variable.Name, "_pod") {
			t.Fatalf("HTTP-only site must not emit an unused pod variable %q", variable.Name)
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
	jvmExpr := findByTitle(d, "JVM Heap 使用量")[0].Targets[0].Expr
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
	if len(findByTitle(d, "JVM Heap 使用量")) != 1 {
		t.Fatal("JVM heap should be generated exactly once")
	}
	for _, panel := range findByTitle(d, "Pod CPU") {
		if panel.FieldConfig == nil {
			t.Fatal("missing field config")
		}
	}
}

func TestKubernetesCPUAndMemoryShareFilters(t *testing.T) {
	site, reg := loadExample(t)
	site.Spec.Applications[0].Kubernetes.ContainerMatchers = config.ContainerMatchers{
		{Name: "container", Operator: config.MatchNotEqual, Value: "POD"},
		{Name: "image", Operator: config.MatchNotEqual, Value: ""},
	}
	d := buildDashboard(t, site, reg, config.Links{})
	for _, title := range []string{"Pod CPU", "Pod 内存 Working Set", "CPU Throttling"} {
		panels := findByTitle(d, title)
		if len(panels) != 1 {
			t.Fatalf("%s count = %d", title, len(panels))
		}
		expr := panels[0].Targets[0].Expr
		for _, want := range []string{
			`container!="POD"`,
			`image!=""`,
			`kube_pod_labels`,
			`label_app="rtd-admin"`,
			`pod=~"${k8s_pod:regex}"`,
		} {
			if !strings.Contains(expr, want) {
				t.Errorf("%s missing shared filter %q:\n%s", title, want, expr)
			}
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
		if v.Name == "http_app_selector" {
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
	if !strings.Contains(joined, "http_app_selector") {
		t.Fatal("HTTP panels must be restricted by the HTTP profile selector")
	}
}

func TestTargetLabelsUsedOnlyForUpSelector(t *testing.T) {
	site, reg := loadExample(t)
	site.Spec.Applications[0].Prometheus.TargetLabels = map[string]string{"cluster": "test-cluster"}
	d := buildDashboard(t, site, reg, config.Links{})
	panels := findByTitle(d, "应用指标采集状态")
	if len(panels) != 1 {
		t.Fatal("application collection status panel missing")
	}
	expr := panels[0].Targets[0].Expr
	if !strings.Contains(expr, `cluster="test-cluster"`) || !strings.Contains(expr, `job="rtd-admin"`) {
		t.Fatalf("up selector missing target labels: %s", expr)
	}
	if strings.Contains(expr, "application=") || strings.Contains(expr, "namespace=") {
		t.Fatalf("up selector must not use application identity labels: %s", expr)
	}
}

func TestApplicationSkywalkingServiceMapping(t *testing.T) {
	site, reg := loadExample(t)
	d, err := Build(site, reg, config.Links{})
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]Variable{}
	for _, variable := range d.Templating.List {
		vars[variable.Name] = variable
	}
	service := vars["service"]
	if service.QueryType != VariableQueryTypeCustom {
		t.Fatalf("service variable type = %q", service.QueryType)
	}
	found := false
	for _, option := range service.Options {
		if option.Value == site.Spec.Applications[0].Skywalking.Service {
			found = true
		}
	}
	if !found {
		t.Fatalf("service variable does not map the configured SkyWalking service: %#v", service.Options)
	}
	if !strings.Contains(service.Options[0].Text, "rtd-admin") {
		t.Fatalf("service option text = %q", service.Options[0].Text)
	}
}

func TestGeneratedDashboardHasNoSubquery(t *testing.T) {
	site, reg := loadExample(t)
	d := buildDashboard(t, site, reg, config.Links{})
	for _, expr := range exprs(d) {
		if strings.TrimSpace(expr) == "" {
			continue // SkyWalking Node Graph is not PromQL
		}
		if strings.Contains(expr, "[5m:]") {
			t.Fatalf("generated dashboard contains an unexpected subquery:\n%s", expr)
		}
		if err := metrics.ValidateQuery(expr); err != nil {
			t.Fatalf("generated query invalid: %v\n%s", err, expr)
		}
	}
}

func TestMissingPodSelectorDoesNotEmitContainerWarning(t *testing.T) {
	site, reg := loadExample(t)
	for i := range site.Spec.Applications {
		site.Spec.Applications[i].Kubernetes.PodSelector = nil
		site.Spec.Applications[i].Kubernetes.ContainerMatchers = nil
	}
	d := buildDashboard(t, site, reg, config.Links{})
	if len(findByTitle(d, "Kubernetes Pod 归属缺失")) != 1 {
		t.Fatal("missing podSelector warning panel missing")
	}
	if len(findByTitle(d, "容器过滤条件提示")) != 0 {
		t.Fatal("container matcher warning must not be emitted when no podSelector exists")
	}
}

func TestGeneratedDashboardVariableFormats(t *testing.T) {
	site, reg := loadExample(t)
	data, err := BuildJSON(site, reg, config.Links{})
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, `:json`) {
		t.Fatal("generated dashboard must not use :json inside PromQL")
	}
	if strings.Contains(text, `pod=~${`) {
		t.Fatal("pod regex matcher must be quoted")
	}
	d := buildDashboard(t, site, reg, config.Links{})
	joined := strings.Join(exprs(d), "\n")
	if !strings.Contains(joined, `pod=~"${k8s_pod:regex}"`) {
		t.Fatal("missing quoted k8s pod regex variable")
	}
	for _, expr := range exprs(d) {
		if strings.Contains(expr, "jvm_memory_used_bytes") && strings.Contains(expr, "pod=~") {
			t.Fatalf("JVM panels must not be filtered by a Pod variable:\n%s", expr)
		}
	}
	if !strings.Contains(joined, `"${k8s_app_selector}"`) ||
		!strings.Contains(joined, `"${jvm_app_selector}"`) {
		t.Fatal("application selector must be used as a quoted PromQL string")
	}
}

func TestVariableQueryTypesAndDatasource(t *testing.T) {
	site, reg := loadExample(t)
	d, err := Build(site, reg, config.Links{})
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]Variable{}
	for _, variable := range d.Templating.List {
		vars[variable.Name] = variable
	}
	for _, name := range []string{"datasource", "skywalking_prometheus", "skywalking_datasource"} {
		if vars[name].QueryType != VariableQueryTypeDatasource {
			t.Errorf("%s query type = %q", name, vars[name].QueryType)
		}
	}
	for _, name := range []string{"http_app_selector", "jvm_app_selector", "k8s_app_selector", "service"} {
		variable := vars[name]
		if variable.QueryType != VariableQueryTypeCustom {
			t.Errorf("%s query type = %q", name, variable.QueryType)
		}
		if variable.ValuesFormat != "csv" {
			t.Errorf("%s valuesFormat = %q", name, variable.ValuesFormat)
		}
	}
	for _, name := range []string{"k8s_pod"} {
		variable := vars[name]
		if variable.QueryType != VariableQueryTypeQueryResult {
			t.Errorf("%s query type = %q", name, variable.QueryType)
		}
		if variable.RegexApplyTo != "value" {
			t.Errorf("%s regexApplyTo = %q", name, variable.RegexApplyTo)
		}
		if variable.Datasource == nil || variable.Datasource.UID != "${datasource}" {
			t.Errorf("%s datasource = %#v", name, variable.Datasource)
		}
		if !strings.HasPrefix(variable.Definition, "query_result(") {
			t.Errorf("%s definition is not query_result: %s", name, variable.Definition)
		}
		query, ok := variable.Query.(map[string]any)
		if !ok {
			t.Fatalf("%s query is not a Prometheus query object: %#v", name, variable.Query)
		}
		if query["qryType"] != 3 {
			t.Errorf("%s qryType = %v, want 3 (VarQueryResult)", name, query["qryType"])
		}
		text, _ := query["query"].(string)
		if !strings.HasPrefix(text, "query_result(") {
			t.Errorf("%s query text = %s", name, text)
		}
		if strings.ContainsAny(text, "\r\n") || strings.ContainsAny(variable.Definition, "\r\n") {
			t.Errorf("%s query must be single-line for Grafana's variable parser", name)
		}
		// Same regex as @grafana/prometheus metric_find_query. It has no `s`
		// flag, so a newline would fall through to /api/v1/series match[].
		if !regexp.MustCompile(`^query_result\((.+)\)\s*$`).MatchString(text) {
			t.Errorf("%s query is not recognized as query_result by Grafana", name)
		}
	}
}

func TestPodVariableQueryIsSerializedAsObject(t *testing.T) {
	site, reg := loadExample(t)
	data, err := BuildJSON(site, reg, config.Links{})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Templating struct {
			List []struct {
				Name  string `json:"name"`
				Query any    `json:"query"`
			} `json:"list"`
		} `json:"templating"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	for _, variable := range doc.Templating.List {
		if variable.Name != "k8s_pod" {
			continue
		}
		object, ok := variable.Query.(map[string]any)
		if !ok {
			t.Fatalf("%s query was serialized as %T, want object", variable.Name, variable.Query)
		}
		qryType, _ := object["qryType"].(float64)
		if int(qryType) != 3 {
			t.Errorf("%s serialized qryType = %v, want 3", variable.Name, object["qryType"])
		}
		text, _ := object["query"].(string)
		if !strings.HasPrefix(text, "query_result(") {
			t.Errorf("%s serialized query = %s", variable.Name, text)
		}
		if strings.ContainsAny(text, "\r\n") {
			t.Errorf("%s serialized query must not contain CR/LF", variable.Name)
		}
		if !regexp.MustCompile(`^query_result\((.+)\)\s*$`).MatchString(text) {
			t.Errorf("%s serialized query is not recognized as query_result by Grafana", variable.Name)
		}
	}
}

func TestPodRegexMatchersAreNotDuplicated(t *testing.T) {
	site, reg := loadExample(t)
	for i := range site.Spec.Applications {
		site.Spec.Applications[i].Kubernetes.PodSelector = &config.PodSelector{
			Mode: config.PodSelectorPodRegex,
			Pod:  "^" + site.Spec.Applications[i].Application + "-.*$",
		}
	}
	d := buildDashboard(t, site, reg, config.Links{})
	for _, panel := range findByTitle(d, "Pod CPU") {
		expr := panel.Targets[0].Expr
		if count := strings.Count(expr, `pod=~"${k8s_pod:regex}"`); count != len(site.Spec.Applications) {
			t.Fatalf("dynamic pod matcher count = %d, want %d:\n%s", count, len(site.Spec.Applications), expr)
		}
		for _, app := range site.Spec.Applications {
			static := `pod=~"^` + app.Application + `-.*$"`
			if count := strings.Count(expr, static); count != 1 {
				t.Fatalf("static pod matcher %q count = %d:\n%s", static, count, expr)
			}
		}
	}
}

func TestBuildLinksAndDrillDowns(t *testing.T) {
	site, reg := loadExample(t)
	links := config.Links{
		Skywalking:                  "https://skywalking.example.com",
		Grafana:                     "https://grafana.example.com",
		SkywalkingServiceDashboard:  "https://grafana.example.com/d/sw-service/skywalking-service",
		SkywalkingInstanceDashboard: "https://grafana.example.com/d/sw-instance/skywalking-instance",
		SkywalkingEndpointDashboard: "https://grafana.example.com/d/sw-endpoint/skywalking-endpoint",
		SkywalkingTraceDashboard:    "https://grafana.example.com/d/sw-trace/skywalking-trace",
		ComponentDashboards:         map[string]string{"mysql": "mysql-overview"},
		ComponentDashboardParams: map[string]map[string]string{
			"mysql": {"var-host": "{instance}", "var-node_host": "{host}"},
		},
		Dashboards: []config.LinkEntry{{
			Title: "Kubernetes 集群",
			URL:   "d/k8s-uid/k8s-dashboard?var-NameSpace=dev",
		}},
	}
	data, err := BuildJSON(site, reg, links)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{
		"SkyWalking 服务详情",
		"d/sw-service/skywalking-service",
		"var-service=${service:queryparam}",
		"var-DS_Prometheus=${skywalking_prometheus}",
		"Kubernetes 集群",
		"d/k8s-uid/k8s-dashboard",
		"var-service_instance=${__field.labels.service_instance}",
		"var-endpoint=${__field.labels.endpoint}",
		"${__url_time_range}",
		"https://grafana.example.com/d/mysql-overview?",
		"var-host=10.10.100.10%3A3307",
		`application=\"rtd-admin\"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("generated dashboard missing %q", want)
		}
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

func TestNoSkywalkingDrilldownWithoutDashboardBase(t *testing.T) {
	site, reg := loadExample(t)
	data, err := BuildJSON(site, reg, config.Links{Skywalking: "https://skywalking.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, forbidden := range []string{"var-service_instance=", "var-endpoint=", "Trace 查询"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("generated fake drill-down %q without a Dashboard base", forbidden)
		}
	}
}

func skywalkingDatasources() config.DatasourcesConfig {
	return config.DatasourcesConfig{
		BusinessPrometheus:   config.DatasourceConfig{UID: "prometheus", Name: "Prometheus", Type: "prometheus"},
		SkywalkingPrometheus: config.DatasourceConfig{UID: "sw-prom", Name: "skywalking", Type: "prometheus"},
		Skywalking:           config.DatasourceConfig{UID: "sw-native", Name: "SkyWalking", Type: "apache-skywalking-datasource"},
	}
}

func TestSkywalkingDashboardDatasourcesAndNodeGraph(t *testing.T) {
	site, reg := loadExample(t)
	d, err := BuildWithDatasources(site, reg, config.Links{}, skywalkingDatasources())
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]Variable{}
	for _, variable := range d.Templating.List {
		vars[variable.Name] = variable
	}
	for name, want := range map[string]string{
		"datasource":            "prometheus",
		"skywalking_prometheus": "sw-prom",
		"skywalking_datasource": "sw-native",
	} {
		variable, ok := vars[name]
		if !ok {
			t.Fatalf("missing variable %s", name)
		}
		if got := fmt.Sprint(variable.Current.Value); got != want {
			t.Errorf("%s current = %q, want %q", name, got, want)
		}
	}
	service := vars["service"]
	if service.Type != "custom" || service.Multi || len(service.Options) == 0 {
		t.Fatalf("service variable = %#v", service)
	}
	if vars["layer"].Hide != 2 {
		t.Errorf("layer variable must be hidden, hide=%d", vars["layer"].Hide)
	}

	sawAPM := false
	sawTopology := false
	for _, panel := range findByTitle(d, "服务 Apdex") {
		sawAPM = true
		if panel.Datasource == nil || panel.Datasource.UID != "${skywalking_prometheus}" {
			t.Errorf("APM panel datasource = %#v", panel.Datasource)
		}
		if !strings.Contains(panel.Targets[0].Expr, `service="$service"`) {
			t.Errorf("APM panel must use the selected service: %s", panel.Targets[0].Expr)
		}
	}
	for _, panel := range findByTitle(d, "SkyWalking 服务拓扑") {
		sawTopology = true
		if panel.Type != "nodeGraph" {
			t.Fatalf("topology panel type = %s", panel.Type)
		}
		if panel.Datasource == nil || panel.Datasource.Type != "apache-skywalking-datasource" ||
			panel.Datasource.UID != "${skywalking_datasource}" {
			t.Errorf("topology datasource = %#v", panel.Datasource)
		}
		target := panel.Targets[0]
		if target.Service != "$service" || target.NodeMetrics == "" || target.EdgeMetrics == "" {
			t.Errorf("topology target = %#v", target)
		}
		if target.Layer != "GENERAL" {
			t.Errorf("topology layer = %q", target.Layer)
		}
	}
	if !sawAPM || !sawTopology {
		t.Fatal("SkyWalking APM or topology panel missing")
	}
	for _, variable := range d.Templating.List {
		if variable.Name == "jvm_pod" {
			t.Fatal("JVM Pod variable must not exist")
		}
	}
}

func TestSkywalkingDataLinks(t *testing.T) {
	site, reg := loadExample(t)
	links := config.Links{
		SkywalkingInstanceDashboard: "d/instance-uid/skywalking-instance",
		SkywalkingEndpointDashboard: "d/endpoint-uid/skywalking-endpoint",
		SkywalkingTraceDashboard:    "d/trace-uid/skywalking-trace",
	}
	d := buildDashboard(t, site, reg, links)
	instance := findByTitle(d, "实例请求量 Top 10")
	if len(instance) != 1 || instance[0].FieldConfig == nil {
		t.Fatal("instance panel missing field config")
	}
	instanceLinks := instance[0].FieldConfig.Defaults.Links
	if len(instanceLinks) != 1 || !strings.Contains(instanceLinks[0].URL, "var-service=${service:queryparam}") ||
		!strings.Contains(instanceLinks[0].URL, "var-service_instance=${__field.labels.service_instance}") {
		t.Fatalf("instance link = %#v", instanceLinks)
	}
	endpoint := findByTitle(d, "Endpoint 请求量 Top 10")
	if len(endpoint) != 1 || endpoint[0].FieldConfig == nil {
		t.Fatal("endpoint panel missing field config")
	}
	var endpointURL, traceURL string
	for _, link := range endpoint[0].FieldConfig.Defaults.Links {
		if link.Title == "Endpoint 详情" {
			endpointURL = link.URL
		}
		if link.Title == "Trace 查询" {
			traceURL = link.URL
		}
	}
	if !strings.Contains(endpointURL, "var-endpoint=${__field.labels.endpoint}") ||
		!strings.Contains(endpointURL, "${__url_time_range}") {
		t.Fatalf("endpoint link = %s", endpointURL)
	}
	if !strings.Contains(traceURL, "var-endpoint=${__field.labels.endpoint}") ||
		!strings.Contains(traceURL, "${__url_time_range}") {
		t.Fatalf("trace link = %s", traceURL)
	}
}

func TestSkywalkingDisabledProducesNoAPMPanels(t *testing.T) {
	site, reg := loadExample(t)
	disabled := false
	for i := range site.Spec.Applications {
		site.Spec.Applications[i].Skywalking.Enabled = &disabled
	}
	d := buildDashboard(t, site, reg, config.Links{})
	for _, variable := range d.Templating.List {
		if variable.Name == "skywalking_prometheus" || variable.Name == "skywalking_datasource" || variable.Name == "service" {
			t.Fatalf("unexpected SkyWalking variable %s", variable.Name)
		}
	}
	if len(findByTitle(d, "SkyWalking 服务拓扑")) != 0 || len(findByTitle(d, "服务 Apdex")) != 0 {
		t.Fatal("SkyWalking panels must not be generated when disabled")
	}
}

func TestJVMQueriesHaveNoPodMatcher(t *testing.T) {
	site, reg := loadExample(t)
	d := buildDashboard(t, site, reg, config.Links{})
	for _, panel := range findByTitle(d, "JVM Heap 使用量") {
		if strings.Contains(panel.Targets[0].Expr, "pod=~") {
			t.Fatalf("JVM query must not use a Pod matcher: %s", panel.Targets[0].Expr)
		}
		if !strings.Contains(panel.Targets[0].Expr, "sum(") {
			t.Fatalf("JVM heap must aggregate across pods: %s", panel.Targets[0].Expr)
		}
	}
}
