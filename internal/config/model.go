package config

import "strings"

const (
	// APIVersion is the only supported configuration API version.
	APIVersion = "site-observability/v1"
	// KindSite is the only supported configuration kind.
	KindSite = "Site"

	TargetApplication = "application"
	TargetComponent   = "component"
	TargetDomain      = "domain"

	DefaultEndpointFormat = "{method}:{uri}"
)

// Site is the root of a site configuration document.
type Site struct {
	APIVersion string   `yaml:"apiVersion" json:"apiVersion"`
	Kind       string   `yaml:"kind" json:"kind"`
	Metadata   Metadata `yaml:"metadata" json:"metadata"`
	Spec       Spec     `yaml:"spec" json:"spec"`
}

// Metadata contains site identity information.
type Metadata struct {
	ID          string `yaml:"id" json:"id"`
	Name        string `yaml:"name" json:"name"`
	Environment string `yaml:"environment" json:"environment"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
}

// Spec contains the observable object graph for a site.
type Spec struct {
	Domains      []Domain      `yaml:"domains" json:"domains"`
	Applications []Application `yaml:"applications,omitempty" json:"applications,omitempty"`
	Components   []Component   `yaml:"components,omitempty" json:"components,omitempty"`
	Links        Links         `yaml:"links,omitempty" json:"links,omitempty"`
}

// Domain is an externally probed entry point.
type Domain struct {
	ID       string   `yaml:"id" json:"id"`
	URL      string   `yaml:"url" json:"url"`
	Blackbox Blackbox `yaml:"blackbox" json:"blackbox"`
}

// Blackbox describes the Blackbox Exporter series used for a domain.
type Blackbox struct {
	Job      string `yaml:"job" json:"job"`
	Instance string `yaml:"instance" json:"instance"`
}

// Application is the core unit of monitoring correlation.
type Application struct {
	ID           string     `yaml:"id" json:"id"`
	Application  string     `yaml:"application" json:"application"`
	Namespace    string     `yaml:"namespace" json:"namespace"`
	Prometheus   Prometheus `yaml:"prometheus" json:"prometheus"`
	Skywalking   Skywalking `yaml:"skywalking" json:"skywalking"`
	Metrics      []string   `yaml:"metrics" json:"metrics"`
	Dependencies []string   `yaml:"dependencies,omitempty" json:"dependencies,omitempty"`
}

// Prometheus describes an application selector. Applications are identified
// by application and namespace; instance is intentionally not configurable.
type Prometheus struct {
	Job    string            `yaml:"job" json:"job"`
	Labels map[string]string `yaml:"labels,omitempty" json:"labels,omitempty"`
}

// ComponentPrometheus describes a dependency component's exact series selector.
type ComponentPrometheus struct {
	Job      string            `yaml:"job" json:"job"`
	Instance string            `yaml:"instance" json:"instance"`
	Labels   map[string]string `yaml:"labels,omitempty" json:"labels,omitempty"`
}

// Skywalking contains the service mapping and optional endpoint format.
type Skywalking struct {
	Service        string `yaml:"service" json:"service"`
	EndpointFormat string `yaml:"endpointFormat,omitempty" json:"endpointFormat,omitempty"`
}

// Component is a non-application dependency such as MySQL, Redis or Kafka.
type Component struct {
	ID         string              `yaml:"id" json:"id"`
	Name       string              `yaml:"name" json:"name"`
	Type       string              `yaml:"type" json:"type"`
	Prometheus ComponentPrometheus `yaml:"prometheus" json:"prometheus"`
	Metrics    []string            `yaml:"metrics" json:"metrics"`
}

// Links contains optional observability UI addresses and drill-down targets.
type Links struct {
	Skywalking                    string            `yaml:"skywalking,omitempty" json:"skywalking,omitempty"`
	Prometheus                    string            `yaml:"prometheus,omitempty" json:"prometheus,omitempty"`
	Grafana                       string            `yaml:"grafana,omitempty" json:"grafana,omitempty"`
	SkywalkingServiceURLTemplate  string            `yaml:"skywalkingServiceURLTemplate,omitempty" json:"skywalkingServiceURLTemplate,omitempty"`
	SkywalkingEndpointURLTemplate string            `yaml:"skywalkingEndpointURLTemplate,omitempty" json:"skywalkingEndpointURLTemplate,omitempty"`
	ComponentDashboards           map[string]string `yaml:"componentDashboards,omitempty" json:"componentDashboards,omitempty"`
}

// DefaultSkywalkingService returns the default SkyWalking service name.
func DefaultSkywalkingService(application, namespace string) string {
	return application + "_" + namespace
}

// Normalize fills derived defaults without changing explicitly configured data.
func Normalize(site *Site) {
	if site == nil {
		return
	}
	site.Metadata.ID = strings.TrimSpace(site.Metadata.ID)
	site.Metadata.Name = strings.TrimSpace(site.Metadata.Name)
	site.Metadata.Environment = strings.TrimSpace(site.Metadata.Environment)

	for i := range site.Spec.Domains {
		d := &site.Spec.Domains[i]
		d.ID = strings.TrimSpace(d.ID)
		d.URL = strings.TrimSpace(d.URL)
		d.Blackbox.Job = strings.TrimSpace(d.Blackbox.Job)
		d.Blackbox.Instance = strings.TrimSpace(d.Blackbox.Instance)
	}

	for i := range site.Spec.Applications {
		app := &site.Spec.Applications[i]
		app.ID = strings.TrimSpace(app.ID)
		app.Application = strings.TrimSpace(app.Application)
		app.Namespace = strings.TrimSpace(app.Namespace)
		app.Prometheus.Job = strings.TrimSpace(app.Prometheus.Job)
		app.Skywalking.Service = strings.TrimSpace(app.Skywalking.Service)
		app.Skywalking.EndpointFormat = strings.TrimSpace(app.Skywalking.EndpointFormat)
		if app.Skywalking.Service == "" && app.Application != "" && app.Namespace != "" {
			app.Skywalking.Service = DefaultSkywalkingService(app.Application, app.Namespace)
		}
		if app.Skywalking.EndpointFormat == "" {
			app.Skywalking.EndpointFormat = DefaultEndpointFormat
		}
		if app.Prometheus.Labels == nil {
			app.Prometheus.Labels = map[string]string{}
		}
		if _, ok := app.Prometheus.Labels["application"]; !ok {
			app.Prometheus.Labels["application"] = app.Application
		}
		if _, ok := app.Prometheus.Labels["namespace"]; !ok {
			app.Prometheus.Labels["namespace"] = app.Namespace
		}
	}

	for i := range site.Spec.Components {
		c := &site.Spec.Components[i]
		c.ID = strings.TrimSpace(c.ID)
		c.Name = strings.TrimSpace(c.Name)
		c.Type = strings.TrimSpace(c.Type)
		c.Prometheus.Job = strings.TrimSpace(c.Prometheus.Job)
		c.Prometheus.Instance = strings.TrimSpace(c.Prometheus.Instance)
	}
}

// ApplicationByID returns an application by its stable configuration ID.
func (s *Site) ApplicationByID(id string) (*Application, bool) {
	for i := range s.Spec.Applications {
		if s.Spec.Applications[i].ID == id {
			return &s.Spec.Applications[i], true
		}
	}
	return nil, false
}

// ComponentByID returns a component by its stable configuration ID.
func (s *Site) ComponentByID(id string) (*Component, bool) {
	for i := range s.Spec.Components {
		if s.Spec.Components[i].ID == id {
			return &s.Spec.Components[i], true
		}
	}
	return nil, false
}

// MergeLinks overlays site links on top of tool defaults.
func MergeLinks(base, override Links) Links {
	out := base
	if override.Skywalking != "" {
		out.Skywalking = override.Skywalking
	}
	if override.Prometheus != "" {
		out.Prometheus = override.Prometheus
	}
	if override.Grafana != "" {
		out.Grafana = override.Grafana
	}
	if override.SkywalkingServiceURLTemplate != "" {
		out.SkywalkingServiceURLTemplate = override.SkywalkingServiceURLTemplate
	}
	if override.SkywalkingEndpointURLTemplate != "" {
		out.SkywalkingEndpointURLTemplate = override.SkywalkingEndpointURLTemplate
	}
	if len(override.ComponentDashboards) > 0 {
		out.ComponentDashboards = make(map[string]string, len(base.ComponentDashboards)+len(override.ComponentDashboards))
		for k, v := range base.ComponentDashboards {
			out.ComponentDashboards[k] = v
		}
		for k, v := range override.ComponentDashboards {
			out.ComponentDashboards[k] = v
		}
	}
	return out
}
