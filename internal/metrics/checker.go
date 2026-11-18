package metrics

import (
	"context"
	"fmt"
	"regexp"
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

// taskGate lets dependent checks wait for prerequisite checks without holding
// a concurrency slot. This makes the SKIP propagation deadlock-free.
type taskGate interface {
	wait(context.Context) bool
}

type profileGate struct {
	mu        sync.Mutex
	pending   int
	empty     bool
	done      chan struct{}
	matched   map[string]bool
	closeOnce sync.Once
}

func newProfileGate(required map[string][]string) *profileGate {
	g := &profileGate{
		pending: len(required),
		empty:   len(required) == 0,
		done:    make(chan struct{}),
		matched: make(map[string]bool, len(required)),
	}
	if g.empty {
		close(g.done)
	}
	return g
}

func (g *profileGate) record(metric string, matched bool) {
	g.mu.Lock()
	if _, exists := g.matched[metric]; !exists {
		g.matched[metric] = matched
		if g.pending > 0 {
			g.pending--
		}
	}
	shouldClose := g.pending <= 0
	g.mu.Unlock()
	if shouldClose {
		g.closeOnce.Do(func() { close(g.done) })
	}
}

func (g *profileGate) wait(ctx context.Context) bool {
	select {
	case <-g.done:
		return true
	case <-ctx.Done():
		return false
	}
}

func (g *profileGate) anyMatched() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.empty {
		return true
	}
	for _, matched := range g.matched {
		if matched {
			return true
		}
	}
	return false
}

// podOwnership memoizes the Pod association result so the coverage and
// container-filter checks never repeat the same failure.
type podOwnership struct {
	done     chan struct{}
	closeOne sync.Once
	status   output.Status
	detail   string
	pods     map[string]bool
}

func newPodOwnership() *podOwnership {
	return &podOwnership{done: make(chan struct{}), pods: map[string]bool{}}
}

func (p *podOwnership) wait(ctx context.Context) bool {
	select {
	case <-p.done:
		return true
	case <-ctx.Done():
		return false
	}
}

func (p *podOwnership) finish(check output.Check, pods map[string]bool) output.Check {
	p.status = check.Status
	p.detail = check.Detail
	if pods != nil {
		p.pods = pods
	}
	p.closeOne.Do(func() { close(p.done) })
	return check
}

type checkTask struct {
	category string
	name     string
	run      func(context.Context) output.Check
	gates    []taskGate
	// skip is evaluated only after all gates are ready.
	skip func() (bool, string)
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
			for _, gate := range tasks[i].gates {
				if !gate.wait(ctx) {
					results[i] = c.check(tasks[i].category, tasks[i].name, output.Fail, "校验被取消")
					return
				}
			}
			if tasks[i].skip != nil {
				if skip, detail := tasks[i].skip(); skip {
					results[i] = c.check(tasks[i].category, tasks[i].name, output.Skip, detail)
					return
				}
			}
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
		selector := DomainSelectorFor(domain)
		required, optional := profileRequirements(profile)
		gate := newProfileGate(required)
		tasks = append(tasks, c.metricTasks("站点 "+domain.ID, selector, required, optional, gate, now, lookback)...)
		for _, panel := range profile.Panels {
			panel := panel
			query := RenderCheckQuery(panel.Query, c.Site, selector.String())
			tasks = append(tasks, c.queryTask("Blackbox/"+domain.ID+"/"+panel.ID, query, []taskGate{gate},
				dependencySkip(gate, lookback)))
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

	gates := map[string]*profileGate{}
	ownerships := map[string]*podOwnership{}
	namespaceChecked := map[string]bool{}

	for _, app := range apps {
		for _, profile := range c.activeApplicationProfiles(app) {
			required, optional := profileRequirements(profile)
			gate := newProfileGate(required)
			switch profile.Scope {
			case ScopeApplication:
				gates[app.ID+"\x00"+profile.ID] = gate
				selector := ApplicationSelector(app)
				tasks = append(tasks, c.metricTasks("应用 "+app.ID, selector, required, optional, gate, now, lookback)...)
				for _, panel := range profile.Panels {
					if panel.Aggregation == AggregationSite {
						continue
					}
					name := "应用/" + app.ID + "/" + profile.ID + "/" + panel.ID
					query, err := RenderApplicationQuery(panel.Query, []config.Application{app}, false, "")
					if err != nil {
						tasks = append(tasks, c.failTask("PromQL", name, err.Error()))
						continue
					}
					tasks = append(tasks, c.queryTask(name, RenderCheckQuery(query, c.Site, ""), []taskGate{gate},
						dependencySkip(gate, lookback)))
				}
			case ScopeNamespace:
				if app.Kubernetes.PodSelector == nil {
					tasks = append(tasks, c.warnTask("Prometheus", "应用 "+app.ID+"/Kubernetes Pod 归属",
						"未配置 kubernetes.podSelector；跳过应用级 Kubernetes 查询，避免纳入同 Namespace 其他应用 Pod"))
					continue
				}
				gates[app.ID+"\x00"+profile.ID] = gate
				selector := NewSelector(LabelCondition{Name: "namespace", Value: app.Namespace})
				tasks = append(tasks, c.metricTasks("应用 "+app.ID, selector, required, optional, gate, now, lookback)...)

				ownership := ownerships[app.ID]
				if ownership == nil {
					ownership = newPodOwnership()
					ownerships[app.ID] = ownership
					tasks = append(tasks, c.podOwnershipTask(app, ownership, now, lookback))
				}
				for _, panel := range profile.Panels {
					name := "应用/" + app.ID + "/" + profile.ID + "/" + panel.ID
					query, err := RenderNamespaceQuery(panel.Query, []config.Application{app}, false, "")
					if err != nil {
						tasks = append(tasks, c.failTask("PromQL", name, err.Error()))
						continue
					}
					tasks = append(tasks, c.queryTask(name, RenderCheckQuery(query, c.Site, ""), []taskGate{gate, ownership},
						namespaceDependencySkip(gate, ownership, lookback)))
				}
				if !namespaceChecked[app.Namespace] {
					namespaceChecked[app.Namespace] = true
					tasks = append(tasks, c.namespaceExistenceTask(app, now, lookback))
				}
				tasks = append(tasks, c.kubernetesCoverageTasks(app, ownership, now, lookback)...)
				tasks = append(tasks, c.containerMatcherTasks(app, ownership, now, lookback)...)
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
		var entryGates []*profileGate
		for _, app := range entry {
			if gate, ok := gates[app.ID+"\x00"+profile.ID]; ok {
				entryGates = append(entryGates, gate)
			}
		}
		for _, panel := range sitePanels {
			name := "站点/" + profile.ID + "/" + panel.ID
			query, err := RenderApplicationQuery(panel.Query, entry, false, "")
			if err != nil {
				tasks = append(tasks, c.failTask("PromQL", name, err.Error()))
				continue
			}
			tasks = append(tasks, c.queryTask(name, RenderCheckQuery(query, c.Site, ""), profileGates(entryGates),
				anyDependencySkip(entryGates, lookback)))
		}
	}

	// Auto application health profile (up by job).
	if auto, ok := c.Registry.AutoProfile("platform"); ok {
		required, optional := profileRequirements(auto)
		var upGates []*profileGate
		for _, app := range apps {
			if app.Prometheus.Job == "" {
				continue
			}
			gate := newProfileGate(required)
			upGates = append(upGates, gate)
			tasks = append(tasks, c.metricTasks("应用 "+app.ID, TargetSelectorFor(app), required, optional, gate, now, lookback)...)
		}
		for _, panel := range auto.Panels {
			name := "平台/" + panel.ID
			query, err := RenderJobQuery(panel.Query, apps)
			if err != nil {
				tasks = append(tasks, c.failTask("PromQL", name, err.Error()))
				continue
			}
			tasks = append(tasks, c.queryTask(name, RenderCheckQuery(query, c.Site, ""), profileGates(upGates),
				anyDependencySkip(upGates, lookback)))
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

func profileGates(gates []*profileGate) []taskGate {
	out := make([]taskGate, 0, len(gates))
	for _, gate := range gates {
		out = append(out, gate)
	}
	return out
}

func dependencySkip(gate *profileGate, lookback time.Duration) func() (bool, string) {
	return func() (bool, string) {
		if gate.anyMatched() {
			return false, ""
		}
		return true, fmt.Sprintf("依赖的基础指标在近 %s 内没有匹配序列；根因已在上方指标检查中报告，跳过该下游查询",
			formatLookback(lookback))
	}
}

func anyDependencySkip(gates []*profileGate, lookback time.Duration) func() (bool, string) {
	if len(gates) == 0 {
		return nil
	}
	return func() (bool, string) {
		for _, gate := range gates {
			if gate.anyMatched() {
				return false, ""
			}
		}
		return true, fmt.Sprintf("所有入口应用的基础指标在近 %s 内都没有匹配序列；根因已在上方指标检查中报告，跳过该下游查询",
			formatLookback(lookback))
	}
}

func namespaceDependencySkip(gate *profileGate, ownership *podOwnership, lookback time.Duration) func() (bool, string) {
	return func() (bool, string) {
		if !gate.anyMatched() {
			return true, fmt.Sprintf("依赖的基础指标在近 %s 内没有匹配序列；根因已在上方指标检查中报告，跳过该下游查询",
				formatLookback(lookback))
		}
		if ownership.status == output.Fail {
			return true, "Pod 归属未通过，无法确定查询范围；根因已在 Pod 归属检查中报告，跳过该下游查询"
		}
		return false, ""
	}
}

func (c *Checker) namespaceExistenceTask(app config.Application, now time.Time, lookback time.Duration) checkTask {
	name := "Namespace/" + app.Namespace
	return checkTask{
		category: "Prometheus",
		name:     name,
		run: func(ctx context.Context) output.Check {
			exists, err := c.API.MetricExists(ctx, "kube_pod_info", now.Add(-7*24*time.Hour), now)
			if err != nil {
				return c.check("Prometheus", name, output.Fail, requestErrorDetail(err))
			}
			if !exists {
				return c.check("Prometheus", name, output.Warn,
					"kube_pod_info 不存在，无法独立验证 Namespace；将继续通过 Pod 归属规则校验")
			}
			selector := NewSelector(LabelCondition{Name: "namespace", Value: app.Namespace})
			series, err := c.API.Series(ctx, "kube_pod_info{"+selector.String()+"}", now.Add(-lookback), now)
			if err != nil {
				return c.check("Prometheus", name, output.Fail, requestErrorDetail(err))
			}
			if len(series) == 0 {
				return c.check("Prometheus", name, output.Fail,
					fmt.Sprintf("Namespace %q 在近 %s 内没有 kube_pod_info 序列；请检查 Namespace 名称与采集范围\n\n实际 Selector:\n%s",
						app.Namespace, formatLookback(lookback), selector.String()))
			}
			return c.check("Prometheus", name, output.Pass,
				fmt.Sprintf("Namespace %q 存在，kube_pod_info 序列 %d 条", app.Namespace, len(series)))
		},
	}
}

func (c *Checker) podOwnershipTask(app config.Application, ownership *podOwnership, now time.Time, lookback time.Duration) checkTask {
	selector := app.Kubernetes.PodSelector
	name := "Pod 归属/" + app.ID
	if selector != nil && selector.Mode == config.PodSelectorLabels {
		name += "/" + selector.PodLabelMetric()
	}
	return checkTask{
		category: "Prometheus",
		name:     name,
		run: func(ctx context.Context) output.Check {
			check, pods := c.computePodOwnership(ctx, app, now, lookback)
			return ownership.finish(check, pods)
		},
	}
}

func (c *Checker) computePodOwnership(ctx context.Context, app config.Application, now time.Time, lookback time.Duration) (output.Check, map[string]bool) {
	ps := app.Kubernetes.PodSelector
	name := "Pod 归属/" + app.ID
	if ps == nil {
		return c.check("Prometheus", name, output.Fail, "未配置 podSelector"), nil
	}
	switch ps.Mode {
	case config.PodSelectorLabels:
		metric := ps.PodLabelMetric()
		name += "/" + metric
		exists, err := c.API.MetricExists(ctx, metric, now.Add(-7*24*time.Hour), now)
		if err != nil {
			return c.check("Prometheus", name, output.Fail, requestErrorDetail(err)), nil
		}
		if !exists {
			return c.check("Prometheus", name, output.Fail,
				"Pod 归属指标 "+metric+" 不存在（近 7 天无序列）。请改用 pod-regex 模式，或在目标 Prometheus 中确认该指标后再配置。"), nil
		}
		conditions := []LabelCondition{{Name: "namespace", Value: app.Namespace}}
		for _, key := range sortedKeys(ps.Matchers) {
			conditions = append(conditions, LabelCondition{Name: key, Value: ps.Matchers[key]})
		}
		selector := NewSelector(conditions...)
		series, err := c.API.Series(ctx, metric+"{"+selector.String()+"}", now.Add(-lookback), now)
		if err != nil {
			return c.check("Prometheus", name, output.Fail, requestErrorDetail(err)), nil
		}
		if len(series) == 0 {
			return c.check("Prometheus", name, output.Fail,
				fmt.Sprintf("配置的 Pod 标签在近 %s 内没有匹配序列\n\n实际 Selector:\n%s",
					formatLookback(lookback), selector.String())), nil
		}
		for _, key := range sortedKeys(ps.Matchers) {
			if countLabel(series, key) == 0 {
				return c.check("Prometheus", name, output.Fail,
					fmt.Sprintf("匹配序列 %d 条，但没有任何序列带有标签 %s；无法建立可靠的 Pod 归属", len(series), key)), nil
			}
		}
		podCount := countLabel(series, "pod")
		if podCount == 0 {
			return c.check("Prometheus", name, output.Fail,
				fmt.Sprintf("匹配序列 %d 条，但没有任何序列带有 pod 标签；无法建立 Pod 集合", len(series))), nil
		}
		pods := labelSet(series, "pod")
		if podCount < len(series) {
			return c.check("Prometheus", name, output.Warn,
				fmt.Sprintf("匹配序列 %d 条，仅 %d 条带有 pod 标签；无法建立完整 Pod 集合，资源面板可能缺数据", len(series), podCount)), pods
		}
		cadvisorPods, err := c.cadvisorPods(ctx, app, now, lookback)
		if err != nil {
			return c.check("Prometheus", name, output.Fail, requestErrorDetail(err)), pods
		}
		matched := 0
		for pod := range pods {
			if cadvisorPods[pod] {
				matched++
			}
		}
		switch {
		case matched == 0:
			return c.check("Prometheus", name, output.Fail,
				fmt.Sprintf("匹配 %d 个 Pod，但与 cAdvisor 指标没有交集；Kubernetes 资源面板无法获得数据。请检查 Namespace 和 Pod 标签是否属于该 Prometheus 集群。", len(pods))), pods
		case matched < len(pods):
			return c.check("Prometheus", name, output.Warn,
				fmt.Sprintf("匹配 %d 个 Pod，其中仅 %d 个与 cAdvisor 指标交集匹配；无法确认 Pod 集合完整性，部分资源面板可能缺数据", len(pods), matched)), pods
		default:
			return c.check("Prometheus", name, output.Pass,
				fmt.Sprintf("匹配 %d 个 Pod，其中 %d 个与 cAdvisor 指标交集匹配", len(pods), matched)), pods
		}
	case config.PodSelectorPodRegex:
		if ps.Pod == "" {
			return c.check("Prometheus", name, output.Fail, "pod-regex 模式缺少 pod 正则"), nil
		}
		selector := NewSelector(
			LabelCondition{Name: "namespace", Value: app.Namespace},
			LabelCondition{Name: "pod", Value: ps.Pod, Regex: true},
		)
		pods, err := c.cadvisorPodsWithSelector(ctx, selector, now, lookback)
		if err != nil {
			return c.check("Prometheus", name, output.Fail, requestErrorDetail(err)), nil
		}
		if len(pods) == 0 {
			return c.check("Prometheus", name, output.Fail,
				fmt.Sprintf("Pod 正则在近 %s 内没有匹配到 cAdvisor 序列\n\n实际 Selector:\n%s\n\nDeployment 滚动更新后请确认正则仍覆盖新实例。",
					formatLookback(lookback), selector.String())), nil
		}
		return c.check("Prometheus", name, output.Pass,
			fmt.Sprintf("pod-regex 模式匹配 %d 个 Pod（示例：%s）；Deployment 滚动更新后请确认正则仍覆盖新实例",
				len(pods), sampleValues(pods, 5))), pods
	default:
		return c.check("Prometheus", name, output.Fail, "podSelector.mode 非法"), nil
	}
}

// cadvisorPods returns the union of Pods visible in the two cAdvisor metrics
// that the resource panels depend on. Using a single metric would incorrectly
// fail ownership when only one of CPU or memory has been scraped.
func (c *Checker) cadvisorPods(ctx context.Context, app config.Application, now time.Time, lookback time.Duration) (map[string]bool, error) {
	ps := app.Kubernetes.PodSelector
	if ps == nil {
		return map[string]bool{}, nil
	}
	var selector Selector
	if ps.Mode == config.PodSelectorPodRegex && ps.Pod != "" {
		selector = NewSelector(
			LabelCondition{Name: "namespace", Value: app.Namespace},
			LabelCondition{Name: "pod", Value: ps.Pod, Regex: true},
		)
	} else {
		selector = NewSelector(
			LabelCondition{Name: "namespace", Value: app.Namespace},
			LabelCondition{Name: "pod", Value: ".+", Regex: true},
		)
	}
	return c.cadvisorPodsWithSelector(ctx, selector, now, lookback)
}

func (c *Checker) cadvisorPodsWithSelector(ctx context.Context, selector Selector, now time.Time, lookback time.Duration) (map[string]bool, error) {
	pods := map[string]bool{}
	var firstErr error
	for _, metric := range []string{"container_memory_working_set_bytes", "container_cpu_usage_seconds_total"} {
		series, err := c.API.Series(ctx, metric+"{"+selector.String()+"}", now.Add(-lookback), now)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for pod := range labelSet(series, "pod") {
			pods[pod] = true
		}
	}
	if len(pods) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return pods, nil
}

var kubernetesCoverageMetrics = []string{
	"container_memory_working_set_bytes",
	"container_cpu_usage_seconds_total",
	"container_cpu_cfs_throttled_periods_total",
	"container_cpu_cfs_periods_total",
}

// kubernetesCoverageTasks verifies that each cAdvisor metric actually covers
// the application's pods and that container matchers do not filter everything.
func (c *Checker) kubernetesCoverageTasks(app config.Application, ownership *podOwnership, now time.Time, lookback time.Duration) []checkTask {
	if app.Kubernetes.PodSelector == nil {
		return nil
	}
	var tasks []checkTask
	for _, metric := range kubernetesCoverageMetrics {
		metric := metric
		name := "Kubernetes 指标/" + app.ID + "/" + metric
		tasks = append(tasks, checkTask{
			category: "Prometheus",
			name:     name,
			gates:    []taskGate{ownership},
			run: func(ctx context.Context) output.Check {
				if ownership.status == output.Fail {
					return c.check("Prometheus", name, output.Skip,
						"Pod 归属未通过，无法确认应用范围；根因已在 Pod 归属检查中报告，跳过该覆盖检查")
				}
				if ownership.status == output.Warn {
					return c.check("Prometheus", name, output.Warn,
						"Pod 归属未能完全确认，无法判断资源指标覆盖完整性；根因已在 Pod 归属检查中报告")
				}
				exists, err := c.API.MetricExists(ctx, metric, now.Add(-7*24*time.Hour), now)
				if err != nil {
					return c.check("Prometheus", name, output.Fail, requestErrorDetail(err))
				}
				if !exists {
					return c.check("Prometheus", name, output.Skip,
						"指标不存在，已在指标检查中报告，无需重复检查应用覆盖")
				}
				pods := ownership.pods
				selector := NewSelector(
					LabelCondition{Name: "namespace", Value: app.Namespace},
					LabelCondition{Name: "pod", Value: ".+", Regex: true},
				)
				series, err := c.API.Series(ctx, metric+"{"+selector.String()+"}", now.Add(-lookback), now)
				if err != nil {
					return c.check("Prometheus", name, output.Fail, requestErrorDetail(err))
				}
				appSeries, filtered, risky := 0, 0, 0
				for _, labels := range series {
					pod := string(labels["pod"])
					if !pods[pod] {
						continue
					}
					appSeries++
					if isRiskyContainer(labels) {
						risky++
					}
					matched := true
					for _, matcher := range app.Kubernetes.ContainerMatchers {
						if !matcherMatches(labels, matcher) {
							matched = false
							break
						}
					}
					if matched {
						filtered++
					}
				}
				switch {
				case appSeries == 0:
					return c.check("Prometheus", name, output.Warn,
						fmt.Sprintf("应用 Pod 存在，但该指标在近 %s 内没有匹配序列（应用 Pod 数量 %d）",
							formatLookback(lookback), len(pods)))
				case len(app.Kubernetes.ContainerMatchers) > 0 && filtered == 0:
					return c.check("Prometheus", name, output.Fail,
						fmt.Sprintf("应用 Pod 序列 %d 条，但容器过滤后为 0；过滤条件会隐藏全部应用资源数据", appSeries))
				case len(app.Kubernetes.ContainerMatchers) == 0 && risky > 0:
					return c.check("Prometheus", name, output.Warn,
						fmt.Sprintf("应用 Pod 序列 %d 条，其中 %d 条为空容器或 POD 基础容器；未配置 containerMatchers，统计可能失真", appSeries, risky))
				default:
					return c.check("Prometheus", name, output.Pass,
						fmt.Sprintf("应用 Pod 序列 %d 条，容器过滤后 %d 条", appSeries, filtered))
				}
			},
		})
	}
	return tasks
}

func isRiskyContainer(labelSet model.LabelSet) bool {
	value, ok := labelSet["container"]
	if !ok {
		// cAdvisor parent cgroups and pause containers usually have no
		// container label at all; both would double count resource usage.
		return true
	}
	if value == "" || value == "POD" {
		return true
	}
	return strings.Contains(strings.ToLower(string(labelSet["image"])), "pause")
}

func (c *Checker) containerMatcherTasks(app config.Application, ownership *podOwnership, now time.Time, lookback time.Duration) []checkTask {
	if !app.HasMetric("kubernetes") || app.Kubernetes.PodSelector == nil {
		return nil
	}
	name := "容器过滤/" + app.ID
	return []checkTask{{
		category: "Prometheus",
		name:     name,
		gates:    []taskGate{ownership},
		run: func(ctx context.Context) output.Check {
			if ownership.status == output.Fail {
				return c.check("Prometheus", name, output.Skip,
					"Pod 归属未通过，无法确定应用 Pod 集合；根因已在 Pod 归属检查中报告，跳过容器过滤检查")
			}
			if ownership.status == output.Warn {
				return c.check("Prometheus", name, output.Warn,
					"Pod 归属未能完全确认，容器过滤只能在已确认的 Pod 范围内检查；根因已在 Pod 归属检查中报告")
			}
			selector := NewSelector(
				LabelCondition{Name: "namespace", Value: app.Namespace},
				LabelCondition{Name: "pod", Value: ".+", Regex: true},
			)
			series, err := c.API.Series(ctx, "container_memory_working_set_bytes{"+selector.String()+"}", now.Add(-lookback), now)
			if err != nil {
				return c.check("Prometheus", name, output.Fail, requestErrorDetail(err))
			}
			var appSeries []model.LabelSet
			for _, labels := range series {
				if ownership.pods[string(labels["pod"])] {
					appSeries = append(appSeries, labels)
				}
			}
			if len(appSeries) == 0 {
				return c.check("Prometheus", name, output.Warn,
					fmt.Sprintf("应用 Pod 在近 %s 内没有 container_memory_working_set_bytes 序列，无法核对容器过滤条件",
						formatLookback(lookback)))
			}
			if len(app.Kubernetes.ContainerMatchers) == 0 {
				return c.check("Prometheus", name, output.Warn, containerRiskDetail(appSeries))
			}
			for _, matcher := range app.Kubernetes.ContainerMatchers {
				matched := 0
				for _, labels := range appSeries {
					if matcherMatches(labels, matcher) {
						matched++
					}
				}
				if matched == 0 {
					return c.check("Prometheus", name, output.Fail,
						fmt.Sprintf("应用 Pod 范围内 %d 条序列，没有序列满足 %s %s %q；容器过滤条件会把应用指标全部过滤为空",
							len(appSeries), matcher.Name, matcher.EffectiveOperator(), matcher.Value))
				}
			}
			matchedAll := 0
			for _, labels := range appSeries {
				ok := true
				for _, matcher := range app.Kubernetes.ContainerMatchers {
					if !matcherMatches(labels, matcher) {
						ok = false
						break
					}
				}
				if ok {
					matchedAll++
				}
			}
			if matchedAll == 0 {
				return c.check("Prometheus", name, output.Fail,
					fmt.Sprintf("单个过滤条件均有匹配，但组合后应用 Pod 范围内匹配序列为 0；请检查 operator 组合"))
			}
			return c.check("Prometheus", name, output.Pass,
				fmt.Sprintf("已核对 %d 个过滤条件；应用 Pod 范围内 %d 条序列，组合匹配 %d 条",
					len(app.Kubernetes.ContainerMatchers), len(appSeries), matchedAll))
		},
	}}
}

func containerRiskDetail(series []model.LabelSet) string {
	risky := 0
	business := 0
	for _, labels := range series {
		if isRiskyContainer(labels) {
			risky++
			continue
		}
		business++
	}
	if business == 0 {
		return "未配置容器过滤条件，且应用 cAdvisor 序列没有可识别的业务容器标签；无法区分业务容器、Pause 容器与 Pod 父级 cgroup，Pod CPU/内存统计可能失真"
	}
	if risky > 0 {
		return fmt.Sprintf("未配置容器过滤条件；应用 Pod 范围内 %d 条序列属于空容器、Pause 容器或 POD 基础容器，Pod CPU/内存统计可能重复计入", risky)
	}
	return "未配置容器过滤条件；当前未发现空容器、Pause 或 POD 基础容器序列，但仍建议确认后配置 containerMatchers"
}

func matcherMatches(labelSet model.LabelSet, matcher config.LabelMatcher) bool {
	actual := string(labelSet[model.LabelName(matcher.Name)])
	value := matcher.Value
	switch matcher.EffectiveOperator() {
	case config.MatchEqual:
		return actual == value
	case config.MatchNotEqual:
		return actual != value
	case config.MatchRegex:
		ok, err := regexp.MatchString("^(?:"+value+")$", actual)
		return err == nil && ok
	case config.MatchNotRegex:
		ok, err := regexp.MatchString("^(?:"+value+")$", actual)
		return err == nil && !ok
	default:
		return false
	}
}

func (c *Checker) variableTasks() []checkTask {
	var tasks []checkTask
	for _, profileID := range []string{"jvm", "kubernetes"} {
		expression, err := BuildPodVariableExpression(c.Site, profileID, ProfileSelectorVar(profileID))
		if err != nil {
			tasks = append(tasks, c.failTask("Dashboard 变量", ProfilePodVar(profileID), err.Error()))
			continue
		}
		if expression == "" {
			continue
		}
		firstApp := firstAppForProfile(c.Site, profileID)
		if firstApp == "" {
			continue
		}
		expr := RenderCheckQuery(expression, c.Site, "")
		name := ProfilePodVar(profileID)
		tasks = append(tasks, checkTask{
			category: "Dashboard 变量",
			name:     name,
			run: func(ctx context.Context) output.Check {
				value, err := c.API.Query(ctx, expr)
				if err != nil {
					return c.check("Dashboard 变量", name, output.Fail,
						"Pod 变量查询不是合法 PromQL: "+requestErrorDetail(err))
				}
				if value == nil || isEmptyValue(value) {
					return c.check("Dashboard 变量", name, output.Warn,
						"Pod 变量查询语法合法，但当前时间窗口没有数据")
				}
				return c.check("Dashboard 变量", name, output.Pass, "Pod 变量查询合法且返回数据")
			},
		})
	}
	return tasks
}

func firstAppForProfile(site *config.Site, profileID string) string {
	for _, app := range site.Spec.Applications {
		if !app.HasMetric(profileID) {
			continue
		}
		if profileID == "kubernetes" && app.Kubernetes.PodSelector == nil {
			continue
		}
		return app.ID
	}
	return ""
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

func (c *Checker) failTask(category, name, detail string) checkTask {
	return checkTask{
		category: category,
		name:     name,
		run: func(context.Context) output.Check {
			return c.check(category, name, output.Fail, detail)
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

func sampleValues(values map[string]bool, limit int) string {
	items := make([]string, 0, len(values))
	for value := range values {
		items = append(items, value)
	}
	sort.Strings(items)
	if len(items) > limit {
		items = items[:limit]
	}
	return strings.Join(items, ", ")
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
			selector := ComponentSelectorFor(component)
			required, optional := profileRequirements(profile)
			gate := newProfileGate(required)
			tasks = append(tasks, c.metricTasks("组件 "+component.ID, selector, required, optional, gate, now, lookback)...)
			for _, panel := range profile.Panels {
				tasks = append(tasks, c.queryTask(
					"组件/"+component.ID+"/"+profile.ID+"/"+panel.ID,
					RenderCheckQuery(panel.Query, c.Site, selector.String()),
					[]taskGate{gate}, dependencySkip(gate, lookback),
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

func (c *Checker) metricTasks(target string, selector Selector, required, optional map[string][]string, gate *profileGate, now time.Time, lookback time.Duration) []checkTask {
	var tasks []checkTask
	for _, metric := range sortedKeysOf(required) {
		metric := metric
		req := required[metric]
		opts := optional[metric]
		tasks = append(tasks, checkTask{
			category: "Prometheus",
			name:     target + "/" + metric,
			run: func(ctx context.Context) output.Check {
				check, matched := c.checkMetric(ctx, target, metric, selector, req, opts, now, lookback)
				if gate != nil {
					gate.record(metric, matched)
				}
				return check
			},
		})
	}
	return tasks
}

func (c *Checker) checkMetric(ctx context.Context, target, metric string, selector Selector, required, optional []string, now time.Time, lookback time.Duration) (output.Check, bool) {
	name := target + "/" + metric
	exists, err := c.API.MetricExists(ctx, metric, now.Add(-7*24*time.Hour), now)
	if err != nil {
		return c.check("Prometheus", name, output.Fail, requestErrorDetail(err)), false
	}
	if !exists {
		return c.check("Prometheus", name, output.Fail,
			"指标不存在（近 7 天无序列）\n\n实际 Selector:\n"+selector.String()), false
	}

	match := metric + "{" + selector.String() + "}"
	series, err := c.API.Series(ctx, match, now.Add(-lookback), now)
	if err != nil {
		return c.check("Prometheus", name, output.Fail, requestErrorDetail(err)), false
	}
	if len(series) == 0 {
		detail := fmt.Sprintf("指标存在（近 7 天），但最近 %s 内未匹配到序列。\n\n实际 Selector:\n%s",
			formatLookback(lookback), selector.String())
		if diagnosis := c.selectorDiagnosis(ctx, metric, selector, now); len(diagnosis) > 0 {
			detail += "\n\n诊断结果:\n" + numberedLines(diagnosis)
		}
		return c.check("Prometheus", name, output.Warn, detail), false
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
	summary := strings.Join(detail, "；") + "\n\n实际 Selector:\n" + selector.String()
	if len(incomplete) > 0 {
		return c.check("Prometheus", name, output.Fail,
			summary+"\n\n原因：必需标签不完整（"+strings.Join(incomplete, ", ")+"），不能把部分标签存在当作全部通过"), true
	}
	if len(missingOptional) > 0 {
		return c.check("Prometheus", name, output.Warn,
			summary+"\n\n可选标签缺失（"+strings.Join(missingOptional, ", ")+"）"), true
	}
	return c.check("Prometheus", name, output.Pass, summary+"\n\n必需标签齐全"), true
}

// selectorDiagnosis uses the labels actually returned by Prometheus. It never
// guesses a label value. Each condition is checked individually so a missing
// extra label can be distinguished from an empty time window.
func (c *Checker) selectorDiagnosis(ctx context.Context, metric string, selector Selector, now time.Time) []string {
	if selector.Empty() {
		return nil
	}
	var lines []string
	allIndividual := true
	checked := 0
	for _, condition := range selector.Conditions {
		if condition.Regex {
			continue
		}
		values, err := c.labelValueCounts(ctx, metric, condition.Name)
		if err != nil {
			lines = append(lines, fmt.Sprintf("无法读取标签 %s 的值: %s", condition.Name, requestErrorDetail(err)))
			allIndividual = false
			continue
		}
		checked++
		if values[condition.Value] > 0 {
			lines = append(lines, fmt.Sprintf("%s=%s：存在", condition.Name, QuoteLabelValue(condition.Value)))
			continue
		}
		allIndividual = false
		line := fmt.Sprintf("%s=%s：不存在", condition.Name, QuoteLabelValue(condition.Value))
		if available := availableValues(values, 5); available != "" {
			line += "；实际存在值: " + available
		}
		lines = append(lines, line)
	}
	if checked > 0 && allIndividual {
		series, err := c.API.Series(ctx, metric+"{"+selector.String()+"}", now.Add(-7*24*time.Hour), now)
		if err != nil {
			lines = append(lines, "无法核对标签组合: "+requestErrorDetail(err))
		} else if len(series) == 0 {
			lines = append(lines, "各标签单独存在，但组合条件在近 7 天没有匹配；额外标签组合或采集范围可能是根因")
		}
	}
	return lines
}

func (c *Checker) labelValueCounts(ctx context.Context, metric, label string) (map[string]int, error) {
	value, err := c.API.Query(ctx, fmt.Sprintf("count by (%s) (%s)", label, metric))
	if err != nil {
		return nil, err
	}
	vector, ok := value.(model.Vector)
	if !ok {
		return nil, fmt.Errorf("返回类型不是瞬时向量")
	}
	out := map[string]int{}
	for _, sample := range vector {
		if value, ok := sample.Metric[model.LabelName(label)]; ok {
			out[string(value)] += int(sample.Value)
		}
	}
	return out, nil
}

func availableValues(values map[string]int, limit int) string {
	items := make([]string, 0, len(values))
	for value := range values {
		items = append(items, `"`+value+`"`)
	}
	sort.Strings(items)
	if len(items) > limit {
		items = items[:limit]
		items = append(items, "...")
	}
	return strings.Join(items, ", ")
}

func numberedLines(lines []string) string {
	var b strings.Builder
	for i, line := range lines {
		fmt.Fprintf(&b, "%d. %s", i+1, line)
		if i != len(lines)-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func (c *Checker) queryTask(name, query string, gates []taskGate, skip func() (bool, string)) checkTask {
	return checkTask{
		category: "PromQL",
		name:     name,
		gates:    gates,
		skip:     skip,
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

func formatLookback(lookback time.Duration) string {
	if lookback <= 0 {
		return "1h"
	}
	if lookback%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(lookback/time.Hour))
	}
	if lookback < time.Hour {
		return fmt.Sprintf("%dm", int(lookback/time.Minute))
	}
	hours := int(lookback / time.Hour)
	minutes := int((lookback % time.Hour) / time.Minute)
	if minutes == 0 {
		return fmt.Sprintf("%dh", hours)
	}
	return fmt.Sprintf("%dh%dm", hours, minutes)
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
