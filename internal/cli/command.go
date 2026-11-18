package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"site-observability/internal/config"
	"site-observability/internal/dashboard"
	"site-observability/internal/discovery"
	"site-observability/internal/metrics"
	"site-observability/internal/output"
)

type app struct {
	in      io.Reader
	out     io.Writer
	errOut  io.Writer
	version string
	now     func() time.Time
}

type rootOptions struct {
	configPath    string
	sitesDir      string
	dashboardsDir string
	reportsDir    string
	verbose       bool
}

type siteAction struct {
	validateOnly bool
	generate     bool
	checkProm    bool
	requireProm  bool
	skipProm     bool
	skipReason   string
	// deferEnvironmentCheck prints the "dashboard generated, run check next"
	// notice for a non-strict build.
	deferEnvironmentCheck bool
	dashboardDir          string
	reportDir             string
	links                 config.Links
	datasources           config.DatasourcesConfig
	skywalkingClient      *discovery.Client
	lookback              time.Duration
	concurrency           int
}

// Execute runs the CLI and returns a process exit code.
func Execute(args []string, in io.Reader, out, errOut io.Writer, version string) int {
	a := &app{in: in, out: out, errOut: errOut, version: version, now: time.Now}
	root := a.rootCommand()
	root.SetIn(in)
	root.SetOut(out)
	root.SetErr(errOut)
	root.SetArgs(args)
	root.SilenceUsage = true
	root.SilenceErrors = true

	if err := root.ExecuteContext(context.Background()); err != nil {
		if errors.Is(err, ErrCancelled) {
			fmt.Fprintln(out, "已取消，未写入文件。")
			return 0
		}
		if errors.Is(err, ErrDraftSaved) {
			return 0
		}
		fmt.Fprintf(errOut, "错误: %v\n", err)
		return 1
	}
	return 0
}

func (a *app) rootCommand() *cobra.Command {
	opts := &rootOptions{}
	root := &cobra.Command{
		Use:   "sitegen",
		Short: "站点全链路 Grafana Dashboard 生成器",
		Long:  "sitegen 通过站点 YAML 组织应用、依赖组件与指标模板，生成可重复构建的 Grafana Dashboard JSON。",
		CompletionOptions: cobra.CompletionOptions{
			DisableDefaultCmd: true,
		},
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			level := slog.LevelWarn
			if opts.verbose {
				level = slog.LevelDebug
			}
			slog.SetDefault(slog.New(slog.NewTextHandler(a.errOut, &slog.HandlerOptions{Level: level})))
			return nil
		},
	}
	root.PersistentFlags().StringVar(&opts.configPath, "config", "", "工具配置文件路径（默认发现 ./sitegen.yaml 或 ~/.sitegen/config.yaml）")
	root.PersistentFlags().StringVar(&opts.sitesDir, "sites-dir", "sites", "站点 YAML 目录")
	root.PersistentFlags().StringVar(&opts.dashboardsDir, "dashboards-dir", "dashboards", "Dashboard 输出目录")
	root.PersistentFlags().StringVar(&opts.reportsDir, "reports-dir", "reports", "校验报告输出目录")
	root.PersistentFlags().BoolVar(&opts.verbose, "verbose", false, "输出调试日志")

	root.AddCommand(a.initCommand(opts))
	root.AddCommand(a.createCommand(opts))
	root.AddCommand(a.editCommand(opts))
	root.AddCommand(a.validateCommand(opts))
	root.AddCommand(a.discoverCommand(opts))
	root.AddCommand(a.checkCommand(opts))
	root.AddCommand(a.generateCommand(opts))
	root.AddCommand(a.buildCommand(opts))
	root.AddCommand(a.listCommand(opts))
	root.AddCommand(a.versionCommand(opts))
	return root
}

func (a *app) initCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "初始化项目目录和工具配置",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dirs := []string{opts.sitesDir, opts.dashboardsDir, opts.reportsDir}
			for _, dir := range dirs {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					return fmt.Errorf("创建目录 %s: %w", dir, err)
				}
				fmt.Fprintf(a.out, "目录已就绪: %s\n", dir)
			}
			configPath := opts.configPath
			if configPath == "" {
				configPath = "sitegen.yaml"
			}
			if config.Exists(configPath) {
				fmt.Fprintf(a.out, "配置文件已存在，未覆盖: %s\n", configPath)
			} else {
				sample := `# sitegen 工具配置。凭据请通过环境变量提供，不要写入本文件。
prometheus:
  url: ""
  timeout: 15s
  tlsCAFile: ""
  tlsInsecureSkipVerify: false

links:
  skywalking: ""
  prometheus: ""
  grafana: ""
  skywalkingServiceURLTemplate: ""
  skywalkingEndpointURLTemplate: ""
  componentDashboards: {}
`
				if err := output.WriteAtomic(configPath, []byte(sample), 0o644, false); err != nil {
					return fmt.Errorf("写入工具配置: %w", err)
				}
				fmt.Fprintf(a.out, "已创建工具配置: %s\n", configPath)
			}
			fmt.Fprintln(a.out, "\n下一步: sitegen create")
			return nil
		},
	}
}

func (a *app) createCommand(opts *rootOptions) *cobra.Command {
	var outputPath, promURL, timeoutRaw string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "交互式创建站点配置（支持草稿恢复和步骤回退）",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := metrics.LoadDefault()
			if err != nil {
				return err
			}
			prompter := NewTerminalPrompter(a.in, a.out)
			resume, oldDraft, err := a.chooseDraft(prompter, opts.sitesDir)
			if err != nil {
				return err
			}
			client := a.bestEffortClient(opts.configPath, promURL, timeoutRaw)
			probe := ClusterProbe(nil)
			if client != nil {
				probe = client
			}
			wizard := &Wizard{
				Prompter: prompter,
				Registry: reg,
				Out:      a.out,
				Probe:    probe,
				SitesDir: opts.sitesDir,
				Resume:   resume,
			}
			var site *config.Site
			if resume != nil {
				site, err = wizard.Create()
			} else if client != nil {
				mode, selectErr := prompter.Select("站点配置创建", []string{
					"自动发现并创建（推荐）",
					"手动配置（连接 Prometheus 辅助校验）",
					"离线创建（不连接 Prometheus）",
				}, 0)
				if selectErr != nil {
					return selectErr
				}
				switch mode {
				case 0:
					site, err = a.autoCreate(wizard, client)
					if errors.Is(err, errUseManual) {
						site, err = wizard.Create()
					}
				case 1:
					site, err = wizard.Create()
				default:
					wizard.Probe = nil
					site, err = wizard.Create()
				}
			} else {
				fmt.Fprintln(a.out, "未配置 Prometheus，进入手动配置。")
				wizard.Probe = nil
				site, err = wizard.Create()
			}
			if err != nil {
				return err
			}
			if outputPath == "" {
				outputPath = filepath.Join(opts.sitesDir, site.Metadata.ID+".yaml")
			}
			if config.Exists(outputPath) {
				overwrite, err := prompter.Confirm("目标文件已存在，是否覆盖（保留 .bak 备份）?", false)
				if err != nil {
					return err
				}
				if !overwrite {
					return ErrCancelled
				}
			}
			if err := config.Write(outputPath, site, true); err != nil {
				return err
			}
			_ = DeleteDraft(oldDraft)
			_ = DeleteDraft(DraftPath(opts.sitesDir, site.Metadata.ID))
			fmt.Fprintf(a.out, "\n已写入站点配置: %s\n", outputPath)
			fmt.Fprintf(a.out, "下一步: sitegen build --site %s\n", outputPath)
			fmt.Fprintf(a.out, "环境验收: sitegen check --site %s\n", outputPath)
			return nil
		},
	}
	cmd.Flags().StringVarP(&outputPath, "output", "o", "", "输出文件路径（默认 sites/<site-id>.yaml）")
	cmd.Flags().StringVar(&promURL, "prometheus-url", "", "用于向导中试查询 Pod/容器标签的 Prometheus URL")
	cmd.Flags().StringVar(&timeoutRaw, "timeout", "", "Prometheus 请求超时（如 15s）")
	return cmd
}

func (a *app) editCommand(opts *rootOptions) *cobra.Command {
	var sitePath, promURL, timeoutRaw string
	cmd := &cobra.Command{
		Use:   "edit",
		Short: "按分区修改已有站点配置",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if sitePath == "" {
				return fmt.Errorf("必须指定 --site")
			}
			site, err := config.Load(sitePath)
			if err != nil {
				return err
			}
			reg, err := metrics.LoadDefault()
			if err != nil {
				return err
			}
			prompter := NewTerminalPrompter(a.in, a.out)
			wizard := &Wizard{
				Prompter: prompter,
				Registry: reg,
				Out:      a.out,
				Probe:    a.bestEffortProbe(opts.configPath, promURL, timeoutRaw),
			}
			edited, err := wizard.Edit(site)
			if err != nil {
				if errors.Is(err, ErrViewOnly) {
					fmt.Fprintln(a.out, "仅查看配置，未修改文件。")
					return nil
				}
				return err
			}
			if reflect.DeepEqual(site, edited) {
				fmt.Fprintln(a.out, "未检测到变更，未写入文件。")
				return nil
			}
			if err := config.Write(sitePath, edited, true); err != nil {
				return err
			}
			fmt.Fprintf(a.out, "\n已更新站点配置: %s\n", sitePath)
			return nil
		},
	}
	cmd.Flags().StringVar(&sitePath, "site", "", "站点 YAML 文件")
	cmd.Flags().StringVar(&promURL, "prometheus-url", "", "用于向导中试查询 Pod/容器标签的 Prometheus URL")
	cmd.Flags().StringVar(&timeoutRaw, "timeout", "", "Prometheus 请求超时（如 15s）")
	return cmd
}

// chooseDraft lists unfinished create drafts and lets the user resume, restart
// or delete them. It never applies a draft to a formal YAML file by itself.
func (a *app) chooseDraft(prompter Prompter, sitesDir string) (*Draft, string, error) {
	files, err := FindDrafts(sitesDir)
	if err != nil {
		return nil, "", err
	}
	if len(files) == 0 {
		return nil, "", nil
	}
	options := make([]string, 0, len(files)+2)
	for _, file := range files {
		label := file
		if draft, err := LoadDraft(file); err == nil {
			label = fmt.Sprintf("%s（站点 %s，阶段 %s）", filepath.Base(file), draft.Site.Metadata.ID, draft.Stage)
		}
		options = append(options, label)
	}
	options = append(options, "重新开始（保留草稿）", "删除全部草稿并重新开始")
	index, err := prompter.Select("检测到未完成的创建草稿", options, 0)
	if err != nil {
		return nil, "", err
	}
	switch index {
	case len(files):
		return nil, "", nil
	case len(files) + 1:
		for _, file := range files {
			_ = DeleteDraft(file)
		}
		return nil, "", nil
	default:
		draft, err := LoadDraft(files[index])
		if err != nil {
			fmt.Fprintf(a.errOut, "草稿 %s 无法恢复: %v\n将重新开始，草稿文件保留在原处。\n", files[index], err)
			return nil, "", nil
		}
		fmt.Fprintf(a.out, "恢复草稿: %s（阶段 %s）\n", files[index], draft.Stage)
		return draft, files[index], nil
	}
}

// bestEffortProbe builds an optional Prometheus client for wizard-time
// discovery. Failures are reported as hints, never as wizard failures.
func (a *app) bestEffortProbe(configPath, promURL, timeoutRaw string) ClusterProbe {
	client := a.bestEffortClient(configPath, promURL, timeoutRaw)
	if client == nil {
		return nil
	}
	return client
}

func (a *app) validateCommand(opts *rootOptions) *cobra.Command {
	var sitePath string
	var all bool
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "校验站点 YAML 配置",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			files, err := selectSites(sitePath, all, opts.sitesDir)
			if err != nil {
				return err
			}
			reg, err := metrics.LoadDefault()
			if err != nil {
				return err
			}
			action := siteAction{
				validateOnly: true,
				reportDir:    opts.reportsDir,
				dashboardDir: opts.dashboardsDir,
			}
			return a.processFiles(cmd.Context(), files, reg, nil, action)
		},
	}
	cmd.Flags().StringVar(&sitePath, "site", "", "站点 YAML 文件")
	cmd.Flags().BoolVar(&all, "all", false, "处理目录下所有站点")
	return cmd
}

func (a *app) checkCommand(opts *rootOptions) *cobra.Command {
	var sitePath string
	var all bool
	var promURL, skywalkingURL, timeoutRaw string
	var lookback time.Duration
	var concurrency int
	cmd := &cobra.Command{
		Use:   "check",
		Short: "检查 Prometheus 指标、标签和 PromQL",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			files, err := selectSites(sitePath, all, opts.sitesDir)
			if err != nil {
				return err
			}
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
			var client *discovery.Client
			if available {
				client, err = discovery.NewClient(promCfg)
				if err != nil {
					return err
				}
			}
			skywalkingClient := a.skywalkingClient(skywalkingURL, timeoutRaw, tool)
			if err := validateLookback(lookback, concurrency); err != nil {
				return err
			}
			action := siteAction{
				checkProm:        true,
				requireProm:      true,
				skipReason:       "未配置 Prometheus URL",
				reportDir:        opts.reportsDir,
				dashboardDir:     opts.dashboardsDir,
				links:            tool.Links,
				datasources:      tool.Datasources,
				skywalkingClient: skywalkingClient,
				lookback:         lookback,
				concurrency:      concurrency,
			}
			return a.processFiles(cmd.Context(), files, reg, client, action)
		},
	}
	cmd.Flags().StringVar(&sitePath, "site", "", "站点 YAML 文件")
	cmd.Flags().BoolVar(&all, "all", false, "处理目录下所有站点")
	cmd.Flags().StringVar(&promURL, "prometheus-url", "", "Prometheus URL（覆盖环境变量和配置文件）")
	cmd.Flags().StringVar(&skywalkingURL, "skywalking-prometheus-url", "", "SkyWalking Prometheus URL（可选，用于 APM 数据校验）")
	cmd.Flags().StringVar(&timeoutRaw, "timeout", "", "Prometheus 请求超时（如 15s）")
	cmd.Flags().DurationVar(&lookback, "lookback", time.Hour, "校验时间窗口")
	cmd.Flags().IntVar(&concurrency, "concurrency", 4, "Prometheus 查询并发度（1-16）")
	return cmd
}

func (a *app) generateCommand(opts *rootOptions) *cobra.Command {
	var sitePath string
	var all bool
	cmd := &cobra.Command{
		Use:   "generate",
		Short: "仅根据 YAML 生成 Dashboard JSON",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			files, err := selectSites(sitePath, all, opts.sitesDir)
			if err != nil {
				return err
			}
			reg, err := metrics.LoadDefault()
			if err != nil {
				return err
			}
			tool, err := a.loadToolConfig(opts.configPath, false)
			if err != nil {
				return err
			}
			action := siteAction{
				generate:     true,
				reportDir:    opts.reportsDir,
				dashboardDir: opts.dashboardsDir,
				links:        tool.Links,
				datasources:  tool.Datasources,
			}
			return a.processFiles(cmd.Context(), files, reg, nil, action)
		},
	}
	cmd.Flags().StringVar(&sitePath, "site", "", "站点 YAML 文件")
	cmd.Flags().BoolVar(&all, "all", false, "处理目录下所有站点")
	return cmd
}

func (a *app) buildCommand(opts *rootOptions) *cobra.Command {
	var sitePath string
	var all, skipProm, strictCheck bool
	var promURL, skywalkingURL, timeoutRaw string
	var lookback time.Duration
	var concurrency int
	cmd := &cobra.Command{
		Use:   "build",
		Short: "静态校验并生成 Dashboard（实时数据用 check 验收）",
		Long: "build 只做 YAML 结构、配置语义、PromQL 语法和 Dashboard 结构校验，然后生成 Dashboard JSON。" +
			"Prometheus 实时数据问题不会阻止普通构建；显式使用 --strict-check 时才会把环境校验失败视为构建失败。",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			files, err := selectSites(sitePath, all, opts.sitesDir)
			if err != nil {
				return err
			}
			reg, err := metrics.LoadDefault()
			if err != nil {
				return err
			}
			if skipProm {
				strictCheck = false
			}
			tool, err := a.loadToolConfig(opts.configPath, false)
			if err != nil {
				return err
			}
			if err := validateLookback(lookback, concurrency); err != nil {
				return err
			}
			action := siteAction{
				generate:              true,
				checkProm:             strictCheck,
				requireProm:           strictCheck,
				deferEnvironmentCheck: !strictCheck,
				reportDir:             opts.reportsDir,
				dashboardDir:          opts.dashboardsDir,
				links:                 tool.Links,
				datasources:           tool.Datasources,
				skywalkingClient:      a.skywalkingClient(skywalkingURL, timeoutRaw, tool),
				lookback:              lookback,
				concurrency:           concurrency,
			}
			var client *discovery.Client
			if strictCheck {
				promCfg, available, err := resolvePromConfig(promURL, timeoutRaw, tool)
				if err != nil {
					return err
				}
				if available {
					client, err = discovery.NewClient(promCfg)
					if err != nil {
						return err
					}
				} else {
					action.skipReason = "严格检查需要 Prometheus URL（--prometheus-url 或 SITEGEN_PROMETHEUS_URL）"
				}
			}
			return a.processFiles(cmd.Context(), files, reg, client, action)
		},
	}
	cmd.Flags().StringVar(&sitePath, "site", "", "站点 YAML 文件")
	cmd.Flags().BoolVar(&all, "all", false, "处理目录下所有站点")
	cmd.Flags().StringVar(&promURL, "prometheus-url", "", "Prometheus URL（覆盖环境变量和配置文件）")
	cmd.Flags().StringVar(&skywalkingURL, "skywalking-prometheus-url", "", "SkyWalking Prometheus URL（可选，用于 APM 数据校验）")
	cmd.Flags().StringVar(&timeoutRaw, "timeout", "", "Prometheus 请求超时（如 15s）")
	cmd.Flags().DurationVar(&lookback, "lookback", time.Hour, "严格检查的时间窗口")
	cmd.Flags().IntVar(&concurrency, "concurrency", 4, "Prometheus 查询并发度（1-16）")
	cmd.Flags().BoolVar(&strictCheck, "strict-check", false, "执行实时 Prometheus 检查，并以失败结果阻止构建")
	cmd.Flags().BoolVar(&skipProm, "skip-prometheus", false, "兼容旧参数；build 默认已不执行实时数据检查")
	return cmd
}

func (a *app) listCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "查看已有站点",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			files, err := discoverSiteFiles(opts.sitesDir)
			if err != nil {
				return err
			}
			if len(files) == 0 {
				return fmt.Errorf("%s 下没有站点 YAML", opts.sitesDir)
			}
			tw := tabwriter.NewWriter(a.out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\t名称\t环境\t域名\t应用\t组件\t文件")
			var failures []string
			for _, file := range files {
				site, err := config.Load(file)
				if err != nil {
					failures = append(failures, fmt.Sprintf("%s: %v", file, err))
					fmt.Fprintf(tw, "ERROR\t%s\t\t\t\t\t%s\n", filepath.Base(file), err)
					continue
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%d\t%s\n",
					site.Metadata.ID, site.Metadata.Name, site.Metadata.Environment,
					len(site.Spec.Domains), len(site.Spec.Applications), len(site.Spec.Components), file)
			}
			_ = tw.Flush()
			if len(failures) > 0 {
				return fmt.Errorf("%d 个站点文件读取失败", len(failures))
			}
			return nil
		},
	}
}

func (a *app) versionCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "查看版本",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintf(a.out, "sitegen %s\n", a.version)
			if reg, err := metrics.LoadDefault(); err == nil {
				fmt.Fprintf(a.out, "templates %s+%s\n", reg.Version, reg.Digest)
			}
			fmt.Fprintf(a.out, "go %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
			return nil
		},
	}
}

func (a *app) processFiles(ctx context.Context, files []string, reg *metrics.Registry, client *discovery.Client, action siteAction) error {
	var failed int
	var firstErr error
	for _, file := range files {
		slog.Debug("处理站点配置", "file", file)
		_, err := a.processSite(ctx, file, reg, client, action)
		if err != nil {
			failed++
			if firstErr == nil {
				firstErr = err
			}
			if len(files) > 1 {
				fmt.Fprintf(a.errOut, "失败: %s: %v\n", file, err)
			}
		}
	}
	if len(files) > 1 {
		fmt.Fprintf(a.out, "\n批量处理汇总: 共 %d 个站点，成功 %d，失败 %d\n", len(files), len(files)-failed, failed)
	}
	if failed == 1 && len(files) == 1 {
		return firstErr
	}
	if failed > 0 {
		return fmt.Errorf("%d 个站点处理失败", failed)
	}
	return nil
}

func (a *app) processSite(ctx context.Context, path string, reg *metrics.Registry, client *discovery.Client, action siteAction) (*output.Report, error) {
	site, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	report := &output.Report{
		SiteID:          site.Metadata.ID,
		SiteName:        site.Metadata.Name,
		SiteFile:        path,
		TemplateVersion: reg.Version + "+" + reg.Digest,
		GeneratedAt:     a.now(),
		PrometheusCheck: action.checkProm && !action.skipProm,
	}
	report.Add("模板", "指标模板注册表", output.Pass, "版本 "+reg.Version+"，摘要 "+reg.Digest)
	issues := config.Validate(site, reg.Resolve)
	if len(issues) == 0 {
		report.Add("配置", "Schema 与关联校验", output.Pass, "配置合法")
	} else {
		for _, issue := range issues {
			status := output.Warn
			if issue.Severity == config.SeverityError {
				status = output.Fail
			}
			report.Add("配置", issue.Path, status, issue.Message)
		}
	}
	reportPath := a.reportPath(site, action.reportDir)
	if issues.HasErrors() {
		return report, a.failWithReport(report, reportPath, fmt.Errorf("站点 %s 配置校验失败", site.Metadata.ID))
	}

	if action.validateOnly {
		if err := report.WriteText(reportPath); err != nil {
			return report, fmt.Errorf("写入校验报告 %s: %w", reportPath, err)
		}
		a.printReport(report, reportPath)
		return report, nil
	}

	// check validates the generated variable model without writing a dashboard.
	// build/generate add the same checks after the dashboard is built below.
	if action.checkProm && !action.generate {
		links := config.MergeLinks(action.links, site.Spec.Links)
		if d, err := dashboard.BuildWithDatasources(site, reg, links, action.datasources); err != nil {
			report.Add("Dashboard 变量", "静态生成", output.Fail, err.Error())
		} else {
			addDashboardVariableChecks(report, d)
		}
	}

	if action.checkProm {
		switch {
		case action.skipProm:
			report.Add("Prometheus", "数据校验", output.Skip, action.skipReason)
		case client == nil:
			if action.requireProm {
				report.Add("Prometheus", "配置", output.Fail, action.skipReason)
				cause := fmt.Errorf("站点 %s 需要 Prometheus URL", site.Metadata.ID)
				return report, a.failWithReport(report, reportPath, cause)
			}
			report.Add("Prometheus", "数据校验", output.Skip, action.skipReason)
		default:
			checker := &metrics.Checker{
				API:      client,
				Site:     site,
				Registry: reg,
				Options: metrics.CheckOptions{
					Lookback:    action.lookback,
					Concurrency: action.concurrency,
					Now:         a.now,
				},
			}
			report.AddAll(checker.Run(ctx))
		}
		if siteUsesSkywalking(site) {
			links := config.MergeLinks(action.links, site.Spec.Links)
			if action.skywalkingClient != nil {
				sk := &metrics.SkywalkingChecker{
					API:     action.skywalkingClient,
					Site:    site,
					Options: metrics.CheckOptions{Lookback: action.lookback, Now: a.now},
				}
				report.AddAll(sk.Run(ctx))
			} else {
				report.Add("SkyWalking", "APM 数据校验", output.Warn,
					"未配置 SkyWalking Prometheus URL（--skywalking-prometheus-url / SITEGEN_SKYWALKING_PROMETHEUS_URL / prometheus.skywalkingUrl）；已跳过 APM 数据校验")
			}
			for _, item := range []struct {
				name string
				raw  string
			}{
				{"实例下钻链接", links.SkywalkingInstanceDashboard},
				{"Endpoint 下钻链接", links.SkywalkingEndpointDashboard},
				{"Trace 下钻链接", links.SkywalkingTraceDashboard},
			} {
				if item.raw == "" {
					report.Add("SkyWalking", item.name, output.Warn,
						"未配置 "+item.name+"；对应面板不会生成下钻地址")
				}
			}
		}
		if report.HasFailures() {
			cause := fmt.Errorf("站点 %s Prometheus 校验失败", site.Metadata.ID)
			return report, a.failWithReport(report, reportPath, cause)
		}
	}

	if action.generate {
		links := config.MergeLinks(action.links, site.Spec.Links)
		d, err := dashboard.BuildWithDatasources(site, reg, links, action.datasources)
		if err != nil {
			report.Add("Dashboard", "生成", output.Fail, err.Error())
			return report, a.failWithReport(report, reportPath, err)
		}
		dashboardPath := filepath.Join(action.dashboardDir, site.Metadata.ID+".json")
		if err := dashboard.Write(d, dashboardPath); err != nil {
			report.Add("Dashboard", "写入", output.Fail, err.Error())
			return report, a.failWithReport(report, reportPath, err)
		}
		report.DashboardPath = dashboardPath
		report.Add("Dashboard", "结构校验与原子写入", output.Pass, "已写入 "+dashboardPath)
		addDashboardVariableChecks(report, d)
	}
	if action.deferEnvironmentCheck {
		report.DeferredEnvironmentCheck = true
		report.Add("环境验收", "实时数据检查", output.Skip,
			"普通 build 不阻止因流量或采集就绪状态导致的无数据；请运行 sitegen check 完成环境验收")
	}

	if err := report.WriteText(reportPath); err != nil {
		return report, fmt.Errorf("写入校验报告 %s: %w", reportPath, err)
	}
	a.printReport(report, reportPath)
	return report, nil
}

// addDashboardVariableChecks verifies the generated variable model and all
// panel matchers without pretending that Grafana browser interpolation has
// been tested. The final WARN is intentional: only a real Grafana import and
// variable switch can prove the browser-side chain end to end.
func addDashboardVariableChecks(report *output.Report, d *dashboard.Dashboard) {
	if d == nil {
		return
	}
	var modelIssues []string
	podVariables := 0
	for _, variable := range d.Templating.List {
		switch variable.QueryType {
		case dashboard.VariableQueryTypeDatasource:
			switch variable.Name {
			case "skywalking_prometheus":
				if variable.Query != "prometheus" {
					modelIssues = append(modelIssues, variable.Name+" 必须是 prometheus 数据源")
				}
			case "skywalking_datasource":
				if variable.Query != "apache-skywalking-datasource" {
					modelIssues = append(modelIssues, variable.Name+" 必须是 apache-skywalking-datasource")
				}
			}
		case dashboard.VariableQueryTypeCustom:
			if variable.Multi {
				modelIssues = append(modelIssues, variable.Name+" 不是单选变量")
			}
			if len(variable.Options) == 0 {
				modelIssues = append(modelIssues, variable.Name+" 没有选项")
			}
			if variable.ValuesFormat != "csv" {
				modelIssues = append(modelIssues, variable.Name+" valuesFormat 不是 csv")
			}
		case dashboard.VariableQueryTypeQueryResult:
			podVariables++
			if variable.Datasource == nil || variable.Datasource.UID != "${datasource}" {
				modelIssues = append(modelIssues, variable.Name+" 未绑定 Dashboard 数据源")
			}
			if variable.RegexApplyTo != "value" {
				modelIssues = append(modelIssues, variable.Name+" regexApplyTo 不是 value")
			}
			if variable.AllValue == "" {
				modelIssues = append(modelIssues, variable.Name+" 缺少 AllValue")
			}
			query, qryType := variableQueryText(variable.Query)
			if qryType != 3 {
				modelIssues = append(modelIssues, variable.Name+" 的 qryType 不是 VarQueryResult(3)，会被错误提交到 match[]")
			}
			if !strings.HasPrefix(query, "query_result(") {
				modelIssues = append(modelIssues, variable.Name+" 查询类型不是 query_result")
			}
			if strings.ContainsAny(query, "\r\n") {
				modelIssues = append(modelIssues,
					variable.Name+" 的 query_result 包含换行；Grafana 变量解析会误判为 Series Query 并提交到 match[]")
			}
			if strings.Contains(query, ":json") {
				modelIssues = append(modelIssues, variable.Name+" 查询仍使用 :json")
			}
		}
	}
	if len(modelIssues) == 0 {
		report.Add("Dashboard 变量", "变量模型", output.Pass,
			fmt.Sprintf("Profile 应用变量为单选 custom，%d 个 Pod 变量使用 query_result，AllValue 与数据源绑定合法", podVariables))
		for _, variable := range d.Templating.List {
			switch variable.QueryType {
			case dashboard.VariableQueryTypeDatasource:
				report.Add("Dashboard 变量", variable.Name, output.Pass,
					fmt.Sprintf("datasource 变量，类型 %v", variable.Query))
				if fmt.Sprint(variable.Current.Value) == "" {
					report.Add("Dashboard 变量", variable.Name+" 默认值", output.Warn,
						"未配置默认 UID；导入 Grafana 后需要选择对应数据源")
				}
			case dashboard.VariableQueryTypeCustom:
				report.Add("Dashboard 变量", variable.Name, output.Pass,
					fmt.Sprintf("单选 custom 变量，包含 %d 个选项", len(variable.Options)))
			case dashboard.VariableQueryTypeQueryResult:
				report.Add("Dashboard 变量", variable.Name+" 定义", output.Pass,
					"query_result 查询（qryType=3，单行），regexApplyTo=value，AllValue=.*")
			}
		}
	} else {
		report.Add("Dashboard 变量", "变量模型", output.Fail, strings.Join(modelIssues, "；"))
	}

	var exprs []string
	var walk func([]dashboard.Panel)
	walk = func(panels []dashboard.Panel) {
		for _, panel := range panels {
			for _, target := range panel.Targets {
				exprs = append(exprs, target.Expr)
			}
			if panel.Panels != nil {
				walk(*panel.Panels)
			}
		}
	}
	walk(d.Panels)

	var matcherIssues []string
	for _, expr := range exprs {
		if strings.Contains(expr, ":json") {
			matcherIssues = append(matcherIssues, "存在 :json 变量")
		}
		if strings.Contains(expr, "=~${") || strings.Contains(expr, "!~${") {
			matcherIssues = append(matcherIssues, "正则 Matcher 变量未加双引号")
		}
	}
	if len(matcherIssues) == 0 {
		report.Add("Dashboard 变量", "Pod Matcher", output.Pass,
			"未发现 :json 或未加引号的正则变量；静态 Pod 归属与动态 Pod 选择分别生成")
	} else {
		report.Add("Dashboard 变量", "Pod Matcher", output.Fail,
			strings.Join(sortedUniqueStrings(matcherIssues), "；"))
	}

	report.Add("Dashboard 变量", "Grafana 实际插值", output.Warn,
		"尚未在目标 Grafana 中导入并切换应用/Pod；当前仅完成生成模型与 Prometheus 展开查询校验")
}

func variableQueryText(query any) (string, int) {
	switch value := query.(type) {
	case string:
		return value, 0
	case map[string]any:
		text, _ := value["query"].(string)
		switch typed := value["qryType"].(type) {
		case int:
			return text, typed
		case float64:
			return text, int(typed)
		default:
			return text, 0
		}
	default:
		return "", 0
	}
}

func sortedUniqueStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func (a *app) failWithReport(report *output.Report, reportPath string, cause error) error {
	if err := report.WriteText(reportPath); err != nil {
		return fmt.Errorf("%v；同时写入校验报告失败: %w", cause, err)
	}
	a.printReport(report, reportPath)
	return cause
}

func (a *app) printReport(report *output.Report, reportPath string) {
	for _, line := range report.SummaryLines() {
		fmt.Fprintln(a.out, line)
	}
	for _, check := range report.Checks {
		if check.Status != output.Warn && check.Status != output.Fail {
			continue
		}
		detail := strings.TrimSpace(check.Detail)
		if detail == "" {
			fmt.Fprintf(a.out, "[%s] %s / %s\n", check.Status, check.Category, check.Name)
			continue
		}
		lines := strings.Split(detail, "\n")
		fmt.Fprintf(a.out, "[%s] %s / %s: %s\n", check.Status, check.Category, check.Name, lines[0])
		for _, line := range lines[1:] {
			fmt.Fprintf(a.out, "       %s\n", line)
		}
	}
	fmt.Fprintf(a.out, "%s\n", report.SummaryLine())
	if report.DashboardPath != "" {
		fmt.Fprintf(a.out, "Dashboard: %s\n", report.DashboardPath)
	}
	fmt.Fprintf(a.out, "报告: %s\n", reportPath)
	if report.DashboardPath != "" && report.DeferredEnvironmentCheck {
		fmt.Fprintln(a.out, "Dashboard 已生成。")
		fmt.Fprintln(a.out, "环境数据检查尚未通过，部分面板可能暂时没有数据。")
		fmt.Fprintln(a.out, "请执行 sitegen check 进行环境验收。")
	}
}

func (a *app) reportPath(site *config.Site, reportsDir string) string {
	id := site.Metadata.ID
	if id == "" {
		id = "site"
	}
	if config.ValidateSlug(id, 35) != nil {
		id = slugify(id)
		if id == "" {
			id = "site"
		}
	}
	return filepath.Join(reportsDir, id+"-validation.txt")
}

func (a *app) loadToolConfig(path string, validatePrometheus bool) (*config.ToolConfig, error) {
	tool, _, err := config.LoadToolConfig(path)
	if err != nil {
		return nil, err
	}
	issues := config.ValidateToolConfig(tool)
	if !validatePrometheus {
		filtered := issues[:0]
		for _, issue := range issues {
			if strings.HasPrefix(issue.Path, "prometheus.") {
				continue
			}
			filtered = append(filtered, issue)
		}
		issues = filtered
	}
	if issues.HasErrors() {
		return nil, fmt.Errorf("工具配置校验失败: %s", strings.Join(issues.ErrorStrings(), "; "))
	}
	return tool, nil
}

func resolvePromConfig(flagURL, flagTimeout string, tool *config.ToolConfig) (discovery.Config, bool, error) {
	url := firstNonEmpty(flagURL, os.Getenv("SITEGEN_PROMETHEUS_URL"), tool.Prometheus.URL)
	if strings.TrimSpace(url) == "" {
		return discovery.Config{}, false, nil
	}
	timeoutRaw := firstNonEmpty(flagTimeout, os.Getenv("SITEGEN_PROMETHEUS_TIMEOUT"), tool.Prometheus.Timeout)
	timeout, err := config.ParseTimeout(timeoutRaw)
	if err != nil {
		return discovery.Config{}, false, fmt.Errorf("Prometheus timeout: %w", err)
	}
	tlsCA := firstNonEmpty(os.Getenv("SITEGEN_PROMETHEUS_TLS_CA"), tool.Prometheus.TLSCAFile)
	skipVerify := tool.Prometheus.TLSInsecureSkipVerify
	if raw := strings.TrimSpace(os.Getenv("SITEGEN_PROMETHEUS_TLS_SKIP_VERIFY")); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return discovery.Config{}, false, fmt.Errorf("SITEGEN_PROMETHEUS_TLS_SKIP_VERIFY 必须为 true/false")
		}
		skipVerify = value
	}
	username := os.Getenv("SITEGEN_PROMETHEUS_USERNAME")
	password := os.Getenv("SITEGEN_PROMETHEUS_PASSWORD")
	token := os.Getenv("SITEGEN_PROMETHEUS_TOKEN")
	if token != "" && (username != "" || password != "") {
		return discovery.Config{}, false, fmt.Errorf("Basic Auth 与 Bearer Token 不能同时配置")
	}
	return discovery.Config{
		URL:                   strings.TrimSpace(url),
		Timeout:               timeout,
		Username:              username,
		Password:              password,
		BearerToken:           token,
		TLSCAFile:             tlsCA,
		TLSInsecureSkipVerify: skipVerify,
	}, true, nil
}

func siteUsesSkywalking(site *config.Site) bool {
	if site == nil {
		return false
	}
	for _, app := range site.Spec.Applications {
		if app.SkywalkingEnabled() {
			return true
		}
	}
	return false
}

func (a *app) skywalkingClient(flagURL, timeoutRaw string, tool *config.ToolConfig) *discovery.Client {
	rawURL := firstNonEmpty(flagURL, os.Getenv("SITEGEN_SKYWALKING_PROMETHEUS_URL"), tool.Prometheus.SkywalkingURL)
	if strings.TrimSpace(rawURL) == "" {
		return nil
	}
	timeout, err := config.ParseTimeout(firstNonEmpty(timeoutRaw, os.Getenv("SITEGEN_PROMETHEUS_TIMEOUT"), tool.Prometheus.Timeout))
	if err != nil {
		fmt.Fprintf(a.errOut, "提示: SkyWalking Prometheus timeout 非法: %v\n", err)
		return nil
	}
	skipVerify := tool.Prometheus.TLSInsecureSkipVerify
	if raw := strings.TrimSpace(os.Getenv("SITEGEN_SKYWALKING_PROMETHEUS_TLS_SKIP_VERIFY")); raw != "" {
		if value, err := strconv.ParseBool(raw); err == nil {
			skipVerify = value
		}
	}
	client, err := discovery.NewClient(discovery.Config{
		URL:                   strings.TrimSpace(rawURL),
		Timeout:               timeout,
		Username:              os.Getenv("SITEGEN_SKYWALKING_PROMETHEUS_USERNAME"),
		Password:              os.Getenv("SITEGEN_SKYWALKING_PROMETHEUS_PASSWORD"),
		BearerToken:           os.Getenv("SITEGEN_SKYWALKING_PROMETHEUS_TOKEN"),
		TLSCAFile:             firstNonEmpty(os.Getenv("SITEGEN_SKYWALKING_PROMETHEUS_TLS_CA"), tool.Prometheus.TLSCAFile),
		TLSInsecureSkipVerify: skipVerify,
		// A Grafana datasource proxy is a valid read-only endpoint for APM
		// validation and requires a non-root base path.
		AllowPath: true,
	})
	if err != nil {
		fmt.Fprintf(a.errOut, "提示: 无法创建 SkyWalking Prometheus 客户端: %v\n", err)
		return nil
	}
	return client
}

func validateLookback(lookback time.Duration, concurrency int) error {
	if lookback <= 0 {
		return fmt.Errorf("--lookback 必须大于 0")
	}
	if lookback > 7*24*time.Hour {
		return fmt.Errorf("--lookback 不能超过 7 天")
	}
	if concurrency < 1 || concurrency > 16 {
		return fmt.Errorf("--concurrency 必须在 1-16 之间")
	}
	return nil
}

func selectSites(sitePath string, all bool, sitesDir string) ([]string, error) {
	if sitePath != "" && all {
		return nil, fmt.Errorf("--site 与 --all 不能同时使用")
	}
	if sitePath == "" && !all {
		return nil, fmt.Errorf("必须指定 --site 或 --all")
	}
	if sitePath != "" {
		return []string{sitePath}, nil
	}
	files, err := discoverSiteFiles(sitesDir)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("%s 下没有站点 YAML", sitesDir)
	}
	return files, nil
}

func discoverSiteFiles(dir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			name := entry.Name()
			if name != "." && strings.HasPrefix(name, ".") && path != dir {
				return filepath.SkipDir
			}
			return nil
		}
		name := strings.ToLower(entry.Name())
		if strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml") {
			if strings.HasSuffix(name, ".bak.yaml") {
				return nil
			}
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("扫描站点目录 %s: %w", dir, err)
	}
	sort.Strings(files)
	return files, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
