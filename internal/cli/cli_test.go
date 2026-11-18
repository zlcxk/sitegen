package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/common/model"

	"site-observability/internal/config"
	"site-observability/internal/metrics"
)

type mapPrompter struct {
	out         *bytes.Buffer
	confirmMap  map[string]bool
	multiMap    map[string][]int
	selectMap   map[string]int
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
	case strings.Contains(label, "Pod 标签名称"):
		value = "label_app"
	case strings.Contains(label, "Pod 标签值"):
		value = "wizard-app"
	case strings.Contains(label, "Pod 名称正则"):
		value = "^wizard-app-.*$"
	case strings.Contains(label, "Pod 标签匹配 Metric"):
		if value == "" {
			value = "kube_pod_labels"
		}
	case strings.Contains(label, "容器过滤标签名称"):
		value = "container"
	case strings.Contains(label, "容器过滤标签值"):
		value = "app"
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
	for key, value := range p.selectMap {
		if strings.Contains(label, key) {
			if value < 0 || value >= len(options) {
				return defaultIndex, nil
			}
			return value, nil
		}
	}
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
			"继续添加域名":     false,
			"是否关联应用":     true,
			"自动生成":       true,
			"继续添加应用":     false,
			"是否配置依赖组件":   true,
			"继续添加依赖组件":   false,
			"是否配置容器过滤规则": false,
			"确认写入":       true,
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
	podSelector := site.Spec.Applications[0].Kubernetes.PodSelector
	if podSelector == nil || podSelector.Mode != config.PodSelectorLabels ||
		podSelector.Matchers["label_app"] != "wizard-app" {
		t.Errorf("Kubernetes pod selector was not collected by the wizard: %#v", podSelector)
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
		Prompter: &mapPrompter{
			out:         out,
			useDefaults: true,
			confirmMap: map[string]bool{
				"是否配置容器过滤规则": false,
			},
		},
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
	if edited.Spec.Applications[0].Kubernetes.PodSelector == nil {
		t.Fatal("existing Kubernetes pod selector was not preserved")
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
				"container":"app","node":"node-1","phase":"Running","label_app":"rtd-admin"
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
		"--prometheus-url", server.URL, "--strict-check",
		"--dashboards-dir", filepath.Join(dir, "dashboards"),
		"--reports-dir", filepath.Join(dir, "reports"),
	}, strings.NewReader(""), &out, &errOut, "test")
	if code != 0 {
		t.Fatalf("exit = %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "Prometheus") || !strings.Contains(out.String(), "PASS=") {
		t.Fatalf("expected strict Prometheus check\n%s", out.String())
	}
	if strings.Contains(out.String(), "FAIL=") && !strings.Contains(out.String(), "FAIL=0") {
		t.Fatalf("unexpected Prometheus failures\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "dashboards", "order-admin-test.json")); err != nil {
		t.Fatal(err)
	}
}

func TestBuildIgnoresPrometheusFailureByDefault(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"status":"error","errorType":"internal","error":"boom"}`)
	}))
	defer bad.Close()
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	code := Execute([]string{
		"build", "--site", "../../sites/examples/order-admin-test.yaml",
		"--prometheus-url", bad.URL,
		"--dashboards-dir", filepath.Join(dir, "dashboards"),
		"--reports-dir", filepath.Join(dir, "reports"),
	}, strings.NewReader(""), &out, &errOut, "test")
	if code != 0 {
		t.Fatalf("non-strict build must not fail on environment data\nstdout:\n%s\nstderr:\n%s", out.String(), errOut.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "dashboards", "order-admin-test.json")); err != nil {
		t.Fatalf("dashboard was not generated: %v", err)
	}
	if !strings.Contains(out.String(), "sitegen check") {
		t.Fatalf("build output must direct the user to check\n%s", out.String())
	}
}

func TestStrictBuildFailurePreservesPreviousDashboard(t *testing.T) {
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
		"--prometheus-url", bad.URL, "--strict-check",
		"--dashboards-dir", filepath.Join(dir, "dashboards"),
		"--reports-dir", filepath.Join(dir, "reports"),
	}, strings.NewReader(""), &out, &errOut, "test")
	if code == 0 {
		t.Fatal("expected strict build failure")
	}
	after, err := os.ReadFile(dashboardPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(previous, after) {
		t.Fatal("failed build overwrote the previous dashboard")
	}
}

type fakeProbe struct {
	metricExists bool
	series       []model.LabelSet
}

func (f fakeProbe) MetricExists(context.Context, string, time.Time, time.Time) (bool, error) {
	return f.metricExists, nil
}

func (f fakeProbe) Series(context.Context, string, time.Time, time.Time) ([]model.LabelSet, error) {
	return f.series, nil
}

func (f fakeProbe) Query(context.Context, string) (model.Value, error) {
	return model.Vector{}, nil
}

func TestWizardRejectsMissingPodLabelsMetric(t *testing.T) {
	p := &navPrompter{
		mapPrompter: mapPrompter{confirmMap: wizardBaseConfirmMap()},
		actions: []StepAction{
			StepNext, // metadata
			StepNext, // domains
			StepNext, // applications
			StepNext, // kubernetes: labels unavailable, choose regex
			StepNext, // entry applications
			StepSkip, // target labels
			StepSkip, // container matchers
			StepSkip, // skywalking
			StepSkip, // components
			StepSkip, // dependencies
		},
	}
	dir := t.TempDir()
	wizard, _ := newTestWizard(t, p, dir)
	wizard.Probe = fakeProbe{
		metricExists: false,
		series:       []model.LabelSet{{"namespace": "test", "pod": "wizard-app-1", "container": "main"}},
	}
	site, err := wizard.Create()
	if err != nil {
		t.Fatalf("Create() error = %v\noutput:\n%s", err, wizard.Out)
	}
	selector := site.Spec.Applications[0].Kubernetes.PodSelector
	if selector == nil || selector.Mode != config.PodSelectorPodRegex {
		t.Fatalf("missing kube_pod_labels must force pod-regex, got %#v", selector)
	}
	if !strings.Contains(wizard.Out.(*bytes.Buffer).String(), "不存在") {
		t.Fatal("wizard must explain that kube_pod_labels does not exist")
	}
}

// navPrompter adds a deterministic step-navigation script on top of
// mapPrompter. It lets tests exercise back/edit/skip/quit without a TTY.
type navPrompter struct {
	mapPrompter
	actions []StepAction
	used    int
}

func (p *navPrompter) ChooseStepAction(title string, index, total int, canBack, optional bool) (StepAction, error) {
	if p.used >= len(p.actions) {
		return StepNext, nil
	}
	action := p.actions[p.used]
	p.used++
	return action, nil
}

func wizardBaseConfirmMap() map[string]bool {
	return map[string]bool{
		"继续添加域名":   false,
		"是否关联应用":   true,
		"继续添加应用":   false,
		"继续添加依赖组件": false,
		"确认写入":     true,
	}
}

func newTestWizard(t *testing.T, prompter Prompter, sitesDir string) (*Wizard, *metrics.Registry) {
	t.Helper()
	reg, err := metrics.LoadDefault()
	if err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	if mp, ok := prompter.(*navPrompter); ok {
		mp.out = out
	}
	return &Wizard{Prompter: prompter, Registry: reg, Out: out, SitesDir: sitesDir}, reg
}

func TestWizardBackPreservesLaterData(t *testing.T) {
	p := &navPrompter{
		mapPrompter: mapPrompter{confirmMap: wizardBaseConfirmMap()},
		actions: []StepAction{
			StepNext, // metadata
			StepNext, // domains
			StepNext, // applications
			StepBack, // return from kubernetes to applications
			StepNext, // applications: keep existing
			StepNext, // kubernetes
			StepNext, // entry applications
			StepSkip, // target labels
			StepSkip, // container matchers
			StepSkip, // skywalking
			StepSkip, // components
			StepSkip, // dependencies
		},
	}
	dir := t.TempDir()
	wizard, reg := newTestWizard(t, p, dir)
	site, err := wizard.Create()
	if err != nil {
		t.Fatalf("Create() error = %v\noutput:\n%s", err, wizard.Out)
	}
	if len(site.Spec.Applications) != 1 {
		t.Fatalf("application was duplicated or lost: %#v", site.Spec.Applications)
	}
	if len(site.Spec.Domains) != 1 || len(site.Spec.Applications[0].Dependencies) != 0 {
		t.Fatal("returning to an earlier stage must preserve later collected data")
	}
	if issues := config.Validate(site, reg.Resolve); issues.HasErrors() {
		t.Fatalf("validation errors: %v", issues.ErrorStrings())
	}
}

func TestWizardQuitSavesDraftAndDoesNotWriteSite(t *testing.T) {
	p := &navPrompter{
		mapPrompter: mapPrompter{confirmMap: wizardBaseConfirmMap()},
		actions:     []StepAction{StepNext, StepQuit},
	}
	dir := t.TempDir()
	wizard, _ := newTestWizard(t, p, dir)
	_, err := wizard.Create()
	if !errors.Is(err, ErrDraftSaved) {
		t.Fatalf("Create() error = %v, want ErrDraftSaved", err)
	}
	files, err := FindDrafts(dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("draft files = %#v, err = %v", files, err)
	}
	draft, err := LoadDraft(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if draft.Stage != "domains" {
		t.Errorf("draft stage = %q, want domains", draft.Stage)
	}
	if draft.Site.Metadata.ID != "wizard-site" {
		t.Errorf("draft site ID = %q", draft.Site.Metadata.ID)
	}
	if _, err := os.Stat(filepath.Join(dir, "wizard-site.yaml")); !os.IsNotExist(err) {
		t.Fatal("quit must not write a formal site YAML")
	}
}

func TestWizardInterruptionKeepsLastCompletedDraft(t *testing.T) {
	// Simulate an abrupt interrupt: the reader ends after the metadata stage.
	// The draft written after metadata must still be recoverable.
	out := &bytes.Buffer{}
	input := strings.NewReader("\ninterrupt-site\nInterrupt Site\ntest\n\n")
	reg, err := metrics.LoadDefault()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	wizard := &Wizard{
		Prompter: NewTerminalPrompter(input, out),
		Registry: reg,
		Out:      out,
		SitesDir: dir,
	}
	if _, err := wizard.Create(); !errors.Is(err, ErrCancelled) {
		t.Fatalf("Create() error = %v, want ErrCancelled", err)
	}
	files, err := FindDrafts(dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("draft files = %#v, err = %v", files, err)
	}
	draft, err := LoadDraft(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if draft.Stage != "domains" || draft.Site.Metadata.ID != "interrupt-site" {
		t.Fatalf("unexpected draft: stage=%q id=%q", draft.Stage, draft.Site.Metadata.ID)
	}
}

func TestWizardResumeFromPreviewDraft(t *testing.T) {
	existing, err := config.Load("../../sites/examples/order-admin-test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	p := &mapPrompter{confirmMap: map[string]bool{"确认写入": true}}
	dir := t.TempDir()
	wizard, _ := newTestWizard(t, p, dir)
	wizard.Resume = &Draft{Stage: "preview", Site: existing}
	site, err := wizard.Create()
	if err != nil {
		t.Fatalf("resume Create() error = %v", err)
	}
	if site.Metadata.ID != existing.Metadata.ID {
		t.Fatalf("resumed site ID = %q", site.Metadata.ID)
	}
}

func TestTerminalPrompterRetriesInvalidInputAndSupportsBack(t *testing.T) {
	out := &bytes.Buffer{}
	p := NewTerminalPrompter(strings.NewReader("BAD ID\ngood-id\n"), out)
	value, err := p.Input("站点 ID", "", func(value string) error {
		return config.ValidateSlug(value, 35)
	})
	if err != nil {
		t.Fatal(err)
	}
	if value != "good-id" {
		t.Fatalf("value = %q", value)
	}
	if !strings.Contains(out.String(), "输入无效") {
		t.Fatal("invalid input must be reported and re-prompted")
	}

	p = NewTerminalPrompter(strings.NewReader(":b\n"), out)
	if _, err := p.Input("任意输入", "", nil); !errors.Is(err, ErrStepBack) {
		t.Fatalf(":b error = %v, want ErrStepBack", err)
	}
}

func TestEditOnlyChangesSelectedPartition(t *testing.T) {
	reg, err := metrics.LoadDefault()
	if err != nil {
		t.Fatal(err)
	}
	existing, err := config.Load("../../sites/examples/order-admin-test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	original := *existing
	out := &bytes.Buffer{}
	p := &mapPrompter{
		out: out,
		selectMap: map[string]int{
			"选择要编辑的配置分区": 4, // Kubernetes 容器过滤
		},
		confirmMap: map[string]bool{
			"应用 rtd-admin-test 配置容器过滤规则":   true,
			"应用 user-center-test 配置容器过滤规则": false,
			"继续添加容器过滤条件":                   false,
			"确认写入修改":                       true,
		},
	}
	wizard := &Wizard{Prompter: p, Registry: reg, Out: out}
	edited, err := wizard.Edit(existing)
	if err != nil {
		t.Fatalf("Edit() error = %v\noutput:\n%s", err, out.String())
	}
	if len(edited.Spec.Domains) != len(original.Spec.Domains) ||
		!reflect.DeepEqual(edited.Spec.Domains[0], original.Spec.Domains[0]) {
		t.Fatal("editing container matchers changed domain configuration")
	}
	if len(edited.Spec.Applications) != len(original.Spec.Applications) {
		t.Fatal("editing container matchers changed the application count")
	}
	if len(edited.Spec.Applications[0].Dependencies) != len(original.Spec.Applications[0].Dependencies) {
		t.Fatal("editing container matchers changed dependencies")
	}
	matchers := edited.Spec.Applications[0].Kubernetes.ContainerMatchers
	if len(matchers) != 1 || matchers[0].Name != "container" || matchers[0].Value != "app" {
		t.Fatalf("container matchers = %#v", matchers)
	}
	if len(edited.Spec.Applications[1].Kubernetes.ContainerMatchers) != 0 {
		t.Fatal("application the user declined must not be modified")
	}
}

func TestEditEntryApplicationsDoesNotRepromptDomainFields(t *testing.T) {
	reg, err := metrics.LoadDefault()
	if err != nil {
		t.Fatal(err)
	}
	existing, err := config.Load("../../sites/examples/order-admin-test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// Give the second application HTTP so there are two selectable entry apps.
	for i := range existing.Spec.Applications {
		if existing.Spec.Applications[i].ID == "user-center-test" {
			existing.Spec.Applications[i].Metrics = append(existing.Spec.Applications[i].Metrics, "http")
		}
	}
	originalURL := existing.Spec.Domains[0].URL
	originalBlackbox := existing.Spec.Domains[0].Blackbox
	out := &bytes.Buffer{}
	p := &mapPrompter{
		out: out,
		selectMap: map[string]int{
			"选择要编辑的配置分区": 1, // 域名和入口应用
			"域名配置":       0, // 保留现有域名
		},
		multiMap: map[string][]int{
			"的入口应用": {1},
		},
		confirmMap: map[string]bool{"确认写入修改": true},
	}
	wizard := &Wizard{Prompter: p, Registry: reg, Out: out}
	edited, err := wizard.Edit(existing)
	if err != nil {
		t.Fatalf("Edit() error = %v\noutput:\n%s", err, out.String())
	}
	if edited.Spec.Domains[0].URL != originalURL || edited.Spec.Domains[0].Blackbox != originalBlackbox {
		t.Fatal("changing entry applications must not rewrite domain fields")
	}
	if len(edited.Spec.Domains[0].EntryApplications) != 1 ||
		edited.Spec.Domains[0].EntryApplications[0] != "user-center-test" {
		t.Fatalf("entry applications = %#v", edited.Spec.Domains[0].EntryApplications)
	}
	if len(edited.Spec.Applications) != len(existing.Spec.Applications) {
		t.Fatal("changing entry applications must not modify applications")
	}
	if !strings.Contains(out.String(), "变更摘要") {
		t.Fatal("edit must print a change summary")
	}
}
