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
	Expr         string     `json:"expr,omitempty"`
	Format       string     `json:"format,omitempty"`
	Instant      bool       `json:"instant,omitempty"`
	LegendFormat string     `json:"legendFormat,omitempty"`
	Range        bool       `json:"range"`
	RefID        string     `json:"refId"`
	// SkyWalking native Node Graph fields.
	Key         string `json:"key,omitempty"`
	Service     string `json:"service,omitempty"`
	Layer       string `json:"layer,omitempty"`
	NodeMetrics string `json:"nodeMetrics,omitempty"`
	EdgeMetrics string `json:"edgeMetrics,omitempty"`
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
	Links      []DataLink        `json:"links,omitempty"`
}

// DataLink is a per-field or per-row dashboard link.
type DataLink struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	TargetBlank bool   `json:"targetBlank,omitempty"`
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

// Variable query type constants. Grafana 11.4 does not serialize a dedicated
// queryType field for Prometheus variables; the type is encoded by the query
// function (query_result / label_values) and the variable Type. Keeping it in
// the model makes the generator's intent explicit and testable.
const (
	VariableQueryTypeCustom      = "custom"
	VariableQueryTypeDatasource  = "datasource"
	VariableQueryTypeQueryResult = "query_result"
	VariableQueryTypeLabelValues = "label_values"
)

type Variable struct {
	AllValue     string           `json:"allValue,omitempty"`
	Current      VariableCurrent  `json:"current"`
	Datasource   *Datasource      `json:"datasource,omitempty"`
	Definition   string           `json:"definition,omitempty"`
	Hide         int              `json:"hide"`
	IncludeAll   bool             `json:"includeAll,omitempty"`
	Label        string           `json:"label,omitempty"`
	Multi        bool             `json:"multi,omitempty"`
	Name         string           `json:"name"`
	Options      []VariableOption `json:"options,omitempty"`
	Query        any              `json:"query"`
	QueryType    string           `json:"-"` // internal, see constants above
	Regex        string           `json:"regex,omitempty"`
	RegexApplyTo string           `json:"regexApplyTo,omitempty"`
	Refresh      int              `json:"refresh,omitempty"`
	Sort         int              `json:"sort,omitempty"`
	Type         string           `json:"type"`
	ValuesFormat string           `json:"valuesFormat,omitempty"`
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
