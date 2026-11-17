package metrics

import "testing"

func TestLoadDefaultRegistry(t *testing.T) {
	reg, err := LoadDefault()
	if err != nil {
		t.Fatalf("LoadDefault() error = %v", err)
	}
	if reg.Version != "1.0.0" {
		t.Fatalf("Version = %q, want 1.0.0", reg.Version)
	}
	for _, id := range []string{"blackbox", "custom-up", "http", "jvm", "kafka-basic", "kubernetes", "mysql-basic", "nacos-basic", "platform", "redis-basic"} {
		if _, ok := reg.Profile(id); !ok {
			t.Errorf("missing profile %q", id)
		}
	}
	if len(reg.ProfilesForApplication()) == 0 {
		t.Error("expected selectable application profiles")
	}
	if len(reg.ProfilesForComponent("mysql")) == 0 {
		t.Error("expected mysql component profiles")
	}
	if _, ok := reg.AutoProfile("blackbox"); !ok {
		t.Error("blackbox must be an automatic profile")
	}
}

func TestRegistryRejectsUnknownPlaceholders(t *testing.T) {
	profile := &Profile{
		ID:     "x",
		Name:   "X",
		Target: targetApplication,
		Scope:  ScopeApplication,
		Panels: []Panel{{
			ID:              "p",
			Name:            "P",
			Query:           `sum(metric{%SELECTOR%, foo="%OTHER%"})`,
			Unit:            "short",
			Panel:           "stat",
			Sections:        []string{SectionOverview},
			RequiredMetrics: []string{"metric"},
		}},
	}
	if err := validateProfile(profile); err == nil {
		t.Fatal("expected placeholder validation error")
	}
}
