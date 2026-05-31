package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/google/go-github/v50/github"
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
	var capturedPrompt string
	sd := &slugDeriver{
		runAgy: func(ctx context.Context, prompt string) ([]byte, error) {
			capturedPrompt = prompt
			return []byte("  Test_Mock_Slug! \n"), nil
		},
	}

	slug, err := sd.derive(context.Background(), "My Awesome Feature Title")
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

func TestDeriveFeatureSlug_DoubleUnderscores(t *testing.T) {
	sd := &slugDeriver{
		runAgy: func(ctx context.Context, prompt string) ([]byte, error) {
			return []byte("  test__mock__slug \n"), nil
		},
	}

	slug, err := sd.derive(context.Background(), "Title")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedSlug := "test_mock_slug"
	if slug != expectedSlug {
		t.Errorf("expected slug %q, got %q", expectedSlug, slug)
	}
}

func TestDeriveFeatureSlug_InvalidWordCount(t *testing.T) {
	// Case 1: 2 words
	sd1 := &slugDeriver{
		runAgy: func(ctx context.Context, prompt string) ([]byte, error) {
			return []byte("too_short"), nil
		},
	}
	_, err := sd1.derive(context.Background(), "Title")
	if err == nil {
		t.Error("expected error for 2-word slug, got nil")
	}

	// Case 2: 4 words
	sd2 := &slugDeriver{
		runAgy: func(ctx context.Context, prompt string) ([]byte, error) {
			return []byte("this_is_too_long"), nil
		},
	}
	_, err = sd2.derive(context.Background(), "Title")
	if err == nil {
		t.Error("expected error for 4-word slug, got nil")
	}
}

func TestDeriveFeatureSlug_Error(t *testing.T) {
	sd := &slugDeriver{
		runAgy: func(ctx context.Context, prompt string) ([]byte, error) {
			return nil, fmt.Errorf("agy execution failed")
		},
	}

	_, err := sd.derive(context.Background(), "Some title")
	if err == nil {
		t.Error("expected error from deriveFeatureSlug, got nil")
	}
}

func TestEnsureIssueBranchExists_AlreadyExists(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	client := github.NewClient(nil)
	u, _ := url.Parse(server.URL + "/")
	client.BaseURL = u
	client.UploadURL = u

	// Mock endpoint for checking if target branch exists
	mux.HandleFunc("/repos/test-owner/test-repo/git/ref/heads/feature/my-branch", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("expected GET request, got %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"ref": "refs/heads/feature/my-branch", "object": {"sha": "existing_sha"}}`)
	})

	err := ensureIssueBranchExists(context.Background(), client, "test-owner", "test-repo", "feature/my-branch")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEnsureIssueBranchExists_DoesNotExist_CreatesIt(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	client := github.NewClient(nil)
	u, _ := url.Parse(server.URL + "/")
	client.BaseURL = u
	client.UploadURL = u

	// 1. Mock checking target branch exists (should return 404)
	mux.HandleFunc("/repos/test-owner/test-repo/git/ref/heads/feature/my-branch", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message": "Not Found"}`)
	})

	// 2. Mock fetching repository (returns default branch "main")
	mux.HandleFunc("/repos/test-owner/test-repo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"default_branch": "main"}`)
	})

	// 3. Mock fetching default branch reference (returns latest SHA)
	mux.HandleFunc("/repos/test-owner/test-repo/git/ref/heads/main", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"ref": "refs/heads/main", "object": {"sha": "latest_commit_sha_123"}}`)
	})

	// 4. Mock creating new branch reference (returns 201 Created)
	mux.HandleFunc("/repos/test-owner/test-repo/git/refs", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST request, got %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"ref": "refs/heads/feature/my-branch", "object": {"sha": "latest_commit_sha_123"}}`)
	})

	err := ensureIssueBranchExists(context.Background(), client, "test-owner", "test-repo", "feature/my-branch")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
