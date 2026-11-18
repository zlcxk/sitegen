package metrics

import (
	"context"
	"fmt"
	"strings"
	"time"

	"site-observability/internal/config"
	"site-observability/internal/output"
)

// SkywalkingChecker validates SkyWalking APM metrics against the Prometheus
// endpoint that scrapes SkyWalking OAP. It is separate from Checker because
// these metrics usually live in a different Prometheus instance.
type SkywalkingChecker struct {
	API     PromAPI
	Site    *config.Site
	Options CheckOptions
}

// Run returns one connectivity check plus per-application service metric
// checks. Missing APM data is WARN, not FAIL: it must not invalidate the rest
// of the site dashboard.
func (c *SkywalkingChecker) Run(ctx context.Context) []output.Check {
	now := time.Now()
	if c.Options.Now != nil {
		now = c.Options.Now()
	}
	lookback := c.Options.Lookback
	if lookback <= 0 {
		lookback = time.Hour
	}
	// SkyWalking's Prometheus proxy rejects `vector(1)`. Probe with a real
	// SkyWalking service metric instead; an empty vector is still a healthy
	// response.
	pingExpr := "vector(1)"
	for _, app := range c.Site.Spec.Applications {
		if app.SkywalkingEnabled() && app.Skywalking.Service != "" {
			pingExpr = `service_cpm{layer="GENERAL", service=` + QuoteLabelValue(app.Skywalking.Service) + `}`
			break
		}
	}
	if _, err := c.API.Query(ctx, pingExpr); err != nil {
		return []output.Check{c.check("连通性", output.Warn,
			"SkyWalking Prometheus 不可用；APM 面板可能显示无数据: "+requestErrorDetail(err))}
	}
	checks := []output.Check{c.check("连通性", output.Pass, "SkyWalking Prometheus 查询 API 可访问")}
	for _, app := range c.Site.Spec.Applications {
		if !app.SkywalkingEnabled() {
			continue
		}
		service := strings.TrimSpace(app.Skywalking.Service)
		if service == "" {
			checks = append(checks, c.check("服务映射/"+app.ID, output.Fail,
				"应用启用了 SkyWalking APM，但 service 为空"))
			continue
		}
		selector := NewSelector(
			LabelCondition{Name: "layer", Value: "GENERAL"},
			LabelCondition{Name: "service", Value: service},
		)
		for _, metric := range []string{
			"service_apdex", "service_sla", "service_cpm",
			"service_resp_time", "service_percentile",
		} {
			checks = append(checks, c.checkMetric(ctx, app, metric, selector, now, lookback))
		}
		for _, metric := range []string{
			"service_instance_cpm", "service_instance_resp_time", "service_instance_sla",
			"endpoint_cpm", "endpoint_resp_time", "endpoint_sla",
		} {
			checks = append(checks, c.checkTopN(ctx, app, metric, service, now))
		}
	}
	checks = append(checks, c.check("拓扑", output.Warn,
		"SkyWalking 原生 Node Graph 需要在目标 Grafana 中验证；本检查只验证 SkyWalking Prometheus 指标"))
	return checks
}

func (c *SkywalkingChecker) check(name string, status output.Status, detail string) output.Check {
	return output.Check{Category: "SkyWalking", Name: name, Status: status, Detail: detail}
}

func (c *SkywalkingChecker) checkMetric(ctx context.Context, app config.Application, metric string, selector Selector, now time.Time, lookback time.Duration) output.Check {
	name := app.ID + "/" + metric
	// SkyWalking's Prometheus proxy only accepts direct metric selectors; it
	// rejects label-values based existence checks and count(). Query the exact
	// metric selector used by the dashboard instead.
	value, err := c.API.Query(ctx, metric+"{"+selector.String()+"}")
	if err != nil {
		return c.check(name, output.Warn, "SkyWalking 指标查询失败: "+requestErrorDetail(err))
	}
	if value == nil || isEmptyValue(value) {
		return c.check(name, output.Warn,
			fmt.Sprintf("服务 %q 在近 %s 内没有 %s 数据\n\n实际 Selector:\n%s",
				app.Skywalking.Service, formatLookback(lookback), metric, selector.String()))
	}
	return c.check(name, output.Pass,
		fmt.Sprintf("服务 %q 返回 %s 数据（实际 Selector: %s）",
			app.Skywalking.Service, metric, selector.String()))
}

func (c *SkywalkingChecker) checkTopN(ctx context.Context, app config.Application, metric, service string, now time.Time) output.Check {
	name := app.ID + "/" + metric
	order := "DES"
	if strings.HasSuffix(metric, "_sla") {
		order = "ASC"
	}
	query := fmt.Sprintf(`%s{layer="GENERAL",order=%s,parent_service=%s,top_n="10"}`,
		metric, QuoteLabelValue(order), QuoteLabelValue(service))
	value, err := c.API.Query(ctx, query)
	if err != nil {
		return c.check(name, output.Warn, "SkyWalking 查询失败: "+requestErrorDetail(err))
	}
	if value == nil || isEmptyValue(value) {
		return c.check(name, output.Warn, "服务实例/Endpoint 指标当前没有数据")
	}
	return c.check(name, output.Pass, "SkyWalking 实例/Endpoint 指标可查询")
}
