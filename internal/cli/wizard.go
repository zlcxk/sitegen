package cli

import (
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"site-observability/internal/config"
	"site-observability/internal/metrics"
)

// Wizard collects a site graph interactively. It only returns a site after the
// user confirms the preview; writing is the caller's responsibility.
type Wizard struct {
	Prompter Prompter
	Registry *metrics.Registry
	Out      io.Writer
}

func (w *Wizard) Create() (*config.Site, error) {
	site := &config.Site{
		APIVersion: config.APIVersion,
		Kind:       config.KindSite,
	}
	if err := w.promptMetadata(site, nil); err != nil {
		return nil, err
	}
	if err := w.promptDomains(site); err != nil {
		return nil, err
	}
	if err := w.promptApplications(site); err != nil {
		return nil, err
	}
	if err := w.promptEntryApplications(site); err != nil {
		return nil, err
	}
	if err := w.promptComponents(site); err != nil {
		return nil, err
	}
	if err := w.promptDependencies(site, nil); err != nil {
		return nil, err
	}
	return w.finish(site)
}

func (w *Wizard) Edit(existing *config.Site) (*config.Site, error) {
	if existing == nil {
		return nil, fmt.Errorf("没有可编辑的站点配置")
	}
	site := &config.Site{
		APIVersion: config.APIVersion,
		Kind:       config.KindSite,
		Metadata:   existing.Metadata,
		Spec: config.Spec{
			Links: existing.Spec.Links,
		},
	}
	if err := w.promptMetadata(site, existing); err != nil {
		return nil, err
	}
	if len(existing.Spec.Domains) > 0 {
		keep, err := w.Prompter.Confirm("保留现有域名配置?", true)
		if err != nil {
			return nil, err
		}
		if keep {
			site.Spec.Domains = append([]config.Domain(nil), existing.Spec.Domains...)
		}
	}
	if len(site.Spec.Domains) == 0 {
		if err := w.promptDomains(site); err != nil {
			return nil, err
		}
	}

	if len(existing.Spec.Applications) > 0 {
		keep, err := w.Prompter.Confirm("保留现有应用配置?", true)
		if err != nil {
			return nil, err
		}
		if keep {
			site.Spec.Applications = append([]config.Application(nil), existing.Spec.Applications...)
		}
	}
	if len(site.Spec.Applications) == 0 {
		if err := w.promptApplications(site); err != nil {
			return nil, err
		}
	}
	if err := w.promptEntryApplications(site); err != nil {
		return nil, err
	}

	if len(existing.Spec.Components) > 0 {
		keep, err := w.Prompter.Confirm("保留现有依赖组件配置?", true)
		if err != nil {
			return nil, err
		}
		if keep {
			site.Spec.Components = append([]config.Component(nil), existing.Spec.Components...)
		}
	}
	if len(site.Spec.Components) == 0 {
		if err := w.promptComponents(site); err != nil {
			return nil, err
		}
	}

	if err := w.promptDependencies(site, existing); err != nil {
		return nil, err
	}
	return w.finish(site)
}

func (w *Wizard) promptMetadata(site *config.Site, existing *config.Site) error {
	defaultID, defaultName, defaultEnv, defaultDesc := "", "", "test", ""
	if existing != nil {
		defaultID = existing.Metadata.ID
		defaultName = existing.Metadata.Name
		defaultEnv = existing.Metadata.Environment
		defaultDesc = existing.Metadata.Description
	}
	id, err := w.Prompter.Input("站点 ID（小写字母、数字、连字符）", defaultID, func(value string) error {
		if err := config.ValidateSlug(value, 35); err != nil {
			return err
		}
		if value != defaultID && w.idTaken(site, value) {
			return fmt.Errorf("ID %q 已被其他对象使用", value)
		}
		return nil
	})
	if err != nil {
		return err
	}
	name, err := w.Prompter.Input("站点名称", defaultName, nonEmpty("站点名称"))
	if err != nil {
		return err
	}
	env, err := w.Prompter.Input("环境（如 dev/test/prod）", defaultEnv, nonEmpty("环境"))
	if err != nil {
		return err
	}
	desc, err := w.Prompter.Input("描述（可留空）", defaultDesc, nil)
	if err != nil {
		return err
	}
	site.Metadata.ID = id
	site.Metadata.Name = name
	site.Metadata.Environment = env
	site.Metadata.Description = desc
	return nil
}

func (w *Wizard) promptDomains(site *config.Site) error {
	for {
		rawURL, err := w.Prompter.Input("域名 URL", "", func(value string) error {
			return config.ValidateHTTPURL(value)
		})
		if err != nil {
			return err
		}
		defaultID := uniqueID(site, domainID(rawURL))
		id, err := w.Prompter.Input("域名 ID", defaultID, func(value string) error {
			if err := config.ValidateSlug(value, 35); err != nil {
				return err
			}
			if w.idTaken(site, value) {
				return fmt.Errorf("ID %q 已被使用", value)
			}
			return nil
		})
		if err != nil {
			return err
		}
		job, err := w.Prompter.Input("Blackbox Job", "blackbox-sites", nonEmpty("Blackbox Job"))
		if err != nil {
			return err
		}
		instance, err := w.Prompter.Input("Blackbox Instance", rawURL, nonEmpty("Blackbox Instance"))
		if err != nil {
			return err
		}
		site.Spec.Domains = append(site.Spec.Domains, config.Domain{
			ID:  id,
			URL: rawURL,
			Blackbox: config.Blackbox{
				Job:      job,
				Instance: instance,
			},
		})
		more, err := w.Prompter.Confirm("继续添加域名?", false)
		if err != nil {
			return err
		}
		if !more {
			return nil
		}
	}
}

func (w *Wizard) promptApplications(site *config.Site) error {
	add, err := w.Prompter.Confirm("是否关联应用?", true)
	if err != nil {
		return err
	}
	if !add {
		return nil
	}
	profiles := w.Registry.ProfilesForApplication()
	for {
		application, err := w.Prompter.Input("Application", "", nonEmpty("Application"))
		if err != nil {
			return err
		}
		namespace, err := w.Prompter.Input("Namespace", "", nonEmpty("Namespace"))
		if err != nil {
			return err
		}
		defaultID := uniqueID(site, slugify(application+"-"+namespace))
		id, err := w.Prompter.Input("应用配置 ID", defaultID, func(value string) error {
			if err := config.ValidateSlug(value, 35); err != nil {
				return err
			}
			if w.idTaken(site, value) {
				return fmt.Errorf("ID %q 已被使用", value)
			}
			return nil
		})
		if err != nil {
			return err
		}
		job, err := w.Prompter.Input("Prometheus Job", application, nonEmpty("Prometheus Job"))
		if err != nil {
			return err
		}
		labels, err := w.promptExtraLabels()
		if err != nil {
			return err
		}
		serviceDefault := config.DefaultSkywalkingService(application, namespace)
		auto, err := w.Prompter.Confirm("自动生成 SkyWalking Service Name（"+serviceDefault+"）?", true)
		if err != nil {
			return err
		}
		service := serviceDefault
		if !auto {
			service, err = w.Prompter.Input("SkyWalking Service Name", serviceDefault, func(value string) error {
				if strings.TrimSpace(value) == "" {
					return fmt.Errorf("不能为空")
				}
				return w.serviceTaken(site, value)
			})
			if err != nil {
				return err
			}
		}
		endpointFormat, err := w.Prompter.Input("SkyWalking Endpoint 格式", config.DefaultEndpointFormat, nonEmpty("Endpoint 格式"))
		if err != nil {
			return err
		}
		selectedMetrics, err := w.promptProfiles("启用指标模板", profiles, true)
		if err != nil {
			return err
		}
		app := config.Application{
			ID:          id,
			Application: application,
			Namespace:   namespace,
			Prometheus: config.Prometheus{
				Job:    job,
				Labels: labels,
			},
			Skywalking: config.Skywalking{
				Service:        service,
				EndpointFormat: endpointFormat,
			},
			Metrics: selectedMetrics,
		}
		site.Spec.Applications = append(site.Spec.Applications, app)
		config.Normalize(site)
		more, err := w.Prompter.Confirm("继续添加应用?", false)
		if err != nil {
			return err
		}
		if !more {
			return nil
		}
	}
}

func (w *Wizard) promptComponents(site *config.Site) error {
	add, err := w.Prompter.Confirm("是否配置依赖组件?", true)
	if err != nil {
		return err
	}
	if !add {
		return nil
	}
	types := []string{"mysql", "redis", "kafka", "nacos", "custom"}
	for {
		index, err := w.Prompter.Select("组件类型", []string{"MySQL", "Redis", "Kafka", "Nacos", "自定义"}, 0)
		if err != nil {
			return err
		}
		componentType := types[index]
		defaultName := componentDisplayName(componentType) + " " + strconv.Itoa(len(site.Spec.Components)+1)
		name, err := w.Prompter.Input("组件名称", defaultName, nonEmpty("组件名称"))
		if err != nil {
			return err
		}
		defaultID := uniqueID(site, slugify(name))
		id, err := w.Prompter.Input("组件 ID", defaultID, func(value string) error {
			if err := config.ValidateSlug(value, 35); err != nil {
				return err
			}
			if w.idTaken(site, value) {
				return fmt.Errorf("ID %q 已被使用", value)
			}
			return nil
		})
		if err != nil {
			return err
		}
		job, err := w.Prompter.Input("Prometheus Job", "", nonEmpty("Prometheus Job"))
		if err != nil {
			return err
		}
		instance, err := w.Prompter.Input("Instance", "", nonEmpty("Instance"))
		if err != nil {
			return err
		}
		labels, err := w.promptExtraLabels()
		if err != nil {
			return err
		}
		profiles := w.Registry.ProfilesForComponent(componentType)
		selected, err := w.promptProfiles("启用组件指标模板", profiles, true)
		if err != nil {
			return err
		}
		site.Spec.Components = append(site.Spec.Components, config.Component{
			ID:   id,
			Name: name,
			Type: componentType,
			Prometheus: config.ComponentPrometheus{
				Job:      job,
				Instance: instance,
				Labels:   labels,
			},
			Metrics: selected,
		})
		more, err := w.Prompter.Confirm("继续添加依赖组件?", false)
		if err != nil {
			return err
		}
		if !more {
			return nil
		}
	}
}

// promptEntryApplications binds each domain to the applications that actually
// serve external traffic. It is an explicit relationship, never inferred from
// application names.
func (w *Wizard) promptEntryApplications(site *config.Site) error {
	if len(site.Spec.Domains) == 0 || len(site.Spec.Applications) == 0 {
		return nil
	}
	labels := make([]string, 0, len(site.Spec.Applications))
	ids := make([]string, 0, len(site.Spec.Applications))
	for _, app := range site.Spec.Applications {
		ids = append(ids, app.ID)
		labels = append(labels, app.ID+"（"+app.Application+"/"+app.Namespace+"）")
	}
	for i := range site.Spec.Domains {
		domain := &site.Spec.Domains[i]
		defaults := make([]bool, len(site.Spec.Applications))
		for _, entry := range domain.EntryApplications {
			for j, id := range ids {
				if id == entry {
					defaults[j] = true
				}
			}
		}
		selected, err := w.Prompter.MultiSelect(
			"域名 "+domain.ID+" 的入口应用（站点外部请求量只统计这些应用，可留空）",
			labels, defaults)
		if err != nil {
			return err
		}
		domain.EntryApplications = nil
		for _, index := range selected {
			domain.EntryApplications = append(domain.EntryApplications, ids[index])
		}
	}
	return nil
}

type dependencyOption struct {
	ID    string
	Label string
}

func (w *Wizard) promptDependencies(site *config.Site, existing *config.Site) error {
	if len(site.Spec.Applications) == 0 {
		return nil
	}
	options := make([]dependencyOption, 0, len(site.Spec.Applications)+len(site.Spec.Components))
	for _, app := range site.Spec.Applications {
		options = append(options, dependencyOption{
			ID:    app.ID,
			Label: "应用 " + app.ID + "（" + app.Application + "/" + app.Namespace + "）",
		})
	}
	for _, component := range site.Spec.Components {
		options = append(options, dependencyOption{
			ID:    component.ID,
			Label: "组件 " + component.ID + "（" + component.Name + " / " + component.Type + "）",
		})
	}
	existingDeps := map[string][]string{}
	if existing != nil {
		for _, app := range existing.Spec.Applications {
			existingDeps[app.ID] = app.Dependencies
		}
	}
	for i := range site.Spec.Applications {
		app := &site.Spec.Applications[i]
		var candidates []dependencyOption
		var labels []string
		for _, option := range options {
			if option.ID == app.ID {
				continue
			}
			candidates = append(candidates, option)
			labels = append(labels, option.Label)
		}
		if len(candidates) == 0 {
			continue
		}
		defaults := make([]bool, len(candidates))
		for _, dep := range existingDeps[app.ID] {
			for j, option := range candidates {
				if option.ID == dep {
					defaults[j] = true
				}
			}
		}
		selected, err := w.Prompter.MultiSelect("应用 "+app.ID+" 的依赖", labels, defaults)
		if err != nil {
			return err
		}
		app.Dependencies = nil
		for _, index := range selected {
			app.Dependencies = append(app.Dependencies, candidates[index].ID)
		}
	}
	return nil
}

func (w *Wizard) promptProfiles(label string, profiles []*metrics.Profile, defaultAll bool) ([]string, error) {
	if len(profiles) == 0 {
		return nil, nil
	}
	labels := make([]string, 0, len(profiles))
	defaults := make([]bool, 0, len(profiles))
	for _, profile := range profiles {
		labels = append(labels, profile.ID+" - "+profile.Name)
		defaults = append(defaults, defaultAll)
	}
	selected, err := w.Prompter.MultiSelect(label, labels, defaults)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(selected))
	for _, index := range selected {
		out = append(out, profiles[index].ID)
	}
	return out, nil
}

func (w *Wizard) promptExtraLabels() (map[string]string, error) {
	raw, err := w.Prompter.Input("额外 Prometheus 标签（key=value，逗号分隔，可留空）", "", func(value string) error {
		_, err := parseLabels(value)
		return err
	})
	if err != nil {
		return nil, err
	}
	labels, err := parseLabels(raw)
	if err != nil {
		return nil, err
	}
	return labels, nil
}

func (w *Wizard) finish(site *config.Site) (*config.Site, error) {
	config.Normalize(site)
	issues := config.Validate(site, w.Registry.Resolve)
	for _, issue := range issues {
		prefix := "WARN"
		if issue.Severity == config.SeverityError {
			prefix = "ERROR"
		}
		fmt.Fprintf(w.Out, "%s %s: %s\n", prefix, issue.Path, issue.Message)
	}
	if issues.HasErrors() {
		return nil, fmt.Errorf("配置校验失败，未写入任何文件")
	}
	w.printPreview(site, issues)
	ok, err := w.Prompter.Confirm("确认写入配置?", true)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrCancelled
	}
	return site, nil
}

func (w *Wizard) printPreview(site *config.Site, issues config.Issues) {
	fmt.Fprintln(w.Out, "\n配置预览")
	fmt.Fprintln(w.Out, "--------")
	fmt.Fprintf(w.Out, "站点: %s (%s)\n", site.Metadata.Name, site.Metadata.ID)
	fmt.Fprintf(w.Out, "环境: %s\n", site.Metadata.Environment)
	if site.Metadata.Description != "" {
		fmt.Fprintf(w.Out, "描述: %s\n", site.Metadata.Description)
	}
	fmt.Fprintln(w.Out, "域名:")
	for _, domain := range site.Spec.Domains {
		fmt.Fprintf(w.Out, "  - %s  %s  blackbox=%s/%s\n", domain.ID, domain.URL, domain.Blackbox.Job, domain.Blackbox.Instance)
		if len(domain.EntryApplications) > 0 {
			fmt.Fprintf(w.Out, "    entryApplications=%s\n", strings.Join(domain.EntryApplications, ", "))
		}
	}
	fmt.Fprintln(w.Out, "应用:")
	for _, app := range site.Spec.Applications {
		fmt.Fprintf(w.Out, "  - %s  %s/%s  job=%s\n", app.ID, app.Application, app.Namespace, app.Prometheus.Job)
		fmt.Fprintf(w.Out, "    SkyWalking=%s  metrics=%s\n", app.Skywalking.Service, strings.Join(app.Metrics, ", "))
		if len(app.Dependencies) > 0 {
			fmt.Fprintf(w.Out, "    dependencies=%s\n", strings.Join(app.Dependencies, ", "))
		}
	}
	if len(site.Spec.Components) > 0 {
		fmt.Fprintln(w.Out, "依赖组件:")
		for _, component := range site.Spec.Components {
			fmt.Fprintf(w.Out, "  - %s  %s (%s)  job=%s instance=%s metrics=%s\n",
				component.ID, component.Name, component.Type, component.Prometheus.Job,
				component.Prometheus.Instance, strings.Join(component.Metrics, ", "))
		}
	}
	if len(issues) > 0 {
		fmt.Fprintln(w.Out, "校验提示:")
		for _, issue := range issues {
			fmt.Fprintf(w.Out, "  - [%s] %s: %s\n", issue.Severity, issue.Path, issue.Message)
		}
	}
	fmt.Fprintln(w.Out)
}

func (w *Wizard) idTaken(site *config.Site, id string) bool {
	if site.Metadata.ID == id {
		return true
	}
	for _, domain := range site.Spec.Domains {
		if domain.ID == id {
			return true
		}
	}
	for _, app := range site.Spec.Applications {
		if app.ID == id {
			return true
		}
	}
	for _, component := range site.Spec.Components {
		if component.ID == id {
			return true
		}
	}
	return false
}

func (w *Wizard) serviceTaken(site *config.Site, service string) error {
	for _, app := range site.Spec.Applications {
		if app.Skywalking.Service == service {
			return fmt.Errorf("SkyWalking Service %q 已映射到应用 %s", service, app.ID)
		}
	}
	return nil
}

var extraLabelRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

func parseLabels(raw string) (map[string]string, error) {
	labels := map[string]string{}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return labels, nil
	}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			return nil, fmt.Errorf("标签 %q 必须使用 key=value 格式", part)
		}
		key := strings.TrimSpace(kv[0])
		value := strings.TrimSpace(kv[1])
		if !extraLabelRE.MatchString(key) {
			return nil, fmt.Errorf("非法标签名 %q", key)
		}
		switch key {
		case "application", "namespace", "job", "instance":
			return nil, fmt.Errorf("标签 %q 由专用字段管理，不能作为额外标签", key)
		}
		if value == "" {
			return nil, fmt.Errorf("标签 %q 的值不能为空", key)
		}
		labels[key] = value
	}
	return labels, nil
}

func uniqueID(site *config.Site, candidate string) string {
	candidate = slugify(candidate)
	if candidate == "" {
		candidate = "item"
	}
	checker := func(id string) bool {
		if site.Metadata.ID == id {
			return true
		}
		for _, d := range site.Spec.Domains {
			if d.ID == id {
				return true
			}
		}
		for _, a := range site.Spec.Applications {
			if a.ID == id {
				return true
			}
		}
		for _, c := range site.Spec.Components {
			if c.ID == id {
				return true
			}
		}
		return false
	}
	if !checker(candidate) {
		return candidate
	}
	base := candidate
	for i := 2; i < 1000; i++ {
		candidate = slugify(base + "-" + strconv.Itoa(i))
		if !checker(candidate) {
			return candidate
		}
	}
	return candidate
}

func slugify(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
		if b.Len() >= 35 {
			break
		}
	}
	out := strings.Trim(b.String(), "-")
	for len(out) > 0 && !isAlphaNum(out[len(out)-1]) {
		out = out[:len(out)-1]
	}
	if out == "" {
		return ""
	}
	return out
}

func isAlphaNum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
}

func domainID(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return "domain"
	}
	return slugify(u.Hostname())
}

func componentDisplayName(componentType string) string {
	switch componentType {
	case "mysql":
		return "MySQL"
	case "redis":
		return "Redis"
	case "kafka":
		return "Kafka"
	case "nacos":
		return "Nacos"
	default:
		return "Custom"
	}
}

func nonEmpty(field string) func(string) error {
	return func(value string) error {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s不能为空", field)
		}
		return nil
	}
}
