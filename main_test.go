package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

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

	// Override polling times for test speed
	oldInterval := pollingInterval
	oldTimeout := pollingTimeout
	pollingInterval = 1 * time.Millisecond
	pollingTimeout = 100 * time.Millisecond
	defer func() {
		pollingInterval = oldInterval
		pollingTimeout = oldTimeout
	}()

	// Mock commandRunner
	originalCommandRunner := commandRunner
	defer func() { commandRunner = originalCommandRunner }()

	var capturedCommands [][]string
	commandRunner = func(name string, args ...string) ([]byte, error) {
		capturedCommands = append(capturedCommands, append([]string{name}, args...))
		if name == devpodExe && len(args) > 0 && args[0] == "list" {
			// Return list containing only the standard container (not the issue container)
			return []byte("test-repo Running\n"), nil
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

	var currentLabels = map[string]bool{
		"seraphine-feature": true,
	}
	var labelUpdates []string
	var labelDeletes []string
	mux.HandleFunc("/repos/test-owner/test-repo/issues/", func(w http.ResponseWriter, r *http.Request) {
		t.Logf("Mock GitHub API (subtree): %s %s called", r.Method, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			var labelsJSON []string
			for l := range currentLabels {
				labelsJSON = append(labelsJSON, fmt.Sprintf(`{"name": "%s"}`, l))
			}
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"number": 42, "title": "My Awesome Feature", "labels": [%s]}`, strings.Join(labelsJSON, ","))
		} else if r.Method == http.MethodPost {
			var labels []string
			if err := json.NewDecoder(r.Body).Decode(&labels); err == nil {
				for _, l := range labels {
					currentLabels[l] = true
					labelUpdates = append(labelUpdates, l)
				}
			}
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `[]`)
		} else if r.Method == http.MethodDelete {
			parts := strings.Split(r.URL.Path, "/")
			if len(parts) > 0 {
				label := parts[len(parts)-1]
				delete(currentLabels, label)
				labelDeletes = append(labelDeletes, label)
			}
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `[]`)
		}
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
	var foundSendKeys bool
	for _, cmd := range capturedCommands {
		if cmd[0] == devpodExe && cmd[1] == "up" {
			devpodUpCalled = true
			expectedURL := "git@github.com:test-owner/test-repo@feature/mock_feature_slug_42"
			if cmd[2] != expectedURL {
				t.Errorf("expected URL %q, got %q", expectedURL, cmd[2])
			}
			if cmd[3] != "--id" || cmd[4] != "test-repo_42" {
				t.Errorf("expected --id test-repo_42, got %v", cmd[3:])
			}
		}
		if cmd[0] == devpodExe && cmd[1] == "ssh" && cmd[2] == "test-repo_42" && cmd[3] == "--command" {
			cmdStr := cmd[4]
			if strings.Contains(cmdStr, "send-keys") && strings.Contains(cmdStr, "Take a look at the status of this issue") {
				foundSendKeys = true
			}
		}
	}

	if !devpodUpCalled {
		t.Error("expected devpod up command to be called for issue 42, but it was not")
	}
	if !foundSendKeys {
		t.Error("expected default issue startup command to be injected, but it was not")
	}

	// Verify label transitions
	var foundCreatingUpdate, foundReadyUpdate bool
	for _, l := range labelUpdates {
		if l == "container-creating" {
			foundCreatingUpdate = true
		}
		if l == "container-ready" {
			foundReadyUpdate = true
		}
	}
	if !foundCreatingUpdate {
		t.Errorf("expected 'container-creating' label to be added, but updates were: %v", labelUpdates)
	}
	if !foundReadyUpdate {
		t.Errorf("expected 'container-ready' label to be added, but updates were: %v", labelUpdates)
	}

	var foundCreatingDelete bool
	for _, l := range labelDeletes {
		if l == "container-creating" {
			foundCreatingDelete = true
		}
	}
	if !foundCreatingDelete {
		t.Errorf("expected 'container-creating' label to be deleted during ready transition, but deletes were: %v", labelDeletes)
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

	// Mock agy command slug derivation
	originalDeriverRunAgy := defaultDeriver.runAgy
	defer func() { defaultDeriver.runAgy = originalDeriverRunAgy }()
	defaultDeriver.runAgy = func(ctx context.Context, prompt string) ([]byte, error) {
		return []byte("mock_feature_slug"), nil
	}

	// Mock commandRunner
	originalCommandRunner := commandRunner
	defer func() { commandRunner = originalCommandRunner }()

	var capturedCommands [][]string
	commandRunner = func(name string, args ...string) ([]byte, error) {
		capturedCommands = append(capturedCommands, append([]string{name}, args...))
		if name == "devpod" && len(args) > 0 && args[0] == "list" {
			// Return list containing both standard and issue containers running
			return []byte("test-repo Running\ntest-repo_42 Running\n"), nil
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
			if len(cmd) > 4 && cmd[4] == "test-repo_42" {
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
			return []byte("test-repo Running\ntest-repo_1 Running\ntest-repo_2 Running\ntest-repo_3 Running\n"), nil
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
		if cmd[0] == devpodExe && cmd[1] == "stop" && cmd[2] == "test-repo_1" {
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
			return []byte("test-repo Running\ntest-repo_4 Running\n"), nil
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
		if cmd[0] == devpodExe && cmd[1] == "stop" && cmd[2] == "test-repo_4" {
			stopCommandCalled = true
		}
		if cmd[0] == devpodExe && cmd[1] == "delete" && cmd[2] == "test-repo_4" {
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

func TestParseFlags_StartupCommand(t *testing.T) {
	cfg, err := parseFlags([]string{"-startup_command", "echo 'hello'"})
	if err != nil {
		t.Fatalf("unexpected error parsing flags: %v", err)
	}

	if cfg.startupCommand != "echo 'hello'" {
		t.Errorf("expected startupCommand to be 'echo \\'hello\\'', got %s", cfg.startupCommand)
	}
}

func TestShellQuote(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"hello", "'hello'"},
		{"hello world", "'hello world'"},
		{"hello 'world'", "'hello '\\''world'\\'''"},
	}

	for _, tc := range tests {
		got := shellQuote(tc.input)
		if got != tc.expected {
			t.Errorf("shellQuote(%q) = %q, expected %q", tc.input, got, tc.expected)
		}
	}
}

func TestRun_InjectStartupCommand(t *testing.T) {
	// Override polling times for test speed
	oldInterval := pollingInterval
	oldTimeout := pollingTimeout
	pollingInterval = 1 * time.Millisecond
	pollingTimeout = 100 * time.Millisecond
	defer func() {
		pollingInterval = oldInterval
		pollingTimeout = oldTimeout
	}()

	tmpFile, err := os.CreateTemp("", "container_list_*")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString("test-owner/test-repo\n"); err != nil {
		t.Fatalf("failed to write to temp file: %v", err)
	}
	tmpFile.Close()

	// Mock commandRunner
	originalCommandRunner := commandRunner
	defer func() { commandRunner = originalCommandRunner }()

	var capturedCommands [][]string
	var hasSessionAttempts int
	commandRunner = func(name string, args ...string) ([]byte, error) {
		capturedCommands = append(capturedCommands, append([]string{name}, args...))
		if name == devpodExe && len(args) > 0 && args[0] == "list" {
			// Container is not running initially
			return []byte(""), nil
		}
		if name == devpodExe && len(args) >= 4 && args[0] == "ssh" && args[2] == "--command" {
			cmdStr := args[3]
			if strings.Contains(cmdStr, "has-session") {
				hasSessionAttempts++
				if hasSessionAttempts < 2 {
					// Fail first attempt to test polling retry
					return []byte("no session"), fmt.Errorf("session not ready")
				}
				// Succeed on second attempt
				return []byte("session exists"), nil
			}
		}
		return []byte("success"), nil
	}

	// Disable GitHub Client to bypass GH calls
	originalProvider := gitHubClientProvider
	defer func() { gitHubClientProvider = originalProvider }()
	gitHubClientProvider = func() (*github.Client, error) {
		return nil, fmt.Errorf("no gh client in this test")
	}

	cfg := &config{
		once:           true,
		containerList:  tmpFile.Name(),
		startupCommand: "echo 'hello'",
	}

	err = run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the captured commands
	var foundUp bool
	var foundHasSession bool
	var foundSendKeys bool
	for _, cmd := range capturedCommands {
		if cmd[0] == devpodExe && cmd[1] == "up" {
			foundUp = true
		}
		if cmd[0] == devpodExe && cmd[1] == "ssh" && cmd[2] == "test-repo" && cmd[3] == "--command" {
			cmdStr := cmd[4]
			if strings.Contains(cmdStr, "has-session") {
				foundHasSession = true
			}
			if strings.Contains(cmdStr, "send-keys") {
				foundSendKeys = true
				expectedSendKeys := "tmux send-keys -t test-repo 'echo '\\''hello'\\''' C-m"
				if cmdStr != expectedSendKeys {
					t.Errorf("expected send-keys command %q, got %q", expectedSendKeys, cmdStr)
				}
			}
		}
	}

	if !foundUp {
		t.Error("expected devpod up to be called")
	}
	if !foundHasSession {
		t.Error("expected tmux has-session to be polled")
	}
	if !foundSendKeys {
		t.Error("expected tmux send-keys to be called")
	}
	if hasSessionAttempts < 2 {
		t.Errorf("expected at least 2 attempts to check session, got %d", hasSessionAttempts)
	}
}

func TestRun_InjectStartupCommandTimeout(t *testing.T) {
	// Override polling times for test speed
	oldInterval := pollingInterval
	oldTimeout := pollingTimeout
	pollingInterval = 1 * time.Millisecond
	pollingTimeout = 10 * time.Millisecond
	defer func() {
		pollingInterval = oldInterval
		pollingTimeout = oldTimeout
	}()

	tmpFile, err := os.CreateTemp("", "container_list_*")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString("test-owner/test-repo\n"); err != nil {
		t.Fatalf("failed to write to temp file: %v", err)
	}
	tmpFile.Close()

	// Mock commandRunner
	originalCommandRunner := commandRunner
	defer func() { commandRunner = originalCommandRunner }()

	var capturedCommands [][]string
	commandRunner = func(name string, args ...string) ([]byte, error) {
		capturedCommands = append(capturedCommands, append([]string{name}, args...))
		if name == devpodExe && len(args) > 0 && args[0] == "list" {
			// Container is not running initially
			return []byte(""), nil
		}
		if name == devpodExe && len(args) >= 4 && args[0] == "ssh" && args[2] == "--command" {
			cmdStr := args[3]
			if strings.Contains(cmdStr, "has-session") {
				// Always fail to force a timeout
				return []byte("no session"), fmt.Errorf("session not ready")
			}
		}
		return []byte("success"), nil
	}

	// Disable GitHub Client to bypass GH calls
	originalProvider := gitHubClientProvider
	defer func() { gitHubClientProvider = originalProvider }()
	gitHubClientProvider = func() (*github.Client, error) {
		return nil, fmt.Errorf("no gh client in this test")
	}

	cfg := &config{
		once:           true,
		containerList:  tmpFile.Name(),
		startupCommand: "echo 'hello'",
	}

	err = run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify that send-keys was NOT called
	for _, cmd := range capturedCommands {
		if cmd[0] == devpodExe && cmd[1] == "ssh" && cmd[2] == "test-repo" && cmd[3] == "--command" {
			cmdStr := cmd[4]
			if strings.Contains(cmdStr, "send-keys") {
				t.Error("did not expect send-keys to be called when polling times out")
			}
		}
	}
}

func TestRun_ScanAndLaunchIssueContainer_Failure(t *testing.T) {
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

	// Mock commandRunner - make devpod up fail
	originalCommandRunner := commandRunner
	defer func() { commandRunner = originalCommandRunner }()

	commandRunner = func(name string, args ...string) ([]byte, error) {
		if name == devpodExe && len(args) > 0 && args[0] == "list" {
			return []byte("test-repo Running\n"), nil
		}
		if name == devpodExe && len(args) > 0 && args[0] == "up" {
			return []byte("failed to start container"), fmt.Errorf("up failed")
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

	var currentLabels = map[string]bool{
		"seraphine-feature": true,
	}
	var labelUpdates []string
	var labelDeletes []string
	mux.HandleFunc("/repos/test-owner/test-repo/issues/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			var labelsJSON []string
			for l := range currentLabels {
				labelsJSON = append(labelsJSON, fmt.Sprintf(`{"name": "%s"}`, l))
			}
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"number": 42, "title": "My Awesome Feature", "labels": [%s]}`, strings.Join(labelsJSON, ","))
		} else if r.Method == http.MethodPost {
			var labels []string
			if err := json.NewDecoder(r.Body).Decode(&labels); err == nil {
				for _, l := range labels {
					currentLabels[l] = true
					labelUpdates = append(labelUpdates, l)
				}
			}
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `[]`)
		} else if r.Method == http.MethodDelete {
			parts := strings.Split(r.URL.Path, "/")
			if len(parts) > 0 {
				label := parts[len(parts)-1]
				delete(currentLabels, label)
				labelDeletes = append(labelDeletes, label)
			}
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `[]`)
		}
	})

	mux.HandleFunc("/repos/test-owner/test-repo/git/ref/heads/feature/mock_feature_slug_42", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"ref": "refs/heads/feature/mock_feature_slug_42", "object": {"sha": "latest_sha"}}`)
	})

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

	// Verify label transitions
	var foundCreatingUpdate, foundFailedUpdate bool
	for _, l := range labelUpdates {
		if l == "container-creating" {
			foundCreatingUpdate = true
		}
		if l == "container-failed" {
			foundFailedUpdate = true
		}
	}
	if !foundCreatingUpdate {
		t.Errorf("expected 'container-creating' label update, got %v", labelUpdates)
	}
	if !foundFailedUpdate {
		t.Errorf("expected 'container-failed' label update, got %v", labelUpdates)
	}

	// Check if container-creating was deleted
	var foundCreatingDelete bool
	for _, l := range labelDeletes {
		if l == "container-creating" {
			foundCreatingDelete = true
		}
	}
	if !foundCreatingDelete {
		t.Errorf("expected 'container-creating' label to be deleted on failure transition, got %v", labelDeletes)
	}
}

func TestRun_RecreateIssueContainerOnHashChange(t *testing.T) {
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

	// Override getConfigDir for test isolation
	originalGetConfigDir := getConfigDir
	tempDir := t.TempDir()
	getConfigDir = func() string {
		return tempDir
	}
	defer func() { getConfigDir = originalGetConfigDir }()

	// Write an initial tracked SHA for the issue container
	trackedSHAs := loadTrackedSHAs()
	containerID := "test-repo_42"
	trackedSHAs[containerID] = "devcontainer.json:old_sha_123"
	if err := saveTrackedSHAs(trackedSHAs); err != nil {
		t.Fatalf("failed to save mock tracked SHAs: %v", err)
	}

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

	// Override polling times for speed
	oldInterval := pollingInterval
	oldTimeout := pollingTimeout
	pollingInterval = 1 * time.Millisecond
	pollingTimeout = 100 * time.Millisecond
	defer func() {
		pollingInterval = oldInterval
		pollingTimeout = oldTimeout
	}()

	// Mock agy command slug derivation
	originalDeriverRunAgy := defaultDeriver.runAgy
	defer func() { defaultDeriver.runAgy = originalDeriverRunAgy }()
	defaultDeriver.runAgy = func(ctx context.Context, prompt string) ([]byte, error) {
		return []byte("mock_feature_slug"), nil
	}

	// Mock commandRunner
	originalCommandRunner := commandRunner
	defer func() { commandRunner = originalCommandRunner }()

	var capturedCommands [][]string
	commandRunner = func(name string, args ...string) ([]byte, error) {
		capturedCommands = append(capturedCommands, append([]string{name}, args...))
		if name == devpodExe && len(args) > 0 && args[0] == "list" {
			// Container is already running
			return []byte("test-repo Running\ntest-repo_42 Running\n"), nil
		}
		return []byte("success"), nil
	}

	// Setup GitHub API mock responses
	// 1. Issues list
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

	// 2. Issue detail (to keep it open and active, preventing cleanup)
	mux.HandleFunc("/repos/test-owner/test-repo/issues/42", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"number": 42, "state": "open", "updated_at": "2026-05-31T12:00:00Z", "labels": [{"name": "seraphine"}]}`)
	})

	// 3. New devcontainer file contents to produce a NEW SHA (new_sha_456)
	mux.HandleFunc("/repos/test-owner/test-repo/contents/.devcontainer/devcontainer.json", func(w http.ResponseWriter, r *http.Request) {
		// Verify the requested reference contains the branch name we expect
		ref := r.URL.Query().Get("ref")
		if ref == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		expectedRef := "feature/mock_feature_slug_42"
		if ref != expectedRef {
			t.Errorf("expected ref parameter %q, got %q", expectedRef, ref)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"type": "file", "encoding": "base64", "content": "e30=", "sha": "new_sha_456"}`) // e30= is base64 for "{}"
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

	// Verify that the container was recreated (deleted and launched again)
	var deleted bool
	var recreated bool
	for _, cmd := range capturedCommands {
		if cmd[0] == devpodExe && cmd[1] == "delete" && cmd[2] == "test-repo_42" {
			deleted = true
		}
		if cmd[0] == devpodExe && cmd[1] == "up" {
			expectedURL := "git@github.com:test-owner/test-repo@feature/mock_feature_slug_42"
			if len(cmd) > 2 && cmd[2] == expectedURL && cmd[4] == "test-repo_42" {
				recreated = true
			}
		}
	}

	if !deleted {
		t.Error("expected container test-repo_42 to be deleted before recreation, but it was not")
	}
	if !recreated {
		t.Error("expected container test-repo_42 to be recreated (up) with the issue branch, but it was not")
	}

	// Verify that tracked SHA got updated in the file
	updatedTracked := loadTrackedSHAs()
	expectedTrackedSHA := "devcontainer.json:new_sha_456"
	if updatedTracked[containerID] != expectedTrackedSHA {
		t.Errorf("expected updated tracked SHA %q, got %q", expectedTrackedSHA, updatedTracked[containerID])
	}
}

func TestRenameDockerContainer_Success(t *testing.T) {
	originalCommandRunner := commandRunner
	defer func() { commandRunner = originalCommandRunner }()

	var capturedCommands [][]string
	commandRunner = func(name string, args ...string) ([]byte, error) {
		capturedCommands = append(capturedCommands, append([]string{name}, args...))
		if name == "docker" && len(args) > 0 && args[0] == "ps" {
			// Return mock docker ps output with Devpod labels matching our container ID
			return []byte("container_id_123|devpod-temp-name|some-image|sh.loft.devpod.workspace.id=test-repo_42,some-other-label\n"), nil
		}
		return []byte("success"), nil
	}

	renameDockerContainer("test-repo_42")

	// Verify that docker rename was called with correct parameters
	var dockerRenameCalled bool
	for _, cmd := range capturedCommands {
		if cmd[0] == "docker" && cmd[1] == "rename" {
			dockerRenameCalled = true
			if cmd[2] != "container_id_123" {
				t.Errorf("expected source container ID 'container_id_123', got %q", cmd[2])
			}
			if cmd[3] != "test-repo_42" {
				t.Errorf("expected target container name 'test-repo_42', got %q", cmd[3])
			}
		}
	}

	if !dockerRenameCalled {
		t.Error("expected docker rename command to be called, but it was not")
	}
}

func TestRenameDockerContainer_AlreadyNamedCorrectly(t *testing.T) {
	originalCommandRunner := commandRunner
	defer func() { commandRunner = originalCommandRunner }()

	var capturedCommands [][]string
	commandRunner = func(name string, args ...string) ([]byte, error) {
		capturedCommands = append(capturedCommands, append([]string{name}, args...))
		if name == "docker" && len(args) > 0 && args[0] == "ps" {
			return []byte("container_id_123|test-repo_42|some-image|sh.loft.devpod.workspace.id=test-repo_42,some-other-label\n"), nil
		}
		return []byte("success"), nil
	}

	renameDockerContainer("test-repo_42")

	// Verify that docker rename was NOT called
	for _, cmd := range capturedCommands {
		if cmd[0] == "docker" && cmd[1] == "rename" {
			t.Error("expected docker rename command NOT to be called, but it was")
		}
	}
}

func TestReportStartupFailure_NoPreexistingIssue(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	client := github.NewClient(nil)
	u, _ := url.Parse(server.URL + "/")
	client.BaseURL = u
	client.UploadURL = u

	var listCalled, createCalled bool

	// Mock both list and create
	mux.HandleFunc("/repos/test-owner/test-repo/issues", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			createCalled = true
			var req github.IssueRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("failed to decode request body: %v", err)
			}
			if req.GetTitle() != "Issue Container Startup Failed" {
				t.Errorf("expected title 'Issue Container Startup Failed', got %q", req.GetTitle())
			}
			if len(*req.Labels) != 1 || (*req.Labels)[0] != "seraphine-bug" {
				t.Errorf("expected labels ['seraphine-bug'], got %v", req.Labels)
			}
			body := req.GetBody()
			if !strings.Contains(body, "feature/my-branch_42") {
				t.Errorf("expected body to contain branch, got %q", body)
			}
			if !strings.Contains(body, "#42") {
				t.Errorf("expected body to contain original issue ref, got %q", body)
			}
			if !strings.Contains(body, "some startup error") {
				t.Errorf("expected body to contain startup error, got %q", body)
			}
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"number": 99}`)
			return
		}
		listCalled = true
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `[{"number": 1, "title": "Some other issue", "state": "open"}]`)
	})

	reportStartupFailure(context.Background(), client, "test-owner", "test-repo", "feature/my-branch_42", 42, fmt.Errorf("some startup error"), "some log output")

	if !listCalled {
		t.Error("expected list issues to be called")
	}
	if !createCalled {
		t.Error("expected create issue to be called")
	}
}

func TestReportStartupFailure_PreexistingIssueExists(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	client := github.NewClient(nil)
	u, _ := url.Parse(server.URL + "/")
	client.BaseURL = u
	client.UploadURL = u

	var listCalled, createCalled bool

	mux.HandleFunc("/repos/test-owner/test-repo/issues", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			createCalled = true
			w.WriteHeader(http.StatusCreated)
			return
		}
		listCalled = true
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `[{"number": 99, "title": "Issue Container Startup Failed", "state": "open"}]`)
	})

	reportStartupFailure(context.Background(), client, "test-owner", "test-repo", "feature/my-branch_42", 42, fmt.Errorf("some startup error"), "some log output")

	if !listCalled {
		t.Error("expected list issues to be called")
	}
	if createCalled {
		t.Error("expected create issue NOT to be called since one already exists")
	}
}

func TestWithGitHubRetry_SuccessAfterRetry(t *testing.T) {
	var callCount int
	err := withGitHubRetry(context.Background(), func() (*github.Response, error) {
		callCount++
		if callCount < 3 {
			resp := &github.Response{
				Response: &http.Response{
					StatusCode: http.StatusTooManyRequests,
				},
			}
			return resp, fmt.Errorf("rate limit exceeded")
		}
		resp := &github.Response{
			Response: &http.Response{
				StatusCode: http.StatusOK,
			},
		}
		return resp, nil
	})

	if err != nil {
		t.Fatalf("expected successful execution after retries, got error: %v", err)
	}
	if callCount != 3 {
		t.Errorf("expected 3 calls, got %d", callCount)
	}
}


