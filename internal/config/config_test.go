package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

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
