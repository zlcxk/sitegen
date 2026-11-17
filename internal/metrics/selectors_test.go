package metrics

import (
	"strings"
	"testing"

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
	// An extra label used by only one application must not be dropped from
	// that application's selector, and must not leak into another app.
	other := ApplicationTargetSelector(site.Spec.Applications[2])
	if strings.Contains(other, "team=") {
		t.Errorf("app-b selector must not inherit app-a's extra label: %q", other)
	}
}

func TestRenderApplicationQueryKeepsObjectRelations(t *testing.T) {
	site := selectorSite()
	query := RenderApplicationQuery("sum(rate(my_metric{%SELECTOR%}[5m]))", site.Spec.Applications, false)
	for _, want := range []string{
		`application="app-a", namespace="test", job="job-a", team="a"`,
		`application="app-a", namespace="dev", job="job-a", team="a"`,
		`application="app-b", namespace="prod", job="job-b"`,
	} {
		if !strings.Contains(query, want) {
			t.Errorf("query missing exact app selector %q:\n%s", want, query)
		}
	}
	if strings.Contains(query, `application=~"app-a|app-b"`) || strings.Contains(query, `namespace=~"test|dev|prod"`) {
		t.Fatalf("independent label alternation would allow cross matching:\n%s", query)
	}
	if !strings.Contains(query, " or ") {
		t.Fatal("multi-application query must union per-object expressions")
	}
}

func TestRenderApplicationQueryFiltersByEscapedAppSelector(t *testing.T) {
	site := selectorSite()
	query := RenderApplicationQuery("sum(rate(my_metric{%SELECTOR%}[5m]))", site.Spec.Applications, true)
	if !strings.Contains(query, "sitegen_app") {
		t.Fatal("query must carry a derived application identity label")
	}
	if !strings.Contains(query, `${app_selector:json}`) {
		t.Fatal("app selector must use Grafana JSON escaping to prevent PromQL injection")
	}
}

func TestRenderApplicationQueryHandlesVariableBraces(t *testing.T) {
	site := selectorSite()
	query := RenderApplicationQuery(
		"sum by (pod) (my_metric{%SELECTOR%, pod=~${pod:json}}[5m])",
		site.Spec.Applications[:1], true)
	if !strings.Contains(query, `pod=~${pod:json}}, "sitegen_app"`) {
		t.Fatalf("metric selector closing brace was confused with the variable brace:\n%s", query)
	}
	if strings.Count(query, "label_replace(my_metric{") != 1 {
		t.Fatalf("unexpected label_replace rendering:\n%s", query)
	}
}

func TestRenderNamespaceQueryJoinsDeclaredPodOwnership(t *testing.T) {
	site := selectorSite()
	query := RenderNamespaceQuery("sum by (pod) (rate(container_metric{%SELECTOR%}[5m]))", site.Spec.Applications[:1], false)
	for _, want := range []string{
		`namespace="test"`,
		`pod=~${pod:json}`,
		`kube_pod_labels`,
		`label_app="app-a"`,
		`on (namespace, pod)`,
		`sitegen_app`,
	} {
		if !strings.Contains(query, want) {
			t.Errorf("namespace query missing %q:\n%s", want, query)
		}
	}
	if strings.Contains(query, `container!="POD"`) || strings.Contains(query, `container!=""`) {
		t.Fatal("namespace query must not hard-code unverified container filters")
	}
}

func TestRenderNamespaceQueryPodRegexCompatibility(t *testing.T) {
	site := selectorSite()
	app := site.Spec.Applications[0]
	app.Kubernetes.PodSelector = &config.PodSelector{Mode: config.PodSelectorPodRegex, Pod: "^app-a-.*$"}
	query := RenderNamespaceQuery("sum(rate(container_metric{%SELECTOR%}[5m]))", []config.Application{app}, false)
	if !strings.Contains(query, `pod=~"^app-a-.*$"`) {
		t.Fatalf("pod regex not applied: %s", query)
	}
}

func TestRenderJobQueryUnionsJobs(t *testing.T) {
	site := selectorSite()
	query := RenderJobQuery("min by (job) (up{%SELECTOR%})", site.Spec.Applications)
	if !strings.Contains(query, `up{job="job-a"}`) || !strings.Contains(query, `up{job="job-b"}`) {
		t.Fatalf("job union missing expected selectors: %s", query)
	}
}

func TestBuildPodVariableExpressionUsesCurrentApplication(t *testing.T) {
	site := selectorSite()
	expr := BuildPodVariableExpression(site)
	for _, want := range []string{
		`jvm_memory_used_bytes{application="app-a", namespace="test", job="job-a", team="a"}`,
		`kube_pod_labels{namespace="test", label_app="app-a"}`,
		`sitegen_app`,
		`${app_selector:json}`,
	} {
		if !strings.Contains(expr, want) {
			t.Errorf("pod variable expression missing %q:\n%s", want, expr)
		}
	}
	for _, appID := range []string{"app-a-test", "app-b-prod"} {
		if !strings.Contains(expr, `"`+appID+`"`) {
			t.Errorf("pod variable expression missing app id %q", appID)
		}
	}
	if strings.Contains(expr, `"app-a-dev"`) {
		t.Error("HTTP-only application must not create a pod variable entry")
	}
}

func TestBuildPodVariableExpressionEmptyForHTTPOnlySite(t *testing.T) {
	site := selectorSite()
	for i := range site.Spec.Applications {
		site.Spec.Applications[i].Metrics = []string{"http"}
		site.Spec.Applications[i].Kubernetes = config.Kubernetes{}
	}
	if expr := BuildPodVariableExpression(site); expr != "" {
		t.Fatalf("HTTP-only site must not emit a pod variable: %s", expr)
	}
}
