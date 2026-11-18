package discovery

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/common/model"

	"site-observability/internal/config"
	"site-observability/internal/metrics"
)

type fakeDiscoverAPI struct {
	missing map[string]bool
}

func (f *fakeDiscoverAPI) Ping(context.Context) error { return nil }

func (f *fakeDiscoverAPI) MetricExists(_ context.Context, metric string, _, _ time.Time) (bool, error) {
	return !f.missing[metric], nil
}

func (f *fakeDiscoverAPI) LabelNames(_ context.Context, match string, _, _ time.Time) ([]string, error) {
	switch {
	case strings.Contains(match, "mysql_up"), strings.Contains(match, "redis_up"):
		return []string{"job", "instance", "mysql_cluster"}, nil
	case strings.Contains(match, "probe_success"):
		return []string{"job", "instance"}, nil
	case strings.Contains(match, "container_"):
		return []string{"namespace", "pod", "container", "image"}, nil
	default:
		return []string{"application", "namespace", "job", "method", "status", "uri", "pod", "area", "id", "container", "image", "service"}, nil
	}
}

func (f *fakeDiscoverAPI) LabelValues(_ context.Context, label, match string, _, _ time.Time) ([]string, error) {
	switch label {
	case "method":
		return []string{"GET", "POST"}, nil
	case "status":
		return []string{"200"}, nil
	case "uri":
		return []string{"/orders", "/users"}, nil
	case "mysql_cluster":
		return []string{"dev"}, nil
	case "service":
		return []string{"rtd-admin"}, nil
	default:
		return nil, nil
	}
}

func (f *fakeDiscoverAPI) QueryAt(_ context.Context, expr string, _ time.Time) (model.Value, error) {
	switch {
	case strings.HasPrefix(expr, "count by (application, namespace, job) (http_server_requests_seconds_count)"):
		return vector(
			map[string]string{"application": "rtd-admin", "namespace": "dev", "job": "rtd-admin"},
			map[string]string{"application": "rtd-buyer", "namespace": "test", "job": "rtd-buyer"},
		), nil
	case strings.HasPrefix(expr, "count by (application, namespace, job) (jvm_memory_used_bytes)"):
		return vector(map[string]string{"application": "rtd-admin", "namespace": "dev", "job": "rtd-admin"}), nil
	case strings.HasPrefix(expr, "count by (job, instance) (probe_success)"):
		return vector(map[string]string{"job": "blackbox", "instance": "https://admin.example.com"}), nil
	case strings.HasPrefix(expr, "count by (namespace) (container_memory_working_set_bytes)"):
		return vector(map[string]string{"namespace": "dev"}), nil
	case strings.HasPrefix(expr, "count by (pod) (container_cpu_usage_seconds_total"):
		return vector(map[string]string{"pod": "rtd-admin-1"}), nil
	case strings.HasPrefix(expr, "count by (container, image) (container_memory_working_set_bytes"):
		return vector(
			map[string]string{},
			map[string]string{"image": "registry/pause:3.10"},
			map[string]string{"container": "main", "image": "registry/app:1"},
		), nil
	case strings.HasPrefix(expr, "count by (job, instance) (mysql_up)"):
		return vector(map[string]string{"job": "mysql", "instance": "mysql:3306"}), nil
	case strings.HasPrefix(expr, "count by (job, instance) (redis_up)"):
		return vector(map[string]string{"job": "redis", "instance": "redis:6379"}), nil
	case strings.HasPrefix(expr, "count by (job, instance) (kafka_brokers)"):
		return model.Vector{}, nil
	case strings.HasPrefix(expr, "count("):
		if strings.Contains(expr, "kube_pod_labels") {
			return model.Vector{}, nil
		}
		return vector(map[string]string{}), nil
	default:
		return model.Vector{}, nil
	}
}

func vector(groups ...map[string]string) model.Vector {
	out := make(model.Vector, 0, len(groups))
	for _, group := range groups {
		metric := model.Metric{}
		for name, value := range group {
			metric[model.LabelName(name)] = model.LabelValue(value)
		}
		out = append(out, &model.Sample{Metric: metric, Value: 1, Timestamp: model.Time(1)})
	}
	return out
}

func discoverFixture(t *testing.T, site *config.Site) *Report {
	t.Helper()
	reg, err := metrics.LoadDefault()
	if err != nil {
		t.Fatal(err)
	}
	api := &fakeDiscoverAPI{missing: map[string]bool{"kube_pod_labels": true}}
	report, err := Discover(context.Background(), api, reg, DiscoverOptions{
		Lookback: time.Hour,
		Now:      func() time.Time { return time.Unix(1700000000, 0) },
		Site:     site,
	})
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func TestDiscoverFindsApplicationsProfilesAndComponents(t *testing.T) {
	report := discoverFixture(t, nil)
	if !report.Connected {
		t.Fatal("expected connected report")
	}
	if len(report.Applications) != 2 {
		t.Fatalf("applications = %#v", report.Applications)
	}
	admin := report.Applications[0]
	if admin.Application != "rtd-admin" || admin.Namespace != "dev" {
		t.Fatalf("unexpected first application: %#v", admin)
	}
	if !admin.Profiles["http"].Available || !admin.Profiles["jvm"].Available {
		t.Fatalf("rtd-admin profiles = %#v", admin.Profiles)
	}
	if len(admin.ExtraLabels["service"]) == 0 {
		t.Fatalf("stable extra labels were not discovered: %#v", admin.ExtraLabels)
	}
	buyer := report.Applications[1]
	if buyer.Application != "rtd-buyer" || buyer.Profiles["http"].Available == false {
		t.Fatalf("unexpected second application: %#v", buyer)
	}
	if _, ok := buyer.Profiles["jvm"]; ok {
		t.Fatal("rtd-buyer must not get a JVM profile without JVM data")
	}
	if len(report.Domains) != 1 || report.Domains[0].Instance != "https://admin.example.com" {
		t.Fatalf("domains = %#v", report.Domains)
	}
	if len(report.Components) != 2 {
		t.Fatalf("components = %#v", report.Components)
	}
	if report.Components[0].Type != "mysql" || report.Components[1].Type != "redis" {
		t.Fatalf("unexpected components: %#v", report.Components)
	}
	if got := report.Kubernetes.PodRegexByApp["rtd-admin\x00dev"]; got.MatchedPods != 1 {
		t.Fatalf("pod regex suggestion = %#v", got)
	}
	matcher := report.Kubernetes.ContainerSuggestion["rtd-admin\x00dev"]
	if matcher.Name != "container" || matcher.Operator != config.MatchNotEqual || matcher.Value != "" {
		t.Fatalf("container matcher suggestion = %#v", matcher)
	}
	if matcher.After != 1 || matcher.ParentOnly != 1 || matcher.PauseOnly != 1 {
		t.Fatalf("container matcher counts = %#v", matcher)
	}
}

func TestDiscoverRespectsSiteFilter(t *testing.T) {
	site := &config.Site{
		Metadata: config.Metadata{ID: "site", Name: "Site", Environment: "dev"},
		Spec: config.Spec{
			Domains: []config.Domain{{
				ID: "admin", URL: "https://admin.example.com",
				Blackbox: config.Blackbox{Job: "blackbox", Instance: "https://admin.example.com"},
			}},
			Applications: []config.Application{{
				ID: "rtd-admin-dev", Application: "rtd-admin", Namespace: "dev",
				Prometheus: config.Prometheus{Job: "rtd-admin"},
				Metrics:    []string{"http", "jvm"},
			}},
			Components: []config.Component{{
				ID: "mysql", Name: "MySQL", Type: "mysql",
				Prometheus: config.ComponentPrometheus{Job: "mysql", Instance: "mysql:3306"},
				Metrics:    []string{"mysql-basic"},
			}},
		},
	}
	config.Normalize(site)
	report := discoverFixture(t, site)
	if len(report.Applications) != 1 || report.Applications[0].Application != "rtd-admin" {
		t.Fatalf("site filter did not limit applications: %#v", report.Applications)
	}
	if len(report.Components) != 1 || report.Components[0].Type != "mysql" {
		t.Fatalf("site filter did not limit components: %#v", report.Components)
	}
	if len(report.Domains) != 1 {
		t.Fatalf("site filter did not limit domains: %#v", report.Domains)
	}
}

func TestLooksLikeSelectorRejectsPromQLExpressions(t *testing.T) {
	for _, valid := range []string{`up`, `up{job="prometheus"}`, `http_server_requests_seconds_count{application=~"rtd-.*"}`} {
		if !looksLikeSelector(valid) {
			t.Errorf("valid selector rejected: %s", valid)
		}
	}
	for _, invalid := range []string{`sum(rate(up[5m]))`, `count(up)`, `up == 1`} {
		if looksLikeSelector(invalid) {
			t.Errorf("PromQL expression accepted as selector: %s", invalid)
		}
	}
}
