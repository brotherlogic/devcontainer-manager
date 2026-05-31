package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
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

func TestRun_ScanAndLaunchIssueContainer(t *testing.T) {
	// Create a temporary container list file
	tmpFile, err := os.CreateTemp("", "container_list_*")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString("test-owner/test-repo\n"); err != nil {
		t.Fatalf("failed to write to temp file: %v", err)
	}
	tmpFile.Close()

	// Mock HTTP server
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	client := github.NewClient(nil)
	u, _ := url.Parse(server.URL + "/")
	client.BaseURL = u
	client.UploadURL = u

	// Inject getGitHubClient provider
	originalProvider := gitHubClientProvider
	defer func() { gitHubClientProvider = originalProvider }()
	gitHubClientProvider = func() (*github.Client, error) {
		return client, nil
	}

	// Mock commandRunner
	originalCommandRunner := commandRunner
	defer func() { commandRunner = originalCommandRunner }()

	var capturedCommands [][]string
	commandRunner = func(name string, args ...string) ([]byte, error) {
		capturedCommands = append(capturedCommands, append([]string{name}, args...))
		if name == devpodExe && len(args) > 0 && args[0] == "list" {
			// Return list containing only the standard container (not the issue container)
			return []byte("test-owner-test-repo Running\n"), nil
		}
		return []byte("success"), nil
	}

	// Mock agy command slug derivation
	originalDeriverRunAgy := defaultDeriver.runAgy
	defer func() { defaultDeriver.runAgy = originalDeriverRunAgy }()
	defaultDeriver.runAgy = func(ctx context.Context, prompt string) ([]byte, error) {
		return []byte("mock_feature_slug"), nil
	}

	// Setup GitHub API mock responses
	// 1. Fetching open issues
	mux.HandleFunc("/repos/test-owner/test-repo/issues", func(w http.ResponseWriter, r *http.Request) {
		t.Logf("Mock GitHub API: GET /repos/test-owner/test-repo/issues called")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `[
			{
				"number": 42,
				"title": "My Awesome Feature",
				"labels": [{"name": "seraphine-feature"}]
			}
		]`)
	})

	// 2. Fetching target branch (returns 200 OK so ensureIssueBranchExists passes immediately)
	mux.HandleFunc("/repos/test-owner/test-repo/git/ref/heads/feature/mock_feature_slug_42", func(w http.ResponseWriter, r *http.Request) {
		t.Logf("Mock GitHub API: GET /repos/test-owner/test-repo/git/ref/heads/feature/mock_feature_slug_42 called")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"ref": "refs/heads/feature/mock_feature_slug_42", "object": {"sha": "latest_sha"}}`)
	})

	// 3. Mock get repo composite SHA (bypass to keep test simple, returning 404/not found or composite SHA)
	mux.HandleFunc("/repos/test-owner/test-repo/contents/.devcontainer/devcontainer.json", func(w http.ResponseWriter, r *http.Request) {
		t.Logf("Mock GitHub API: GET /repos/test-owner/test-repo/contents/.devcontainer/devcontainer.json called")
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/repos/test-owner/test-repo/contents/devcontainer.json", func(w http.ResponseWriter, r *http.Request) {
		t.Logf("Mock GitHub API: GET /repos/test-owner/test-repo/contents/devcontainer.json called")
		w.WriteHeader(http.StatusNotFound)
	})

	cfg := &config{
		once:               true,
		containerList:      tmpFile.Name(),
		maxIssueContainers: 5,
	}

	err = run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify that devpod up was called with the correct parameters
	var devpodUpCalled bool
	for _, cmd := range capturedCommands {
		if cmd[0] == devpodExe && cmd[1] == "up" {
			devpodUpCalled = true
			expectedURL := "https://github.com/test-owner/test-repo@feature/mock_feature_slug_42"
			if cmd[2] != expectedURL {
				t.Errorf("expected URL %q, got %q", expectedURL, cmd[2])
			}
			if cmd[3] != "--id" || cmd[4] != "test-owner-test-repo-issue-42" {
				t.Errorf("expected --id test-owner-test-repo-issue-42, got %v", cmd[3:])
			}
		}
	}

	if !devpodUpCalled {
		t.Error("expected devpod up command to be called for issue 42, but it was not")
	}
}

func TestRun_SkipLaunchIfAlreadyActive(t *testing.T) {
	// Create a temporary container list file
	tmpFile, err := os.CreateTemp("", "container_list_*")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString("test-owner/test-repo\n"); err != nil {
		t.Fatalf("failed to write to temp file: %v", err)
	}
	tmpFile.Close()

	// Mock HTTP server
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	client := github.NewClient(nil)
	u, _ := url.Parse(server.URL + "/")
	client.BaseURL = u
	client.UploadURL = u

	// Inject getGitHubClient provider
	originalProvider := gitHubClientProvider
	defer func() { gitHubClientProvider = originalProvider }()
	gitHubClientProvider = func() (*github.Client, error) {
		return client, nil
	}

	// Mock commandRunner
	originalCommandRunner := commandRunner
	defer func() { commandRunner = originalCommandRunner }()

	var capturedCommands [][]string
	commandRunner = func(name string, args ...string) ([]byte, error) {
		capturedCommands = append(capturedCommands, append([]string{name}, args...))
		if name == "devpod" && len(args) > 0 && args[0] == "list" {
			// Return list containing both standard and issue containers running
			return []byte("test-owner-test-repo Running\ntest-owner-test-repo-issue-42 Running\n"), nil
		}
		return []byte("success"), nil
	}

	// Setup GitHub API mock responses
	mux.HandleFunc("/repos/test-owner/test-repo/issues", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `[
			{
				"number": 42,
				"title": "My Awesome Feature",
				"labels": [{"name": "seraphine-feature"}]
			}
		]`)
	})

	// Bypasses for repository content
	mux.HandleFunc("/repos/test-owner/test-repo/contents/.devcontainer/devcontainer.json", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/repos/test-owner/test-repo/contents/devcontainer.json", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	cfg := &config{
		once:               true,
		containerList:      tmpFile.Name(),
		maxIssueContainers: 5,
	}

	err = run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify that devpod up was NOT called for issue 42
	for _, cmd := range capturedCommands {
		if cmd[0] == devpodExe && cmd[1] == "up" {
			if len(cmd) > 4 && cmd[4] == "test-owner-test-repo-issue-42" {
				t.Errorf("did not expect devpod up to be called for issue 42, but it was")
			}
		}
	}
}

func TestRun_HibernationOfOldestContainers(t *testing.T) {
	// Create a temporary container list file
	tmpFile, err := os.CreateTemp("", "container_list_*")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString("test-owner/test-repo\n"); err != nil {
		t.Fatalf("failed to write to temp file: %v", err)
	}
	tmpFile.Close()

	// Mock HTTP server
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	client := github.NewClient(nil)
	u, _ := url.Parse(server.URL + "/")
	client.BaseURL = u
	client.UploadURL = u

	// Inject getGitHubClient provider
	originalProvider := gitHubClientProvider
	defer func() { gitHubClientProvider = originalProvider }()
	gitHubClientProvider = func() (*github.Client, error) {
		return client, nil
	}

	// Mock commandRunner
	originalCommandRunner := commandRunner
	defer func() { commandRunner = originalCommandRunner }()

	var capturedCommands [][]string
	commandRunner = func(name string, args ...string) ([]byte, error) {
		capturedCommands = append(capturedCommands, append([]string{name}, args...))
		if name == devpodExe && len(args) > 0 && args[0] == "list" {
			// Return list containing standard container and 3 running issue containers
			return []byte("test-owner-test-repo Running\ntest-owner-test-repo-issue-1 Running\ntest-owner-test-repo-issue-2 Running\ntest-owner-test-repo-issue-3 Running\n"), nil
		}
		return []byte("success"), nil
	}

	// Mock GitHub API responses for get issue details
	mux.HandleFunc("/repos/test-owner/test-repo/issues/1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"number": 1, "state": "open", "updated_at": "2026-05-31T12:00:00Z", "labels": [{"name": "seraphine"}]}`)
	})
	mux.HandleFunc("/repos/test-owner/test-repo/issues/2", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"number": 2, "state": "open", "updated_at": "2026-05-31T13:00:00Z", "labels": [{"name": "seraphine"}]}`)
	})
	mux.HandleFunc("/repos/test-owner/test-repo/issues/3", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"number": 3, "state": "open", "updated_at": "2026-05-31T14:00:00Z", "labels": [{"name": "seraphine"}]}`)
	})

	// Bypasses for repository content
	mux.HandleFunc("/repos/test-owner/test-repo/contents/.devcontainer/devcontainer.json", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/repos/test-owner/test-repo/contents/devcontainer.json", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/repos/test-owner/test-repo/issues", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `[]`)
	})

	cfg := &config{
		once:               true,
		containerList:      tmpFile.Name(),
		maxIssueContainers: 2,
	}

	err = run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify that devpod stop was called on the oldest running container (issue 1)
	var stopCommandCalledForIssue1 bool
	for _, cmd := range capturedCommands {
		if cmd[0] == devpodExe && cmd[1] == "stop" && cmd[2] == "test-owner-test-repo-issue-1" {
			stopCommandCalledForIssue1 = true
		}
	}

	if !stopCommandCalledForIssue1 {
		t.Error("expected devpod stop to be called for the oldest container (issue 1) to satisfy hibernation limits, but it was not")
	}
}

func TestRun_CleanupOfClosedOrUnlabeledContainers(t *testing.T) {
	// Create a temporary container list file
	tmpFile, err := os.CreateTemp("", "container_list_*")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString("test-owner/test-repo\n"); err != nil {
		t.Fatalf("failed to write to temp file: %v", err)
	}
	tmpFile.Close()

	// Mock HTTP server
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	client := github.NewClient(nil)
	u, _ := url.Parse(server.URL + "/")
	client.BaseURL = u
	client.UploadURL = u

	// Inject getGitHubClient provider
	originalProvider := gitHubClientProvider
	defer func() { gitHubClientProvider = originalProvider }()
	gitHubClientProvider = func() (*github.Client, error) {
		return client, nil
	}

	// Mock commandRunner
	originalCommandRunner := commandRunner
	defer func() { commandRunner = originalCommandRunner }()

	var capturedCommands [][]string
	commandRunner = func(name string, args ...string) ([]byte, error) {
		capturedCommands = append(capturedCommands, append([]string{name}, args...))
		if name == devpodExe && len(args) > 0 && args[0] == "list" {
			// Return list containing standard container and 1 running issue container (issue 4)
			return []byte("test-owner-test-repo Running\ntest-owner-test-repo-issue-4 Running\n"), nil
		}
		return []byte("success"), nil
	}

	// Mock GitHub API responses: Issue 4 is closed
	mux.HandleFunc("/repos/test-owner/test-repo/issues/4", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"number": 4, "state": "closed", "updated_at": "2026-05-31T12:00:00Z", "labels": []}`)
	})

	// Bypasses for repository content
	mux.HandleFunc("/repos/test-owner/test-repo/contents/.devcontainer/devcontainer.json", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/repos/test-owner/test-repo/contents/devcontainer.json", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/repos/test-owner/test-repo/issues", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `[]`)
	})

	cfg := &config{
		once:               true,
		containerList:      tmpFile.Name(),
		maxIssueContainers: 5,
	}

	err = run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify that devpod stop and devpod delete were called on container 4
	var stopCommandCalled bool
	var deleteCommandCalled bool
	for _, cmd := range capturedCommands {
		if cmd[0] == devpodExe && cmd[1] == "stop" && cmd[2] == "test-owner-test-repo-issue-4" {
			stopCommandCalled = true
		}
		if cmd[0] == devpodExe && cmd[1] == "delete" && cmd[2] == "test-owner-test-repo-issue-4" {
			deleteCommandCalled = true
		}
	}

	if !stopCommandCalled {
		t.Error("expected devpod stop to be called for closed issue 4 container during cleanup, but it was not")
	}
	if !deleteCommandCalled {
		t.Error("expected devpod delete to be called for closed issue 4 container during cleanup, but it was not")
	}
}
