package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"site-observability/internal/config"
	"site-observability/internal/discovery"
)

var errUseManual = errors.New("切换到手动配置")

// autoCreate builds a site from real discovery results. It only asks the user
// to choose/confirm candidates; every metric, job, instance, pod and container
// value originates from the discovery report.
func (a *app) autoCreate(wizard *Wizard, client *discovery.Client) (*config.Site, error) {
	site := &config.Site{APIVersion: config.APIVersion, Kind: config.KindSite}
	if err := wizard.promptMetadata(site, site); err != nil {
		return nil, err
	}

	var report *discovery.Report
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		var err error
		report, err = discovery.Discover(ctx, client, wizard.Registry, discovery.DiscoverOptions{
			Lookback: time.Hour,
			Now:      a.now,
		})
		cancel()
		if err == nil {
			break
		}
		fmt.Fprintf(wizard.Out, "\nPrometheus 发现失败: %v\n", err)
		choice, selectErr := wizard.Prompter.Select("发现失败", []string{"重试", "切换手动配置", "退出"}, 0)
		if selectErr != nil {
			return nil, selectErr
		}
		switch choice {
		case 0:
			continue
		case 1:
			return nil, errUseManual
		default:
			return nil, err
		}
	}

	var confirmItems []string

	// Domains -----------------------------------------------------------------
	domainFilter, err := wizard.Prompter.Input("域名 URL 过滤（可留空）", "", nil)
	if err != nil {
		return nil, err
	}
	domains := filterDomains(report.Domains, domainFilter)
	if len(domains) == 0 {
		fmt.Fprintln(wizard.Out, "未发现可用的 Blackbox HTTP 域名，改为手动添加域名。")
		if err := wizard.promptDomains(site); err != nil {
			return nil, err
		}
	} else {
		instances := make([]string, 0, len(domains))
		labels := make([]string, 0, len(domains))
		defaults := make([]bool, len(domains))
		allDefault := len(domains) <= 20
		for i, domain := range domains {
			instances = append(instances, domain.Instance)
			labels = append(labels, fmt.Sprintf("%s（job=%s）", domain.Instance, domain.Job))
			defaults[i] = allDefault
		}
		selected, err := wizard.Prompter.MultiSelect("选择要纳入站点的域名（可多选）", labels, defaults)
		if err != nil {
			return nil, err
		}
		for _, index := range selected {
			domain := domains[index]
			site.Spec.Domains = append(site.Spec.Domains, config.Domain{
				ID:  uniqueID(site, domainID(domain.Instance)),
				URL: domain.Instance,
				Blackbox: config.Blackbox{
					Job:      domain.Job,
					Instance: domain.Instance,
				},
			})
		}
		if len(site.Spec.Domains) == 0 {
			return nil, fmt.Errorf("至少需要选择一个域名；如需手动录入，请使用手动配置")
		}
	}

	// Applications ------------------------------------------------------------
	appFilter, err := wizard.Prompter.Input("应用名称 / Namespace 过滤（可留空）", "", nil)
	if err != nil {
		return nil, err
	}
	apps := filterApplications(report.Applications, appFilter)
	if len(apps) == 0 {
		return nil, fmt.Errorf("未从真实指标中发现 HTTP/JVM 应用；请使用手动配置")
	}
	appLabels := make([]string, 0, len(apps))
	appDefaults := make([]bool, len(apps))
	appDefaultAll := len(apps) <= 20
	for i, app := range apps {
		profiles := availableProfiles(app)
		appLabels = append(appLabels, fmt.Sprintf("%s / %s（Jobs: %s；Profiles: %s）",
			app.Application, app.Namespace, strings.Join(app.Jobs, ", "), strings.Join(profiles, ", ")))
		appDefaults[i] = appDefaultAll && len(profiles) > 0
	}
	selectedApps, err := wizard.Prompter.MultiSelect("选择应用（可多选，Pod 归属会使用已验证规则）", appLabels, appDefaults)
	if err != nil {
		return nil, err
	}
	if len(selectedApps) == 0 {
		return nil, fmt.Errorf("至少需要选择一个应用；如需手动录入，请使用手动配置")
	}

	kubernetesLabelCount, kubernetesRegexCount := 0, 0
	for _, index := range selectedApps {
		candidate := apps[index]
		job := ""
		if len(candidate.Jobs) == 1 {
			job = candidate.Jobs[0]
		} else {
			selectedJob, err := wizard.Prompter.Select(
				fmt.Sprintf("应用 %s / %s 存在多个 Job，请选择", candidate.Application, candidate.Namespace),
				candidate.Jobs, 0)
			if err != nil {
				return nil, err
			}
			job = candidate.Jobs[selectedJob]
			confirmItems = append(confirmItems,
				fmt.Sprintf("%s / %s 使用 Job %s（存在多个候选）", candidate.Application, candidate.Namespace, job))
		}
		enabled := availableProfiles(candidate)
		app := config.Application{
			ID:          uniqueID(site, slugify(candidate.Application+"-"+candidate.Namespace)),
			Application: candidate.Application,
			Namespace:   candidate.Namespace,
			Prometheus: config.Prometheus{
				Job: job,
				Labels: map[string]string{
					"application": candidate.Application,
					"namespace":   candidate.Namespace,
				},
			},
			Skywalking: config.Skywalking{
				Service:        config.DefaultSkywalkingService(candidate.Application, candidate.Namespace),
				EndpointFormat: config.DefaultEndpointFormat,
			},
			Metrics: enabled,
		}
		hasKubernetes := contains(enabled, "kubernetes")
		if hasKubernetes {
			key := candidate.Application + "\x00" + candidate.Namespace
			if suggestion, ok := report.Kubernetes.PodLabelByApp[key]; ok && suggestion.MatchedPods > 0 {
				app.Kubernetes.PodSelector = &config.PodSelector{
					Mode:     config.PodSelectorLabels,
					Metric:   config.DefaultPodLabelsMetric,
					Matchers: map[string]string{suggestion.Label: suggestion.Value},
				}
				kubernetesLabelCount++
			} else if suggestion, ok := report.Kubernetes.PodRegexByApp[key]; ok && suggestion.MatchedPods > 0 {
				app.Kubernetes.PodSelector = &config.PodSelector{
					Mode: config.PodSelectorPodRegex,
					Pod:  suggestion.Regex,
				}
				kubernetesRegexCount++
			} else {
				app.Metrics = removeString(app.Metrics, "kubernetes")
				confirmItems = append(confirmItems,
					fmt.Sprintf("%s / %s 未建立 Pod 归属，已暂不启用 kubernetes Profile", candidate.Application, candidate.Namespace))
			}
			if app.Kubernetes.PodSelector != nil {
				if suggestion, ok := report.Kubernetes.ContainerSuggestion[key]; ok && suggestion.Name != "" && suggestion.After > 0 {
					app.Kubernetes.ContainerMatchers = config.ContainerMatchers{{
						Name:     suggestion.Name,
						Operator: suggestion.Operator,
						Value:    suggestion.Value,
					}}
				} else {
					confirmItems = append(confirmItems,
						fmt.Sprintf("%s / %s 未自动配置 Container Matcher；请用 discover 核对容器标签", candidate.Application, candidate.Namespace))
				}
			}
		}
		site.Spec.Applications = append(site.Spec.Applications, app)
	}

	// Components --------------------------------------------------------------
	if len(report.Components) > 0 {
		componentLabels := make([]string, 0, len(report.Components))
		componentDefaults := make([]bool, len(report.Components))
		for i, component := range report.Components {
			componentLabels = append(componentLabels, fmt.Sprintf("[%s] job=%s instance=%s（%d 条序列）",
				component.Type, component.Job, component.Instance, component.Count))
			componentDefaults[i] = true
		}
		selectedComponents, err := wizard.Prompter.MultiSelect("选择依赖组件（可多选）", componentLabels, componentDefaults)
		if err != nil {
			return nil, err
		}
		for _, index := range selectedComponents {
			candidate := report.Components[index]
			if candidate.Instance == "" {
				confirmItems = append(confirmItems,
					fmt.Sprintf("组件 [%s] job=%s 未发现 Instance，已跳过；请在 edit 中补充后启用", candidate.Type, candidate.Job))
				continue
			}
			profiles := wizard.Registry.ProfilesForComponent(candidate.Type)
			profileIDs := make([]string, 0, len(profiles))
			for _, profile := range profiles {
				profileIDs = append(profileIDs, profile.ID)
			}
			name := componentDisplayName(candidate.Type) + " " + componentDisplayValue(candidate)
			site.Spec.Components = append(site.Spec.Components, config.Component{
				ID:   uniqueID(site, slugify(candidate.Type+"-"+componentDisplayValue(candidate))),
				Name: name,
				Type: candidate.Type,
				Prometheus: config.ComponentPrometheus{
					Job:      candidate.Job,
					Instance: candidate.Instance,
				},
				Metrics: profileIDs,
			})
		}
	}

	// Entry applications ------------------------------------------------------
	config.Normalize(site)
	if err := wizard.promptEntryApplications(site); err != nil {
		return nil, err
	}

	// Summary -----------------------------------------------------------------
	fmt.Fprintln(wizard.Out, "\n自动发现摘要")
	fmt.Fprintln(wizard.Out, "--------------")
	fmt.Fprintf(wizard.Out, "域名: %d；应用: %d；组件: %d\n",
		len(site.Spec.Domains), len(site.Spec.Applications), len(site.Spec.Components))
	fmt.Fprintf(wizard.Out, "Kubernetes Pod 归属: 标签模式 %d 个，正则模式 %d 个\n", kubernetesLabelCount, kubernetesRegexCount)
	if len(confirmItems) > 0 {
		fmt.Fprintln(wizard.Out, "需要人工确认:")
		for _, item := range confirmItems {
			fmt.Fprintf(wizard.Out, "- %s\n", item)
		}
	} else {
		fmt.Fprintln(wizard.Out, "需要人工确认: 无")
	}

	if wizard.SitesDir != "" {
		wizard.saveDraft(site, "preview")
	}
	return wizard.finish(site)
}

func filterDomains(domains []discovery.DomainCandidate, filter string) []discovery.DomainCandidate {
	filter = strings.ToLower(strings.TrimSpace(filter))
	out := make([]discovery.DomainCandidate, 0, len(domains))
	for _, domain := range domains {
		if !strings.HasPrefix(domain.Instance, "http://") && !strings.HasPrefix(domain.Instance, "https://") {
			continue
		}
		if filter != "" && !strings.Contains(strings.ToLower(domain.Instance), filter) {
			continue
		}
		out = append(out, domain)
	}
	return out
}

func filterApplications(apps []discovery.AppCandidate, filter string) []discovery.AppCandidate {
	filter = strings.ToLower(strings.TrimSpace(filter))
	out := make([]discovery.AppCandidate, 0, len(apps))
	for _, app := range apps {
		if filter != "" &&
			!strings.Contains(strings.ToLower(app.Application), filter) &&
			!strings.Contains(strings.ToLower(app.Namespace), filter) {
			continue
		}
		out = append(out, app)
	}
	return out
}

func availableProfiles(app discovery.AppCandidate) []string {
	var out []string
	for profileID, availability := range app.Profiles {
		if availability.Available {
			out = append(out, profileID)
		}
	}
	sort.Strings(out)
	return out
}

func componentDisplayValue(component discovery.ComponentCandidate) string {
	if component.Instance != "" {
		value := component.Instance
		value = strings.TrimPrefix(value, "redis://")
		value = strings.TrimPrefix(value, "mysql://")
		return value
	}
	return component.Job
}

func removeString(values []string, target string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value != target {
			out = append(out, value)
		}
	}
	return out
}

// bestEffortClient builds an optional Prometheus client for discovery.
func (a *app) bestEffortClient(configPath, promURL, timeoutRaw string) *discovery.Client {
	tool, _, err := config.LoadToolConfig(configPath)
	if err != nil {
		return nil
	}
	promCfg, available, err := resolvePromConfig(promURL, timeoutRaw, tool)
	if err != nil || !available {
		return nil
	}
	client, err := discovery.NewClient(promCfg)
	if err != nil {
		fmt.Fprintf(a.errOut, "提示: 无法创建发现用 Prometheus 客户端: %v\n", err)
		return nil
	}
	return client
}
