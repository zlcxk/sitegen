package metrics

import (
	"strings"
	"testing"
)

func variableTemplateProfile(query string) *Profile {
	return &Profile{
		ID:     "test-profile",
		Name:   "Test Profile",
		Target: targetApplication,
		Scope:  ScopeApplication,
		Panels: []Panel{{
			ID:              "panel",
			Name:            "Panel",
			Query:           query,
			Unit:            "none",
			Panel:           "stat",
			Sections:        []string{SectionOverview},
			RequiredMetrics: []string{"test_metric"},
		}},
	}
}

func TestTemplateValidatorRejectsJSONInPromQL(t *testing.T) {
	profile := variableTemplateProfile(`sum(test_metric{%SELECTOR%, pod=~"${test_pod:json}"})`)
	err := validateProfile(profile)
	if err == nil || !strings.Contains(err.Error(), ":json") {
		t.Fatalf("expected :json rejection, got %v", err)
	}
}

func TestTemplateValidatorRejectsUnquotedRegexVariable(t *testing.T) {
	profile := variableTemplateProfile(`sum(test_metric{%SELECTOR%, pod=~${test_pod:regex}})`)
	err := validateProfile(profile)
	if err == nil || !strings.Contains(err.Error(), "双引号") {
		t.Fatalf("expected unquoted regex variable rejection, got %v", err)
	}
}

func TestTemplateValidatorAcceptsQuotedRegexVariable(t *testing.T) {
	profile := variableTemplateProfile(`sum(test_metric{%SELECTOR%, pod=~"${test_pod:regex}"})`)
	if err := validateProfile(profile); err != nil {
		t.Fatalf("quoted :regex variable must be accepted: %v", err)
	}
}
