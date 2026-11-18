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
