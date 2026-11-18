package metrics

import (
	"strings"
	"testing"

	"site-observability/internal/config"
)

func TestQuoteLabelValueEscapesPromQLSpecialCharacters(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{`plain`, `"plain"`},
		{`a"b`, `"a\"b"`},
		{`a\b`, `"a\\b"`},
		{"a\nb", `"a\nb"`},
		{"a\rb", `"a\rb"`},
		{"a\tb", `"a\tb"`},
		{"中文", `"中文"`},
	}
	for _, tc := range cases {
		if got := QuoteLabelValue(tc.input); got != tc.want {
			t.Errorf("QuoteLabelValue(%q) = %s, want %s", tc.input, got, tc.want)
		}
	}
}

func TestComponentSelectorWithQuotesAndUnicodeIsValidPromQL(t *testing.T) {
	component := config.Component{
		ID:   "redis",
		Name: "Redis",
		Type: "redis",
		Prometheus: config.ComponentPrometheus{
			Job:      `scrapeConfig/monitoring/redis-exporter`,
			Instance: "redis://主机:6379",
			Labels:   map[string]string{"cluster": `a"b\c`},
		},
		Metrics: []string{"redis-basic"},
	}
	selector := ComponentSelectorFor(component)
	raw := selector.String()
	if !strings.Contains(raw, `cluster="a\"b\\c"`) {
		t.Fatalf("selector did not escape quotes/backslashes: %s", raw)
	}
	if err := ValidateQuery("max(redis_up{" + raw + "})"); err != nil {
		t.Fatalf("generated selector is not valid PromQL: %v\n%s", err, raw)
	}
}

func TestEmptySelectorRendersEmpty(t *testing.T) {
	if got := NewSelector().String(); got != "" {
		t.Fatalf("empty selector rendered %q", got)
	}
	if !NewSelector().Empty() {
		t.Fatal("NewSelector() must be empty")
	}
}
