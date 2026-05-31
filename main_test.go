package main

import (
	"context"
	"fmt"
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

func TestDeriveFeatureSlug_Success(t *testing.T) {
	originalRunAgyCommand := runAgyCommand
	defer func() { runAgyCommand = originalRunAgyCommand }()

	var capturedPrompt string
	runAgyCommand = func(ctx context.Context, prompt string) ([]byte, error) {
		capturedPrompt = prompt
		return []byte("  Test_Mock_Slug! \n"), nil
	}

	slug, err := deriveFeatureSlug(context.Background(), "My Awesome Feature Title")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedSlug := "test_mock_slug"
	if slug != expectedSlug {
		t.Errorf("expected slug %q, got %q", expectedSlug, slug)
	}

	expectedPrompt := "Given the GitHub issue title: 'My Awesome Feature Title', generate a 3-word slug summarizing the feature. Output exactly three lowercase words separated by underscores, with no other text, punctuation, or explanation."
	if capturedPrompt != expectedPrompt {
		t.Errorf("expected prompt %q, got %q", expectedPrompt, capturedPrompt)
	}
}

func TestDeriveFeatureSlug_Error(t *testing.T) {
	originalRunAgyCommand := runAgyCommand
	defer func() { runAgyCommand = originalRunAgyCommand }()

	runAgyCommand = func(ctx context.Context, prompt string) ([]byte, error) {
		return nil, fmt.Errorf("agy execution failed")
	}

	_, err := deriveFeatureSlug(context.Background(), "Some title")
	if err == nil {
		t.Error("expected error from deriveFeatureSlug, got nil")
	}
}
