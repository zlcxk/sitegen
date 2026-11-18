package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/promqltest"

	"site-observability/internal/config"
)

// TestRenderedUnionExecutesInPromQLEngine runs the generated query in the real
// Prometheus PromQL engine on a deterministic dataset. This catches semantic
// changes that string assertions cannot: the generated query must compute the
// same global rate/increase over per-application Range Vector Selectors.
func TestRenderedUnionExecutesInPromQLEngine(t *testing.T) {
	site := selectorSite()
	apps := site.Spec.Applications[:2]

	rateQuery := compactPromQL(renderApp(t,
		"sum(rate(http_server_requests_seconds_count{%SELECTOR%}[5m]))", apps, false, ""))
	increaseQuery := compactPromQL(renderApp(t,
		"sum(increase(http_server_requests_seconds_count{%SELECTOR%}[5m]))", apps, false, ""))

	script := `
load 15s
  http_server_requests_seconds_count{application="app-a",namespace="test",job="job-a",team="a"} 0+10x20
  http_server_requests_seconds_count{application="app-a",namespace="dev",job="job-a",team="a"} 0+20x20

eval instant at 5m ` + rateQuery + `
  {} 2

eval instant at 5m ` + increaseQuery + `
  {} 600
`
	engine := promql.NewEngine(promql.EngineOpts{
		MaxSamples:           1e7,
		Timeout:              time.Minute,
		EnableAtModifier:     true,
		EnableNegativeOffset: true,
	})
	promqltest.RunTest(t, script, engine)
}

// TestRenderedUnionHandlesCounterReset verifies that per-application rate()
// calls preserve counter-reset semantics when their instant vectors are unioned.
func TestRenderedUnionHandlesCounterReset(t *testing.T) {
	site := selectorSite()
	apps := site.Spec.Applications[:2]
	query := compactPromQL(renderApp(t,
		"sum(rate(reset_total{%SELECTOR%}[5m]))", apps, false, ""))
	script := `
load 1m
  reset_total{application="app-a",namespace="test",job="job-a",team="a"} 0+10x5 0+10x5
  reset_total{application="app-a",namespace="dev",job="job-a",team="a"} 0+20x5 0+20x5

eval instant at 8m ` + query + `
  {} 0.375

eval instant at 11m ` + query + `
  {} 0.5
`
	engine := promql.NewEngine(promql.EngineOpts{
		MaxSamples:           1e7,
		Timeout:              time.Minute,
		EnableAtModifier:     true,
		EnableNegativeOffset: true,
	})
	promqltest.RunTest(t, script, engine)
}

// TestHTTPTemplateQueriesExecuteInPromQLEngine runs the actual HTTP template
// queries for average latency and P95/P99 against a deterministic histogram.
func TestHTTPTemplateQueriesExecuteInPromQLEngine(t *testing.T) {
	reg, err := LoadDefault()
	if err != nil {
		t.Fatal(err)
	}
	profile, ok := reg.Profile("http")
	if !ok {
		t.Fatal("http profile missing")
	}
	app := selectorSite().Spec.Applications[0]
	panels := map[string]string{}
	for _, panel := range profile.Panels {
		switch panel.ID {
		case "http-avg-latency", "http-p95", "http-p99":
			query, err := RenderApplicationQuery(panel.Query, []config.Application{app}, false, "")
			if err != nil {
				t.Fatalf("%s render: %v", panel.ID, err)
			}
			panels[panel.ID] = compactPromQL(query)
		}
	}
	script := `
load 15s
  http_server_requests_seconds_sum{application="app-a",namespace="test",job="job-a",team="a"} 0+20x20
  http_server_requests_seconds_count{application="app-a",namespace="test",job="job-a",team="a"} 0+10x20
  http_server_requests_seconds_bucket{application="app-a",namespace="test",job="job-a",team="a",le="0.1"} 0+10x20
  http_server_requests_seconds_bucket{application="app-a",namespace="test",job="job-a",team="a",le="0.5"} 0+20x20
  http_server_requests_seconds_bucket{application="app-a",namespace="test",job="job-a",team="a",le="1"} 0+30x20
  http_server_requests_seconds_bucket{application="app-a",namespace="test",job="job-a",team="a",le="+Inf"} 0+30x20

eval instant at 5m ` + panels["http-avg-latency"] + `
  {} 2000

eval instant at 5m ` + panels["http-p95"] + `
  {} 924.9999999999999

eval instant at 5m ` + panels["http-p99"] + `
  {} 985
`
	engine := promql.NewEngine(promql.EngineOpts{
		MaxSamples:           1e7,
		Timeout:              time.Minute,
		EnableAtModifier:     true,
		EnableNegativeOffset: true,
	})
	promqltest.RunTest(t, script, engine)
}

// TestRenderedSingleApplicationFilterMatchesOnlyThatApp verifies the
// sitegen_app filter selects exactly one configured object.
func TestRenderedSingleApplicationFilterMatchesOnlyThatApp(t *testing.T) {
	site := selectorSite()
	apps := site.Spec.Applications[:2]
	query := compactPromQL(renderApp(t,
		"sum(rate(http_server_requests_seconds_count{%SELECTOR%}[5m]))", apps, true, "http_app_selector"))
	query = grafanaJSONVarRE.ReplaceAllStringFunc(query, func(string) string {
		return QuoteLabelValue("app-a-test")
	})
	script := `
load 15s
  http_server_requests_seconds_count{application="app-a",namespace="test",job="job-a",team="a"} 0+10x20
  http_server_requests_seconds_count{application="app-a",namespace="dev",job="job-a",team="a"} 0+20x20

eval instant at 5m ` + query + `
  {} 0.6666666666666666
`
	engine := promql.NewEngine(promql.EngineOpts{
		MaxSamples:           1e7,
		Timeout:              time.Minute,
		EnableAtModifier:     true,
		EnableNegativeOffset: true,
	})
	promqltest.RunTest(t, script, engine)
}
