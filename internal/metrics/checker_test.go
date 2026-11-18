package metrics

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/common/model"

	"site-observability/internal/config"
	"site-observability/internal/output"
)

type fakePromAPI struct {
	missing  map[string]bool
	noSeries map[string]bool
	labels   map[string]model.LabelSet
	queryErr error
}

func (f *fakePromAPI) Ping(context.Context) error { return nil }

func (f *fakePromAPI) MetricExists(_ context.Context, metric string, _, _ time.Time) (bool, error) {
	return !f.missing[metric], nil
}

func (f *fakePromAPI) Series(_ context.Context, match string, _, _ time.Time) ([]model.LabelSet, error) {
	metric := match
	if idx := strings.IndexByte(match, '{'); idx >= 0 {
		metric = match[:idx]
	}
	if f.noSeries[metric] {
		return nil, nil
	}
	if labels, ok := f.labels[metric]; ok {
		return []model.LabelSet{labels}, nil
	}
	return []model.LabelSet{{
		"application": "rtd-admin",
		"namespace":   "test",
		"job":         "rtd-admin",
		"instance":    "https://admin-test.rtdgo.tech",
		"status":      "200",
		"method":      "GET",
		"uri":         "/orders",
		"area":        "heap",
		"pod":         "rtd-admin-0",
		"container":   "app",
		"node":        "node-1",
		"phase":       "Running",
	}}, nil
}

func (f *fakePromAPI) Query(context.Context, string) (model.Value, error) {
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	return model.Vector{&model.Sample{Metric: model.Metric{}, Value: 1, Timestamp: model.Time(1)}}, nil
}

func smallSite() *config.Site {
	site := &config.Site{
		APIVersion: config.APIVersion,
		Kind:       config.KindSite,
		Metadata: config.Metadata{
			ID:          "check-site",
			Name:        "Check Site",
			Environment: "test",
		},
		Spec: config.Spec{
			Domains: []config.Domain{{
				ID:       "admin",
				URL:      "https://admin.example.com",
				Blackbox: config.Blackbox{Job: "blackbox", Instance: "https://admin.example.com"},
			}},
			Applications: []config.Application{{
				ID:           "app-test",
				Application:  "rtd-admin",
				Namespace:    "test",
				Prometheus:   config.Prometheus{Job: "rtd-admin"},
				Skywalking:   config.Skywalking{Service: "rtd-admin_test"},
				Metrics:      []string{"http"},
				Dependencies: []string{"mysql-test"},
			}},
			Components: []config.Component{{
				ID:   "mysql-test",
				Name: "MySQL Test",
				Type: "mysql",
				Prometheus: config.ComponentPrometheus{
					Job:      "mysql",
					Instance: "mysql:3306",
				},
				Metrics: []string{"mysql-basic"},
			}},
		},
	}
	config.Normalize(site)
	return site
}

func runChecker(t *testing.T, api PromAPI) []output.Check {
	t.Helper()
	reg, err := LoadDefault()
	if err != nil {
		t.Fatal(err)
	}
	checker := &Checker{
		API:      api,
		Site:     smallSite(),
		Registry: reg,
		Options: CheckOptions{
			Lookback:    time.Hour,
			Concurrency: 3,
			Now:         func() time.Time { return time.Unix(1700000000, 0) },
		},
	}
	return checker.Run(context.Background())
}

func TestCheckerPassesWithMatchingData(t *testing.T) {
	checks := runChecker(t, &fakePromAPI{})
	for _, check := range checks {
		if check.Status == output.Fail {
			t.Fatalf("unexpected FAIL: %s / %s: %s", check.Category, check.Name, check.Detail)
		}
	}
	hasSkip := false
	for _, check := range checks {
		if check.Status == output.Skip {
			hasSkip = true
		}
	}
	if !hasSkip {
		t.Error("expected SKIP entries for disabled templates")
	}
	// Determinism.
	second := runChecker(t, &fakePromAPI{})
	if len(checks) != len(second) {
		t.Fatal("check result count is not deterministic")
	}
	for i := range checks {
		if checks[i].Name != second[i].Name || checks[i].Status != second[i].Status {
			t.Fatalf("check order changed at %d", i)
		}
	}
}

func TestCheckerMissingMetric(t *testing.T) {
	checks := runChecker(t, &fakePromAPI{missing: map[string]bool{"probe_success": true}})
	found := false
	for _, check := range checks {
		if strings.Contains(check.Name, "probe_success") && check.Status == output.Fail &&
			strings.Contains(check.Detail, "指标不存在") {
			found = true
		}
	}
	if !found {
		t.Fatal("missing metric not reported as FAIL")
	}
}

func TestCheckerNoSeriesIsWarn(t *testing.T) {
	checks := runChecker(t, &fakePromAPI{noSeries: map[string]bool{"probe_duration_seconds": true}})
	found := false
	for _, check := range checks {
		if strings.Contains(check.Name, "probe_duration_seconds") && check.Status == output.Warn {
			found = true
		}
	}
	if !found {
		t.Fatal("metric without series not reported as WARN")
	}
}

func TestCheckerMissingRequiredLabel(t *testing.T) {
	labels := model.LabelSet{
		"application": "rtd-admin",
		"namespace":   "test",
		"job":         "rtd-admin",
	}
	checks := runChecker(t, &fakePromAPI{labels: map[string]model.LabelSet{
		"http_server_requests_seconds_count": labels,
	}})
	found := false
	for _, check := range checks {
		if strings.Contains(check.Name, "http_server_requests_seconds_count") &&
			check.Status == output.Fail && strings.Contains(check.Detail, "必需标签不完整") {
			found = true
		}
	}
	if !found {
		t.Fatal("missing labels not reported as FAIL")
	}
}

type selectivePromAPI struct {
	fakePromAPI
	series func(match string) []model.LabelSet
}

func (s *selectivePromAPI) Series(_ context.Context, match string, _, _ time.Time) ([]model.LabelSet, error) {
	if s.series != nil {
		return s.series(match), nil
	}
	return s.fakePromAPI.Series(context.Background(), match, time.Time{}, time.Time{})
}

func TestPodAssociationDoesNotIncludeUnrelatedPods(t *testing.T) {
	reg, err := LoadDefault()
	if err != nil {
		t.Fatal(err)
	}
	site := smallSite()
	site.Spec.Applications[0].Metrics = append(site.Spec.Applications[0].Metrics, "kubernetes")
	site.Spec.Applications[0].Kubernetes.PodSelector = &config.PodSelector{
		Mode:     config.PodSelectorLabels,
		Metric:   config.DefaultPodLabelsMetric,
		Matchers: map[string]string{"label_app": "rtd-admin"},
	}
	api := &selectivePromAPI{series: func(match string) []model.LabelSet {
		switch {
		case strings.HasPrefix(match, "kube_pod_labels"):
			return []model.LabelSet{
				{"namespace": "test", "pod": "rtd-admin-1", "label_app": "rtd-admin"},
			}
		case strings.HasPrefix(match, "container_memory_working_set_bytes"):
			return []model.LabelSet{
				{"namespace": "test", "pod": "rtd-admin-1"},
				{"namespace": "test", "pod": "user-center-1"},
				{"namespace": "test", "pod": "unrelated-worker-1"},
			}
		}
		return nil
	}}
	checker := &Checker{
		API:      api,
		Site:     site,
		Registry: reg,
		Options: CheckOptions{
			Lookback:    time.Hour,
			Concurrency: 2,
			Now:         func() time.Time { return time.Unix(1700000000, 0) },
		},
	}
	checks := checker.Run(context.Background())
	found := false
	for _, check := range checks {
		if check.Name == "Pod 归属/app-test/kube_pod_labels" {
			found = true
			if check.Status != output.Pass {
				t.Fatalf("pod association = %s: %s", check.Status, check.Detail)
			}
			if !strings.Contains(check.Detail, "匹配 1 个 Pod") || !strings.Contains(check.Detail, "1 个与 cAdvisor") {
				t.Fatalf("unexpected pod association detail: %s", check.Detail)
			}
		}
	}
	if !found {
		t.Fatal("pod association check missing")
	}
}

type capturingPromAPI struct {
	fakePromAPI
	matches []string
	series  func(match string) []model.LabelSet
}

func (c *capturingPromAPI) Series(_ context.Context, match string, _, _ time.Time) ([]model.LabelSet, error) {
	c.matches = append(c.matches, match)
	if c.series != nil {
		return c.series(match), nil
	}
	return c.fakePromAPI.Series(context.Background(), match, time.Time{}, time.Time{})
}

func TestCheckerUpSelectorUsesOnlyJobAndTargetLabels(t *testing.T) {
	reg, err := LoadDefault()
	if err != nil {
		t.Fatal(err)
	}
	site := smallSite()
	site.Spec.Applications[0].Prometheus.TargetLabels = map[string]string{"cluster": "test-cluster"}
	api := &capturingPromAPI{}
	checker := &Checker{
		API:      api,
		Site:     site,
		Registry: reg,
		Options: CheckOptions{
			Lookback:    time.Hour,
			Concurrency: 1,
			Now:         func() time.Time { return time.Unix(1700000000, 0) },
		},
	}
	checker.Run(context.Background())
	found := false
	for _, match := range api.matches {
		if !strings.HasPrefix(match, "up{") {
			continue
		}
		found = true
		if !strings.Contains(match, `job="rtd-admin"`) || !strings.Contains(match, `cluster="test-cluster"`) {
			t.Fatalf("up selector missing job/target label: %s", match)
		}
		if strings.Contains(match, "application=") || strings.Contains(match, "namespace=") {
			t.Fatalf("up selector must not use application identity labels: %s", match)
		}
	}
	if !found {
		t.Fatal("no up selector was checked")
	}
}

func TestKubernetesCoverageDetectsMetricMissingForAppPod(t *testing.T) {
	reg, err := LoadDefault()
	if err != nil {
		t.Fatal(err)
	}
	site := smallSite()
	site.Spec.Applications[0].Metrics = []string{"jvm", "kubernetes"}
	site.Spec.Applications[0].Kubernetes.PodSelector = &config.PodSelector{
		Mode:     config.PodSelectorLabels,
		Metric:   config.DefaultPodLabelsMetric,
		Matchers: map[string]string{"label_app": "rtd-admin"},
	}
	api := &selectivePromAPI{series: func(match string) []model.LabelSet {
		switch {
		case strings.HasPrefix(match, "kube_pod_labels"):
			return []model.LabelSet{{"namespace": "test", "pod": "rtd-admin-1", "label_app": "rtd-admin"}}
		case strings.HasPrefix(match, "container_cpu_usage_seconds_total"):
			return []model.LabelSet{
				{"namespace": "test", "pod": "rtd-admin-1", "container": "app"},
				{"namespace": "test", "pod": "unrelated-1", "container": "app"},
			}
		case strings.HasPrefix(match, "container_cpu_cfs_"):
			return []model.LabelSet{{"namespace": "test", "pod": "rtd-admin-1", "container": "app"}}
		case strings.HasPrefix(match, "container_memory_working_set_bytes"):
			return []model.LabelSet{{"namespace": "test", "pod": "unrelated-1", "container": "app"}}
		}
		return nil
	}}
	checker := &Checker{
		API:      api,
		Site:     site,
		Registry: reg,
		Options: CheckOptions{
			Lookback:    time.Hour,
			Concurrency: 1,
			Now:         func() time.Time { return time.Unix(1700000000, 0) },
		},
	}
	checks := checker.Run(context.Background())
	status := map[string]output.Status{}
	for _, check := range checks {
		status[check.Name] = check.Status
	}
	cpu := "Kubernetes 指标/app-test/container_cpu_usage_seconds_total"
	memory := "Kubernetes 指标/app-test/container_memory_working_set_bytes"
	if status[cpu] != output.Pass {
		t.Errorf("cpu coverage status = %s, want PASS", status[cpu])
	}
	if status[memory] != output.Warn {
		t.Errorf("memory coverage status = %s, want WARN", status[memory])
	}
}

func TestContainerMatcherFiltersEverythingFails(t *testing.T) {
	reg, err := LoadDefault()
	if err != nil {
		t.Fatal(err)
	}
	site := smallSite()
	site.Spec.Applications[0].Metrics = []string{"jvm", "kubernetes"}
	site.Spec.Applications[0].Kubernetes.PodSelector = &config.PodSelector{
		Mode:     config.PodSelectorLabels,
		Metric:   config.DefaultPodLabelsMetric,
		Matchers: map[string]string{"label_app": "rtd-admin"},
	}
	site.Spec.Applications[0].Kubernetes.ContainerMatchers = config.ContainerMatchers{
		{Name: "container", Operator: config.MatchEqual, Value: "main"},
	}
	api := &selectivePromAPI{series: func(match string) []model.LabelSet {
		if strings.HasPrefix(match, "kube_pod_labels") {
			return []model.LabelSet{{"namespace": "test", "pod": "rtd-admin-1", "label_app": "rtd-admin"}}
		}
		if strings.Contains(match, "container") {
			return []model.LabelSet{{"namespace": "test", "pod": "rtd-admin-1", "container": "POD"}}
		}
		return nil
	}}
	checker := &Checker{
		API:      api,
		Site:     site,
		Registry: reg,
		Options: CheckOptions{
			Lookback:    time.Hour,
			Concurrency: 1,
			Now:         func() time.Time { return time.Unix(1700000000, 0) },
		},
	}
	checks := checker.Run(context.Background())
	failures := 0
	for _, check := range checks {
		if check.Status == output.Fail && strings.Contains(check.Name, "容器过滤") {
			failures++
		}
	}
	if failures == 0 {
		t.Fatal("container matcher that filters all series must FAIL")
	}
}

func TestMatcherMatchesContainerOperators(t *testing.T) {
	business := model.LabelSet{"container": "app", "image": "registry/app:1"}
	pause := model.LabelSet{"container": "POD"}
	parent := model.LabelSet{"container": ""}
	cases := []struct {
		matcher                 config.LabelMatcher
		business, pause, parent bool
	}{
		{config.LabelMatcher{Name: "container", Operator: config.MatchEqual, Value: "app"}, true, false, false},
		{config.LabelMatcher{Name: "container", Operator: config.MatchNotEqual, Value: "POD"}, true, false, true},
		{config.LabelMatcher{Name: "container", Operator: config.MatchRegex, Value: "app|web"}, true, false, false},
		{config.LabelMatcher{Name: "container", Operator: config.MatchNotRegex, Value: "POD|^$"}, true, false, false},
	}
	for _, tc := range cases {
		if got := matcherMatches(business, tc.matcher); got != tc.business {
			t.Errorf("%+v business = %v, want %v", tc.matcher, got, tc.business)
		}
		if got := matcherMatches(pause, tc.matcher); got != tc.pause {
			t.Errorf("%+v pause = %v, want %v", tc.matcher, got, tc.pause)
		}
		if got := matcherMatches(parent, tc.matcher); got != tc.parent {
			t.Errorf("%+v parent = %v, want %v", tc.matcher, got, tc.parent)
		}
	}
}

func TestCheckerPromQLError(t *testing.T) {
	checks := runChecker(t, &fakePromAPI{queryErr: errors.New("bad_data: parse error")})
	found := false
	for _, check := range checks {
		if check.Category == "PromQL" && check.Status == output.Fail {
			found = true
		}
	}
	if !found {
		t.Fatal("PromQL error not reported as FAIL")
	}
}

func TestNoSeriesCheckReportsActualSelector(t *testing.T) {
	checks := runChecker(t, &fakePromAPI{noSeries: map[string]bool{
		"http_server_requests_seconds_count": true,
	}})
	var found *output.Check
	for i := range checks {
		if checks[i].Name == "应用 app-test/http_server_requests_seconds_count" {
			found = &checks[i]
		}
	}
	if found == nil {
		t.Fatal("HTTP metric check missing")
	}
	if found.Status != output.Warn {
		t.Fatalf("status = %s: %s", found.Status, found.Detail)
	}
	for _, want := range []string{
		"实际 Selector:",
		`application="rtd-admin"`,
		`namespace="test"`,
		`job="rtd-admin"`,
		"诊断结果:",
	} {
		if !strings.Contains(found.Detail, want) {
			t.Errorf("detail missing %q:\n%s", want, found.Detail)
		}
	}
}

func TestUpNoSeriesReportsTargetSelectorOnly(t *testing.T) {
	checks := runChecker(t, &fakePromAPI{noSeries: map[string]bool{"up": true}})
	var found *output.Check
	for i := range checks {
		if checks[i].Name == "应用 app-test/up" {
			found = &checks[i]
		}
	}
	if found == nil {
		t.Fatal("up check missing")
	}
	if !strings.Contains(found.Detail, `job="rtd-admin"`) {
		t.Fatalf("up detail missing job selector: %s", found.Detail)
	}
	if strings.Contains(found.Detail, `application="rtd-admin"`) || strings.Contains(found.Detail, `namespace="test"`) {
		t.Fatalf("up selector must not include application identity labels: %s", found.Detail)
	}
}

type diagnosisPromAPI struct {
	fakePromAPI
}

func (d *diagnosisPromAPI) Query(ctx context.Context, expr string) (model.Value, error) {
	label := ""
	switch {
	case strings.Contains(expr, "count by (application)"):
		label = "application"
	case strings.Contains(expr, "count by (namespace)"):
		label = "namespace"
	case strings.Contains(expr, "count by (job)"):
		label = "job"
	case strings.Contains(expr, "count by (cluster)"):
		return model.Vector{}, nil
	default:
		return d.fakePromAPI.Query(ctx, expr)
	}
	value := ""
	switch label {
	case "application":
		value = "rtd-admin"
	case "namespace":
		value = "test"
	case "job":
		value = "rtd-admin"
	}
	return model.Vector{&model.Sample{
		Metric: model.Metric{model.LabelName(label): model.LabelValue(value)},
		Value:  1,
	}}, nil
}

func TestSelectorDiagnosisUsesRealLabelValues(t *testing.T) {
	reg, err := LoadDefault()
	if err != nil {
		t.Fatal(err)
	}
	site := smallSite()
	site.Spec.Applications[0].Prometheus.Labels["cluster"] = "test-cluster"
	api := &diagnosisPromAPI{fakePromAPI: fakePromAPI{noSeries: map[string]bool{
		"http_server_requests_seconds_count": true,
	}}}
	checker := &Checker{
		API:      api,
		Site:     site,
		Registry: reg,
		Options: CheckOptions{
			Lookback:    time.Hour,
			Concurrency: 2,
			Now:         func() time.Time { return time.Unix(1700000000, 0) },
		},
	}
	checks := checker.Run(context.Background())
	var detail string
	for _, check := range checks {
		if check.Name == "应用 app-test/http_server_requests_seconds_count" {
			detail = check.Detail
		}
	}
	for _, want := range []string{
		`application="rtd-admin"：存在`,
		`job="rtd-admin"：存在`,
		`namespace="test"：存在`,
		`cluster="test-cluster"：不存在`,
	} {
		if !strings.Contains(detail, want) {
			t.Errorf("diagnosis missing %q:\n%s", want, detail)
		}
	}
}

func kubernetesSite() (*config.Site, *Registry) {
	reg, err := LoadDefault()
	if err != nil {
		panic(err)
	}
	site := smallSite()
	site.Spec.Applications[0].Metrics = []string{"jvm", "kubernetes"}
	site.Spec.Applications[0].Kubernetes.PodSelector = &config.PodSelector{
		Mode:     config.PodSelectorLabels,
		Metric:   config.DefaultPodLabelsMetric,
		Matchers: map[string]string{"label_app": "rtd-admin"},
	}
	return site, reg
}

func runKubernetesChecker(t *testing.T, api PromAPI) []output.Check {
	t.Helper()
	site, reg := kubernetesSite()
	checker := &Checker{
		API:      api,
		Site:     site,
		Registry: reg,
		Options: CheckOptions{
			Lookback:    time.Hour,
			Concurrency: 1,
			Now:         func() time.Time { return time.Unix(1700000000, 0) },
		},
	}
	return checker.Run(context.Background())
}

func TestPodOwnershipFailureSkipsDownstream(t *testing.T) {
	checks := runKubernetesChecker(t, &fakePromAPI{missing: map[string]bool{
		config.DefaultPodLabelsMetric: true,
	}})
	status := map[string]output.Status{}
	detail := map[string]string{}
	for _, check := range checks {
		status[check.Name] = check.Status
		detail[check.Name] = check.Detail
	}
	ownership := "Pod 归属/app-test/" + config.DefaultPodLabelsMetric
	if status[ownership] != output.Fail {
		t.Fatalf("pod ownership = %s, want FAIL: %s", status[ownership], detail[ownership])
	}
	for _, name := range []string{
		"Kubernetes 指标/app-test/container_memory_working_set_bytes",
		"Kubernetes 指标/app-test/container_cpu_usage_seconds_total",
		"容器过滤/app-test",
	} {
		if status[name] != output.Skip {
			t.Errorf("%s = %s, want SKIP: %s", name, status[name], detail[name])
		}
	}
}

func TestPodRegexOwnershipSuccessAndFailure(t *testing.T) {
	site, reg := kubernetesSite()
	site.Spec.Applications[0].Kubernetes.PodSelector = &config.PodSelector{
		Mode: config.PodSelectorPodRegex,
		Pod:  "^rtd-admin-.*$",
	}
	success := &selectivePromAPI{series: func(match string) []model.LabelSet {
		switch {
		case strings.HasPrefix(match, "container_cpu_usage_seconds_total"):
			return []model.LabelSet{{"namespace": "test", "pod": "rtd-admin-1", "container": "main"}}
		case strings.HasPrefix(match, "container_memory_working_set_bytes"):
			return []model.LabelSet{{"namespace": "test", "pod": "rtd-admin-1", "container": "main"}}
		}
		return nil
	}}
	checker := &Checker{API: success, Site: site, Registry: reg, Options: CheckOptions{
		Lookback: time.Hour, Concurrency: 1, Now: func() time.Time { return time.Unix(1700000000, 0) },
	}}
	checks := checker.Run(context.Background())
	foundPass := false
	for _, check := range checks {
		if check.Name == "Pod 归属/app-test" {
			foundPass = check.Status == output.Pass
			if !strings.Contains(check.Detail, "rtd-admin-1") {
				t.Errorf("regex ownership detail missing pod sample: %s", check.Detail)
			}
		}
	}
	if !foundPass {
		t.Fatal("pod regex ownership should PASS when cAdvisor has matching pods")
	}

	failure := &selectivePromAPI{series: func(match string) []model.LabelSet { return nil }}
	checker = &Checker{API: failure, Site: site, Registry: reg, Options: CheckOptions{
		Lookback: time.Hour, Concurrency: 1, Now: func() time.Time { return time.Unix(1700000000, 0) },
	}}
	checks = checker.Run(context.Background())
	status := map[string]output.Status{}
	for _, check := range checks {
		status[check.Name] = check.Status
	}
	if status["Pod 归属/app-test"] != output.Fail {
		t.Fatalf("pod regex with no matches must FAIL: %#v", status)
	}
	if status["Kubernetes 指标/app-test/container_cpu_usage_seconds_total"] != output.Skip {
		t.Fatal("coverage must be SKIP after pod regex ownership failure")
	}
}

func TestContainerMatcherOnlyChecksApplicationPods(t *testing.T) {
	site, reg := kubernetesSite()
	site.Spec.Applications[0].Kubernetes.ContainerMatchers = config.ContainerMatchers{
		{Name: "container", Operator: config.MatchEqual, Value: "other"},
	}
	api := &selectivePromAPI{series: func(match string) []model.LabelSet {
		switch {
		case strings.HasPrefix(match, "kube_pod_labels"):
			return []model.LabelSet{{"namespace": "test", "pod": "rtd-admin-1", "label_app": "rtd-admin"}}
		case strings.HasPrefix(match, "container_memory_working_set_bytes"):
			return []model.LabelSet{
				{"namespace": "test", "pod": "rtd-admin-1", "container": "main"},
				{"namespace": "test", "pod": "other-app-1", "container": "other"},
			}
		case strings.HasPrefix(match, "container_cpu_usage_seconds_total"):
			return []model.LabelSet{{"namespace": "test", "pod": "rtd-admin-1", "container": "main"}}
		}
		return nil
	}}
	checker := &Checker{API: api, Site: site, Registry: reg, Options: CheckOptions{
		Lookback: time.Hour, Concurrency: 1, Now: func() time.Time { return time.Unix(1700000000, 0) },
	}}
	checks := checker.Run(context.Background())
	for _, check := range checks {
		if check.Name == "容器过滤/app-test" {
			if check.Status != output.Fail {
				t.Fatalf("container matcher must be scoped to application pods; got %s: %s", check.Status, check.Detail)
			}
			if strings.Contains(check.Detail, "other-app-1") {
				t.Fatalf("detail must not count other application pods: %s", check.Detail)
			}
			return
		}
	}
	t.Fatal("container matcher check missing")
}
