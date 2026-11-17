package metrics

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/common/model"

	"site-observability/internal/config"
	"site-observability/internal/output"
)

// PromAPI is the subset of the Prometheus client used by the checker.
type PromAPI interface {
	Ping(ctx context.Context) error
	Series(ctx context.Context, match string, start, end time.Time) ([]model.LabelSet, error)
	MetricExists(ctx context.Context, metric string, start, end time.Time) (bool, error)
	Query(ctx context.Context, expr string) (model.Value, error)
}

// CheckOptions controls validation query windows and request concurrency.
type CheckOptions struct {
	Lookback    time.Duration
	Concurrency int
	Now         func() time.Time
}

// Checker validates site configuration against live Prometheus data.
type Checker struct {
	API      PromAPI
	Site     *config.Site
	Registry *Registry
	Options  CheckOptions
}

type checkTask struct {
	category string
	name     string
	run      func(context.Context) output.Check
}

func (c *Checker) check(category, name string, status output.Status, detail string) output.Check {
	return output.Check{Category: category, Name: name, Status: status, Detail: detail}
}

// Run performs connectivity, metric, label and PromQL checks. Results are
// returned in a deterministic order even though requests are concurrent.
func (c *Checker) Run(ctx context.Context) []output.Check {
	lookback := c.Options.Lookback
	if lookback <= 0 {
		lookback = time.Hour
	}
	concurrency := c.Options.Concurrency
	if concurrency <= 0 {
		concurrency = 4
	}
	if concurrency > 16 {
		concurrency = 16
	}
	nowFn := c.Options.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	now := nowFn()

	if err := c.API.Ping(ctx); err != nil {
		return []output.Check{c.check("Prometheus", "连通性", output.Fail, connectivityDetail(err))}
	}

	tasks := []checkTask{{
		name:     "连通性",
		category: "Prometheus",
		run: func(context.Context) output.Check {
			return c.check("Prometheus", "连通性", output.Pass, "查询 API 可访问")
		},
	}}

	tasks = append(tasks, c.domainTasks(now, lookback)...)
	tasks = append(tasks, c.applicationTasks(now, lookback)...)
	tasks = append(tasks, c.componentTasks(now, lookback)...)
	tasks = append(tasks, c.skipTasks()...)

	results := make([]output.Check, len(tasks))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i := range tasks {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results[i] = c.check(tasks[i].category, tasks[i].name, output.Fail, "校验被取消")
				return
			}
			results[i] = tasks[i].run(ctx)
		}()
	}
	wg.Wait()

	if c.hasKubernetesWithoutJVM() {
		results = append(results, c.check("Dashboard 变量", "Pod 选择",
			output.Warn, "站点启用了 Kubernetes 指标但未启用 JVM；Pod 变量将回退到 kube_pod_labels，请确认 Pod 带有 application 标签"))
	}
	return results
}

func (c *Checker) domainTasks(now time.Time, lookback time.Duration) []checkTask {
	var tasks []checkTask
	profile, ok := c.Registry.AutoProfile("blackbox")
	if !ok {
		return tasks
	}
	for _, domain := range c.Site.Spec.Domains {
		selector := DomainSelector(domain)
		required, optional := profileRequirements(profile)
		tasks = append(tasks, c.metricTasks("站点 "+domain.ID, selector, required, optional, now, lookback)...)
		for _, panel := range profile.Panels {
			panel := panel
			query := RenderCheckQuery(panel.Query, c.Site, selector)
			tasks = append(tasks, c.queryTask("Blackbox/"+domain.ID+"/"+panel.ID, query))
		}
	}
	return tasks
}

func (c *Checker) applicationTasks(now time.Time, lookback time.Duration) []checkTask {
	var tasks []checkTask
	apps := c.Site.Spec.Applications
	if len(apps) == 0 {
		return tasks
	}

	namespaceChecked := map[string]bool{}
	for _, app := range apps {
		active := c.activeApplicationProfiles(app)
		for _, profile := range active {
			switch profile.Scope {
			case ScopeApplication:
				selector := ApplicationTargetSelector(app)
				required, optional := profileRequirements(profile)
				tasks = append(tasks, c.metricTasks("应用 "+app.ID, selector, required, optional, now, lookback)...)
			case ScopeNamespace:
				if namespaceChecked[app.Namespace] {
					continue
				}
				namespaceChecked[app.Namespace] = true
				selector := `namespace=` + QuoteLabelValue(app.Namespace)
				required, optional := profileRequirements(profile)
				tasks = append(tasks, c.metricTasks("Namespace "+app.Namespace, selector, required, optional, now, lookback)...)
			}
		}
	}

	// Auto application health profile (up by job).
	if auto, ok := c.Registry.AutoProfile("platform"); ok {
		required, optional := profileRequirements(auto)
		for _, app := range apps {
			if app.Prometheus.Job == "" {
				continue
			}
			selector := `job=` + QuoteLabelValue(app.Prometheus.Job)
			tasks = append(tasks, c.metricTasks("应用 "+app.ID, selector, required, optional, now, lookback)...)
		}
		platformSelector := `job=~` + QuoteLabelValue(AppJobsRegex(c.Site))
		for _, panel := range auto.Panels {
			query := RenderCheckQuery(panel.Query, c.Site, platformSelector)
			tasks = append(tasks, c.queryTask("平台/"+panel.ID, query))
		}
	}

	// Execute each active profile panel once using the site-wide selector.
	seenProfiles := map[string]bool{}
	for _, app := range apps {
		for _, profile := range c.activeApplicationProfiles(app) {
			if seenProfiles[profile.ID] {
				continue
			}
			seenProfiles[profile.ID] = true
			selector := profileCheckSelector(profile, c.Site)
			for _, panel := range profile.Panels {
				query := RenderCheckQuery(panel.Query, c.Site, selector)
				tasks = append(tasks, c.queryTask(profile.ID+"/"+panel.ID, query))
			}
		}
	}
	return tasks
}

func (c *Checker) componentTasks(now time.Time, lookback time.Duration) []checkTask {
	var tasks []checkTask
	for _, component := range c.Site.Spec.Components {
		active := c.activeComponentProfiles(component)
		if len(active) == 0 {
			tasks = append(tasks, checkTask{
				category: "Prometheus",
				name:     "组件 " + component.ID,
				run: func(context.Context) output.Check {
					return c.check("Prometheus", "组件 "+component.ID, output.Warn, "组件未启用指标模板")
				},
			})
		}
		for _, profile := range active {
			selector := ComponentSelector(component)
			required, optional := profileRequirements(profile)
			tasks = append(tasks, c.metricTasks("组件 "+component.ID, selector, required, optional, now, lookback)...)
			for _, panel := range profile.Panels {
				tasks = append(tasks, c.queryTask(
					"组件/"+component.ID+"/"+profile.ID+"/"+panel.ID,
					RenderCheckQuery(panel.Query, c.Site, selector),
				))
			}
		}
	}
	return tasks
}

func (c *Checker) skipTasks() []checkTask {
	var tasks []checkTask
	for _, app := range c.Site.Spec.Applications {
		enabled := map[string]bool{}
		for _, id := range app.Metrics {
			enabled[id] = true
		}
		for _, profile := range c.Registry.ProfilesForApplication() {
			if enabled[profile.ID] {
				continue
			}
			profile := profile
			app := app
			tasks = append(tasks, checkTask{
				category: "指标模板",
				name:     app.ID + "/" + profile.ID,
				run: func(context.Context) output.Check {
					return c.check("指标模板", app.ID+"/"+profile.ID, output.Skip, "站点未启用该指标模板")
				},
			})
		}
	}
	for _, component := range c.Site.Spec.Components {
		enabled := map[string]bool{}
		for _, id := range component.Metrics {
			enabled[id] = true
		}
		for _, profile := range c.Registry.ProfilesForComponent(component.Type) {
			if enabled[profile.ID] {
				continue
			}
			profile := profile
			component := component
			tasks = append(tasks, checkTask{
				category: "指标模板",
				name:     component.ID + "/" + profile.ID,
				run: func(context.Context) output.Check {
					return c.check("指标模板", component.ID+"/"+profile.ID, output.Skip, "站点未启用该指标模板")
				},
			})
		}
	}
	return tasks
}

func (c *Checker) metricTasks(target, selector string, required, optional map[string][]string, now time.Time, lookback time.Duration) []checkTask {
	var tasks []checkTask
	for _, metric := range sortedKeysOf(required) {
		metric := metric
		req := required[metric]
		opts := optional[metric]
		tasks = append(tasks, checkTask{
			category: "Prometheus",
			name:     target + "/" + metric,
			run: func(ctx context.Context) output.Check {
				return c.checkMetric(ctx, target, metric, selector, req, opts, now, lookback)
			},
		})
	}
	return tasks
}

func (c *Checker) checkMetric(ctx context.Context, target, metric, selector string, required, optional []string, now time.Time, lookback time.Duration) output.Check {
	exists, err := c.API.MetricExists(ctx, metric, now.Add(-7*24*time.Hour), now)
	if err != nil {
		return c.check("Prometheus", target+"/"+metric, output.Fail, requestErrorDetail(err))
	}
	if !exists {
		return c.check("Prometheus", target+"/"+metric, output.Fail, "指标不存在（近 7 天无序列）")
	}

	match := metric + "{" + selector + "}"
	series, err := c.API.Series(ctx, match, now.Add(-lookback), now)
	if err != nil {
		return c.check("Prometheus", target+"/"+metric, output.Fail, requestErrorDetail(err))
	}
	if len(series) == 0 {
		return c.check("Prometheus", target+"/"+metric, output.Warn,
			fmt.Sprintf("指标存在，但 selector 在近 %s 内没有序列", lookback))
	}
	var missingRequired []string
	for _, label := range required {
		count := 0
		for _, labels := range series {
			if _, ok := labels[model.LabelName(label)]; ok {
				count++
			}
		}
		if count == 0 {
			missingRequired = append(missingRequired, label)
		}
	}
	if len(missingRequired) > 0 {
		return c.check("Prometheus", target+"/"+metric, output.Fail,
			fmt.Sprintf("%d 条序列缺少必需标签: %s", len(series), strings.Join(missingRequired, ", ")))
	}
	var missingOptional []string
	for _, label := range optional {
		found := false
		for _, labels := range series {
			if _, ok := labels[model.LabelName(label)]; ok {
				found = true
				break
			}
		}
		if !found {
			missingOptional = append(missingOptional, label)
		}
	}
	if len(missingOptional) > 0 {
		return c.check("Prometheus", target+"/"+metric, output.Warn,
			fmt.Sprintf("%d 条序列，必需标签齐全；可选标签缺失: %s", len(series), strings.Join(missingOptional, ", ")))
	}
	return c.check("Prometheus", target+"/"+metric, output.Pass,
		fmt.Sprintf("%d 条序列，必需标签齐全", len(series)))
}

func (c *Checker) queryTask(name, query string) checkTask {
	return checkTask{
		category: "PromQL",
		name:     name,
		run: func(ctx context.Context) output.Check {
			value, err := c.API.Query(ctx, query)
			if err != nil {
				return c.check("PromQL", name, output.Fail, requestErrorDetail(err))
			}
			if value == nil || isEmptyValue(value) {
				return c.check("PromQL", name, output.Warn, "查询表达式合法，但当前时间窗口无数据")
			}
			return c.check("PromQL", name, output.Pass, "表达式合法且返回数据")
		},
	}
}

func (c *Checker) activeApplicationProfiles(app config.Application) []*Profile {
	enabled := map[string]bool{}
	for _, id := range app.Metrics {
		enabled[id] = true
	}
	var out []*Profile
	for _, id := range c.Registry.Order {
		profile := c.Registry.Profiles[id]
		if profile.Auto || profile.Target != targetApplication || !enabled[id] {
			continue
		}
		out = append(out, profile)
	}
	return out
}

func (c *Checker) activeComponentProfiles(component config.Component) []*Profile {
	enabled := map[string]bool{}
	for _, id := range component.Metrics {
		enabled[id] = true
	}
	var out []*Profile
	for _, id := range c.Registry.Order {
		profile := c.Registry.Profiles[id]
		if profile.Auto || profile.Target != targetComponent || !enabled[id] {
			continue
		}
		if len(profile.ComponentTypes) > 0 && !strings.Contains(strings.Join(profile.ComponentTypes, ","), component.Type) {
			continue
		}
		out = append(out, profile)
	}
	return out
}

func (c *Checker) hasKubernetesWithoutJVM() bool {
	hasK8s, hasJVM := false, false
	for _, app := range c.Site.Spec.Applications {
		for _, id := range app.Metrics {
			switch id {
			case "kubernetes":
				hasK8s = true
			case "jvm":
				hasJVM = true
			}
		}
	}
	return hasK8s && !hasJVM
}

func profileRequirements(profile *Profile) (required, optional map[string][]string) {
	required = map[string][]string{}
	optional = map[string][]string{}
	for _, panel := range profile.Panels {
		for _, metric := range panel.RequiredMetrics {
			required[metric] = union(required[metric], panel.RequiredLabels)
			optional[metric] = union(optional[metric], panel.OptionalLabels)
		}
	}
	for _, metric := range sortedKeysOf(required) {
		required[metric] = sortedUnique(required[metric])
		optional[metric] = sortedUnique(optional[metric])
	}
	return required, optional
}

func profileCheckSelector(profile *Profile, site *config.Site) string {
	switch profile.Scope {
	case ScopeApplication:
		return ApplicationSelectorConcrete(site)
	case ScopeNamespace:
		return NamespaceSelectorConcrete(site)
	default:
		return ""
	}
}

func isEmptyValue(value model.Value) bool {
	switch v := value.(type) {
	case model.Vector:
		return len(v) == 0
	case model.Matrix:
		return len(v) == 0
	case *model.Scalar:
		return v == nil
	case *model.String:
		return v == nil || v.Value == ""
	default:
		return false
	}
}

func connectivityDetail(err error) string {
	detail := cleanDetail(err.Error())
	lower := strings.ToLower(detail)
	switch {
	case strings.Contains(lower, "401") || strings.Contains(lower, "unauthorized"):
		detail = "认证失败（HTTP 401）；请检查 SITEGEN_PROMETHEUS_USERNAME/PASSWORD 或 TOKEN"
	case strings.Contains(lower, "403") || strings.Contains(lower, "forbidden"):
		detail = "认证通过但无权限（HTTP 403）"
	}
	return truncate(detail, 600)
}

func requestErrorDetail(err error) string {
	return truncate(cleanDetail(err.Error()), 600)
}

func cleanDetail(value string) string {
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	return strings.TrimSpace(value)
}

func truncate(value string, max int) string {
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	return string(runes[:max]) + "..."
}

func union(a, b []string) []string {
	out := append([]string{}, a...)
	out = append(out, b...)
	return out
}

func sortedUnique(values []string) []string {
	unique := map[string]bool{}
	for _, value := range values {
		unique[value] = true
	}
	out := make([]string, 0, len(unique))
	for value := range unique {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func sortedKeysOf(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
