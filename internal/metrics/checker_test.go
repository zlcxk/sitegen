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
			check.Status == output.Fail && strings.Contains(check.Detail, "缺少必需标签") {
			found = true
		}
	}
	if !found {
		t.Fatal("missing labels not reported as FAIL")
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
