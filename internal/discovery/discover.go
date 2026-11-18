package discovery

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/common/model"

	"site-observability/internal/config"
	"site-observability/internal/metrics"
)

// DiscoverOptions controls the read-only discovery window and output size.
type DiscoverOptions struct {
	Lookback      time.Duration
	Now           func() time.Time
	ProfileFilter string
	Site          *config.Site
	MaxCandidates int
}

// MetricStatus records whether a metric exists historically and currently.
type MetricStatus struct {
	Name        string
	Exists      bool
	HasCurrent  bool
	SeriesCount int
	Error       string
}

// ProfileAvailability is the per-profile data readiness for an application.
type ProfileAvailability struct {
	Available      bool
	MetricCount    int
	MissingMetrics []string
	MissingLabels  []string
	Labels         []string
	Note           string
}

// AppCandidate is an application identity discovered from real metric labels.
type AppCandidate struct {
	Application string
	Namespace   string
	Jobs        []string
	Profiles    map[string]ProfileAvailability
	ExtraLabels map[string][]string
}

// DomainCandidate is a Blackbox probe target.
type DomainCandidate struct {
	Instance string
	Job      string
	Count    int
	Duration bool
}

// ComponentCandidate is a MySQL/Redis/Kafka/Nacos/custom exporter target.
type ComponentCandidate struct {
	Type     string
	Job      string
	Instance string
	Metric   string
	Count    int
	Labels   map[string][]string
}

// PodRegexSuggestion is a query-verified suggestion, never a guess.
type PodRegexSuggestion struct {
	Regex       string
	MatchedPods int
	SamplePods  []string
}

// ContainerMatcherSuggestion is derived from actual container/image series.
type ContainerMatcherSuggestion struct {
	Name         string
	Operator     string
	Value        string
	Before       int
	After        int
	ParentOnly   int
	PauseOnly    int
	BusinessOnly int
	Note         string
}

// PodLabelSuggestion is a verified kube_pod_labels ownership rule.
type PodLabelSuggestion struct {
	Label       string
	Value       string
	MatchedPods int
	SamplePods  []string
}

// KubernetesSection describes the real cAdvisor / kube-state-metrics shape.
type KubernetesSection struct {
	Metrics             []MetricStatus
	LabelNames          []string
	Namespaces          []string
	PodsByNamespace     map[string][]string
	ContainerLabels     map[string]map[string][]string // namespace -> label -> values
	PodLabelValues      map[string]map[string][]string // namespace -> label_* -> values
	PodLabelByApp       map[string]PodLabelSuggestion  // application\x00namespace
	PodRegexByApp       map[string]PodRegexSuggestion  // application\x00namespace
	ContainerSuggestion map[string]ContainerMatcherSuggestion
}

// Report is the complete read-only discovery result.
type Report struct {
	Connected    bool
	GeneratedAt  time.Time
	Lookback     time.Duration
	Metrics      []MetricStatus
	Applications []AppCandidate
	Domains      []DomainCandidate
	Kubernetes   KubernetesSection
	Components   []ComponentCandidate
	Warnings     []string
}

// DiscoverAPI is the read-only Prometheus surface used by discovery. It is an
// interface so discovery logic can be tested without a live server.
type DiscoverAPI interface {
	Ping(ctx context.Context) error
	MetricExists(ctx context.Context, metric string, start, end time.Time) (bool, error)
	QueryAt(ctx context.Context, expr string, at time.Time) (model.Value, error)
	LabelNames(ctx context.Context, match string, start, end time.Time) ([]string, error)
	LabelValues(ctx context.Context, label, match string, start, end time.Time) ([]string, error)
}

type discoverer struct {
	api           DiscoverAPI
	registry      *metrics.Registry
	site          *config.Site
	now           time.Time
	lookback      time.Duration
	maxCandidates int
	metricStatus  map[string]MetricStatus
	labelsCache   map[string][]string
}

// Discover queries the real Prometheus and returns candidate site objects.
// It never writes configuration or modifies any backend.
func Discover(ctx context.Context, api DiscoverAPI, registry *metrics.Registry, options DiscoverOptions) (*Report, error) {
	if api == nil {
		return nil, fmt.Errorf("Prometheus 客户端为空")
	}
	if registry == nil {
		return nil, fmt.Errorf("指标模板注册表为空")
	}
	if err := api.Ping(ctx); err != nil {
		return nil, err
	}
	lookback := options.Lookback
	if lookback <= 0 {
		lookback = time.Hour
	}
	now := time.Now()
	if options.Now != nil {
		now = options.Now()
	}
	maxCandidates := options.MaxCandidates
	if maxCandidates <= 0 {
		maxCandidates = 200
	}
	d := &discoverer{
		api:           api,
		registry:      registry,
		site:          options.Site,
		now:           now,
		lookback:      lookback,
		maxCandidates: maxCandidates,
		metricStatus:  map[string]MetricStatus{},
		labelsCache:   map[string][]string{},
	}
	report := &Report{
		Connected:   true,
		GeneratedAt: now,
		Lookback:    lookback,
		Kubernetes: KubernetesSection{
			PodsByNamespace:     map[string][]string{},
			ContainerLabels:     map[string]map[string][]string{},
			PodLabelValues:      map[string]map[string][]string{},
			PodLabelByApp:       map[string]PodLabelSuggestion{},
			PodRegexByApp:       map[string]PodRegexSuggestion{},
			ContainerSuggestion: map[string]ContainerMatcherSuggestion{},
		},
	}
	if err := d.discoverApplications(ctx, report, options.ProfileFilter); err != nil {
		report.Warnings = append(report.Warnings, err.Error())
	}
	if options.ProfileFilter == "" || options.ProfileFilter == "blackbox" {
		if err := d.discoverDomains(ctx, report); err != nil {
			report.Warnings = append(report.Warnings, err.Error())
		}
	}
	if options.ProfileFilter == "" || options.ProfileFilter == "kubernetes" {
		if err := d.discoverKubernetes(ctx, report, options.Site); err != nil {
			report.Warnings = append(report.Warnings, err.Error())
		}
	}
	if err := d.discoverComponents(ctx, report, options.ProfileFilter); err != nil {
		report.Warnings = append(report.Warnings, err.Error())
	}
	report.Metrics = d.sortedMetricStatuses()
	return report, nil
}

func (d *discoverer) discoverApplications(ctx context.Context, report *Report, profileFilter string) error {
	apps := map[string]*AppCandidate{}
	for _, id := range d.registry.Order {
		profile := d.registry.Profiles[id]
		if profile.Auto || profile.Target != config.TargetApplication || profile.Scope == metrics.ScopeNamespace {
			continue
		}
		if profileFilter != "" && profileFilter != id {
			continue
		}
		metric := profile.DiscoveryMetric()
		if metric == "" {
			continue
		}
		status := d.metricStatusOf(ctx, metric)
		if !status.Exists {
			continue
		}
		groups, err := d.groupCount(ctx, metric, profile.DiscoveryGroupBy(), "")
		if err != nil {
			report.Warnings = append(report.Warnings, fmt.Sprintf("%s 应用发现失败: %v", id, err))
			continue
		}
		for _, group := range groups {
			application := group.Labels["application"]
			namespace := group.Labels["namespace"]
			if application == "" || namespace == "" {
				continue
			}
			if d.site != nil && !siteHasApp(d.site, application, namespace) {
				continue
			}
			job := group.Labels["job"]
			key := application + "\x00" + namespace
			candidate := apps[key]
			if candidate == nil {
				if len(apps) >= d.maxCandidates {
					continue
				}
				candidate = &AppCandidate{
					Application: application,
					Namespace:   namespace,
					Profiles:    map[string]ProfileAvailability{},
					ExtraLabels: map[string][]string{},
				}
				apps[key] = candidate
			}
			if job != "" && !containsString(candidate.Jobs, job) {
				candidate.Jobs = append(candidate.Jobs, job)
			}
			availability := d.profileAvailability(ctx, profile, candidate)
			candidate.Profiles[id] = availability
			if len(candidate.ExtraLabels) == 0 {
				candidate.ExtraLabels = d.candidateLabels(ctx, metric, candidate)
			}
		}
	}
	for _, candidate := range apps {
		sort.Strings(candidate.Jobs)
		if len(candidate.Jobs) > 1 {
			for profileID, availability := range candidate.Profiles {
				availability.Note = "多个 Job，需要确认：" + strings.Join(candidate.Jobs, ", ")
				candidate.Profiles[profileID] = availability
			}
		}
		report.Applications = append(report.Applications, *candidate)
	}
	sort.Slice(report.Applications, func(i, j int) bool {
		if report.Applications[i].Application != report.Applications[j].Application {
			return report.Applications[i].Application < report.Applications[j].Application
		}
		return report.Applications[i].Namespace < report.Applications[j].Namespace
	})
	return nil
}

func (d *discoverer) profileAvailability(ctx context.Context, profile *metrics.Profile, candidate *AppCandidate) ProfileAvailability {
	required, _ := profile.Requirements()
	availability := ProfileAvailability{Available: true}
	selector := candidateSelector(candidate, profile.DiscoveryGroupBy())
	discoveryMetric := profile.DiscoveryMetric()

	names, err := d.labelNames(ctx, discoveryMetric, selector)
	if err == nil {
		availability.Labels = names
	}
	for _, metric := range sortedMetricKeys(required) {
		if !d.metricStatusOf(ctx, metric).Exists {
			availability.MissingMetrics = append(availability.MissingMetrics, metric)
			continue
		}
		count, err := d.countFor(ctx, metric, selector)
		if err != nil || count == 0 {
			availability.MissingMetrics = append(availability.MissingMetrics, metric)
			continue
		}
		availability.MetricCount += count
	}
	labelSet := map[string]bool{}
	for _, label := range names {
		labelSet[label] = true
	}
	for _, labels := range required {
		for _, label := range labels {
			if !labelSet[label] && !containsString(availability.MissingLabels, label) {
				availability.MissingLabels = append(availability.MissingLabels, label)
			}
		}
	}
	sort.Strings(availability.MissingMetrics)
	sort.Strings(availability.MissingLabels)
	if len(availability.MissingMetrics) > 0 {
		availability.Available = false
		availability.Note = "缺少指标: " + strings.Join(availability.MissingMetrics, ", ")
	} else if len(availability.MissingLabels) > 0 {
		availability.Available = false
		availability.Note = "缺少必需标签: " + strings.Join(availability.MissingLabels, ", ")
	} else {
		availability.Note = "指标完整"
	}
	return availability
}

func (d *discoverer) discoverDomains(ctx context.Context, report *Report) error {
	profile, ok := d.registry.AutoProfile("blackbox")
	if !ok {
		return nil
	}
	metric := profile.DiscoveryMetric()
	if metric == "" || !d.metricStatusOf(ctx, metric).Exists {
		return nil
	}
	groups, err := d.groupCount(ctx, metric, profile.DiscoveryGroupBy(), "")
	if err != nil {
		return fmt.Errorf("Blackbox 域名发现失败: %w", err)
	}
	durationMetric := ""
	required, _ := profile.Requirements()
	for _, metric := range sortedMetricKeys(required) {
		if strings.Contains(metric, "duration") {
			durationMetric = metric
			break
		}
	}
	for _, group := range groups {
		instance := group.Labels["instance"]
		if instance == "" {
			continue
		}
		if d.site != nil && !siteHasDomain(d.site, instance) {
			continue
		}
		candidate := DomainCandidate{
			Instance: instance,
			Job:      group.Labels["job"],
			Count:    group.Count,
			Duration: durationMetric != "" && d.metricStatusOf(ctx, durationMetric).Exists,
		}
		report.Domains = append(report.Domains, candidate)
	}
	sort.Slice(report.Domains, func(i, j int) bool { return report.Domains[i].Instance < report.Domains[j].Instance })
	return nil
}

func (d *discoverer) discoverKubernetes(ctx context.Context, report *Report, site *config.Site) error {
	names := []string{
		"container_memory_working_set_bytes",
		"container_cpu_usage_seconds_total",
		"container_cpu_cfs_throttled_periods_total",
		"container_cpu_cfs_periods_total",
		"kube_pod_info",
		"kube_pod_status_phase",
		"kube_pod_labels",
	}
	for _, metric := range names {
		report.Kubernetes.Metrics = append(report.Kubernetes.Metrics, d.metricStatusOf(ctx, metric))
	}
	if !d.metricStatusOf(ctx, "container_memory_working_set_bytes").Exists {
		return nil
	}
	namespaces, err := d.groupCount(ctx, "container_memory_working_set_bytes", []string{"namespace"}, "")
	if err != nil {
		return fmt.Errorf("Kubernetes Namespace 发现失败: %w", err)
	}
	siteNamespaces := map[string]bool{}
	if d.site != nil {
		for _, app := range d.site.Spec.Applications {
			siteNamespaces[app.Namespace] = true
		}
	}
	for _, namespace := range namespaces {
		name := namespace.Labels["namespace"]
		if name == "" {
			continue
		}
		if d.site != nil && !siteNamespaces[name] {
			continue
		}
		if len(report.Kubernetes.Namespaces) >= d.maxCandidates {
			break
		}
		report.Kubernetes.Namespaces = append(report.Kubernetes.Namespaces, name)
		podSelector := `namespace=` + metrics.QuoteLabelValue(name)
		if d.site != nil {
			var regexes []string
			for _, app := range d.site.Spec.Applications {
				if app.Namespace == name && app.Application != "" {
					regexes = append(regexes, regexp.QuoteMeta(app.Application)+"-.*")
				}
			}
			if len(regexes) == 0 {
				continue
			}
			podSelector += `, pod=~` + metrics.QuoteLabelValue("^(?:"+strings.Join(regexes, "|")+")$")
		}
		pods, err := d.groupCount(ctx, "container_cpu_usage_seconds_total", []string{"pod"}, podSelector)
		if err == nil {
			var podNames []string
			for _, pod := range pods {
				if pod.Labels["pod"] != "" {
					podNames = append(podNames, pod.Labels["pod"])
				}
			}
			sort.Strings(podNames)
			if len(podNames) > d.maxCandidates {
				podNames = podNames[:d.maxCandidates]
			}
			report.Kubernetes.PodsByNamespace[name] = podNames
		}
		containerGroups, err := d.groupCount(ctx, "container_memory_working_set_bytes", []string{"container", "image"}, podSelector)
		if err == nil {
			values := map[string]map[string][]string{}
			for _, group := range containerGroups {
				for _, label := range []string{"container", "image"} {
					value := group.Labels[label]
					if value == "" {
						continue
					}
					if values[label] == nil {
						values[label] = map[string][]string{}
					}
					if _, ok := values[label][value]; !ok {
						values[label][value] = []string{value}
					}
				}
			}
			report.Kubernetes.ContainerLabels[name] = flattenValueMap(values)
		}
	}

	// If kube_pod_labels exists, show the real label_* values and verify a
	// label-based ownership suggestion. Otherwise the regex path is used.
	if d.metricStatusOf(ctx, "kube_pod_labels").Exists {
		for _, namespace := range report.Kubernetes.Namespaces {
			selector := `namespace=` + metrics.QuoteLabelValue(namespace)
			match := "kube_pod_labels{" + selector + "}"
			names, err := d.labelNames(ctx, "kube_pod_labels", selector)
			if err != nil {
				continue
			}
			values := map[string][]string{}
			for _, name := range names {
				if !strings.HasPrefix(name, "label_") {
					continue
				}
				found, err := d.api.LabelValues(ctx, name, match, d.now.Add(-d.lookback), d.now)
				if err != nil || len(found) == 0 {
					continue
				}
				sort.Strings(found)
				if len(found) > 20 {
					found = found[:20]
				}
				values[name] = found
			}
			report.Kubernetes.PodLabelValues[namespace] = values
		}
	}

	// Suggestions are only for applications the user is likely to add. When a
	// site is supplied, use its configured apps; otherwise use discovered apps.
	var appKeys [][2]string
	if site != nil {
		for _, app := range site.Spec.Applications {
			appKeys = append(appKeys, [2]string{app.Application, app.Namespace})
		}
	}
	for _, app := range report.Applications {
		appKeys = append(appKeys, [2]string{app.Application, app.Namespace})
	}
	seen := map[string]bool{}
	for _, pair := range appKeys {
		application, namespace := pair[0], pair[1]
		key := application + "\x00" + namespace
		if seen[key] {
			continue
		}
		seen[key] = true
		if labelSuggestion := d.suggestPodLabel(ctx, application, namespace, report.Kubernetes.PodLabelValues[namespace]); labelSuggestion.Label != "" {
			report.Kubernetes.PodLabelByApp[key] = labelSuggestion
		}
		suggestion := d.suggestPodRegex(ctx, application, namespace)
		report.Kubernetes.PodRegexByApp[key] = suggestion
		if suggestion.MatchedPods > 0 {
			matcher := d.suggestContainerMatcher(ctx, namespace, suggestion.Regex)
			if matcher.Name != "" {
				report.Kubernetes.ContainerSuggestion[key] = matcher
			}
		}
	}

	containerMemory := d.metricStatusOf(ctx, "container_memory_working_set_bytes").Exists
	containerCPU := d.metricStatusOf(ctx, "container_cpu_usage_seconds_total").Exists
	for i := range report.Applications {
		app := &report.Applications[i]
		key := app.Application + "\x00" + app.Namespace
		availability := ProfileAvailability{Available: false}
		switch {
		case !containerMemory || !containerCPU:
			availability.Note = "cAdvisor 指标不完整"
		case report.Kubernetes.PodLabelByApp[key].MatchedPods > 0:
			availability.Available = true
			availability.Note = "kube_pod_labels 归属已验证"
		case report.Kubernetes.PodRegexByApp[key].MatchedPods > 0:
			availability.Available = true
			availability.Note = "Pod 正则归属已验证"
		default:
			availability.Note = "未建立 Pod 归属"
		}
		app.Profiles["kubernetes"] = availability
	}
	return nil
}

func (d *discoverer) suggestPodLabel(ctx context.Context, application, namespace string, values map[string][]string) PodLabelSuggestion {
	labels := make([]string, 0, len(values))
	for label := range values {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	for _, label := range labels {
		for _, value := range values[label] {
			if value != application {
				continue
			}
			selector := `namespace=` + metrics.QuoteLabelValue(namespace) + `, ` + label + `=` + metrics.QuoteLabelValue(value)
			pods, err := d.groupCount(ctx, "kube_pod_labels", []string{"pod"}, selector)
			if err != nil || len(pods) == 0 {
				continue
			}
			suggestion := PodLabelSuggestion{Label: label, Value: value, MatchedPods: len(pods)}
			for _, pod := range pods {
				if pod.Labels["pod"] != "" {
					suggestion.SamplePods = append(suggestion.SamplePods, pod.Labels["pod"])
				}
			}
			sort.Strings(suggestion.SamplePods)
			if len(suggestion.SamplePods) > 5 {
				suggestion.SamplePods = suggestion.SamplePods[:5]
			}
			return suggestion
		}
	}
	return PodLabelSuggestion{}
}

func (d *discoverer) suggestPodRegex(ctx context.Context, application, namespace string) PodRegexSuggestion {
	// Kubernetes deployment pods normally start with the workload name. Query
	// the suggestion rather than assuming it is correct.
	regex := "^" + regexp.QuoteMeta(application) + "-.*$"
	selector := `namespace=` + metrics.QuoteLabelValue(namespace) + `, pod=~` + metrics.QuoteLabelValue(regex)
	pods, err := d.groupCount(ctx, "container_cpu_usage_seconds_total", []string{"pod"}, selector)
	if err != nil {
		return PodRegexSuggestion{Regex: regex}
	}
	suggestion := PodRegexSuggestion{Regex: regex, MatchedPods: len(pods)}
	for _, pod := range pods {
		if pod.Labels["pod"] != "" {
			suggestion.SamplePods = append(suggestion.SamplePods, pod.Labels["pod"])
		}
	}
	sort.Strings(suggestion.SamplePods)
	if len(suggestion.SamplePods) > 5 {
		suggestion.SamplePods = suggestion.SamplePods[:5]
	}
	return suggestion
}

func (d *discoverer) suggestContainerMatcher(ctx context.Context, namespace, podRegex string) ContainerMatcherSuggestion {
	selector := `namespace=` + metrics.QuoteLabelValue(namespace) + `, pod=~` + metrics.QuoteLabelValue(podRegex)
	groups, err := d.groupCount(ctx, "container_memory_working_set_bytes", []string{"container", "image"}, selector)
	if err != nil || len(groups) == 0 {
		return ContainerMatcherSuggestion{}
	}
	suggestion := ContainerMatcherSuggestion{Name: "container", Operator: config.MatchNotEqual, Value: ""}
	for _, group := range groups {
		container := group.Labels["container"]
		image := group.Labels["image"]
		suggestion.Before += group.Count
		switch {
		case container == "" && image == "":
			suggestion.ParentOnly += group.Count
		case strings.Contains(strings.ToLower(image), "pause"):
			suggestion.PauseOnly += group.Count
		case container == "":
			suggestion.PauseOnly += group.Count
		default:
			suggestion.BusinessOnly += group.Count
			suggestion.After += group.Count
		}
	}
	suggestion.Note = fmt.Sprintf("container!=\"\" 保留业务容器 %d 条；父级 cgroup %d 条、Pause/空容器 %d 条被排除",
		suggestion.After, suggestion.ParentOnly, suggestion.PauseOnly)
	return suggestion
}

func (d *discoverer) discoverComponents(ctx context.Context, report *Report, profileFilter string) error {
	for _, id := range d.registry.Order {
		profile := d.registry.Profiles[id]
		if profile.Auto || profile.Target != config.TargetComponent {
			continue
		}
		if profileFilter != "" && profileFilter != id {
			continue
		}
		metric := profile.DiscoveryMetric()
		if metric == "" {
			continue
		}
		// Generic `up` profiles (Nacos/custom) would list every scrape target.
		// Only include them when the user explicitly asked for that profile.
		if metric == "up" && profileFilter == "" {
			continue
		}
		status := d.metricStatusOf(ctx, metric)
		if !status.Exists {
			continue
		}
		groups, err := d.groupCount(ctx, metric, profile.DiscoveryGroupBy(), "")
		if err != nil {
			report.Warnings = append(report.Warnings, fmt.Sprintf("%s 组件发现失败: %v", id, err))
			continue
		}
		componentType := ""
		if len(profile.ComponentTypes) > 0 {
			componentType = profile.ComponentTypes[0]
		}
		for _, group := range groups {
			if len(report.Components) >= d.maxCandidates {
				return nil
			}
			candidate := ComponentCandidate{
				Type:     componentType,
				Job:      group.Labels["job"],
				Instance: group.Labels["instance"],
				Metric:   metric,
				Count:    group.Count,
			}
			if candidate.Job == "" {
				continue
			}
			if d.site != nil && !siteHasComponent(d.site, candidate) {
				continue
			}
			candidate.Labels = d.componentLabels(ctx, metric, candidate)
			report.Components = append(report.Components, candidate)
		}
	}
	sort.Slice(report.Components, func(i, j int) bool {
		if report.Components[i].Type != report.Components[j].Type {
			return report.Components[i].Type < report.Components[j].Type
		}
		if report.Components[i].Job != report.Components[j].Job {
			return report.Components[i].Job < report.Components[j].Job
		}
		return report.Components[i].Instance < report.Components[j].Instance
	})
	return nil
}

func (d *discoverer) candidateLabels(ctx context.Context, metric string, candidate *AppCandidate) map[string][]string {
	selector := candidateSelector(candidate, []string{"application", "namespace"})
	names, err := d.labelNames(ctx, metric, selector)
	if err != nil {
		return nil
	}
	out := map[string][]string{}
	for _, name := range names {
		if name == "application" || name == "namespace" || name == "job" || name == "__name__" || isDynamicLabel(name) {
			continue
		}
		values, err := d.api.LabelValues(ctx, name, metric+"{"+selector+"}", d.now.Add(-d.lookback), d.now)
		if err != nil || len(values) == 0 {
			continue
		}
		sort.Strings(values)
		if len(values) > 5 {
			values = values[:5]
		}
		out[name] = values
	}
	return out
}

func isDynamicLabel(name string) bool {
	if strings.HasPrefix(name, "exported_") {
		return true
	}
	switch name {
	case "instance", "pod", "container", "id", "uri", "method", "status",
		"outcome", "error", "exception", "endpoint", "le", "quantile", "name", "image":
		return true
	default:
		return false
	}
}

func (d *discoverer) componentLabels(ctx context.Context, metric string, candidate ComponentCandidate) map[string][]string {
	selector := componentSelector(candidate)
	names, err := d.labelNames(ctx, metric, selector)
	if err != nil {
		return nil
	}
	out := map[string][]string{}
	for _, name := range names {
		switch name {
		case "__name__", "job", "instance":
			continue
		}
		if isDynamicLabel(name) {
			continue
		}
		values, err := d.api.LabelValues(ctx, name, metric+"{"+selector+"}", d.now.Add(-d.lookback), d.now)
		if err != nil || len(values) == 0 {
			continue
		}
		sort.Strings(values)
		if len(values) > 5 {
			values = values[:5]
		}
		out[name] = values
	}
	return out
}

func (d *discoverer) metricStatusOf(ctx context.Context, metric string) MetricStatus {
	if status, ok := d.metricStatus[metric]; ok {
		return status
	}
	status := MetricStatus{Name: metric}
	exists, err := d.api.MetricExists(ctx, metric, d.now.Add(-7*24*time.Hour), d.now)
	if err != nil {
		status.Error = err.Error()
		d.metricStatus[metric] = status
		return status
	}
	status.Exists = exists
	if exists {
		count, err := d.countFor(ctx, metric, "")
		if err == nil {
			status.SeriesCount = count
			status.HasCurrent = count > 0
		}
	}
	d.metricStatus[metric] = status
	return status
}

func (d *discoverer) labelNames(ctx context.Context, metric, selector string) ([]string, error) {
	match := metric
	if selector != "" {
		match += "{" + selector + "}"
	}
	cacheKey := match
	if names, ok := d.labelsCache[cacheKey]; ok {
		return names, nil
	}
	names, err := d.api.LabelNames(ctx, match, d.now.Add(-d.lookback), d.now)
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	d.labelsCache[cacheKey] = names
	return names, nil
}

func (d *discoverer) countFor(ctx context.Context, metric, selector string) (int, error) {
	query := "count(" + metric
	if selector != "" {
		query += "{" + selector + "}"
	}
	query += ")"
	value, err := d.api.QueryAt(ctx, query, d.now)
	if err != nil {
		return 0, err
	}
	vector, ok := value.(model.Vector)
	if !ok {
		return 0, nil
	}
	total := 0
	for _, sample := range vector {
		total += int(sample.Value)
	}
	return total, nil
}

type groupResult struct {
	Labels map[string]string
	Count  int
}

func (d *discoverer) groupCount(ctx context.Context, metric string, groupBy []string, extraSelector string) ([]groupResult, error) {
	if len(groupBy) == 0 {
		return nil, fmt.Errorf("groupBy 不能为空")
	}
	query := "count by (" + strings.Join(groupBy, ", ") + ") (" + metric
	if extraSelector != "" {
		query += "{" + extraSelector + "}"
	}
	query += ")"
	value, err := d.api.QueryAt(ctx, query, d.now)
	if err != nil {
		return nil, err
	}
	vector, ok := value.(model.Vector)
	if !ok {
		return nil, nil
	}
	out := make([]groupResult, 0, len(vector))
	for _, sample := range vector {
		labels := map[string]string{}
		for name, value := range sample.Metric {
			labels[string(name)] = string(value)
		}
		out = append(out, groupResult{Labels: labels, Count: int(sample.Value)})
	}
	sort.Slice(out, func(i, j int) bool {
		return groupSortKey(out[i].Labels, groupBy) < groupSortKey(out[j].Labels, groupBy)
	})
	if len(out) > d.maxCandidates {
		out = out[:d.maxCandidates]
	}
	return out, nil
}

func (d *discoverer) sortedMetricStatuses() []MetricStatus {
	out := make([]MetricStatus, 0, len(d.metricStatus))
	for _, status := range d.metricStatus {
		out = append(out, status)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func candidateSelector(candidate *AppCandidate, names []string) string {
	values := map[string]string{
		"application": candidate.Application,
		"namespace":   candidate.Namespace,
	}
	if len(candidate.Jobs) == 1 {
		values["job"] = candidate.Jobs[0]
	}
	var parts []string
	for _, name := range names {
		if value := values[name]; value != "" {
			parts = append(parts, name+"="+metrics.QuoteLabelValue(value))
		}
	}
	return strings.Join(parts, ", ")
}

func componentSelector(candidate ComponentCandidate) string {
	parts := []string{"job=" + metrics.QuoteLabelValue(candidate.Job)}
	if candidate.Instance != "" {
		parts = append(parts, "instance="+metrics.QuoteLabelValue(candidate.Instance))
	}
	return strings.Join(parts, ", ")
}

func groupSortKey(labels map[string]string, groupBy []string) string {
	var parts []string
	for _, name := range groupBy {
		parts = append(parts, labels[name])
	}
	return strings.Join(parts, "\x00")
}

func sortedMetricKeys(required map[string][]string) []string {
	keys := make([]string, 0, len(required))
	for key := range required {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func flattenValueMap(values map[string]map[string][]string) map[string][]string {
	out := map[string][]string{}
	for label, labels := range values {
		for value := range labels {
			out[label] = append(out[label], shortenImageValue(label, value))
		}
		sort.Strings(out[label])
		if len(out[label]) > 5 {
			out[label] = append(out[label][:5], "...")
		}
	}
	return out
}

func shortenImageValue(label, value string) string {
	if label != "image" {
		return value
	}
	if index := strings.Index(value, "@sha256:"); index > 0 {
		return value[:index]
	}
	return value
}

func siteHasApp(site *config.Site, application, namespace string) bool {
	for _, app := range site.Spec.Applications {
		if app.Application == application && app.Namespace == namespace {
			return true
		}
	}
	return false
}

func siteHasDomain(site *config.Site, instance string) bool {
	for _, domain := range site.Spec.Domains {
		if domain.Blackbox.Instance == instance {
			return true
		}
	}
	return false
}

func siteHasComponent(site *config.Site, candidate ComponentCandidate) bool {
	for _, component := range site.Spec.Components {
		if component.Type != candidate.Type {
			continue
		}
		if component.Prometheus.Job != candidate.Job {
			continue
		}
		if candidate.Instance == "" || component.Prometheus.Instance == candidate.Instance {
			return true
		}
	}
	return false
}

func containsString(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}
