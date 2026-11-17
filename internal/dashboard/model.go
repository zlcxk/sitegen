package dashboard

import "encoding/json"

// Dashboard is the strongly typed Grafana dashboard document emitted by
// sitegen. Fields not used by the generator are intentionally omitted.
type Dashboard struct {
	Annotations          Annotations     `json:"annotations"`
	Description          string          `json:"description,omitempty"`
	Editable             bool            `json:"editable"`
	FiscalYearStartMonth int             `json:"fiscalYearStartMonth"`
	GraphTooltip         int             `json:"graphTooltip"`
	ID                   any             `json:"id"`
	Links                []DashboardLink `json:"links,omitempty"`
	Panels               []Panel         `json:"panels"`
	Refresh              string          `json:"refresh"`
	SchemaVersion        int             `json:"schemaVersion"`
	Tags                 []string        `json:"tags"`
	Templating           Templating      `json:"templating"`
	Time                 TimeRange       `json:"time"`
	Timepicker           map[string]any  `json:"timepicker"`
	Timezone             string          `json:"timezone"`
	Title                string          `json:"title"`
	UID                  string          `json:"uid"`
	Version              int             `json:"version"`
	WeekStart            string          `json:"weekStart"`
}

type Annotations struct {
	List []map[string]any `json:"list"`
}

type TimeRange struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type Datasource struct {
	Type string `json:"type"`
	UID  string `json:"uid"`
}

type GridPos struct {
	H int `json:"h"`
	W int `json:"w"`
	X int `json:"x"`
	Y int `json:"y"`
}

type Panel struct {
	Collapsed   *bool          `json:"collapsed,omitempty"`
	Datasource  *Datasource    `json:"datasource,omitempty"`
	Description string         `json:"description,omitempty"`
	FieldConfig *FieldConfig   `json:"fieldConfig,omitempty"`
	GridPos     GridPos        `json:"gridPos"`
	ID          int            `json:"id"`
	Links       []PanelLink    `json:"links,omitempty"`
	Options     map[string]any `json:"options,omitempty"`
	Panels      *[]Panel       `json:"panels,omitempty"`
	Targets     []Target       `json:"targets,omitempty"`
	Title       string         `json:"title"`
	Type        string         `json:"type"`
}

type Target struct {
	Datasource   Datasource `json:"datasource"`
	EditorMode   string     `json:"editorMode,omitempty"`
	Expr         string     `json:"expr"`
	Format       string     `json:"format,omitempty"`
	Instant      bool       `json:"instant,omitempty"`
	LegendFormat string     `json:"legendFormat,omitempty"`
	Range        bool       `json:"range"`
	RefID        string     `json:"refId"`
}

type PanelLink struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	TargetBlank bool   `json:"targetBlank"`
}

type DashboardLink struct {
	AsDropdown  bool     `json:"asDropdown"`
	Icon        string   `json:"icon"`
	IncludeVars bool     `json:"includeVars"`
	KeepTime    bool     `json:"keepTime"`
	Tags        []string `json:"tags"`
	TargetBlank bool     `json:"targetBlank"`
	Title       string   `json:"title"`
	Tooltip     string   `json:"tooltip"`
	Type        string   `json:"type"`
	URL         string   `json:"url"`
}

type FieldConfig struct {
	Defaults  FieldDefaults `json:"defaults"`
	Overrides []any         `json:"overrides"`
}

type FieldDefaults struct {
	Color      map[string]any    `json:"color,omitempty"`
	Custom     map[string]any    `json:"custom,omitempty"`
	Decimals   *int              `json:"decimals,omitempty"`
	Max        *float64          `json:"max,omitempty"`
	Min        *float64          `json:"min,omitempty"`
	Thresholds *ThresholdsConfig `json:"thresholds,omitempty"`
	Unit       string            `json:"unit,omitempty"`
	Mappings   []map[string]any  `json:"mappings,omitempty"`
}

type ThresholdsConfig struct {
	Mode  string          `json:"mode"`
	Steps []ThresholdStep `json:"steps"`
}

type ThresholdStep struct {
	Color string   `json:"color"`
	Value *float64 `json:"value"`
}

type Templating struct {
	List []Variable `json:"list"`
}

type Variable struct {
	AllValue   string           `json:"allValue,omitempty"`
	Current    VariableCurrent  `json:"current"`
	Datasource *Datasource      `json:"datasource,omitempty"`
	Definition string           `json:"definition,omitempty"`
	Hide       int              `json:"hide"`
	IncludeAll bool             `json:"includeAll,omitempty"`
	Label      string           `json:"label,omitempty"`
	Multi      bool             `json:"multi,omitempty"`
	Name       string           `json:"name"`
	Options    []VariableOption `json:"options,omitempty"`
	Query      any              `json:"query"`
	Refresh    int              `json:"refresh,omitempty"`
	Sort       int              `json:"sort,omitempty"`
	Type       string           `json:"type"`
}

type VariableCurrent struct {
	Selected bool   `json:"selected"`
	Text     string `json:"text"`
	Value    any    `json:"value"`
}

type VariableOption struct {
	Selected bool   `json:"selected"`
	Text     string `json:"text"`
	Value    string `json:"value"`
}

// rawDashboard is only used to freeze the embedded base template before
// applying typed generation fields.
type rawDashboard json.RawMessage
