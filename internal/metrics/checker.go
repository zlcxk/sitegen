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
	tasks = append(tasks, c.variableTasks()...)
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

	for _, app := range apps {
		for _, profile := range c.activeApplicationProfiles(app) {
			required, optional := profileRequirements(profile)
			switch profile.Scope {
			case ScopeApplication:
				selector := ApplicationTargetSelector(app)
				tasks = append(tasks, c.metricTasks("应用 "+app.ID, selector, required, optional, now, lookback)...)
				for _, panel := range profile.Panels {
					if panel.Aggregation == AggregationSite {
						continue
					}
					query := RenderApplicationQuery(panel.Query, []config.Application{app}, false)
					tasks = append(tasks, c.queryTask(
						"应用/"+app.ID+"/"+profile.ID+"/"+panel.ID,
						RenderCheckQuery(query, c.Site, ""),
					))
				}
			case ScopeNamespace:
				if app.Kubernetes.PodSelector == nil {
					tasks = append(tasks, c.warnTask("Prometheus", "应用 "+app.ID+"/Kubernetes Pod 归属",
						"未配置 kubernetes.podSelector；跳过应用级 Kubernetes 查询，避免纳入同 Namespace 其他应用 Pod"))
					continue
				}
				selector := `namespace=` + QuoteLabelValue(app.Namespace)
				tasks = append(tasks, c.metricTasks("应用 "+app.ID, selector, required, optional, now, lookback)...)
				for _, panel := range profile.Panels {
					query := RenderNamespaceQuery(panel.Query, []config.Application{app}, false)
					tasks = append(tasks, c.queryTask(
						"应用/"+app.ID+"/"+profile.ID+"/"+panel.ID,
						RenderCheckQuery(query, c.Site, ""),
					))
				}
				tasks = append(tasks, c.podAssociationTasks(app, now, lookback)...)
			}
		}
	}

	// Site-level entry application aggregates use exactly the same compiled
	// selectors as the dashboard: only explicitly declared entry applications.
	for _, profile := range applicationProfiles(c.Registry) {
		var sitePanels []Panel
		for _, panel := range profile.Panels {
			if panel.Aggregation == AggregationSite {
				sitePanels = append(sitePanels, panel)
			}
		}
		if len(sitePanels) == 0 {
			continue
		}
		entry := c.Site.EntryApplicationsWithMetric(profile.ID)
		if len(entry) == 0 {
			if profile.ID == "http" && len(c.Site.ApplicationsWithMetric("http")) > 0 {
				tasks = append(tasks, c.warnTask("Prometheus", "站点/HTTP 入口应用汇总",
					"未配置启用 HTTP 的 entryApplications；不会生成站点级外部请求汇总面板"))
			}
			continue
		}
		for _, panel := range sitePanels {
			query := RenderApplicationQuery(panel.Query, entry, false)
			tasks = append(tasks, c.queryTask("站点/"+profile.ID+"/"+panel.ID, RenderCheckQuery(query, c.Site, "")))
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
		for _, panel := range auto.Panels {
			query := RenderJobQuery(panel.Query, apps)
			tasks = append(tasks, c.queryTask("平台/"+panel.ID, RenderCheckQuery(query, c.Site, "")))
		}
	}
	return tasks
}

func applicationProfiles(registry *Registry) []*Profile {
	var out []*Profile
	for _, id := range registry.Order {
		profile := registry.Profiles[id]
		if profile.Auto || profile.Target != targetApplication {
			continue
		}
		out = append(out, profile)
	}
	return out
}

func (c *Checker) podAssociationTasks(app config.Application, now time.Time, lookback time.Duration) []checkTask {
	tasks := c.containerMatcherTasks(app, now, lookback)
	ps := app.Kubernetes.PodSelector
	if ps == nil {
		return tasks
	}
	switch ps.Mode {
	case config.PodSelectorLabels:
		metric := ps.PodLabelMetric()
		return append(tasks, checkTask{
			category: "Prometheus",
			name:     "Pod 归属/" + app.ID + "/" + metric,
			run: func(ctx context.Context) output.Check {
				exists, err := c.API.MetricExists(ctx, metric, now.Add(-7*24*time.Hour), now)
				if err != nil {
					return c.check("Prometheus", "Pod 归属/"+app.ID+"/"+metric, output.Fail, requestErrorDetail(err))
				}
				if !exists {
					return c.check("Prometheus", "Pod 归属/"+app.ID+"/"+metric, output.Fail,
						"Pod 归属指标 "+metric+" 不存在（近 7 天无序列）")
				}
				selector := `namespace=` + QuoteLabelValue(app.Namespace)
				for _, key := range sortedKeys(ps.Matchers) {
					selector += ", " + key + "=" + QuoteLabelValue(ps.Matchers[key])
				}
				series, err := c.API.Series(ctx, metric+"{"+selector+"}", now.Add(-lookback), now)
				if err != nil {
					return c.check("Prometheus", "Pod 归属/"+app.ID+"/"+metric, output.Fail, requestErrorDetail(err))
				}
				if len(series) == 0 {
					return c.check("Prometheus", "Pod 归属/"+app.ID+"/"+metric, output.Warn,
						fmt.Sprintf("配置的 Pod 标签在近 %s 内没有匹配序列", lookback))
				}
				for _, key := range sortedKeys(ps.Matchers) {
					count := countLabel(series, key)
					if count == 0 {
						return c.check("Prometheus", "Pod 归属/"+app.ID+"/"+metric, output.Warn,
							fmt.Sprintf("匹配序列 %d 条，但没有任何序列带有标签 %s；无法建立可靠的 Pod 归属，应用级 Kubernetes 面板可能无数据", len(series), key))
					}
				}
				if count := countLabel(series, "pod"); count != len(series) {
					return c.check("Prometheus", "Pod 归属/"+app.ID+"/"+metric, output.Warn,
						fmt.Sprintf("匹配序列 %d 条，存在 pod 标签 %d 条；无法建立完整 Pod 集合", len(series), count))
				}
				pods := labelSet(series, "pod")
				cadvisorSelector := `namespace=` + QuoteLabelValue(app.Namespace) + `, pod=~".+"`
				cadvisorSeries, err := c.API.Series(ctx, "container_memory_working_set_bytes{"+cadvisorSelector+"}", now.Add(-lookback), now)
				if err != nil {
					return c.check("Prometheus", "Pod 归属/"+app.ID+"/"+metric, output.Fail, requestErrorDetail(err))
				}
				matched := 0
				for pod := range labelSet(cadvisorSeries, "pod") {
					if pods[pod] {
						matched++
					}
				}
				if matched == 0 {
					return c.check("Prometheus", "Pod 归属/"+app.ID+"/"+metric, output.Warn,
						fmt.Sprintf("匹配 %d 个 Pod，但与 cAdvisor 指标没有交集；Kubernetes 资源面板可能无数据", len(pods)))
				}
				return c.check("Prometheus", "Pod 归属/"+app.ID+"/"+metric, output.Pass,
					fmt.Sprintf("匹配 %d 个 Pod，其中 %d 个与 cAdvisor 指标交集匹配", len(pods), matched))
			},
		})
	case config.PodSelectorPodRegex:
		return append(tasks, checkTask{
			category: "Prometheus",
			name:     "Pod 归属/" + app.ID,
			run: func(ctx context.Context) output.Check {
				if ps.Pod == "" {
					return c.check("Prometheus", "Pod 归属/"+app.ID, output.Fail, "pod-regex 模式缺少 pod 正则")
				}
				selector := `namespace=` + QuoteLabelValue(app.Namespace) + `, pod=~` + QuoteLabelValue(ps.Pod)
				series, err := c.API.Series(ctx, "container_memory_working_set_bytes{"+selector+"}", now.Add(-lookback), now)
				if err != nil {
					return c.check("Prometheus", "Pod 归属/"+app.ID, output.Fail, requestErrorDetail(err))
				}
				pods := labelSet(series, "pod")
				if len(pods) == 0 {
					return c.check("Prometheus", "Pod 归属/"+app.ID, output.Warn,
						fmt.Sprintf("Pod 正则在近 %s 内没有匹配到 cAdvisor 序列；Deployment 滚动更新后请确认正则仍覆盖新实例", lookback))
				}
				return c.check("Prometheus", "Pod 归属/"+app.ID, output.Warn,
					fmt.Sprintf("pod-regex 兼容模式匹配 %d 个 Pod；请确保正则覆盖滚动更新后的实例", len(pods)))
			},
		})
	default:
		return append(tasks, checkTask{
			category: "Prometheus",
			name:     "Pod 归属/" + app.ID,
			run: func(context.Context) output.Check {
				return c.check("Prometheus", "Pod 归属/"+app.ID, output.Fail, "podSelector.mode 非法")
			},
		})
	}
}

func (c *Checker) containerMatcherTasks(app config.Application, now time.Time, lookback time.Duration) []checkTask {
	if len(app.Kubernetes.ContainerMatchers) == 0 || !app.HasMetric("kubernetes") {
		return nil
	}
	return []checkTask{{
		category: "Prometheus",
		name:     "容器过滤/" + app.ID,
		run: func(ctx context.Context) output.Check {
			selector := `namespace=` + QuoteLabelValue(app.Namespace)
			series, err := c.API.Series(ctx, "container_memory_working_set_bytes{"+selector+"}", now.Add(-lookback), now)
			if err != nil {
				return c.check("Prometheus", "容器过滤/"+app.ID, output.Fail, requestErrorDetail(err))
			}
			if len(series) == 0 {
				return c.check("Prometheus", "容器过滤/"+app.ID, output.Warn,
					fmt.Sprintf("近 %s 内没有 container_memory_working_set_bytes 序列，无法核对容器过滤条件", lookback))
			}
			for _, key := range sortedKeys(app.Kubernetes.ContainerMatchers) {
				want := app.Kubernetes.ContainerMatchers[key]
				matched := 0
				for _, labels := range series {
					if string(labels[model.LabelName(key)]) == want {
						matched++
					}
				}
				if matched == 0 {
					return c.check("Prometheus", "容器过滤/"+app.ID, output.Fail,
						fmt.Sprintf("匹配序列 %d 条，但没有序列满足 %s=%q；容器过滤条件会把应用指标全部过滤为空", len(series), key, want))
				}
			}
			return c.check("Prometheus", "容器过滤/"+app.ID, output.Pass,
				fmt.Sprintf("已核对 %d 个过滤标签；Namespace 内共有 %d 条容器序列", len(app.Kubernetes.ContainerMatchers), len(series)))
		},
	}}
}

func (c *Checker) variableTasks() []checkTask {
	var tasks []checkTask
	expression := BuildPodVariableExpression(c.Site)
	if expression == "" {
		return tasks
	}
	firstApp := ""
	if len(c.Site.Spec.Applications) > 0 {
		firstApp = c.Site.Spec.Applications[0].ID
	}
	expr := strings.ReplaceAll(expression, "${app_selector:json}", QuoteLabelValue(firstApp))
	expr = strings.ReplaceAll(expr, "$app_selector", QuoteLabelValue(firstApp))
	tasks = append(tasks, checkTask{
		category: "Dashboard 变量",
		name:     "pod",
		run: func(ctx context.Context) output.Check {
			value, err := c.API.Query(ctx, expr)
			if err != nil {
				return c.check("Dashboard 变量", "pod", output.Fail,
					"Pod 变量查询不是合法 PromQL: "+requestErrorDetail(err))
			}
			if value == nil || isEmptyValue(value) {
				return c.check("Dashboard 变量", "pod", output.Warn,
					"Pod 变量查询语法合法，但当前时间窗口没有数据")
			}
			return c.check("Dashboard 变量", "pod", output.Pass, "Pod 变量查询合法且返回数据")
		},
	})
	return tasks
}

func (c *Checker) warnTask(category, name, detail string) checkTask {
	return checkTask{
		category: category,
		name:     name,
		run: func(context.Context) output.Check {
			return c.check(category, name, output.Warn, detail)
		},
	}
}

func countLabel(series []model.LabelSet, label string) int {
	count := 0
	for _, labels := range series {
		if _, ok := labels[model.LabelName(label)]; ok {
			count++
		}
	}
	return count
}

func labelSet(series []model.LabelSet, label string) map[string]bool {
	out := map[string]bool{}
	for _, labels := range series {
		if value, ok := labels[model.LabelName(label)]; ok {
			out[string(value)] = true
		}
	}
	return out
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
	detail := []string{fmt.Sprintf("匹配序列：%d", len(series))}
	var incomplete []string
	for _, label := range required {
		count := countLabel(series, label)
		detail = append(detail, fmt.Sprintf("存在 %s 标签：%d", label, count))
		if count < len(series) {
			incomplete = append(incomplete, label)
		}
	}
	var missingOptional []string
	for _, label := range optional {
		count := countLabel(series, label)
		detail = append(detail, fmt.Sprintf("可选 %s 标签：%d", label, count))
		if count == 0 {
			missingOptional = append(missingOptional, label)
		}
	}
	summary := strings.Join(detail, "；")
	if len(incomplete) > 0 {
		return c.check("Prometheus", target+"/"+metric, output.Fail,
			summary+"；原因：必需标签不完整（"+strings.Join(incomplete, ", ")+"），不能把部分标签存在当作全部通过")
	}
	if len(missingOptional) > 0 {
		return c.check("Prometheus", target+"/"+metric, output.Warn,
			summary+"；可选标签缺失（"+strings.Join(missingOptional, ", ")+"）")
	}
	return c.check("Prometheus", target+"/"+metric, output.Pass, summary+"；必需标签齐全")
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
