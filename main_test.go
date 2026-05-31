package main

import (
	"testing"
)

func TestParseFlags_Defaults(t *testing.T) {
	cfg, err := parseFlags([]string{})
	if err != nil {
		t.Fatalf("unexpected error parsing flags: %v", err)
	}

	if cfg.once != false {
		t.Errorf("expected once to be false, got %v", cfg.once)
	}

	if cfg.containerList != "container.list.template" {
		t.Errorf("expected containerList to be 'container.list.template', got %s", cfg.containerList)
	}

	if cfg.maxIssueContainers != 5 {
		t.Errorf("expected maxIssueContainers to be 5, got %d", cfg.maxIssueContainers)
	}
}

func TestParseFlags_ExplicitValue(t *testing.T) {
	cfg, err := parseFlags([]string{"-max_issue_containers", "10", "-once", "-container_list", "custom.list"})
	if err != nil {
		t.Fatalf("unexpected error parsing flags: %v", err)
	}

	if cfg.once != true {
		t.Errorf("expected once to be true, got %v", cfg.once)
	}

	if cfg.containerList != "custom.list" {
		t.Errorf("expected containerList to be 'custom.list', got %s", cfg.containerList)
	}

	if cfg.maxIssueContainers != 10 {
		t.Errorf("expected maxIssueContainers to be 10, got %d", cfg.maxIssueContainers)
	}
}

func TestParseFlags_InvalidValue(t *testing.T) {
	_, err := parseFlags([]string{"-max_issue_containers", "invalid_int"})
	if err == nil {
		t.Error("expected error parsing invalid max_issue_containers flag, got nil")
	}
}
