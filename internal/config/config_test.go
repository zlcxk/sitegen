package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"site-observability/internal/config"
	"site-observability/internal/metrics"
)

func loadExample(t *testing.T) (*config.Site, *metrics.Registry) {
	t.Helper()
	reg, err := metrics.LoadDefault()
	if err != nil {
		t.Fatal(err)
	}
	site, err := config.Load("../../sites/examples/order-admin-test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return site, reg
}

func TestLoadExampleAndValidate(t *testing.T) {
	site, reg := loadExample(t)
	issues := config.Validate(site, reg.Resolve)
	if issues.HasErrors() {
		t.Fatalf("unexpected errors: %v", issues.ErrorStrings())
	}
	if site.Spec.Applications[1].Skywalking.Service != "user-center-application_test" {
		t.Errorf("service = %q", site.Spec.Applications[1].Skywalking.Service)
	}
	if site.Spec.Applications[0].Prometheus.Labels["application"] != "rtd-admin" {
		t.Error("normalization did not add labels")
	}
}

func TestLoadRejectsInvalidUTF8File(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.yaml")
	data := []byte("apiVersion: site-observability/v1\nkind: Site\nmetadata:\n  id: bad\n  name: ")
	data = append(data, 0xff)
	data = append(data, []byte("\n  environment: test\nspec:\n  domains: []\n")...)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected invalid UTF-8 error")
	}
	if !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("error does not mention UTF-8: %v", err)
	}
}

func TestUnknownFieldGivesPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.yaml")
	data := `apiVersion: site-observability/v1
kind: Site
metadata:
  id: bad
  name: Bad
  environment: test
  typoField: x
spec:
  domains: []
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "metadata.typoField") {
		t.Fatalf("error does not include field path: %v", err)
	}
}

func TestValidationRejectsDuplicateIDsAndUnknownDependency(t *testing.T) {
	site, reg := loadExample(t)
	site.Spec.Applications[1].ID = site.Spec.Applications[0].ID
	site.Spec.Applications[0].Dependencies = append(site.Spec.Applications[0].Dependencies, "missing-component")
	issues := config.Validate(site, reg.Resolve)
	if !issues.HasErrors() {
		t.Fatal("expected validation errors")
	}
	joined := strings.Join(issues.ErrorStrings(), "\n")
	if !strings.Contains(joined, "全局唯一") {
		t.Errorf("missing duplicate ID error: %s", joined)
	}
	if !strings.Contains(joined, "不存在") {
		t.Errorf("missing dependency error: %s", joined)
	}
}

func TestValidationRejectsDuplicateComponentEntity(t *testing.T) {
	site, reg := loadExample(t)
	duplicate := site.Spec.Components[0]
	duplicate.ID = "mysql-test-copy"
	site.Spec.Components = append(site.Spec.Components, duplicate)
	issues := config.Validate(site, reg.Resolve)
	if !issues.HasErrors() {
		t.Fatal("expected duplicate component error")
	}
	if !strings.Contains(strings.Join(issues.ErrorStrings(), "\n"), "组件定义") {
		t.Fatal("expected duplicate component message")
	}
}

func TestValidationRejectsSkywalkingDuplicate(t *testing.T) {
	site, reg := loadExample(t)
	site.Spec.Applications[1].Skywalking.Service = site.Spec.Applications[0].Skywalking.Service
	issues := config.Validate(site, reg.Resolve)
	if !issues.HasErrors() {
		t.Fatal("expected validation errors")
	}
	if !strings.Contains(strings.Join(issues.ErrorStrings(), "\n"), "SkyWalking Service") {
		t.Fatal("expected SkyWalking duplicate error")
	}
}

func TestValidationRequiresApplicationAndNamespace(t *testing.T) {
	site, reg := loadExample(t)
	site.Spec.Applications[0].Application = ""
	issues := config.Validate(site, reg.Resolve)
	if !issues.HasErrors() {
		t.Fatal("expected validation errors")
	}
}

func TestMarshalLoadRoundTrip(t *testing.T) {
	site, _ := loadExample(t)
	data, err := config.Marshal(site)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := config.Decode(data, "roundtrip.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(site, decoded) {
		t.Fatalf("roundtrip mismatch\nwant: %#v\ngot: %#v", site, decoded)
	}
}

func TestMarshalLoadRoundTripNewFields(t *testing.T) {
	site, _ := loadExample(t)
	site.Spec.Applications[0].Prometheus.TargetLabels = map[string]string{"cluster": "test-cluster"}
	site.Spec.Applications[0].Kubernetes.ContainerMatchers = config.ContainerMatchers{
		{Name: "container", Operator: config.MatchNotEqual, Value: "POD"},
		{Name: "image", Operator: config.MatchNotEqual, Value: ""},
	}
	site.Spec.Applications[0].Skywalking.UI = config.SkywalkingUI{
		BaseURL:             "https://skywalking.example.com",
		ServiceURLTemplate:  "https://skywalking.example.com/service?name={service}",
		EndpointURLTemplate: "https://skywalking.example.com/endpoint?name={service}",
	}
	data, err := config.Marshal(site)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := config.Decode(data, "roundtrip.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(site, decoded) {
		t.Fatalf("roundtrip mismatch\nwant: %#v\ngot: %#v", site, decoded)
	}
}

func TestWriteCreatesBackupAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "site.yaml")
	site, _ := loadExample(t)
	if err := config.Write(path, site, true); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	site.Metadata.Name = "更新后的名称"
	if err := config.Write(path, site, true); err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != string(original) {
		t.Fatal("backup does not contain the previous file")
	}
	updated, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Metadata.Name != "更新后的名称" {
		t.Fatal("updated file not written")
	}
}

func TestLinksValidation(t *testing.T) {
	site, reg := loadExample(t)
	site.Spec.Links.Skywalking = "ftp://bad"
	issues := config.Validate(site, reg.Resolve)
	if !issues.HasErrors() {
		t.Fatal("expected invalid link error")
	}
	site.Spec.Links.Skywalking = "https://skywalking.example.com"
	site.Spec.Links.SkywalkingServiceURLTemplate = "https://skywalking.example.com/service?name={service}"
	site.Spec.Links.ComponentDashboards = map[string]string{"mysql": "mysql-overview"}
	issues = config.Validate(site, reg.Resolve)
	if issues.HasErrors() {
		t.Fatalf("unexpected errors: %v", issues.ErrorStrings())
	}
}

func TestValidationRejectsUnknownEntryApplication(t *testing.T) {
	site, reg := loadExample(t)
	site.Spec.Domains[0].EntryApplications = []string{"missing-app"}
	issues := config.Validate(site, reg.Resolve)
	if !issues.HasErrors() {
		t.Fatal("expected unknown entry application error")
	}
	if !strings.Contains(strings.Join(issues.ErrorStrings(), "\n"), "entryApplications") {
		t.Fatal("expected entryApplications path in error")
	}
}

func TestValidationAllowsMultipleEntryAppsAndSharedEntryApp(t *testing.T) {
	site, reg := loadExample(t)
	site.Spec.Domains = append(site.Spec.Domains, config.Domain{
		ID:                "api",
		URL:               "https://api.example.com",
		Blackbox:          config.Blackbox{Job: "blackbox", Instance: "https://api.example.com"},
		EntryApplications: []string{"rtd-admin-test", "user-center-test"},
	})
	site.Spec.Domains[0].EntryApplications = []string{"rtd-admin-test", "user-center-test"}
	if issues := config.Validate(site, reg.Resolve); issues.HasErrors() {
		t.Fatalf("unexpected errors: %v", issues.ErrorStrings())
	}
	if ids := site.EntryApplicationIDs(); len(ids) != 2 {
		t.Fatalf("entry applications must be de-duplicated: %#v", ids)
	}
	if apps := site.EntryApplicationsWithMetric("http"); len(apps) != 1 || apps[0].ID != "rtd-admin-test" {
		t.Fatalf("only HTTP-enabled entry apps belong to the site HTTP aggregate: %#v", apps)
	}
}

func TestValidationWarnsWhenHTTPHasNoEntryApplications(t *testing.T) {
	site, reg := loadExample(t)
	for i := range site.Spec.Domains {
		site.Spec.Domains[i].EntryApplications = nil
	}
	issues := config.Validate(site, reg.Resolve)
	if issues.HasErrors() {
		t.Fatalf("unexpected errors: %v", issues.ErrorStrings())
	}
	if !strings.Contains(strings.Join(issues.ErrorStrings(), "\n"), "entryApplications") {
		t.Fatal("expected explicit warning about missing entry applications")
	}
}

func TestValidationRejectsInvalidPodSelector(t *testing.T) {
	site, reg := loadExample(t)
	site.Spec.Applications[0].Kubernetes.PodSelector = &config.PodSelector{Mode: "labels"}
	issues := config.Validate(site, reg.Resolve)
	if !issues.HasErrors() {
		t.Fatal("expected labels mode without matchers to fail")
	}
	site.Spec.Applications[0].Kubernetes.PodSelector = &config.PodSelector{Mode: "pod-regex", Pod: "(["}
	if issues := config.Validate(site, reg.Resolve); !issues.HasErrors() {
		t.Fatal("expected invalid pod regex to fail")
	}
}

func TestValidationWarnsWhenPodSelectorMissing(t *testing.T) {
	site, reg := loadExample(t)
	site.Spec.Applications[0].Kubernetes.PodSelector = nil
	issues := config.Validate(site, reg.Resolve)
	if issues.HasErrors() {
		t.Fatalf("unexpected errors: %v", issues.ErrorStrings())
	}
	if !strings.Contains(strings.Join(issues.ErrorStrings(), "\n"), "podSelector") {
		t.Fatal("expected warning about missing podSelector")
	}
}

func TestValidationRejectsReservedSitegenLabel(t *testing.T) {
	site, reg := loadExample(t)
	site.Spec.Applications[0].Prometheus.Labels[config.SitegenAppLabel] = "x"
	site.Spec.Applications[1].Prometheus.Labels[config.SitegenAppLabel] = "x"
	issues := config.Validate(site, reg.Resolve)
	if !issues.HasErrors() {
		t.Fatal("expected reserved label error")
	}
	if !strings.Contains(strings.Join(issues.ErrorStrings(), "\n"), "sitegen_app") {
		t.Fatal("expected sitegen_app reserved label message")
	}
}

func TestContainerMatchersHistoricalMapFormStillLoads(t *testing.T) {
	var doc struct {
		ContainerMatchers config.ContainerMatchers `yaml:"containerMatchers"`
	}
	if err := yaml.Unmarshal([]byte("containerMatchers:\n  container: main\n  image: \"\"\n"), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.ContainerMatchers) != 2 {
		t.Fatalf("matchers = %#v", doc.ContainerMatchers)
	}
	for _, matcher := range doc.ContainerMatchers {
		if matcher.EffectiveOperator() != config.MatchEqual {
			t.Errorf("historical map entry must default to equal: %#v", matcher)
		}
	}
}

func TestValidationAllowsNotEqualEmptyContainerMatcher(t *testing.T) {
	site, reg := loadExample(t)
	site.Spec.Applications[0].Kubernetes.ContainerMatchers = config.ContainerMatchers{
		{Name: "image", Operator: config.MatchNotEqual, Value: ""},
	}
	if issues := config.Validate(site, reg.Resolve); issues.HasErrors() {
		t.Fatalf("not_equal \"\" must be accepted: %v", issues.ErrorStrings())
	}
}

func TestValidationRejectsEmptyEqualContainerMatcher(t *testing.T) {
	site, reg := loadExample(t)
	site.Spec.Applications[0].Kubernetes.ContainerMatchers = config.ContainerMatchers{
		{Name: "image", Operator: config.MatchEqual, Value: ""},
	}
	issues := config.Validate(site, reg.Resolve)
	if !issues.HasErrors() {
		t.Fatal("equal \"\" must be rejected")
	}
}

func TestValidationRejectsJobAndInstanceContainerMatchers(t *testing.T) {
	for _, name := range []string{"job", "instance", "namespace", "pod"} {
		site, reg := loadExample(t)
		site.Spec.Applications[0].Kubernetes.ContainerMatchers = config.ContainerMatchers{
			{Name: name, Operator: config.MatchEqual, Value: "rtd-admin"},
		}
		issues := config.Validate(site, reg.Resolve)
		joined := strings.Join(issues.ErrorStrings(), "\n")
		if !issues.HasErrors() || !strings.Contains(joined, name) {
			t.Fatalf("container matcher %q must be rejected, got: %s", name, joined)
		}
	}
}

func TestValidationRejectsInvalidUTF8Everywhere(t *testing.T) {
	invalid := string([]byte{0xff, 0xfe})
	cases := []struct {
		name   string
		mutate func(*config.Site)
		path   string
	}{
		{"application", func(s *config.Site) { s.Spec.Applications[0].Application = invalid }, ".application"},
		{"namespace", func(s *config.Site) { s.Spec.Applications[0].Namespace = invalid }, ".namespace"},
		{"job", func(s *config.Site) { s.Spec.Applications[0].Prometheus.Job = invalid }, ".prometheus.job"},
		{"label value", func(s *config.Site) { s.Spec.Applications[0].Prometheus.Labels["team"] = invalid }, ".prometheus.labels.team"},
		{"target label", func(s *config.Site) { s.Spec.Applications[0].Prometheus.TargetLabels["cluster"] = invalid }, ".prometheus.targetLabels.cluster"},
		{"component job", func(s *config.Site) { s.Spec.Components[0].Prometheus.Job = invalid }, "spec.components[0].prometheus.job"},
		{"component instance", func(s *config.Site) { s.Spec.Components[0].Prometheus.Instance = invalid }, "spec.components[0].prometheus.instance"},
		{"component label", func(s *config.Site) { s.Spec.Components[0].Prometheus.Labels = map[string]string{"cluster": invalid} }, "spec.components[0].prometheus.labels.cluster"},
		{"pod regex", func(s *config.Site) {
			s.Spec.Applications[0].Kubernetes.PodSelector = &config.PodSelector{Mode: config.PodSelectorPodRegex, Pod: invalid}
		}, ".podSelector.pod"},
		{"container matcher value", func(s *config.Site) {
			s.Spec.Applications[0].Kubernetes.ContainerMatchers = config.ContainerMatchers{{Name: "image", Operator: config.MatchNotEqual, Value: invalid}}
		}, ".containerMatchers[0].value"},
		{"skywalking service", func(s *config.Site) { s.Spec.Applications[0].Skywalking.Service = invalid }, ".skywalking.service"},
	}
	for _, tc := range cases {
		site, reg := loadExample(t)
		tc.mutate(site)
		issues := config.Validate(site, reg.Resolve)
		joined := strings.Join(issues.ErrorStrings(), "\n")
		if !issues.HasErrors() || !strings.Contains(joined, "UTF-8") {
			t.Errorf("%s: expected invalid UTF-8 error, got: %s", tc.name, joined)
			continue
		}
		if !strings.Contains(joined, tc.path) {
			t.Errorf("%s: expected path %q, got: %s", tc.name, tc.path, joined)
		}
	}
}

func TestContainerMatchersListFormLoads(t *testing.T) {
	var doc struct {
		ContainerMatchers config.ContainerMatchers `yaml:"containerMatchers"`
	}
	data := `containerMatchers:
  - name: container
    operator: not_equal
    value: POD
  - name: image
    operator: not_equal
    value: ""
`
	if err := yaml.Unmarshal([]byte(data), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.ContainerMatchers) != 2 || doc.ContainerMatchers[0].Operator != config.MatchNotEqual {
		t.Fatalf("matchers = %#v", doc.ContainerMatchers)
	}
}

func TestValidationRequiresComponentJobAndInstance(t *testing.T) {
	site, reg := loadExample(t)
	site.Spec.Components[0].Prometheus.Job = ""
	site.Spec.Components[0].Prometheus.Instance = ""
	issues := config.Validate(site, reg.Resolve)
	joined := strings.Join(issues.ErrorStrings(), "\n")
	if !issues.HasErrors() || !strings.Contains(joined, "prometheus.job") || !strings.Contains(joined, "prometheus.instance") {
		t.Fatalf("expected component job/instance errors, got: %s", joined)
	}
}

func TestValidationRejectsInvalidContainerMatcherOperator(t *testing.T) {
	site, reg := loadExample(t)
	site.Spec.Applications[0].Kubernetes.ContainerMatchers = config.ContainerMatchers{
		{Name: "container", Operator: "contains", Value: "POD"},
	}
	issues := config.Validate(site, reg.Resolve)
	if !issues.HasErrors() {
		t.Fatal("expected invalid operator error")
	}
	if !strings.Contains(strings.Join(issues.ErrorStrings(), "\n"), "operator") {
		t.Fatal("expected operator field in error")
	}
}

func TestValidationAcceptsTargetLabelsAndRejectsReserved(t *testing.T) {
	site, reg := loadExample(t)
	site.Spec.Applications[0].Prometheus.TargetLabels = map[string]string{"cluster": "test-cluster"}
	if issues := config.Validate(site, reg.Resolve); issues.HasErrors() {
		t.Fatalf("unexpected errors: %v", issues.ErrorStrings())
	}
	site.Spec.Applications[0].Prometheus.TargetLabels["job"] = "other"
	if issues := config.Validate(site, reg.Resolve); !issues.HasErrors() {
		t.Fatal("expected reserved target label error")
	}
}

func TestValidationValidatesSkywalkingUI(t *testing.T) {
	site, reg := loadExample(t)
	site.Spec.Applications[0].Skywalking.UI = config.SkywalkingUI{
		BaseURL:             "https://skywalking.example.com",
		ServiceURLTemplate:  "https://skywalking.example.com/service?name={service}",
		EndpointURLTemplate: "https://skywalking.example.com/endpoint?name={service}",
	}
	if issues := config.Validate(site, reg.Resolve); issues.HasErrors() {
		t.Fatalf("unexpected errors: %v", issues.ErrorStrings())
	}
	site.Spec.Applications[0].Skywalking.UI.BaseURL = "ftp://bad"
	if issues := config.Validate(site, reg.Resolve); !issues.HasErrors() {
		t.Fatal("expected invalid SkyWalking UI URL error")
	}
}

func TestContainerMatcherWarningOnlyWhenPodSelectorExists(t *testing.T) {
	site, reg := loadExample(t)
	for i := range site.Spec.Applications {
		site.Spec.Applications[i].Kubernetes.ContainerMatchers = nil
		site.Spec.Applications[i].Kubernetes.PodSelector = nil
	}
	issues := config.Validate(site, reg.Resolve)
	joined := strings.Join(issues.ErrorStrings(), "\n")
	if strings.Contains(joined, "containerMatchers") {
		t.Fatalf("container matcher warning must not be emitted when no Pod selector exists:\n%s", joined)
	}
	if !strings.Contains(joined, "podSelector") {
		t.Fatal("expected podSelector warning")
	}
}

func TestExampleToolConfig(t *testing.T) {
	cfg, path, err := config.LoadToolConfig("../../sitegen.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if path == "" {
		t.Fatal("config path not reported")
	}
	issues := config.ValidateToolConfig(cfg)
	if issues.HasErrors() {
		t.Fatalf("unexpected tool config errors: %v", issues.ErrorStrings())
	}
}

func TestToolConfigTimeout(t *testing.T) {
	if _, err := config.ParseTimeout("15s"); err != nil {
		t.Fatal(err)
	}
	if _, err := config.ParseTimeout("nope"); err == nil {
		t.Fatal("expected invalid duration error")
	}
	if _, err := config.ParseTimeout("15m"); err == nil {
		t.Fatal("expected max duration error")
	}
}
