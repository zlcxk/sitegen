package metrics

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/common/model"

	"site-observability/internal/config"
	"site-observability/internal/output"
)

type skywalkingFakeAPI struct {
	fakePromAPI
	empty map[string]bool
}

func (f *skywalkingFakeAPI) Query(ctx context.Context, expr string) (model.Value, error) {
	for metric := range f.empty {
		if strings.Contains(expr, metric) {
			return model.Vector{}, nil
		}
	}
	return f.fakePromAPI.Query(ctx, expr)
}

func skywalkingSite() *config.Site {
	enabled := true
	site := &config.Site{
		APIVersion: config.APIVersion,
		Kind:       config.KindSite,
		Metadata:   config.Metadata{ID: "sw-site", Name: "SW Site", Environment: "dev"},
		Spec: config.Spec{
			Domains: []config.Domain{{
				ID: "admin", URL: "https://admin.example.com",
				Blackbox: config.Blackbox{Job: "blackbox", Instance: "https://admin.example.com"},
			}},
			Applications: []config.Application{{
				ID:          "rtd-admin-dev",
				Application: "rtd-admin",
				Namespace:   "dev",
				Prometheus:  config.Prometheus{Job: "rtd-admin"},
				Skywalking: config.Skywalking{
					Enabled: &enabled,
					Service: "rtd-admin_dev",
				},
				Metrics: []string{"jvm"},
			}},
		},
	}
	config.Normalize(site)
	return site
}

func TestSkywalkingCheckerPassesWithData(t *testing.T) {
	checker := &SkywalkingChecker{
		API:  &fakePromAPI{},
		Site: skywalkingSite(),
		Options: CheckOptions{
			Lookback: time.Hour,
			Now:      func() time.Time { return time.Unix(1700000000, 0) },
		},
	}
	checks := checker.Run(context.Background())
	hasService := false
	hasTopologyWarn := false
	for _, check := range checks {
		if check.Status == output.Fail {
			t.Fatalf("unexpected FAIL: %s: %s", check.Name, check.Detail)
		}
		if check.Name == "rtd-admin-dev/service_cpm" && check.Status == output.Pass {
			hasService = true
		}
		if check.Name == "拓扑" && check.Status == output.Warn {
			hasTopologyWarn = true
		}
	}
	if !hasService {
		t.Fatal("service_cpm check missing or not PASS")
	}
	if !hasTopologyWarn {
		t.Fatal("native topology must remain an explicit WARN until Grafana verification")
	}
}

func TestSkywalkingCheckerMissingMetricIsWarn(t *testing.T) {
	checker := &SkywalkingChecker{
		API:  &skywalkingFakeAPI{empty: map[string]bool{"service_apdex": true}},
		Site: skywalkingSite(),
		Options: CheckOptions{
			Lookback: time.Hour,
			Now:      func() time.Time { return time.Unix(1700000000, 0) },
		},
	}
	checks := checker.Run(context.Background())
	for _, check := range checks {
		if check.Name == "rtd-admin-dev/service_apdex" {
			if check.Status != output.Warn {
				t.Fatalf("missing APM metric must be WARN, got %s: %s", check.Status, check.Detail)
			}
			return
		}
	}
	t.Fatal("service_apdex check missing")
}

func TestSkywalkingCheckerDisabledAppIsSkipped(t *testing.T) {
	site := skywalkingSite()
	disabled := false
	site.Spec.Applications[0].Skywalking.Enabled = &disabled
	checker := &SkywalkingChecker{
		API:  &fakePromAPI{missing: map[string]bool{"service_cpm": true}},
		Site: site,
		Options: CheckOptions{
			Lookback: time.Hour,
			Now:      func() time.Time { return time.Unix(1700000000, 0) },
		},
	}
	for _, check := range checker.Run(context.Background()) {
		if check.Name == "rtd-admin-dev/service_cpm" {
			t.Fatal("disabled SkyWalking app must not be checked")
		}
	}
}
