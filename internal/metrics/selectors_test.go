package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"site-observability/internal/config"
)

func selectorSite() *config.Site {
	return &config.Site{
		Spec: config.Spec{
			Domains: []config.Domain{{
				ID:                "admin",
				EntryApplications: []string{"app-a-test"},
			}},
			Applications: []config.Application{
				{
					ID:          "app-a-test",
					Application: "app-a",
					Namespace:   "test",
					Prometheus: config.Prometheus{
						Job:    "job-a",
						Labels: map[string]string{"team": "a"},
					},
					Metrics: []string{"http", "jvm", "kubernetes"},
					Kubernetes: config.Kubernetes{
						PodSelector: &config.PodSelector{
							Mode:     config.PodSelectorLabels,
							Metric:   config.DefaultPodLabelsMetric,
							Matchers: map[string]string{"label_app": "app-a"},
						},
					},
				},
				{
					ID:          "app-a-dev",
					Application: "app-a",
					Namespace:   "dev",
					Prometheus: config.Prometheus{
						Job:    "job-a",
						Labels: map[string]string{"team": "a"},
					},
					Metrics: []string{"http"},
				},
				{
					ID:          "app-b-prod",
					Application: "app-b",
					Namespace:   "prod",
					Prometheus:  config.Prometheus{Job: "job-b"},
					Metrics:     []string{"jvm"},
				},
			},
		},
	}
}

func renderApp(t *testing.T, query string, apps []config.Application, filter bool, selectorVar string) string {
	t.Helper()
	out, err := RenderApplicationQuery(query, apps, filter, selectorVar)
	if err != nil {
		t.Fatalf("RenderApplicationQuery() error = %v", err)
	}
	return out
}

func renderNamespace(t *testing.T, query string, apps []config.Application, filter bool, selectorVar string) string {
	t.Helper()
	out, err := RenderNamespaceQuery(query, apps, filter, selectorVar)
	if err != nil {
		t.Fatalf("RenderNamespaceQuery() error = %v", err)
	}
	return out
}

func compactPromQL(query string) string {
	return strings.Join(strings.Fields(query), " ")
}

func TestApplicationTargetSelectorIsExact(t *testing.T) {
	site := selectorSite()
	selector := ApplicationTargetSelector(site.Spec.Applications[0])
	for _, want := range []string{
		`application="app-a"`,
		`namespace="test"`,
		`job="job-a"`,
		`team="a"`,
	} {
		if !strings.Contains(selector, want) {
			t.Errorf("selector %q missing %q", selector, want)
		}
	}
	if strings.Contains(selector, "|") {
		t.Errorf("selector must not use alternation: %q", selector)
	}
	other := ApplicationTargetSelector(site.Spec.Applications[2])
	if strings.Contains(other, "team=") {
		t.Errorf("app-b selector must not inherit app-a's extra label: %q", other)
	}
}

func TestRenderApplicationQueryKeepsRangeSelectorSemantics(t *testing.T) {
	site := selectorSite()
	query := renderApp(t, "sum(rate(my_metric{%SELECTOR%}[5m]))", site.Spec.Applications, false, "")
	for _, want := range []string{
		`application="app-a"`,
		`namespace="test"`,
		`namespace="dev"`,
		`namespace="prod"`,
		`[5m]`,
	} {
		if !strings.Contains(query, want) {
			t.Errorf("query missing %q:\n%s", want, query)
		}
	}
	if strings.Contains(query, "[5m:]") {
		t.Fatalf("range selector must not be converted to a subquery:\n%s", query)
	}
	if strings.Count(query, "rate(") != 3 {
		t.Fatalf("each application must get its own rate() call:\n%s", query)
	}
	if !strings.Contains(compactPromQL(query), " or ") {
		t.Fatal("multi-application query must union per-object expressions")
	}
	if strings.Contains(query, `application=~"app-a|app-b"`) || strings.Contains(query, `namespace=~"test|dev|prod"`) {
		t.Fatalf("independent label alternation would allow cross matching:\n%s", query)
	}
}

func TestRenderedQueryHasNoSubqueryAndKeepsRanges(t *testing.T) {
	site := selectorSite()
	query := renderApp(t, "sum(rate(my_metric{%SELECTOR%}[5m]))", site.Spec.Applications, true, "http_app_selector")
	expr, err := promQLParser.ParseExpr(sanitizeGrafanaVars(query))
	if err != nil {
		t.Fatalf("generated query is not valid PromQL: %v\n%s", err, query)
	}
	subqueries, matrices, rates := 0, 0, 0
	parser.Inspect(expr, func(node parser.Node, _ []parser.Node) error {
		switch n := node.(type) {
		case *parser.SubqueryExpr:
			subqueries++
		case *parser.MatrixSelector:
			matrices++
			if n.Range.Seconds() != 300 {
				t.Errorf("range selector changed: %s", n.Range)
			}
		case *parser.Call:
			if n.Func != nil && n.Func.Name == "rate" {
				rates++
			}
		}
		return nil
	})
	if subqueries != 0 {
		t.Fatalf("generated query contains %d subquery node(s):\n%s", subqueries, query)
	}
	if matrices != len(site.Spec.Applications) || rates != len(site.Spec.Applications) {
		t.Fatalf("expected one range selector and rate() per application, got matrices=%d rates=%d:\n%s",
			matrices, rates, query)
	}
}

func TestRenderApplicationQueryKeepsIncreaseRange(t *testing.T) {
	site := selectorSite()
	query := renderApp(t, "sum(increase(counter_total{%SELECTOR%}[5m]))", site.Spec.Applications[:2], false, "")
	if strings.Contains(query, "[5m:]") {
		t.Fatalf("increase() range selector became a subquery:\n%s", query)
	}
	if strings.Count(query, "increase(") != 2 {
		t.Fatalf("expected one increase() per application:\n%s", query)
	}
	if !strings.Contains(query, "[5m]") {
		t.Fatalf("original range selector missing:\n%s", query)
	}
}

func TestRenderApplicationQueryKeepsHistogramAndTopKScope(t *testing.T) {
	site := selectorSite()
	histogram := renderApp(t,
		"histogram_quantile(0.95, sum by (le) (rate(bucket_metric{%SELECTOR%}[5m])))",
		site.Spec.Applications[:2], false, "")
	if strings.Contains(histogram, "[5m:]") {
		t.Fatalf("histogram query contains a subquery:\n%s", histogram)
	}
	compactHistogram := compactPromQL(histogram)
	if !strings.Contains(compactHistogram, "histogram_quantile(") ||
		!strings.Contains(compactHistogram, "sum by (le)") ||
		strings.Count(compactHistogram, "rate(") != 2 {
		t.Fatalf("outer histogram aggregation changed:\n%s", histogram)
	}
	topk := renderApp(t,
		"topk(10, sum by (application, namespace) (rate(count_metric{%SELECTOR%}[5m])))",
		site.Spec.Applications[:2], false, "")
	if strings.Contains(topk, "[5m:]") {
		t.Fatalf("topk query contains a subquery:\n%s", topk)
	}
	compactTopK := compactPromQL(topk)
	if !strings.Contains(compactTopK, "topk(") || !strings.Contains(compactTopK, "10,") ||
		!strings.Contains(compactTopK, "sum by (application, namespace)") {
		t.Fatalf("topk scope changed:\n%s", topk)
	}
}

func TestRenderApplicationQueryFiltersByProfileSelector(t *testing.T) {
	site := selectorSite()
	query := renderApp(t, "sum(rate(my_metric{%SELECTOR%}[5m]))", site.Spec.Applications, true, "http_app_selector")
	if !strings.Contains(query, "sitegen_app") {
		t.Fatal("query must carry a derived application identity label")
	}
	if !strings.Contains(query, `${http_app_selector:json}`) {
		t.Fatalf("profile selector must use Grafana JSON escaping:\n%s", query)
	}
	if strings.Contains(query, "[5m:]") {
		t.Fatalf("range selector must not become a subquery:\n%s", query)
	}
}

func TestRenderApplicationQueryHandlesVariableBraces(t *testing.T) {
	site := selectorSite()
	query := renderApp(t,
		"sum by (pod) (my_metric{%SELECTOR%, area=\"heap\", pod=~${jvm_pod:json}})",
		site.Spec.Applications[:1], true, "jvm_app_selector")
	compact := compactPromQL(query)
	if !strings.Contains(compact, `pod=~${jvm_pod:json}`) {
		t.Fatalf("pod variable token was damaged:\n%s", query)
	}
	if strings.Count(compact, "label_replace(") != 2 {
		t.Fatalf("unexpected label_replace rendering:\n%s", query)
	}
	if !strings.Contains(query, `${jvm_app_selector:json}`) {
		t.Fatalf("profile selector missing:\n%s", query)
	}
}

func TestRenderNamespaceQueryJoinsDeclaredPodOwnership(t *testing.T) {
	site := selectorSite()
	query := renderNamespace(t, "sum by (pod) (rate(container_metric{%SELECTOR%}[5m]))", site.Spec.Applications[:1], false, "k8s_app_selector")
	for _, want := range []string{
		`namespace="test"`,
		`pod=~${k8s_pod:json}`,
		`kube_pod_labels`,
		`label_app="app-a"`,
		`on (namespace, pod)`,
		`sitegen_app`,
		`[5m]`,
	} {
		if !strings.Contains(query, want) {
			t.Errorf("namespace query missing %q:\n%s", want, query)
		}
	}
	if strings.Contains(query, "[5m:]") {
		t.Fatalf("namespace query contains a subquery:\n%s", query)
	}
	if strings.Contains(query, `container!="POD"`) || strings.Contains(query, `container!=""`) {
		t.Fatal("namespace query must not hard-code unverified container filters")
	}
}

func TestRenderNamespaceQueryPodRegexCompatibility(t *testing.T) {
	site := selectorSite()
	app := site.Spec.Applications[0]
	app.Kubernetes.PodSelector = &config.PodSelector{Mode: config.PodSelectorPodRegex, Pod: "^app-a-.*$"}
	query := renderNamespace(t, "sum(rate(container_metric{%SELECTOR%}[5m]))", []config.Application{app}, false, "")
	if !strings.Contains(query, `pod=~"^app-a-.*$"`) {
		t.Fatalf("pod regex not applied: %s", query)
	}
}

func TestRenderNamespaceQueryUsesContainerMatcherOperators(t *testing.T) {
	site := selectorSite()
	app := site.Spec.Applications[0]
	app.Kubernetes.ContainerMatchers = config.ContainerMatchers{
		{Name: "container", Operator: config.MatchNotEqual, Value: "POD"},
		{Name: "image", Operator: config.MatchNotEqual, Value: ""},
		{Name: "app", Operator: config.MatchRegex, Value: "rtd-.*"},
	}
	query := renderNamespace(t, "sum(rate(container_metric{%SELECTOR%}[5m]))", []config.Application{app}, false, "")
	for _, want := range []string{`container!="POD"`, `image!=""`, `app=~"rtd-.*"`} {
		if !strings.Contains(query, want) {
			t.Errorf("namespace query missing container matcher %q:\n%s", want, query)
		}
	}
}

func TestRenderJobQueryUsesOnlyJobAndTargetLabels(t *testing.T) {
	site := selectorSite()
	app := site.Spec.Applications[0]
	app.Prometheus.TargetLabels = map[string]string{"cluster": "test-cluster"}
	query, err := RenderJobQuery("min by (job) (up{%SELECTOR%})", []config.Application{app})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, `job="job-a"`) || !strings.Contains(query, `cluster="test-cluster"`) {
		t.Fatalf("target selector missing job/target label: %s", query)
	}
	for _, forbidden := range []string{`application=`, `namespace=`} {
		if strings.Contains(query, forbidden) {
			t.Fatalf("up selector must not use application identity label %q: %s", forbidden, query)
		}
	}

	multi, err := RenderJobQuery("min by (job) (up{%SELECTOR%})", site.Spec.Applications)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(multi, `up{job="job-a"`) || !strings.Contains(multi, `up{job="job-b"}`) {
		t.Fatalf("job union missing expected selectors: %s", multi)
	}
}

func TestBuildPodVariableExpressionUsesCurrentApplication(t *testing.T) {
	site := selectorSite()
	expr, err := BuildPodVariableExpression(site, "kubernetes", "k8s_app_selector")
	if err != nil {
		t.Fatal(err)
	}
	compact := compactPromQL(expr)
	for _, want := range []string{
		`kube_pod_labels{`,
		`label_app="app-a"`,
		`namespace="test"`,
		`sitegen_app`,
		`${k8s_app_selector:json}`,
	} {
		if !strings.Contains(compact, want) {
			t.Errorf("pod variable expression missing %q:\n%s", want, expr)
		}
	}
	if !strings.Contains(expr, `"app-a-test"`) {
		t.Errorf("pod variable expression missing app id: %s", expr)
	}
	if strings.Contains(expr, `"app-a-dev"`) {
		t.Error("HTTP-only application must not create a Kubernetes pod variable entry")
	}

	jvmExpr, err := BuildPodVariableExpression(site, "jvm", "jvm_app_selector")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(jvmExpr, "jvm_memory_used_bytes") || !strings.Contains(jvmExpr, `"app-b-prod"`) {
		t.Errorf("jvm pod variable incomplete: %s", jvmExpr)
	}
}

func TestBuildPodVariableExpressionEmptyForHTTPOnlySite(t *testing.T) {
	site := selectorSite()
	for i := range site.Spec.Applications {
		site.Spec.Applications[i].Metrics = []string{"http"}
		site.Spec.Applications[i].Kubernetes = config.Kubernetes{}
	}
	expr, err := BuildPodVariableExpression(site, "jvm", "jvm_app_selector")
	if err != nil {
		t.Fatal(err)
	}
	if expr != "" {
		t.Fatalf("HTTP-only site must not emit a pod variable: %s", expr)
	}
}
