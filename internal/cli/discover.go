package cli

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"site-observability/internal/config"
	"site-observability/internal/discovery"
	"site-observability/internal/metrics"
)

func (a *app) discoverCommand(opts *rootOptions) *cobra.Command {
	var sitePath, profile, promURL, timeoutRaw string
	var lookback time.Duration
	cmd := &cobra.Command{
		Use:   "discover",
		Short: "只读发现 Prometheus 中的指标、应用、域名、Pod 和组件候选",
		Long: "discover 只查询 Prometheus 并展示真实候选，不修改站点 YAML、不修改 Prometheus/Kubernetes/Grafana，" +
			"也不生成 Dashboard。输出中的指标存在性、标签值、Pod 数量和容器标签均来自实际查询。",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := metrics.LoadDefault()
			if err != nil {
				return err
			}
			tool, err := a.loadToolConfig(opts.configPath, true)
			if err != nil {
				return err
			}
			promCfg, available, err := resolvePromConfig(promURL, timeoutRaw, tool)
			if err != nil {
				return err
			}
			if !available {
				return fmt.Errorf("discover 需要 Prometheus URL；请设置 --prometheus-url 或 SITEGEN_PROMETHEUS_URL")
			}
			client, err := discovery.NewClient(promCfg)
			if err != nil {
				return err
			}
			var site *config.Site
			if sitePath != "" {
				site, err = config.Load(sitePath)
				if err != nil {
					return err
				}
				issues := config.Validate(site, reg.Resolve)
				if issues.HasErrors() {
					return fmt.Errorf("站点配置校验失败: %s", strings.Join(issues.ErrorStrings(), "; "))
				}
			}
			if lookback <= 0 {
				lookback = time.Hour
			}
			report, err := discovery.Discover(cmd.Context(), client, reg, discovery.DiscoverOptions{
				Lookback:      lookback,
				ProfileFilter: profile,
				Site:          site,
				Now:           a.now,
			})
			if err != nil {
				return err
			}
			a.printDiscoveryReport(report, sitePath)
			return nil
		},
	}
	cmd.Flags().StringVar(&sitePath, "site", "", "可选：站点 YAML，用于限定应用和组件候选")
	cmd.Flags().StringVar(&profile, "profile", "", "只发现指定 Profile（如 http、jvm、kubernetes、mysql-basic）")
	cmd.Flags().StringVar(&promURL, "prometheus-url", "", "Prometheus URL（覆盖环境变量和配置文件）")
	cmd.Flags().StringVar(&timeoutRaw, "timeout", "", "Prometheus 请求超时（如 15s）")
	cmd.Flags().DurationVar(&lookback, "lookback", time.Hour, "序列与标签验证时间窗口")
	return cmd
}

func (a *app) printDiscoveryReport(report *discovery.Report, sitePath string) {
	fmt.Fprintln(a.out, "Prometheus 发现报告")
	fmt.Fprintln(a.out, "==================")
	fmt.Fprintf(a.out, "连接状态: %s\n", passOrFail(report.Connected))
	if sitePath != "" {
		fmt.Fprintf(a.out, "限定站点: %s\n", sitePath)
	}
	fmt.Fprintf(a.out, "Lookback: %s\n", formatDuration(report.Lookback))
	fmt.Fprintf(a.out, "生成时间: %s\n", report.GeneratedAt.Format(time.RFC3339))

	if len(report.Applications) > 0 {
		fmt.Fprintln(a.out, "\n应用候选（HTTP / JVM）")
		fmt.Fprintln(a.out, "------------------------")
		for i, app := range report.Applications {
			fmt.Fprintf(a.out, "%d. %s / %s\n", i+1, app.Application, app.Namespace)
			fmt.Fprintf(a.out, "   Job: %s\n", strings.Join(app.Jobs, ", "))
			if len(app.Jobs) > 1 {
				fmt.Fprintln(a.out, "   注意: 同一应用存在多个 Job，创建时必须确认")
			}
			for _, profileID := range sortedProfileIDs(app.Profiles) {
				availability := app.Profiles[profileID]
				if profileID == "kubernetes" {
					fmt.Fprintf(a.out, "   %s: %s（%s）\n", profileID, availableLabel(availability.Available), availability.Note)
				} else {
					fmt.Fprintf(a.out, "   %s: %s（series=%d）\n", profileID, availableLabel(availability.Available), availability.MetricCount)
				}
				if len(availability.MissingMetrics) > 0 {
					fmt.Fprintf(a.out, "     缺失指标: %s\n", strings.Join(availability.MissingMetrics, ", "))
				}
				if len(availability.Labels) > 0 {
					fmt.Fprintf(a.out, "     发现标签: %s\n", strings.Join(availability.Labels, ", "))
				}
				if len(availability.MissingLabels) > 0 {
					fmt.Fprintf(a.out, "     缺失标签: %s\n", strings.Join(availability.MissingLabels, ", "))
				}
				if availability.Note != "" && availability.Note != "指标完整" {
					fmt.Fprintf(a.out, "     说明: %s\n", availability.Note)
				}
			}
			for _, label := range sortedLabelValues(app.ExtraLabels) {
				fmt.Fprintf(a.out, "   标签 %s: %s\n", label, strings.Join(app.ExtraLabels[label], ", "))
			}
		}
	}

	if len(report.Domains) > 0 {
		fmt.Fprintln(a.out, "\nBlackbox 域名候选")
		fmt.Fprintln(a.out, "------------------")
		for i, domain := range report.Domains {
			fmt.Fprintf(a.out, "%d. %s\n", i+1, domain.Instance)
			fmt.Fprintf(a.out, "   Job: %s；当前序列: %d；探测耗时指标: %s\n",
				domain.Job, domain.Count, passOrFail(domain.Duration))
		}
	}

	if len(report.Kubernetes.Metrics) > 0 {
		fmt.Fprintln(a.out, "\nKubernetes")
		fmt.Fprintln(a.out, "----------")
		for _, status := range report.Kubernetes.Metrics {
			fmt.Fprintf(a.out, "  %s: %s（当前序列=%d）\n", status.Name, metricState(status), status.SeriesCount)
		}
		if len(report.Kubernetes.Namespaces) > 0 {
			fmt.Fprintln(a.out, "  Namespace 与 Pod:")
			for _, namespace := range report.Kubernetes.Namespaces {
				pods := report.Kubernetes.PodsByNamespace[namespace]
				fmt.Fprintf(a.out, "    %s: %d 个 Pod", namespace, len(pods))
				if len(pods) > 0 {
					sample := pods
					if len(sample) > 5 {
						sample = sample[:5]
					}
					fmt.Fprintf(a.out, "（样例: %s）", strings.Join(sample, ", "))
				}
				fmt.Fprintln(a.out)
				if labels := report.Kubernetes.ContainerLabels[namespace]; len(labels) > 0 {
					for _, label := range sortedLabelValues(labels) {
						fmt.Fprintf(a.out, "      %s: %s\n", label, strings.Join(labels[label], ", "))
					}
				}
			}
		}
		if len(report.Kubernetes.PodLabelByApp) > 0 {
			fmt.Fprintln(a.out, "  Pod 标签归属建议（kube_pod_labels 已查询验证）:")
			for _, key := range sortedValueKeysSuggestion(report.Kubernetes.PodLabelByApp) {
				suggestion := report.Kubernetes.PodLabelByApp[key]
				app, namespace := splitAppKey(key)
				fmt.Fprintf(a.out, "    %s / %s: %s=%q -> %d 个 Pod", app, namespace, suggestion.Label, suggestion.Value, suggestion.MatchedPods)
				if len(suggestion.SamplePods) > 0 {
					fmt.Fprintf(a.out, "（样例: %s）", strings.Join(suggestion.SamplePods, ", "))
				}
				fmt.Fprintln(a.out)
			}
		} else {
			fmt.Fprintln(a.out, "  kube_pod_labels: 不存在或没有匹配的标签值；请使用经过验证的 Pod 正则")
		}
		if len(report.Kubernetes.PodRegexByApp) > 0 {
			fmt.Fprintln(a.out, "  Pod 正则建议（已查询验证）:")
			for _, key := range sortedValueKeysSuggestion(report.Kubernetes.PodRegexByApp) {
				suggestion := report.Kubernetes.PodRegexByApp[key]
				app, namespace := splitAppKey(key)
				fmt.Fprintf(a.out, "    %s / %s: %s -> %d 个 Pod", app, namespace, suggestion.Regex, suggestion.MatchedPods)
				if len(suggestion.SamplePods) > 0 {
					fmt.Fprintf(a.out, "（样例: %s）", strings.Join(suggestion.SamplePods, ", "))
				}
				fmt.Fprintln(a.out)
			}
		}
		if len(report.Kubernetes.ContainerSuggestion) > 0 {
			fmt.Fprintln(a.out, "  Container Matcher 建议（已用真实容器标签验证）:")
			for _, key := range sortedValueKeysSuggestion(report.Kubernetes.ContainerSuggestion) {
				suggestion := report.Kubernetes.ContainerSuggestion[key]
				app, namespace := splitAppKey(key)
				fmt.Fprintf(a.out, "    %s / %s: %s %s %q；%s\n",
					app, namespace, suggestion.Name, suggestion.Operator, suggestion.Value, suggestion.Note)
			}
		}
	}

	if len(report.Components) > 0 {
		fmt.Fprintln(a.out, "\n组件候选")
		fmt.Fprintln(a.out, "----------")
		for i, component := range report.Components {
			fmt.Fprintf(a.out, "%d. [%s] job=%s instance=%s metric=%s series=%d\n",
				i+1, component.Type, component.Job, component.Instance, component.Metric, component.Count)
			for _, label := range sortedLabelValues(component.Labels) {
				fmt.Fprintf(a.out, "   %s: %s\n", label, strings.Join(component.Labels[label], ", "))
			}
		}
	}

	if len(report.Warnings) > 0 {
		fmt.Fprintln(a.out, "\n警告")
		fmt.Fprintln(a.out, "----")
		for _, warning := range report.Warnings {
			fmt.Fprintf(a.out, "- %s\n", warning)
		}
	}
	fmt.Fprintln(a.out, "\n提示: discover 只读；创建配置仍需用户确认最终 Selector 和依赖关系。")
}

func availableLabel(available bool) string {
	if available {
		return "可用"
	}
	return "不完整"
}

func metricState(status discovery.MetricStatus) string {
	switch {
	case status.Error != "":
		return "查询失败: " + status.Error
	case !status.Exists:
		return "不存在"
	case status.HasCurrent:
		return "存在（当前有数据）"
	default:
		return "历史存在，当前无数据"
	}
}

func passOrFail(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}

func formatDuration(value time.Duration) string {
	if value <= 0 {
		return "1h"
	}
	if value%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(value/time.Hour))
	}
	if value < time.Hour {
		return fmt.Sprintf("%dm", int(value/time.Minute))
	}
	return value.String()
}

func sortedProfileIDs(profiles map[string]discovery.ProfileAvailability) []string {
	keys := make([]string, 0, len(profiles))
	for key := range profiles {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedLabelValues(values map[string][]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedValueKeysSuggestion[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func splitAppKey(key string) (string, string) {
	parts := strings.SplitN(key, "\x00", 2)
	if len(parts) != 2 {
		return key, ""
	}
	return parts[0], parts[1]
}
