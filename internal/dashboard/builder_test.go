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
	if dashboard.Refresh != "30s" {
		t.Errorf("Refresh = %q", dashboard.Refresh)
	}
	if dashboard.Time.From != "now-1h" {
		t.Errorf("Time.From = %q", dashboard.Time.From)
	}
	vars := map[string]bool{}
	for _, v := range dashboard.Templating.List {
		vars[v.Name] = true
	}
	for _, name := range []string{"datasource", "application", "namespace", "pod", "component", "instance"} {
		if !vars[name] {
			t.Errorf("missing variable %q", name)
		}
	}
	if len(dashboard.Panels) == 0 {
		t.Fatal("no panels generated")
	}
	if strings.Contains(string(first), "SkyWalking 拓扑图") {
		t.Fatal("dashboard must not fake a SkyWalking topology")
	}
	if !strings.Contains(string(first), "YAML 显式依赖") {
		t.Fatal("explicit YAML dependency view missing")
	}
}

func TestBuildWithoutHTTPDoesNotGenerateHTTPPanels(t *testing.T) {
	site, reg := loadExample(t)
	site.Spec.Applications[0].Metrics = []string{"jvm"}
	site.Spec.Applications[1].Metrics = []string{"jvm"}
	data, err := BuildJSON(site, reg, config.Links{})
	if err != nil {
		t.Fatalf("BuildJSON() error = %v", err)
	}
	text := string(data)
	if strings.Contains(text, "http_server_requests_seconds_count") {
		t.Fatal("HTTP panels generated for a site with HTTP disabled")
	}
}

func TestBuildSameApplicationDifferentNamespaces(t *testing.T) {
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
	site.Spec.Domains[0].ID = "admin"
	if issues := config.Validate(site, reg.Resolve); issues.HasErrors() {
		t.Fatalf("config validation errors: %v", issues.ErrorStrings())
	}
	data, err := BuildJSON(site, reg, config.Links{})
	if err != nil {
		t.Fatalf("BuildJSON() error = %v", err)
	}
	var d Dashboard
	if err := json.Unmarshal(data, &d); err != nil {
		t.Fatal(err)
	}
	for _, v := range d.Templating.List {
		if v.Name == "application" {
			if v.Multi != true || !v.IncludeAll {
				t.Error("application variable must support multi-select and All")
			}
		}
	}
	if !strings.Contains(string(data), "namespace=~") {
		t.Fatal("queries must use a namespace matcher")
	}
}

func TestBuildLinksAndSiteGuards(t *testing.T) {
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
	if !strings.Contains(text, "https://grafana.example.com/d/mysql-overview") {
		t.Fatal("component Grafana drill-down link missing")
	}
	if !strings.Contains(text, `application=~\"rtd-admin|user-center-application\"`) {
		t.Fatal("static application guard missing")
	}
	if !strings.Contains(text, `namespace=~\"test\"`) {
		t.Fatal("static namespace guard missing")
	}
	for _, hidden := range []string{"site_namespaces", "app_jobs"} {
		if strings.Contains(text, `"name": "`+hidden+`"`) {
			t.Fatalf("unexpected hidden variable %q", hidden)
		}
	}
}
