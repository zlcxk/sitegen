package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/common/model"

	"site-observability/internal/config"
	"site-observability/internal/metrics"
)

// ClusterProbe is the read-only Prometheus surface used to verify Pod labels
// and cAdvisor labels while the user is filling in the wizard. It is optional:
// without it the wizard still works, but cannot check metric existence.
type ClusterProbe interface {
	MetricExists(ctx context.Context, metric string, start, end time.Time) (bool, error)
	Series(ctx context.Context, match string, start, end time.Time) ([]model.LabelSet, error)
	Query(ctx context.Context, expr string) (model.Value, error)
}

// Wizard collects a site graph interactively. It only returns a site after the
// user confirms the preview; writing is the caller's responsibility.
type Wizard struct {
	Prompter Prompter
	Registry *metrics.Registry
	Out      io.Writer
	Probe    ClusterProbe
	// SitesDir enables automatic draft saving under <sites>/.drafts.
	SitesDir string
	// Resume restores an interrupted create session.
	Resume *Draft
}

type wizardStage struct {
	id       string
	title    string
	optional bool
	run      func(*config.Site) error
}

var stageOrder = []string{
	"metadata", "domains", "applications", "kubernetes-pod", "entry-applications",
	"target-labels", "container-matchers", "skywalking-ui", "components", "dependencies",
}

func stageIndex(id string) int {
	if id == "preview" {
		return len(stageOrder)
	}
	for i, stageID := range stageOrder {
		if stageID == id {
			return i
		}
	}
	return 0
}

func (w *Wizard) allStages() []wizardStage {
	return []wizardStage{
		{id: "metadata", title: "站点基础信息", run: func(site *config.Site) error {
			return w.promptMetadata(site, site)
		}},
		{id: "domains", title: "域名配置", run: w.promptDomains},
		{id: "applications", title: "应用和指标模板", run: func(site *config.Site) error {
			return w.promptApplications(site, false)
		}},
		{id: "kubernetes-pod", title: "Kubernetes Pod 归属", optional: true, run: w.promptKubernetesPodSettings},
		{id: "entry-applications", title: "入口应用", run: w.promptEntryApplications},
		{id: "target-labels", title: "Prometheus Target Labels", optional: true, run: w.promptTargetLabelsSettings},
		{id: "container-matchers", title: "Kubernetes 容器过滤", optional: true, run: w.promptContainerMatcherSettings},
		{id: "skywalking-ui", title: "SkyWalking 下钻", optional: true, run: w.promptSkywalkingUISettings},
		{id: "components", title: "依赖组件", optional: true, run: func(site *config.Site) error {
			return w.promptComponents(site, false)
		}},
		{id: "dependencies", title: "应用依赖关系", optional: true, run: w.promptDependencies},
	}
}

// Create runs the interactive flow. Each stage mutates the same site object,
// so returning to an earlier stage never discards later completed data.
func (w *Wizard) Create() (*config.Site, error) {
	site := &config.Site{
		APIVersion: config.APIVersion,
		Kind:       config.KindSite,
	}
	start := 0
	if w.Resume != nil && w.Resume.Site != nil {
		site = w.Resume.Site
		config.Normalize(site)
		start = stageIndex(w.Resume.Stage)
	}
	if err := w.runStages(site, start); err != nil {
		return nil, err
	}
	return w.finish(site)
}

func (w *Wizard) runStages(site *config.Site, start int) error {
	stages := w.allStages()
	if start < 0 || start > len(stages) {
		start = 0
	}
	index := start
	for index < len(stages) {
		stage := stages[index]
		action, err := w.chooseAction(stage, index, len(stages))
		if err != nil {
			return err
		}
		switch action {
		case StepBack:
			if index > 0 {
				index--
			}
			continue
		case StepSkip:
			if !stage.optional {
				continue
			}
			w.saveDraft(site, nextStageID(stages, index))
			index++
			continue
		case StepQuit:
			w.saveDraftAndNotify(site, stage.id)
			return ErrDraftSaved
		case StepEdit, StepNext:
		}

		err = stage.run(site)
		switch {
		case err == nil:
			config.Normalize(site)
			w.saveDraft(site, nextStageID(stages, index))
			index++
		case errors.Is(err, ErrStepBack):
			if index > 0 {
				index--
			}
		case errors.Is(err, ErrStepEdit):
			// Re-run the same stage with the values already collected.
		case errors.Is(err, ErrStepSkip):
			if stage.optional {
				w.saveDraft(site, nextStageID(stages, index))
				index++
			}
		case errors.Is(err, ErrStepQuit):
			w.saveDraftAndNotify(site, stage.id)
			return ErrDraftSaved
		default:
			return err
		}
	}
	return nil
}

func nextStageID(stages []wizardStage, index int) string {
	if index+1 < len(stages) {
		return stages[index+1].id
	}
	return "preview"
}

func (w *Wizard) chooseAction(stage wizardStage, index, total int) (StepAction, error) {
	if navigator, ok := w.Prompter.(Navigator); ok {
		return navigator.ChooseStepAction(stage.title, index, total, index > 0, stage.optional)
	}
	return StepNext, nil
}

func (w *Wizard) saveDraft(site *config.Site, stage string) {
	if w.SitesDir == "" {
		return
	}
	if _, err := SaveDraft(w.SitesDir, site, stage); err != nil {
		fmt.Fprintf(w.Out, "草稿保存失败: %v\n", err)
	}
}

func (w *Wizard) saveDraftAndNotify(site *config.Site, stage string) {
	if w.SitesDir == "" {
		fmt.Fprintln(w.Out, "已退出向导；未启用草稿目录。")
		return
	}
	path, err := SaveDraft(w.SitesDir, site, stage)
	if err != nil {
		fmt.Fprintf(w.Out, "草稿保存失败: %v\n", err)
		return
	}
	if path != "" {
		fmt.Fprintf(w.Out, "\n草稿已保存: %s\n恢复方式: sitegen create\n", path)
	}
}

// Edit shows the partition menu and changes only the selected section.
func (w *Wizard) Edit(existing *config.Site) (*config.Site, error) {
	if existing == nil {
		return nil, fmt.Errorf("没有可编辑的站点配置")
	}
	original, err := cloneSite(existing)
	if err != nil {
		return nil, err
	}
	site, err := cloneSite(existing)
	if err != nil {
		return nil, err
	}
	partitions := w.partitions()
	titles := make([]string, len(partitions))
	for i, partition := range partitions {
		titles[i] = partition.title
	}
	index, err := w.Prompter.Select("选择要编辑的配置分区", titles, 0)
	if err != nil {
		return nil, err
	}
	if partitions[index].run == nil {
		w.printPreview(site, nil)
		return nil, ErrViewOnly
	}
	if err := partitions[index].run(site); err != nil {
		if errors.Is(err, ErrStepBack) || errors.Is(err, ErrStepSkip) {
			return nil, ErrCancelled
		}
		return nil, err
	}
	config.Normalize(site)
	issues := config.Validate(site, w.Registry.Resolve)
	if len(issues) > 0 {
		for _, issue := range issues {
			fmt.Fprintf(w.Out, "[%s] %s: %s\n", issue.Severity, issue.Path, issue.Message)
		}
		if issues.HasErrors() {
			return nil, fmt.Errorf("配置校验失败，未写入任何文件")
		}
	}
	w.printChangeSummary(original, site, index+1, partitions[index].title)
	ok, err := w.Prompter.Confirm("确认写入修改?", true)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrCancelled
	}
	return site, nil
}

func (w *Wizard) partitions() []wizardStage {
	return []wizardStage{
		{id: "metadata", title: "站点基础信息", run: func(site *config.Site) error {
			return w.promptMetadata(site, site)
		}},
		{id: "domains", title: "域名和入口应用", run: func(site *config.Site) error {
			if err := w.promptDomains(site); err != nil {
				return err
			}
			return w.promptEntryApplications(site)
		}},
		{id: "applications", title: "应用和指标模板", run: func(site *config.Site) error {
			return w.promptApplications(site, true)
		}},
		{id: "kubernetes-pod", title: "Kubernetes Pod 归属", run: w.promptKubernetesPodSettings},
		{id: "container-matchers", title: "Kubernetes 容器过滤", run: w.promptContainerMatcherSettings},
		{id: "target-labels", title: "Prometheus Target Labels", run: w.promptTargetLabelsSettings},
		{id: "skywalking-ui", title: "SkyWalking 下钻", run: w.promptSkywalkingUISettings},
		{id: "components", title: "依赖组件", run: func(site *config.Site) error {
			return w.promptComponents(site, true)
		}},
		{id: "dependencies", title: "应用依赖关系", run: w.promptDependencies},
		{id: "view", title: "查看完整配置", run: nil},
	}
}

func (w *Wizard) printChangeSummary(original, edited *config.Site, partition int, title string) {
	fmt.Fprintf(w.Out, "\n变更摘要（分区 %d: %s）\n", partition, title)
	if reflect.DeepEqual(original, edited) {
		fmt.Fprintln(w.Out, "未检测到变更。")
		return
	}
	fmt.Fprintf(w.Out, "站点: %s / %s / %s\n", edited.Metadata.ID, edited.Metadata.Name, edited.Metadata.Environment)
	fmt.Fprintf(w.Out, "域名: %d 个；应用: %d 个；依赖组件: %d 个\n",
		len(edited.Spec.Domains), len(edited.Spec.Applications), len(edited.Spec.Components))
	fmt.Fprintln(w.Out, "未选中的配置分区保持不变。")
}

func cloneSite(site *config.Site) (*config.Site, error) {
	data, err := config.Marshal(site)
	if err != nil {
		return nil, fmt.Errorf("复制站点配置: %w", err)
	}
	clone, err := config.Decode(data, "clone")
	if err != nil {
		return nil, fmt.Errorf("复制站点配置: %w", err)
	}
	return clone, nil
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
	if len(site.Spec.Domains) == 0 {
		add, err := w.Prompter.Confirm("添加域名?", true)
		if err != nil {
			return err
		}
		if !add {
			return nil
		}
		domain, err := w.collectDomain(site, nil)
		if err != nil {
			return err
		}
		site.Spec.Domains = append(site.Spec.Domains, domain)
		more, err := w.Prompter.Confirm("继续添加域名?", false)
		if err != nil {
			return err
		}
		if !more {
			return nil
		}
	}
	for {
		options := []string{"保留现有域名，继续", "编辑已有域名", "添加新域名", "完成"}
		choice, err := w.Prompter.Select("域名配置", options, 0)
		if err != nil {
			return err
		}
		switch choice {
		case 0, 3:
			return nil
		case 1:
			labels := make([]string, 0, len(site.Spec.Domains)+1)
			for _, domain := range site.Spec.Domains {
				labels = append(labels, domain.ID+"（"+domain.URL+"）")
			}
			labels = append(labels, "取消")
			selected, err := w.Prompter.Select("选择要编辑的域名", labels, 0)
			if err != nil {
				return err
			}
			if selected == len(site.Spec.Domains) {
				continue
			}
			edited, err := w.collectDomain(site, &site.Spec.Domains[selected])
			if err != nil {
				return err
			}
			site.Spec.Domains[selected] = edited
		case 2:
			domain, err := w.collectDomain(site, nil)
			if err != nil {
				return err
			}
			site.Spec.Domains = append(site.Spec.Domains, domain)
		}
	}
}

func (w *Wizard) collectDomain(site *config.Site, existing *config.Domain) (config.Domain, error) {
	urlDefault, idDefault, jobDefault, instanceDefault := "", "", "blackbox-sites", ""
	if existing != nil {
		urlDefault = existing.URL
		idDefault = existing.ID
		jobDefault = existing.Blackbox.Job
		instanceDefault = existing.Blackbox.Instance
	}
	rawURL, err := w.Prompter.Input("域名 URL", urlDefault, func(value string) error {
		return config.ValidateHTTPURL(value)
	})
	if err != nil {
		return config.Domain{}, err
	}
	if idDefault == "" {
		idDefault = uniqueID(site, domainID(rawURL))
	}
	id, err := w.Prompter.Input("域名 ID", idDefault, func(value string) error {
		if err := config.ValidateSlug(value, 35); err != nil {
			return err
		}
		if (existing == nil || value != existing.ID) && w.idTaken(site, value) {
			return fmt.Errorf("ID %q 已被使用", value)
		}
		return nil
	})
	if err != nil {
		return config.Domain{}, err
	}
	job, err := w.Prompter.Input("Blackbox Job", jobDefault, nonEmpty("Blackbox Job"))
	if err != nil {
		return config.Domain{}, err
	}
	if instanceDefault == "" {
		instanceDefault = rawURL
	}
	instance, err := w.Prompter.Input("Blackbox Instance", instanceDefault, nonEmpty("Blackbox Instance"))
	if err != nil {
		return config.Domain{}, err
	}
	domain := config.Domain{
		ID:       id,
		URL:      rawURL,
		Blackbox: config.Blackbox{Job: job, Instance: instance},
	}
	if existing != nil {
		domain.EntryApplications = existing.EntryApplications
	}
	return domain, nil
}

// promptApplications supports both initial creation and partition editing.
// When preferEdit is true the menu defaults to editing an existing application.
func (w *Wizard) promptApplications(site *config.Site, preferEdit bool) error {
	profiles := w.Registry.ProfilesForApplication()
	if len(site.Spec.Applications) == 0 {
		add, err := w.Prompter.Confirm("是否关联应用?", true)
		if err != nil {
			return err
		}
		if !add {
			return nil
		}
		for {
			app, err := w.collectApplication(site, nil, profiles)
			if err != nil {
				return err
			}
			site.Spec.Applications = append(site.Spec.Applications, app)
			more, err := w.Prompter.Confirm("继续添加应用?", false)
			if err != nil {
				return err
			}
			if !more {
				return nil
			}
		}
	}
	for {
		options := []string{"保留现有应用，继续", "编辑已有应用", "添加新应用", "完成"}
		defaultIndex := 0
		if preferEdit {
			defaultIndex = 1
		}
		choice, err := w.Prompter.Select("应用配置", options, defaultIndex)
		if err != nil {
			return err
		}
		switch choice {
		case 0, 3:
			return nil
		case 1:
			labels := make([]string, 0, len(site.Spec.Applications)+1)
			for _, app := range site.Spec.Applications {
				labels = append(labels, app.ID+"（"+app.Application+"/"+app.Namespace+"）")
			}
			labels = append(labels, "取消")
			selected, err := w.Prompter.Select("选择要编辑的应用", labels, 0)
			if err != nil {
				return err
			}
			if selected == len(site.Spec.Applications) {
				continue
			}
			edited, err := w.collectApplication(site, &site.Spec.Applications[selected], profiles)
			if err != nil {
				return err
			}
			site.Spec.Applications[selected] = edited
		case 2:
			app, err := w.collectApplication(site, nil, profiles)
			if err != nil {
				return err
			}
			site.Spec.Applications = append(site.Spec.Applications, app)
		}
	}
}

func (w *Wizard) collectApplication(site *config.Site, existing *config.Application, profiles []*metrics.Profile) (config.Application, error) {
	applicationDefault, namespaceDefault, idDefault, jobDefault := "", "", "", ""
	var selectedMetrics []string
	labels := map[string]string{}
	targetLabels := map[string]string{}
	kubernetes := config.Kubernetes{}
	skywalking := config.Skywalking{}
	dependencies := []string(nil)
	endpointFormat := config.DefaultEndpointFormat
	if existing != nil {
		applicationDefault = existing.Application
		namespaceDefault = existing.Namespace
		idDefault = existing.ID
		jobDefault = existing.Prometheus.Job
		selectedMetrics = existing.Metrics
		labels = copyNonIdentityLabels(existing.Prometheus.Labels)
		targetLabels = existing.Prometheus.TargetLabels
		kubernetes = existing.Kubernetes
		skywalking = existing.Skywalking
		dependencies = existing.Dependencies
		if existing.Skywalking.EndpointFormat != "" {
			endpointFormat = existing.Skywalking.EndpointFormat
		}
	}
	application, err := w.Prompter.Input("Application", applicationDefault, nonEmpty("Application"))
	if err != nil {
		return config.Application{}, err
	}
	namespace, err := w.Prompter.Input("Namespace", namespaceDefault, nonEmpty("Namespace"))
	if err != nil {
		return config.Application{}, err
	}
	if idDefault == "" {
		idDefault = uniqueID(site, slugify(application+"-"+namespace))
	}
	id, err := w.Prompter.Input("应用配置 ID", idDefault, func(value string) error {
		if err := config.ValidateSlug(value, 35); err != nil {
			return err
		}
		if (existing == nil || value != existing.ID) && w.idTaken(site, value) {
			return fmt.Errorf("ID %q 已被使用", value)
		}
		return nil
	})
	if err != nil {
		return config.Application{}, err
	}
	if jobDefault == "" {
		jobDefault = application
	}
	job, err := w.Prompter.Input("Prometheus Job", jobDefault, nonEmpty("Prometheus Job"))
	if err != nil {
		return config.Application{}, err
	}
	selected, err := w.promptProfiles("启用指标模板", profiles, selectedMetrics)
	if err != nil {
		return config.Application{}, err
	}
	labels["application"] = application
	labels["namespace"] = namespace
	if skywalking.Service == "" {
		skywalking.Service = config.DefaultSkywalkingService(application, namespace)
	} else if existing != nil && skywalking.Service == config.DefaultSkywalkingService(existing.Application, existing.Namespace) &&
		(existing.Application != application || existing.Namespace != namespace) {
		skywalking.Service = config.DefaultSkywalkingService(application, namespace)
	}
	if skywalking.EndpointFormat == "" {
		skywalking.EndpointFormat = endpointFormat
	}
	return config.Application{
		ID:          id,
		Application: application,
		Namespace:   namespace,
		Prometheus: config.Prometheus{
			Job:          job,
			Labels:       labels,
			TargetLabels: targetLabels,
		},
		Skywalking:   skywalking,
		Metrics:      selected,
		Kubernetes:   kubernetes,
		Dependencies: dependencies,
	}, nil
}

func copyNonIdentityLabels(labels map[string]string) map[string]string {
	out := map[string]string{}
	for key, value := range labels {
		switch key {
		case "application", "namespace", "job", "instance", config.SitegenAppLabel:
			continue
		}
		out[key] = value
	}
	return out
}

func (w *Wizard) promptComponents(site *config.Site, preferEdit bool) error {
	componentTypes := []string{"mysql", "redis", "kafka", "nacos", "custom"}
	typeOptions := []string{"MySQL", "Redis", "Kafka", "Nacos", "自定义"}
	if len(site.Spec.Components) == 0 {
		configure, err := w.Prompter.Confirm("是否配置依赖组件?", false)
		if err != nil {
			return err
		}
		if !configure {
			return nil
		}
		component, err := w.collectComponent(site, nil, componentTypes, typeOptions)
		if err != nil {
			return err
		}
		site.Spec.Components = append(site.Spec.Components, component)
		more, err := w.Prompter.Confirm("继续添加依赖组件?", false)
		if err != nil {
			return err
		}
		if !more {
			return nil
		}
	}
	for {
		options := []string{"保留现有依赖组件，继续", "编辑已有组件", "添加新组件", "完成"}
		defaultIndex := 0
		if preferEdit {
			defaultIndex = 1
		}
		choice, err := w.Prompter.Select("依赖组件配置", options, defaultIndex)
		if err != nil {
			return err
		}
		switch choice {
		case 0, 3:
			return nil
		case 1:
			labels := make([]string, 0, len(site.Spec.Components)+1)
			for _, component := range site.Spec.Components {
				labels = append(labels, component.ID+"（"+component.Name+" / "+component.Type+"）")
			}
			labels = append(labels, "取消")
			selected, err := w.Prompter.Select("选择要编辑的组件", labels, 0)
			if err != nil {
				return err
			}
			if selected == len(site.Spec.Components) {
				continue
			}
			edited, err := w.collectComponent(site, &site.Spec.Components[selected], componentTypes, typeOptions)
			if err != nil {
				return err
			}
			site.Spec.Components[selected] = edited
		case 2:
			component, err := w.collectComponent(site, nil, componentTypes, typeOptions)
			if err != nil {
				return err
			}
			site.Spec.Components = append(site.Spec.Components, component)
		}
	}
}

func (w *Wizard) collectComponent(site *config.Site, existing *config.Component, componentTypes, typeOptions []string) (config.Component, error) {
	defaultType := 0
	nameDefault, idDefault, jobDefault, instanceDefault := "", "", "", ""
	var selectedMetrics []string
	var labels map[string]string
	if existing != nil {
		for i, componentType := range componentTypes {
			if componentType == existing.Type {
				defaultType = i
			}
		}
		nameDefault = existing.Name
		idDefault = existing.ID
		jobDefault = existing.Prometheus.Job
		instanceDefault = existing.Prometheus.Instance
		selectedMetrics = existing.Metrics
		labels = existing.Prometheus.Labels
	}
	index, err := w.Prompter.Select("组件类型", typeOptions, defaultType)
	if err != nil {
		return config.Component{}, err
	}
	componentType := componentTypes[index]
	if nameDefault == "" {
		nameDefault = componentDisplayName(componentType) + " " + strconv.Itoa(len(site.Spec.Components)+1)
	}
	name, err := w.Prompter.Input("组件名称", nameDefault, nonEmpty("组件名称"))
	if err != nil {
		return config.Component{}, err
	}
	if idDefault == "" {
		idDefault = uniqueID(site, slugify(name))
	}
	id, err := w.Prompter.Input("组件 ID", idDefault, func(value string) error {
		if err := config.ValidateSlug(value, 35); err != nil {
			return err
		}
		if (existing == nil || value != existing.ID) && w.idTaken(site, value) {
			return fmt.Errorf("ID %q 已被使用", value)
		}
		return nil
	})
	if err != nil {
		return config.Component{}, err
	}
	job, err := w.Prompter.Input("Prometheus Job", jobDefault, nonEmpty("Prometheus Job"))
	if err != nil {
		return config.Component{}, err
	}
	instance, err := w.Prompter.Input("Instance", instanceDefault, nonEmpty("Instance"))
	if err != nil {
		return config.Component{}, err
	}
	profiles := w.Registry.ProfilesForComponent(componentType)
	selected, err := w.promptProfiles("启用组件指标模板", profiles, selectedMetrics)
	if err != nil {
		return config.Component{}, err
	}
	return config.Component{
		ID:   id,
		Name: name,
		Type: componentType,
		Prometheus: config.ComponentPrometheus{
			Job:      job,
			Instance: instance,
			Labels:   labels,
		},
		Metrics: selected,
	}, nil
}

var wizardLabelNameRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

func (w *Wizard) promptTargetLabelsSettings(site *config.Site) error {
	for i := range site.Spec.Applications {
		app := &site.Spec.Applications[i]
		if len(app.Prometheus.TargetLabels) > 0 {
			keep, err := w.Prompter.Confirm("应用 "+app.ID+" 保留现有 up Target Labels?", true)
			if err != nil {
				return err
			}
			if keep {
				continue
			}
			app.Prometheus.TargetLabels = nil
		}
		configure, err := w.Prompter.Confirm("是否为应用 "+app.ID+" 配置 up 指标 Target Labels?", false)
		if err != nil {
			return err
		}
		if !configure {
			continue
		}
		raw, err := w.Prompter.Input("Target Labels（key=value，逗号分隔，仅用于 up）", "", func(value string) error {
			_, err := parseLabels(value)
			return err
		})
		if err != nil {
			return err
		}
		labels, err := parseLabels(raw)
		if err != nil {
			return err
		}
		app.Prometheus.TargetLabels = labels
	}
	return nil
}

func (w *Wizard) promptKubernetesPodSettings(site *config.Site) error {
	any := false
	for i := range site.Spec.Applications {
		app := &site.Spec.Applications[i]
		if !app.HasMetric("kubernetes") {
			continue
		}
		any = true
		if app.Kubernetes.PodSelector != nil {
			keep, err := w.Prompter.Confirm("应用 "+app.ID+" 保留现有 Kubernetes Pod 归属配置?", true)
			if err != nil {
				return err
			}
			if keep {
				continue
			}
			app.Kubernetes.PodSelector = nil
			app.Kubernetes.ContainerMatchers = nil
		}
		if err := w.promptPodSelector(app); err != nil {
			return err
		}
	}
	if !any {
		fmt.Fprintln(w.Out, "没有启用 kubernetes 指标模板的应用，跳过 Pod 归属配置。")
	}
	return nil
}

func (w *Wizard) promptPodSelector(app *config.Application) error {
	for {
		defaultMetric := config.DefaultPodLabelsMetric
		if app.Kubernetes.PodSelector != nil && app.Kubernetes.PodSelector.Metric != "" {
			defaultMetric = app.Kubernetes.PodSelector.Metric
		}
		labelsAvailable := true
		if w.Probe != nil {
			ctx, cancel := probeContext()
			exists, err := w.Probe.MetricExists(ctx, defaultMetric, time.Now().Add(-7*24*time.Hour), time.Now())
			cancel()
			if err != nil {
				fmt.Fprintf(w.Out, "无法连接 Prometheus 检查 %s: %v\n", defaultMetric, err)
			} else if !exists {
				labelsAvailable = false
				fmt.Fprintf(w.Out, "\n指标 %s 在目标 Prometheus 中不存在（近 7 天无序列）。\n", defaultMetric)
				fmt.Fprintln(w.Out, "不能把不存在的 Pod 标签指标当作已配置成功。请选择 Pod 名称正则，或暂不配置。")
			}
		}
		options := []string{"Kubernetes Pod 标签匹配", "Pod 名称正则匹配", "暂不配置，后续手动完善"}
		if !labelsAvailable {
			options = []string{"Pod 名称正则匹配", "暂不配置，后续手动完善"}
		}
		index, err := w.Prompter.Select("Kubernetes 应用 "+app.ID+" 的 Pod 归属方式", options, 0)
		if err != nil {
			return err
		}
		mode := index
		if !labelsAvailable {
			mode = index + 1
		}
		switch mode {
		case 0:
			if err := w.collectPodLabels(app, defaultMetric); err != nil {
				if errors.Is(err, errRetryStep) {
					continue
				}
				return err
			}
			return nil
		case 1:
			if err := w.collectPodRegex(app); err != nil {
				if errors.Is(err, errRetryStep) {
					continue
				}
				return err
			}
			return nil
		default:
			app.Kubernetes.PodSelector = nil
			app.Kubernetes.ContainerMatchers = nil
			fmt.Fprintln(w.Out, "未配置 Pod 归属规则。应用级 Kubernetes Pod CPU、内存等面板不会生成，")
			fmt.Fprintln(w.Out, "避免将 Namespace 中其他应用的 Pod 计入当前应用。")
			return nil
		}
	}
}

// errRetryStep is internal: the verification step asked the user for a new
// answer within the same wizard stage.
var errRetryStep = errors.New("重新填写当前配置")

func (w *Wizard) collectPodLabels(app *config.Application, metric string) error {
	if w.Probe != nil {
		if names := w.discoverPodLabelNames(app, metric); len(names) > 0 {
			fmt.Fprintf(w.Out, "发现的 Pod 标签（最多 10 个）: %s\n", strings.Join(names, ", "))
		}
	}
	matchers := map[string]string{}
	for {
		name, err := w.Prompter.Input("Pod 标签名称", "", func(value string) error {
			if !wizardLabelNameRE.MatchString(value) {
				return fmt.Errorf("不是合法的 Prometheus 标签名")
			}
			return nil
		})
		if err != nil {
			return err
		}
		value, err := w.Prompter.Input("Pod 标签值", "", nonEmpty("Pod 标签值"))
		if err != nil {
			return err
		}
		matchers[name] = value
		more, err := w.Prompter.Confirm("继续添加 Pod 标签?", false)
		if err != nil {
			return err
		}
		if !more {
			break
		}
	}
	selector := &config.PodSelector{Mode: config.PodSelectorLabels, Metric: metric, Matchers: matchers}
	if w.Probe != nil {
		conditions := []string{`namespace=` + quoteProbeValue(app.Namespace)}
		for _, key := range sortedStringKeys(matchers) {
			conditions = append(conditions, key+"="+quoteProbeValue(matchers[key]))
		}
		ctx, cancel := probeContext()
		series, err := w.Probe.Series(ctx, metric+"{"+strings.Join(conditions, ", ")+"}", time.Now().Add(-time.Hour), time.Now())
		cancel()
		if err != nil {
			fmt.Fprintf(w.Out, "无法验证 Pod 标签选择条件: %v\n", err)
		} else {
			pods := probePodSet(series)
			if len(pods) == 0 {
				fmt.Fprintln(w.Out, "Pod 标签选择条件当前没有匹配到 Pod。")
				ok, err := w.Prompter.Confirm("仍要写入该 Pod 归属配置?", false)
				if err != nil {
					return err
				}
				if !ok {
					return errRetryStep
				}
			} else {
				fmt.Fprintf(w.Out, "试查询匹配 %d 个 Pod（示例：%s）\n", len(pods), probeSamples(pods, 5))
				ok, err := w.Prompter.Confirm("确认使用该 Pod 归属配置?", true)
				if err != nil {
					return err
				}
				if !ok {
					return errRetryStep
				}
			}
		}
	}
	app.Kubernetes.PodSelector = selector
	return nil
}

func (w *Wizard) collectPodRegex(app *config.Application) error {
	for {
		pod, err := w.Prompter.Input("Pod 名称正则（示例：^rtd-admin-.*$）", "", func(value string) error {
			if strings.TrimSpace(value) == "" {
				return fmt.Errorf("不能为空")
			}
			_, err := regexp.Compile(value)
			return err
		})
		if err != nil {
			return err
		}
		if w.Probe != nil {
			pods, err := w.probePodsByRegex(app.Namespace, pod)
			if err != nil {
				fmt.Fprintf(w.Out, "无法试查询 Pod 正则: %v\n", err)
			} else if len(pods) == 0 {
				fmt.Fprintln(w.Out, "Pod 正则在当前 cAdvisor 指标中没有匹配到 Pod。")
				ok, err := w.Prompter.Confirm("仍要写入该正则?", false)
				if err != nil {
					return err
				}
				if !ok {
					continue
				}
			} else {
				fmt.Fprintf(w.Out, "试查询匹配 %d 个 Pod（示例：%s）\n", len(pods), probeSamples(pods, 5))
				ok, err := w.Prompter.Confirm("确认使用该 Pod 正则?", true)
				if err != nil {
					return err
				}
				if !ok {
					continue
				}
			}
		}
		app.Kubernetes.PodSelector = &config.PodSelector{Mode: config.PodSelectorPodRegex, Pod: pod}
		fmt.Fprintln(w.Out, "提示：Deployment 滚动更新后 Pod 名称会变化，请确保正则能覆盖新实例。")
		return nil
	}
}

func (w *Wizard) discoverPodLabelNames(app *config.Application, metric string) []string {
	if w.Probe == nil {
		return nil
	}
	ctx, cancel := probeContext()
	defer cancel()
	series, err := w.Probe.Series(ctx, metric+`{namespace=`+quoteProbeValue(app.Namespace)+`, pod=~".+"}`,
		time.Now().Add(-time.Hour), time.Now())
	if err != nil {
		return nil
	}
	found := map[string]bool{}
	for _, labels := range series {
		for name := range labels {
			key := string(name)
			if strings.HasPrefix(key, "label_") {
				found[key] = true
			}
		}
	}
	var out []string
	for name := range found {
		out = append(out, name)
	}
	sort.Strings(out)
	if len(out) > 10 {
		out = out[:10]
	}
	return out
}

func (w *Wizard) probePodsByRegex(namespace, regex string) (map[string]bool, error) {
	if w.Probe == nil {
		return nil, nil
	}
	pods := map[string]bool{}
	var firstErr error
	for _, metric := range []string{"container_cpu_usage_seconds_total", "container_memory_working_set_bytes"} {
		ctx, cancel := probeContext()
		series, err := w.Probe.Series(ctx,
			metric+`{namespace=`+quoteProbeValue(namespace)+`, pod=~`+quoteProbeValue(regex)+`}`,
			time.Now().Add(-time.Hour), time.Now())
		cancel()
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for pod := range probePodSet(series) {
			pods[pod] = true
		}
	}
	if len(pods) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return pods, nil
}

func (w *Wizard) promptContainerMatcherSettings(site *config.Site) error {
	any := false
	for i := range site.Spec.Applications {
		app := &site.Spec.Applications[i]
		if !app.HasMetric("kubernetes") || app.Kubernetes.PodSelector == nil {
			continue
		}
		any = true
		if len(app.Kubernetes.ContainerMatchers) > 0 {
			keep, err := w.Prompter.Confirm("应用 "+app.ID+" 保留现有容器过滤规则?", true)
			if err != nil {
				return err
			}
			if keep {
				continue
			}
			app.Kubernetes.ContainerMatchers = nil
		}
		configure, err := w.Prompter.Confirm("是否为应用 "+app.ID+" 配置容器过滤规则?", false)
		if err != nil {
			return err
		}
		if !configure {
			continue
		}
		w.showCAdvisorLabels(app)
		if err := w.collectContainerMatchers(app); err != nil {
			return err
		}
	}
	if !any {
		fmt.Fprintln(w.Out, "没有同时启用 kubernetes 指标和 Pod 归属的应用，跳过容器过滤配置。")
	}
	return nil
}

func (w *Wizard) showCAdvisorLabels(app *config.Application) {
	fmt.Fprintln(w.Out, "\n提示：应用 Prometheus Job 不等于 cAdvisor Job；job、instance、namespace、pod 不能作为容器过滤条件。")
	if w.Probe == nil {
		return
	}
	pods := map[string]bool{}
	ctx, cancel := probeContext()
	selector := app.Kubernetes.PodSelector
	switch selector.Mode {
	case config.PodSelectorLabels:
		conditions := []string{`namespace=` + quoteProbeValue(app.Namespace)}
		for _, key := range sortedStringKeys(selector.Matchers) {
			conditions = append(conditions, key+"="+quoteProbeValue(selector.Matchers[key]))
		}
		series, err := w.Probe.Series(ctx, selector.PodLabelMetric()+"{"+strings.Join(conditions, ", ")+"}",
			time.Now().Add(-time.Hour), time.Now())
		if err == nil {
			for pod := range probePodSet(series) {
				pods[pod] = true
			}
		}
	case config.PodSelectorPodRegex:
		regexPods, err := w.probePodsByRegex(app.Namespace, selector.Pod)
		if err == nil {
			pods = regexPods
		}
	}
	cancel()
	ctx, cancel = probeContext()
	series, err := w.Probe.Series(ctx,
		`container_memory_working_set_bytes{namespace=`+quoteProbeValue(app.Namespace)+`, pod=~".+"}`,
		time.Now().Add(-time.Hour), time.Now())
	cancel()
	if err != nil {
		fmt.Fprintf(w.Out, "无法读取 cAdvisor 标签: %v\n", err)
		return
	}
	values := map[string]map[string]bool{}
	for _, labels := range series {
		if len(pods) > 0 && !pods[string(labels["pod"])] {
			continue
		}
		for _, name := range []string{"container", "image", "name", "id"} {
			value, ok := labels[model.LabelName(name)]
			if !ok {
				continue
			}
			if values[name] == nil {
				values[name] = map[string]bool{}
			}
			values[name][string(value)] = true
		}
	}
	if len(values) == 0 {
		fmt.Fprintln(w.Out, "没有发现可用于容器过滤的 container/image/name/id 标签。")
		return
	}
	fmt.Fprintf(w.Out, "应用 Pod 范围内实际发现的 cAdvisor 标签（%d 个 Pod）:\n", len(pods))
	for _, name := range sortedValueKeys(values) {
		fmt.Fprintf(w.Out, "  %s: %s\n", name, probeSamples(values[name], 3))
	}
}

func (w *Wizard) collectContainerMatchers(app *config.Application) error {
	operators := []string{"equal（=）", "not_equal（!=）", "regex（=~）", "not_regex（!~）"}
	operatorValues := []string{config.MatchEqual, config.MatchNotEqual, config.MatchRegex, config.MatchNotRegex}
	var matchers config.ContainerMatchers
	for {
		name, err := w.Prompter.Input("容器过滤标签名称（container/image/name/id）", "", func(value string) error {
			if !wizardLabelNameRE.MatchString(value) {
				return fmt.Errorf("不是合法的 Prometheus 标签名")
			}
			switch value {
			case "job", "instance":
				return fmt.Errorf("job/instance 不能作为容器过滤条件；应用 Prometheus Job 不等于 cAdvisor Job")
			case "namespace", "pod":
				return fmt.Errorf("%s 由应用配置或 Pod 归属规则管理，不能作为容器过滤条件", value)
			}
			return nil
		})
		if err != nil {
			return err
		}
		index, err := w.Prompter.Select("容器过滤操作符", operators, 0)
		if err != nil {
			return err
		}
		operator := operatorValues[index]
		value, err := w.Prompter.Input("容器过滤标签值（not_equal 可留空）", "", func(value string) error {
			switch operator {
			case config.MatchNotEqual:
				return nil
			case config.MatchRegex, config.MatchNotRegex:
				if value == "" {
					return fmt.Errorf("正则不能为空")
				}
				_, err := regexp.Compile(value)
				return err
			default:
				if value == "" {
					return fmt.Errorf("equal 操作符不允许空值；如需排除空标签，请使用 not_equal")
				}
				return nil
			}
		})
		if err != nil {
			return err
		}
		matchers = append(matchers, config.LabelMatcher{Name: name, Operator: operator, Value: value})
		more, err := w.Prompter.Confirm("继续添加容器过滤条件?", false)
		if err != nil {
			return err
		}
		if !more {
			break
		}
	}
	app.Kubernetes.ContainerMatchers = matchers
	return nil
}

func (w *Wizard) promptSkywalkingUISettings(site *config.Site) error {
	for i := range site.Spec.Applications {
		app := &site.Spec.Applications[i]
		ui := app.Skywalking.UI
		if ui.BaseURL != "" || ui.ServiceURLTemplate != "" || ui.EndpointURLTemplate != "" {
			keep, err := w.Prompter.Confirm("应用 "+app.ID+" 保留现有 SkyWalking UI 下钻配置?", true)
			if err != nil {
				return err
			}
			if keep {
				continue
			}
			app.Skywalking.UI = config.SkywalkingUI{}
			ui = app.Skywalking.UI
		}
		configure, err := w.Prompter.Confirm("是否为应用 "+app.ID+" 配置 SkyWalking UI 地址/下钻模板?", false)
		if err != nil {
			return err
		}
		if !configure {
			continue
		}
		baseURL, err := w.Prompter.Input("SkyWalking UI 地址（可留空）", ui.BaseURL, func(value string) error {
			if strings.TrimSpace(value) == "" {
				return nil
			}
			return config.ValidateHTTPURL(value)
		})
		if err != nil {
			return err
		}
		serviceTemplate, err := w.Prompter.Input(
			"Service URL 模板（支持 {service}/{application}/{namespace}/{site}，可留空）", ui.ServiceURLTemplate,
			func(value string) error {
				if strings.TrimSpace(value) == "" {
					return nil
				}
				return config.ValidateURLTemplate(value)
			})
		if err != nil {
			return err
		}
		endpointTemplate, err := w.Prompter.Input(
			"Endpoint URL 模板（支持 {service}/{application}/{namespace}/{site}，可留空）", ui.EndpointURLTemplate,
			func(value string) error {
				if strings.TrimSpace(value) == "" {
					return nil
				}
				return config.ValidateURLTemplate(value)
			})
		if err != nil {
			return err
		}
		app.Skywalking.UI = config.SkywalkingUI{
			BaseURL:             baseURL,
			ServiceURLTemplate:  serviceTemplate,
			EndpointURLTemplate: endpointTemplate,
		}
	}
	return nil
}

// promptEntryApplications binds each domain to the applications that actually
// serve external traffic. It is an explicit relationship, never inferred from
// application names.
func (w *Wizard) promptEntryApplications(site *config.Site) error {
	if len(site.Spec.Domains) == 0 || len(site.Spec.Applications) == 0 {
		return nil
	}
	httpApps := site.ApplicationsWithMetric("http")
	if len(httpApps) == 0 {
		fmt.Fprintln(w.Out, "没有启用 HTTP 指标的应用，跳过入口应用配置。")
		return nil
	}
	for i := range site.Spec.Domains {
		domain := &site.Spec.Domains[i]
		if len(httpApps) == 1 {
			app := httpApps[0]
			selected := contains(domain.EntryApplications, app.ID) || len(domain.EntryApplications) == 0
			ok, err := w.Prompter.Confirm(
				fmt.Sprintf("域名 %s 只有单个 HTTP 应用 %s，是否设为入口应用?", domain.ID, app.ID), selected)
			if err != nil {
				return err
			}
			if ok {
				domain.EntryApplications = []string{app.ID}
			}
			continue
		}
		labels := make([]string, 0, len(httpApps))
		ids := make([]string, 0, len(httpApps))
		defaults := make([]bool, 0, len(httpApps))
		for _, app := range httpApps {
			labels = append(labels, app.ID+"（"+app.Application+"/"+app.Namespace+"）")
			ids = append(ids, app.ID)
			defaults = append(defaults, contains(domain.EntryApplications, app.ID))
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

func (w *Wizard) promptDependencies(site *config.Site) error {
	if len(site.Spec.Applications) == 0 {
		return nil
	}
	type option struct {
		ID    string
		Label string
	}
	options := make([]option, 0, len(site.Spec.Applications)+len(site.Spec.Components))
	for _, app := range site.Spec.Applications {
		options = append(options, option{
			ID:    app.ID,
			Label: "应用 " + app.ID + "（" + app.Application + "/" + app.Namespace + "）",
		})
	}
	for _, component := range site.Spec.Components {
		options = append(options, option{
			ID:    component.ID,
			Label: "组件 " + component.ID + "（" + component.Name + " / " + component.Type + "）",
		})
	}
	for i := range site.Spec.Applications {
		app := &site.Spec.Applications[i]
		var candidates []option
		var labels []string
		for _, candidate := range options {
			if candidate.ID == app.ID {
				continue
			}
			candidates = append(candidates, candidate)
			labels = append(labels, candidate.Label)
		}
		if len(candidates) == 0 {
			continue
		}
		defaults := make([]bool, len(candidates))
		for _, dep := range app.Dependencies {
			for j, candidate := range candidates {
				if candidate.ID == dep {
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

func (w *Wizard) promptProfiles(label string, profiles []*metrics.Profile, selectedIDs []string) ([]string, error) {
	if len(profiles) == 0 {
		return nil, nil
	}
	labels := make([]string, 0, len(profiles))
	defaults := make([]bool, 0, len(profiles))
	for _, profile := range profiles {
		labels = append(labels, profile.ID+" - "+profile.Name)
		defaults = append(defaults, selectedIDs == nil || contains(selectedIDs, profile.ID))
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
		if w.SitesDir != "" && site.Metadata.ID != "" {
			if path, err := SaveDraft(w.SitesDir, site, "metadata"); err == nil && path != "" {
				fmt.Fprintf(w.Out, "配置校验失败；草稿已保存: %s\n可重新执行 sitegen create 恢复并修改。\n", path)
			}
		}
		return nil, fmt.Errorf("配置校验失败，未写入正式文件")
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
		if len(app.Prometheus.TargetLabels) > 0 {
			var pairs []string
			for _, key := range sortedStringKeys(app.Prometheus.TargetLabels) {
				pairs = append(pairs, key+"="+app.Prometheus.TargetLabels[key])
			}
			fmt.Fprintf(w.Out, "    Target Labels: %s\n", strings.Join(pairs, ", "))
		}
		if app.HasMetric("kubernetes") {
			switch {
			case app.Kubernetes.PodSelector == nil:
				fmt.Fprintln(w.Out, "    Kubernetes:")
				fmt.Fprintln(w.Out, "      Pod Selector: 未配置")
				fmt.Fprintln(w.Out, "      应用级 Kubernetes 面板: 不生成")
			case app.Kubernetes.PodSelector.Mode == config.PodSelectorPodRegex:
				fmt.Fprintf(w.Out, "    Kubernetes:\n      Pod Selector: pod-regex %s\n", app.Kubernetes.PodSelector.Pod)
			default:
				fmt.Fprintf(w.Out, "    Kubernetes:\n      Pod Selector: labels %s",
					app.Kubernetes.PodSelector.PodLabelMetric())
				for _, key := range sortedStringKeys(app.Kubernetes.PodSelector.Matchers) {
					fmt.Fprintf(w.Out, " %s=%s", key, app.Kubernetes.PodSelector.Matchers[key])
				}
				fmt.Fprintln(w.Out)
			}
			if len(app.Kubernetes.ContainerMatchers) > 0 {
				fmt.Fprintf(w.Out, "      Container Matcher: 已配置（%d 条）\n", len(app.Kubernetes.ContainerMatchers))
			} else if app.Kubernetes.PodSelector != nil {
				fmt.Fprintln(w.Out, "      Container Matcher: 未配置")
			}
		}
		if app.Skywalking.UI.BaseURL != "" || app.Skywalking.UI.ServiceURLTemplate != "" || app.Skywalking.UI.EndpointURLTemplate != "" {
			fmt.Fprintf(w.Out, "    SkyWalking UI: %s\n", app.Skywalking.UI.BaseURL)
			if app.Skywalking.UI.ServiceURLTemplate != "" {
				fmt.Fprintf(w.Out, "      Service URL 模板: %s\n", app.Skywalking.UI.ServiceURLTemplate)
			}
			if app.Skywalking.UI.EndpointURLTemplate != "" {
				fmt.Fprintf(w.Out, "      Endpoint URL 模板: %s\n", app.Skywalking.UI.EndpointURLTemplate)
			}
		}
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

func probeContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 15*time.Second)
}

func quoteProbeValue(value string) string {
	return strconv.Quote(value)
}

func probePodSet(series []model.LabelSet) map[string]bool {
	out := map[string]bool{}
	for _, labels := range series {
		if pod, ok := labels["pod"]; ok {
			out[string(pod)] = true
		}
	}
	return out
}

func probeSamples(values map[string]bool, limit int) string {
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

func sortedValueKeys(values map[string]map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedStringKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
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
		case "application", "namespace", "job", "instance", config.SitegenAppLabel:
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

func contains(items []string, value string) bool {
	for _, item := range items {
		if item == value {
			return true
		}
	}
	return false
}
