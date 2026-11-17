package cli

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"site-observability/internal/config"
	"site-observability/internal/metrics"
)

type mapPrompter struct {
	out         *bytes.Buffer
	confirmMap  map[string]bool
	multiMap    map[string][]int
	useDefaults bool
}

func (p *mapPrompter) Input(label, defaultValue string, validate func(string) error) (string, error) {
	value := defaultValue
	if p.useDefaults {
		if validate != nil {
			if err := validate(value); err != nil {
				return "", err
			}
		}
		return value, nil
	}
	switch {
	case strings.Contains(label, "站点 ID"):
		value = "wizard-site"
	case strings.Contains(label, "站点名称"):
		value = "Wizard Site"
	case strings.Contains(label, "环境"):
		value = "test"
	case strings.Contains(label, "域名 URL"):
		value = "https://wizard.example.com"
	case strings.Contains(label, "组件名称"):
		value = "MySQL Wizard"
	case strings.Contains(label, "Application"):
		value = "wizard-app"
	case strings.Contains(label, "Namespace"):
		value = "test"
	case strings.Contains(label, "Prometheus Job") && defaultValue == "":
		value = "mysql-wizard"
	case strings.Contains(label, "Instance"):
		value = "mysql:3306"
	}
	if validate != nil {
		if err := validate(value); err != nil {
			return "", err
		}
	}
	return value, nil
}

func (p *mapPrompter) Confirm(label string, defaultValue bool) (bool, error) {
	for key, value := range p.confirmMap {
		if strings.Contains(label, key) {
			return value, nil
		}
	}
	return defaultValue, nil
}

func (p *mapPrompter) Select(label string, options []string, defaultIndex int) (int, error) {
	return defaultIndex, nil
}

func (p *mapPrompter) MultiSelect(label string, options []string, defaults []bool) ([]int, error) {
	for key, value := range p.multiMap {
		if strings.Contains(label, key) {
			return value, nil
		}
	}
	var selected []int
	for i, def := range defaults {
		if def {
			selected = append(selected, i)
		}
	}
	return selected, nil
}

func TestWizardCreate(t *testing.T) {
	reg, err := metrics.LoadDefault()
	if err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	prompter := &mapPrompter{
		out: out,
		confirmMap: map[string]bool{
			"继续添加域名":   false,
			"是否关联应用":   true,
			"自动生成":     true,
			"继续添加应用":   false,
			"是否配置依赖组件": true,
			"继续添加依赖组件": false,
			"确认写入":     true,
		},
		multiMap: map[string][]int{
			"的依赖": {0},
		},
	}
	wizard := &Wizard{Prompter: prompter, Registry: reg, Out: out}
	site, err := wizard.Create()
	if err != nil {
		t.Fatalf("Create() error = %v\noutput:\n%s", err, out.String())
	}
	if site.Metadata.ID != "wizard-site" {
		t.Errorf("site ID = %q", site.Metadata.ID)
	}
	if len(site.Spec.Domains) != 1 || len(site.Spec.Applications) != 1 || len(site.Spec.Components) != 1 {
		t.Fatalf("unexpected object counts: %#v", site.Spec)
	}
	if site.Spec.Applications[0].Skywalking.Service != "wizard-app_test" {
		t.Errorf("service = %q", site.Spec.Applications[0].Skywalking.Service)
	}
	if got := site.Spec.Applications[0].Dependencies; len(got) != 1 || got[0] != "mysql-wizard" {
		t.Errorf("dependencies = %#v", got)
	}
	issues := config.Validate(site, reg.Resolve)
	if issues.HasErrors() {
		t.Fatalf("validation errors: %v", issues.ErrorStrings())
	}
	if !strings.Contains(out.String(), "配置预览") {
		t.Error("preview not printed")
	}
}

func TestWizardEditKeepsExistingGraph(t *testing.T) {
	reg, err := metrics.LoadDefault()
	if err != nil {
		t.Fatal(err)
	}
	existing, err := config.Load("../../sites/examples/order-admin-test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	wizard := &Wizard{
		Prompter: &mapPrompter{out: out, useDefaults: true},
		Registry: reg,
		Out:      out,
	}
	edited, err := wizard.Edit(existing)
	if err != nil {
		t.Fatalf("Edit() error = %v\noutput:\n%s", err, out.String())
	}
	if edited.Metadata.ID != existing.Metadata.ID || edited.Metadata.Name != existing.Metadata.Name {
		t.Fatal("metadata changed unexpectedly")
	}
	if len(edited.Spec.Applications) != len(existing.Spec.Applications) ||
		len(edited.Spec.Components) != len(existing.Spec.Components) {
		t.Fatal("object graph changed unexpectedly")
	}
	if len(edited.Spec.Applications[0].Dependencies) != len(existing.Spec.Applications[0].Dependencies) {
		t.Fatal("dependencies were not preserved")
	}
	issues := config.Validate(edited, reg.Resolve)
	if issues.HasErrors() {
		t.Fatalf("validation errors: %v", issues.ErrorStrings())
	}
}

func TestGenerateAndBatchContinueOnFailure(t *testing.T) {
	dir := t.TempDir()
	sitesDir := filepath.Join(dir, "sites")
	dashDir := filepath.Join(dir, "dashboards")
	reportDir := filepath.Join(dir, "reports")
	if err := os.MkdirAll(sitesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	example, err := os.ReadFile("../../sites/examples/order-admin-test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sitesDir, "good.yaml"), example, 0o644); err != nil {
		t.Fatal(err)
	}
	bad := strings.Replace(string(example), "id: order-admin-test", "id: BAD ID", 1)
	if err := os.WriteFile(filepath.Join(sitesDir, "bad.yaml"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := Execute([]string{
		"generate", "--all",
		"--sites-dir", sitesDir,
		"--dashboards-dir", dashDir,
		"--reports-dir", reportDir,
	}, strings.NewReader(""), &out, &errOut, "test")
	if code == 0 {
		t.Fatalf("expected non-zero exit code\nstdout:\n%s\nstderr:\n%s", out.String(), errOut.String())
	}
	if _, err := os.Stat(filepath.Join(dashDir, "order-admin-test.json")); err != nil {
		t.Fatalf("valid site dashboard was not generated: %v", err)
	}
	if !strings.Contains(out.String(), "批量处理汇总") {
		t.Error("batch summary missing")
	}
}

func TestValidateCommand(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	code := Execute([]string{
		"validate", "--site", "../../sites/examples/order-admin-test.yaml",
		"--reports-dir", filepath.Join(dir, "reports"),
	}, strings.NewReader(""), &out, &errOut, "test")
	if code != 0 {
		t.Fatalf("exit = %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errOut.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "reports", "order-admin-test-validation.txt")); err != nil {
		t.Fatal(err)
	}
}

func TestBuildWithoutPrometheusContinues(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	code := Execute([]string{
		"build", "--site", "../../sites/examples/order-admin-test.yaml",
		"--dashboards-dir", filepath.Join(dir, "dashboards"),
		"--reports-dir", filepath.Join(dir, "reports"),
	}, strings.NewReader(""), &out, &errOut, "test")
	if code != 0 {
		t.Fatalf("exit = %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "SKIP=1") {
		t.Fatalf("expected skipped Prometheus check\n%s", out.String())
	}
}

func TestInitCommand(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "sitegen.yaml")
	var out, errOut bytes.Buffer
	code := Execute([]string{
		"init",
		"--config", configPath,
		"--sites-dir", filepath.Join(dir, "sites"),
		"--dashboards-dir", filepath.Join(dir, "dashboards"),
		"--reports-dir", filepath.Join(dir, "reports"),
	}, strings.NewReader(""), &out, &errOut, "test")
	if code != 0 {
		t.Fatalf("exit = %d\nstderr:\n%s", code, errOut.String())
	}
	if _, err := os.Stat(configPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sites")); err != nil {
		t.Fatal(err)
	}
}

func okPromHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = r.ParseForm()
		switch {
		case strings.HasSuffix(r.URL.Path, "/api/v1/query"):
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"job":"app"},"value":[1700000000,"1"]}]}}`)
		case strings.HasSuffix(r.URL.Path, "/api/v1/series"):
			fmt.Fprint(w, `{"status":"success","data":[{
				"application":"rtd-admin","namespace":"test","job":"app","instance":"x",
				"status":"200","method":"GET","uri":"/orders","area":"heap","pod":"pod-1",
				"container":"app","node":"node-1","phase":"Running"
			}]}`)
		case strings.HasSuffix(r.URL.Path, "/api/v1/label/__name__/values"):
			matches := r.Form["match[]"]
			name := ""
			if len(matches) > 0 {
				name = matches[0]
			}
			fmt.Fprintf(w, `{"status":"success","data":[%q]}`, name)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"status":"error","errorType":"not_found","error":"not found"}`)
		}
	})
}

func TestBuildWithPrometheus(t *testing.T) {
	server := httptest.NewServer(okPromHandler())
	defer server.Close()
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	code := Execute([]string{
		"build", "--site", "../../sites/examples/order-admin-test.yaml",
		"--prometheus-url", server.URL,
		"--dashboards-dir", filepath.Join(dir, "dashboards"),
		"--reports-dir", filepath.Join(dir, "reports"),
	}, strings.NewReader(""), &out, &errOut, "test")
	if code != 0 {
		t.Fatalf("exit = %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errOut.String())
	}
	if strings.Contains(out.String(), "SKIP=1") {
		t.Fatalf("Prometheus check was skipped\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "dashboards", "order-admin-test.json")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "PASS=") {
		t.Fatal("expected pass summary")
	}
}

func TestBuildFailurePreservesPreviousDashboard(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"status":"error","errorType":"internal","error":"boom"}`)
	}))
	defer bad.Close()
	dir := t.TempDir()
	dashboardPath := filepath.Join(dir, "dashboards", "order-admin-test.json")
	if err := os.MkdirAll(filepath.Dir(dashboardPath), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := []byte(`{"keep":"me"}`)
	if err := os.WriteFile(dashboardPath, previous, 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := Execute([]string{
		"build", "--site", "../../sites/examples/order-admin-test.yaml",
		"--prometheus-url", bad.URL,
		"--dashboards-dir", filepath.Join(dir, "dashboards"),
		"--reports-dir", filepath.Join(dir, "reports"),
	}, strings.NewReader(""), &out, &errOut, "test")
	if code == 0 {
		t.Fatal("expected build failure")
	}
	after, err := os.ReadFile(dashboardPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(previous, after) {
		t.Fatal("failed build overwrote the previous dashboard")
	}
}
