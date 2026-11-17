package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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
	dashboardDir string
	reportDir    string
	links        config.Links
	lookback     time.Duration
	concurrency  int
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
	var outputPath string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "交互式创建站点配置",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := metrics.LoadDefault()
			if err != nil {
				return err
			}
			prompter := NewTerminalPrompter(a.in, a.out)
			wizard := &Wizard{Prompter: prompter, Registry: reg, Out: a.out}
			site, err := wizard.Create()
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
			fmt.Fprintf(a.out, "\n已写入站点配置: %s\n", outputPath)
			fmt.Fprintf(a.out, "下一步: sitegen build --site %s\n", outputPath)
			return nil
		},
	}
	cmd.Flags().StringVarP(&outputPath, "output", "o", "", "输出文件路径（默认 sites/<site-id>.yaml）")
	return cmd
}

func (a *app) editCommand(opts *rootOptions) *cobra.Command {
	var sitePath string
	cmd := &cobra.Command{
		Use:   "edit",
		Short: "修改已有站点配置",
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
			wizard := &Wizard{Prompter: prompter, Registry: reg, Out: a.out}
			edited, err := wizard.Edit(site)
			if err != nil {
				return err
			}
			if err := config.Write(sitePath, edited, true); err != nil {
				return err
			}
			fmt.Fprintf(a.out, "\n已更新站点配置: %s\n", sitePath)
			return nil
		},
	}
	cmd.Flags().StringVar(&sitePath, "site", "", "站点 YAML 文件")
	return cmd
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
	var promURL, timeoutRaw string
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
			if err := validateLookback(lookback, concurrency); err != nil {
				return err
			}
			action := siteAction{
				checkProm:    true,
				requireProm:  true,
				skipReason:   "未配置 Prometheus URL",
				reportDir:    opts.reportsDir,
				dashboardDir: opts.dashboardsDir,
				lookback:     lookback,
				concurrency:  concurrency,
			}
			return a.processFiles(cmd.Context(), files, reg, client, action)
		},
	}
	cmd.Flags().StringVar(&sitePath, "site", "", "站点 YAML 文件")
	cmd.Flags().BoolVar(&all, "all", false, "处理目录下所有站点")
	cmd.Flags().StringVar(&promURL, "prometheus-url", "", "Prometheus URL（覆盖环境变量和配置文件）")
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
	var all, skipProm bool
	var promURL, timeoutRaw string
	var lookback time.Duration
	var concurrency int
	cmd := &cobra.Command{
		Use:   "build",
		Short: "完整校验并生成 Dashboard",
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
			tool, err := a.loadToolConfig(opts.configPath, !skipProm)
			if err != nil {
				return err
			}
			if err := validateLookback(lookback, concurrency); err != nil {
				return err
			}
			action := siteAction{
				generate:     true,
				checkProm:    true,
				skipProm:     skipProm,
				skipReason:   "用户通过 --skip-prometheus 跳过",
				reportDir:    opts.reportsDir,
				dashboardDir: opts.dashboardsDir,
				links:        tool.Links,
				lookback:     lookback,
				concurrency:  concurrency,
			}
			var client *discovery.Client
			if !skipProm {
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
					action.skipReason = "未配置 Prometheus URL"
				}
			}
			return a.processFiles(cmd.Context(), files, reg, client, action)
		},
	}
	cmd.Flags().StringVar(&sitePath, "site", "", "站点 YAML 文件")
	cmd.Flags().BoolVar(&all, "all", false, "处理目录下所有站点")
	cmd.Flags().StringVar(&promURL, "prometheus-url", "", "Prometheus URL（覆盖环境变量和配置文件）")
	cmd.Flags().StringVar(&timeoutRaw, "timeout", "", "Prometheus 请求超时（如 15s）")
	cmd.Flags().DurationVar(&lookback, "lookback", time.Hour, "校验时间窗口")
	cmd.Flags().IntVar(&concurrency, "concurrency", 4, "Prometheus 查询并发度（1-16）")
	cmd.Flags().BoolVar(&skipProm, "skip-prometheus", false, "跳过 Prometheus 数据检查并生成")
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
		if report.HasFailures() {
			cause := fmt.Errorf("站点 %s Prometheus 校验失败", site.Metadata.ID)
			return report, a.failWithReport(report, reportPath, cause)
		}
	}

	if action.generate {
		links := config.MergeLinks(action.links, site.Spec.Links)
		d, err := dashboard.Build(site, reg, links)
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
	}

	if err := report.WriteText(reportPath); err != nil {
		return report, fmt.Errorf("写入校验报告 %s: %w", reportPath, err)
	}
	a.printReport(report, reportPath)
	return report, nil
}

func (a *app) failWithReport(report *output.Report, reportPath string, cause error) error {
	if err := report.WriteText(reportPath); err != nil {
		return fmt.Errorf("%v；同时写入校验报告失败: %w", cause, err)
	}
	a.printReport(report, reportPath)
	return cause
}

func (a *app) printReport(report *output.Report, reportPath string) {
	for _, check := range report.Checks {
		if check.Status == output.Warn || check.Status == output.Fail {
			detail := ""
			if check.Detail != "" {
				detail = ": " + check.Detail
			}
			fmt.Fprintf(a.out, "[%s] %s / %s%s\n", check.Status, check.Category, check.Name, detail)
		}
	}
	fmt.Fprintf(a.out, "%s\n", report.SummaryLine())
	if report.DashboardPath != "" {
		fmt.Fprintf(a.out, "Dashboard: %s\n", report.DashboardPath)
	}
	fmt.Fprintf(a.out, "报告: %s\n", reportPath)
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
