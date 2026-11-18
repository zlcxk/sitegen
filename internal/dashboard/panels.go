package dashboard

import (
	"site-observability/internal/metrics"
)

func renderPanel(template metrics.Panel, title, query string, ds *Datasource, dataLinks []DataLink, siteID string) Panel {
	if ds == nil {
		ds = &Datasource{Type: "prometheus", UID: datasourceUID}
	}
	if template.Panel == "nodeGraph" {
		// The reference SkyWalking Node Graph uses a literal layer. The layer
		// variable remains available for Prometheus APM queries; defaulting the
		// native target to GENERAL avoids depending on interpolation of an
		// undocumented target field.
		layer := "GENERAL"
		if template.NodeGraph != nil && template.NodeGraph.Layer != "" {
			layer = template.NodeGraph.Layer
		}
		return Panel{
			Datasource:  ds,
			Description: template.Description,
			ID:          0,
			Title:       title,
			Type:        "nodeGraph",
			Options: map[string]any{
				"edges":           map[string]any{},
				"layoutAlgorithm": "layered",
				"nodes":           map[string]any{},
				"zoomMode":        "cooperative",
			},
			Targets: []Target{{
				Datasource:  *ds,
				EdgeMetrics: template.NodeGraph.EdgeMetrics,
				Key:         "sitegen-topology-" + siteID,
				Layer:       layer,
				NodeMetrics: template.NodeGraph.NodeMetrics,
				Range:       false,
				RefID:       "A",
				Service:     "$service",
			}},
		}
	}

	panel := Panel{
		Datasource:  ds,
		Description: template.Description,
		ID:          0,
		Title:       title,
		Type:        template.Panel,
		Targets: []Target{{
			Datasource:   *ds,
			EditorMode:   "code",
			Expr:         query,
			Instant:      template.Instant,
			LegendFormat: template.LegendFormat,
			Range:        !template.Instant,
			RefID:        "A",
		}},
	}

	field := FieldConfig{
		Defaults: FieldDefaults{
			Unit:  template.Unit,
			Links: dataLinks,
		},
		Overrides: []any{},
	}
	if len(template.Thresholds) > 0 {
		steps := make([]ThresholdStep, 0, len(template.Thresholds))
		for _, step := range template.Thresholds {
			steps = append(steps, ThresholdStep{Color: step.Color, Value: step.Value})
		}
		field.Defaults.Thresholds = &ThresholdsConfig{Mode: "absolute", Steps: steps}
	}

	switch template.Panel {
	case "timeseries":
		field.Defaults.Color = map[string]any{"mode": "palette-classic"}
		custom := map[string]any{
			"axisCenteredZero":  false,
			"axisColorMode":     "text",
			"axisLabel":         "",
			"axisPlacement":     "auto",
			"barAlignment":      0,
			"drawStyle":         "line",
			"fillOpacity":       10,
			"gradientMode":      "none",
			"hideFrom":          map[string]any{"legend": false, "tooltip": false, "viz": false},
			"insertNulls":       false,
			"lineInterpolation": "linear",
			"lineWidth":         1,
			"pointSize":         5,
			"scaleDistribution": map[string]any{"type": "linear"},
			"showPoints":        "never",
			"spanNulls":         false,
			"stacking":          map[string]any{"group": "A", "mode": "none"},
			"thresholdsStyle":   map[string]any{"mode": thresholdStyle(template)},
		}
		field.Defaults.Custom = custom
		panel.Options = map[string]any{
			"legend":  map[string]any{"calcs": []string{}, "displayMode": "list", "placement": "bottom", "showLegend": true},
			"tooltip": map[string]any{"mode": "multi", "sort": "desc"},
		}
	case "stat":
		field.Defaults.Color = map[string]any{"mode": "thresholds"}
		panel.Options = map[string]any{
			"colorMode":   "value",
			"graphMode":   "area",
			"justifyMode": "auto",
			"orientation": "auto",
			"reduceOptions": map[string]any{
				"calcs":  []string{"lastNotNull"},
				"fields": "",
				"values": false,
			},
			"showPercentChange": false,
			"textMode":          "auto",
			"wideLayout":        true,
		}
	case "table":
		field.Defaults.Color = map[string]any{"mode": "thresholds"}
		field.Defaults.Custom = map[string]any{
			"align": "auto",
			"cellOptions": map[string]any{
				"type": "auto",
			},
			"inspect": false,
		}
		panel.Options = map[string]any{
			"cellHeight": "sm",
			"footer":     map[string]any{"countRows": false, "fields": "", "reducer": "sum", "show": false},
			"showHeader": true,
		}
		panel.Targets[0].Format = "table"
	case "bargauge":
		field.Defaults.Color = map[string]any{"mode": "thresholds"}
		panel.Options = map[string]any{
			"displayMode": "gradient",
			"orientation": "horizontal",
			"reduceOptions": map[string]any{
				"calcs":  []string{"lastNotNull"},
				"fields": "",
				"values": false,
			},
			"showUnfilled": true,
		}
	case "gauge":
		field.Defaults.Color = map[string]any{"mode": "thresholds"}
		panel.Options = map[string]any{
			"orientation": "auto",
			"reduceOptions": map[string]any{
				"calcs":  []string{"lastNotNull"},
				"fields": "",
				"values": false,
			},
			"showThresholdLabels":  false,
			"showThresholdMarkers": true,
			"sizing":               "auto",
		}
	}

	panel.FieldConfig = &field
	return panel
}

func thresholdStyle(template metrics.Panel) string {
	if len(template.Thresholds) > 0 {
		return "line+area"
	}
	return "off"
}

func textPanel(title, content string) Panel {
	return Panel{
		GridPos: GridPos{},
		ID:      0,
		Options: map[string]any{
			"code":    map[string]any{"language": "plaintext", "showLineNumbers": false, "showMiniMap": false},
			"content": content,
			"mode":    "markdown",
		},
		Title: title,
		Type:  "text",
	}
}
