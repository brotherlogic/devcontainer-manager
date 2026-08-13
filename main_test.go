package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brotherlogic/devcontainer-manager/proto"
	srvPkg "github.com/brotherlogic/devcontainer-manager/server"
	"github.com/google/go-github/v50/github"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

	if cfg.maxConcurrency != 10 {
		t.Errorf("expected maxConcurrency to be 10, got %d", cfg.maxConcurrency)
	}
}

func TestParseFlags_ExplicitValue(t *testing.T) {
	cfg, err := parseFlags([]string{"-max_issue_containers", "10", "-once", "-container_list", "custom.list", "-max-concurrency", "15"})
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

	if cfg.maxConcurrency != 15 {
		t.Errorf("expected maxConcurrency to be 15, got %d", cfg.maxConcurrency)
	}
}

func TestParseFlags_InvalidValue(t *testing.T) {
	_, err := parseFlags([]string{"-max_issue_containers", "invalid_int"})
	if err == nil {
		t.Error("expected error parsing invalid max_issue_containers flag, got nil")
	}
}

func TestParseFlags_MaxConcurrencyInvalidValue(t *testing.T) {
	_, err := parseFlags([]string{"-max-concurrency", "invalid_int"})
	if err == nil {
		t.Error("expected error parsing invalid max-concurrency flag, got nil")
	}
}

func TestDeriveFeatureSlug(t *testing.T) {
	slug, err := deriveFeatureSlug("My Awesome Feature Title")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedSlug := "my_awesome_feature"
	if slug != expectedSlug {
		t.Errorf("expected slug %q, got %q", expectedSlug, slug)
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
			return []byte(`[{"id": "test-repo"}]`), nil
		}
		return []byte("success"), nil
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
				"labels": [{"name": "seraphine-feature"}],
				"assignees": [{"login": "user1"}],
				"created_at": "2023-01-01T00:00:00Z"
			}
		]`)
	})

	var latencyCommentPosted bool
	mux.HandleFunc("/repos/test-owner/test-repo/issues/42/comments", func(w http.ResponseWriter, r *http.Request) {
		t.Logf("Mock GitHub API: %s %s called", r.Method, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `[]`)
		} else if r.Method == http.MethodPost {
			latencyCommentPosted = true
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
		}
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
	mux.HandleFunc("/repos/test-owner/test-repo/git/ref/heads/feature/my_awesome_feature_42", func(w http.ResponseWriter, r *http.Request) {
		t.Logf("Mock GitHub API: GET /repos/test-owner/test-repo/git/ref/heads/feature/my_awesome_feature_42 called")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"ref": "refs/heads/feature/my_awesome_feature_42", "object": {"sha": "latest_sha"}}`)
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
			expectedURL := "git@github.com:test-owner/test-repo@feature/my_awesome_feature_42"
			if cmd[2] != expectedURL {
				t.Errorf("expected URL %q, got %q", expectedURL, cmd[2])
			}
			if cmd[3] != "--id" || cmd[4] != "test-repo-42" {
				t.Errorf("expected --id test-repo-42, got %v", cmd[3:])
			}
		}
		if cmd[0] == devpodExe && cmd[1] == "ssh" && cmd[2] == "test-repo-42" && cmd[3] == "--command" {
			cmdStr := cmd[4]
			if strings.Contains(cmdStr, "send-keys") && strings.Contains(cmdStr, "Take a look at the status of issue #42") {
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

	if !latencyCommentPosted {
		t.Errorf("expected latency comment to be posted upon successful provisioning, but it was not")
	}

	// Verify ManagerService.List and proto.DevcontainerConfig
	mgrServer := srvPkg.NewServer(globalCache, nil)
	listResp, listErr := mgrServer.List(context.Background(), &proto.ListRequest{})
	if listErr != nil {
		t.Fatalf("unexpected error calling List: %v", listErr)
	}

	var foundIssueContainer bool
	for _, cfg := range listResp.Configs {
		if cfg.Id == "test-repo-42" {
			foundIssueContainer = true
			if cfg.State != proto.State_DCM_READY {
				t.Errorf("expected state DCM_READY, got %v", cfg.State)
			}
			if cfg.Request == nil {
				t.Errorf("expected request to be populated")
			} else if cfg.Request.Identifier == nil {
				t.Errorf("expected request identifier to be populated")
			} else {
				if issueId, ok := cfg.Request.Identifier.Id.(*proto.Identifier_IssueNumber); ok {
					if issueId.IssueNumber != 42 {
						t.Errorf("expected issue number 42, got %d", issueId.IssueNumber)
					}
				} else {
					t.Errorf("expected identifier to be IssueNumber, got %T", cfg.Request.Identifier.Id)
				}
			}
		}
	}
	if !foundIssueContainer {
		t.Error("expected test-repo-42 in ManagerService.List response")
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
			return []byte(`[{"id": "test-repo"}, {"id": "test-repo-42"}]`), nil
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
				"labels": [{"name": "seraphine-feature"}],
				"assignees": [{"login": "user1"}]
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
			if len(cmd) > 4 && cmd[4] == "test-repo-42" {
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
			return []byte(`[{"id": "test-repo"}, {"id": "test-repo-1"}, {"id": "test-repo-2"}, {"id": "test-repo-3"}]`), nil
		}
		return []byte("success"), nil
	}

	// Mock GitHub API responses for get issue details
	mux.HandleFunc("/repos/test-owner/test-repo/issues/1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"number": 1, "state": "open", "updated_at": "2026-05-31T12:00:00Z", "labels": [{"name": "seraphine"}], "assignees": [{"login": "user1"}]}`)
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

	var addedLabels []string
	mux.HandleFunc("/repos/test-owner/test-repo/issues/1/labels", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var labels []string
			json.NewDecoder(r.Body).Decode(&labels)
			addedLabels = append(addedLabels, labels...)
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `[]`)
		}
	})
	mux.HandleFunc("/repos/test-owner/test-repo/issues/1/labels/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusOK)
		}
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
		if cmd[0] == devpodExe && cmd[1] == "stop" && cmd[2] == "test-repo-1" {
			stopCommandCalledForIssue1 = true
		}
	}

	if !stopCommandCalledForIssue1 {
		t.Error("expected devpod stop to be called for the oldest container (issue 1) to satisfy hibernation limits, but it was not")
	}

	var hasContainerAsleep bool
	for _, l := range addedLabels {
		if l == "container-asleep" {
			hasContainerAsleep = true
			break
		}
	}
	if !hasContainerAsleep {
		t.Errorf("expected label 'container-asleep' to be added for hibernated issue 1, got added labels: %v", addedLabels)
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
			return []byte(`[{"id": "test-repo"}, {"id": "test-repo-4"}]`), nil
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
		if cmd[0] == devpodExe && cmd[1] == "stop" && cmd[2] == "test-repo-4" {
			stopCommandCalled = true
		}
		if cmd[0] == devpodExe && cmd[1] == "delete" && cmd[2] == "test-repo-4" {
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
			return []byte("[]"), nil
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
			return []byte("[]"), nil
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
			return []byte(`[{"id": "test-repo"}]`), nil
		}
		if name == devpodExe && len(args) > 0 && args[0] == "up" {
			return []byte("failed to start container"), fmt.Errorf("up failed")
		}
		return []byte("success"), nil
	}

	// Mock agy command slug derivation

	// Setup GitHub API mock responses
	mux.HandleFunc("/repos/test-owner/test-repo/issues", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `[
			{
				"number": 42,
				"title": "My Awesome Feature",
				"labels": [{"name": "seraphine-feature"}],
				"assignees": [{"login": "user1"}]
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

	mux.HandleFunc("/repos/test-owner/test-repo/git/ref/heads/feature/my_awesome_feature_42", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"ref": "refs/heads/feature/my_awesome_feature_42", "object": {"sha": "latest_sha"}}`)
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
	containerID := "test-repo-42"
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

	// Mock commandRunner
	originalCommandRunner := commandRunner
	defer func() { commandRunner = originalCommandRunner }()

	var capturedCommands [][]string
	commandRunner = func(name string, args ...string) ([]byte, error) {
		capturedCommands = append(capturedCommands, append([]string{name}, args...))
		if name == devpodExe && len(args) > 0 && args[0] == "list" {
			// Container is already running
			return []byte(`[{"id": "test-repo"}, {"id": "test-repo-42"}]`), nil
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
				"labels": [{"name": "seraphine-feature"}],
				"assignees": [{"login": "user1"}]
			}
		]`)
	})

	// 2. Issue detail (to keep it open and active, preventing cleanup)
	mux.HandleFunc("/repos/test-owner/test-repo/issues/42", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"number": 42, "state": "open", "updated_at": "2026-05-31T12:00:00Z", "labels": [{"name": "seraphine"}], "assignees": [{"login": "user1"}]}`)
	})

	// 3. New devcontainer file contents to produce a NEW SHA (new_sha_456)
	mux.HandleFunc("/repos/test-owner/test-repo/contents/.devcontainer/devcontainer.json", func(w http.ResponseWriter, r *http.Request) {
		// Verify the requested reference contains the branch name we expect
		ref := r.URL.Query().Get("ref")
		if ref == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		expectedRef := "feature/my_awesome_feature_42"
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
	var foundSendKeys bool
	for _, cmd := range capturedCommands {
		if cmd[0] == devpodExe && cmd[1] == "delete" && cmd[2] == "test-repo-42" {
			deleted = true
		}
		if cmd[0] == devpodExe && cmd[1] == "up" {
			expectedURL := "git@github.com:test-owner/test-repo@feature/my_awesome_feature_42"
			if len(cmd) > 2 && cmd[2] == expectedURL && cmd[4] == "test-repo-42" {
				recreated = true
			}
		}
		if cmd[0] == devpodExe && cmd[1] == "ssh" && cmd[2] == "test-repo-42" && cmd[3] == "--command" {
			cmdStr := cmd[4]
			if strings.Contains(cmdStr, "send-keys") && strings.Contains(cmdStr, "Take a look at the status of issue #42") {
				foundSendKeys = true
			}
		}
	}

	if !deleted {
		t.Error("expected container test-repo-42 to be deleted before recreation, but it was not")
	}
	if !recreated {
		t.Error("expected container test-repo-42 to be recreated (up) with the issue branch, but it was not")
	}
	if !foundSendKeys {
		t.Error("expected dynamic issue startup command with issue #42 to be injected during recreation, but it was not")
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
			return []byte("container_id_123|devpod-temp-name|dev.containers.id=test-repo_42,some-other-label\n"), nil
		}
		if name == devpodExe && len(args) > 0 && args[0] == "list" {
			return []byte(`[{"id": "test-repo_42", "uid": "test-repo_42"}]`), nil
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
			return []byte("container_id_123|test-repo_42|dev.containers.id=test-repo_42,some-other-label\n"), nil
		}
		if name == devpodExe && len(args) > 0 && args[0] == "list" {
			return []byte(`[{"id": "test-repo_42", "uid": "test-repo_42"}]`), nil
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

func TestLogWithPrefixAndCommandRunner(t *testing.T) {
	// 1. Test logWithPrefix
	var buf bytes.Buffer
	originalOutput := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(originalOutput)

	logWithPrefix("test-owner/test-repo", "hello %s", "world")
	logStr := buf.String()
	if !strings.Contains(logStr, "[test-owner/test-repo] hello world") {
		t.Errorf("expected log to contain '[test-owner/test-repo] hello world', got %q", logStr)
	}

	// 2. Test commandRunner prefixing/splitting output by line
	buf.Reset()
	originalCommandRunner := commandRunner
	defer func() { commandRunner = originalCommandRunner }()

	commandRunner = func(name string, args ...string) ([]byte, error) {
		return []byte("line1\nline2\n\nline3"), nil
	}

	out, err := runCommandWithLog("test-owner/test-repo", "some-cmd")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != "line1\nline2\n\nline3" {
		t.Errorf("expected original output, got %q", string(out))
	}

	logStr = buf.String()
	expectedLines := []string{
		"[test-owner/test-repo] line1",
		"[test-owner/test-repo] line2",
		"[test-owner/test-repo] line3",
	}
	for _, expected := range expectedLines {
		if !strings.Contains(logStr, expected) {
			t.Errorf("expected log to contain %q, got %q", expected, logStr)
		}
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

func TestTrackedSHAsConcurrency(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "devcontainer-manager-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	oldGetConfigDir := getConfigDir
	getConfigDir = func() string {
		return tempDir
	}
	defer func() { getConfigDir = oldGetConfigDir }()

	trackedSHAs := loadTrackedSHAs()

	const goroutines = 10
	const iterations = 50

	var wg sync.WaitGroup
	wg.Add(goroutines * 2)

	for i := 0; i < goroutines; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				repo := fmt.Sprintf("owner/repo-%d-%d", id, j)
				updateAndSaveRepoSHA(repo, "some-sha", trackedSHAs)
			}
		}(i)

		go func(id int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				_ = loadTrackedSHAs()
			}
		}(i)
	}

	wg.Wait()
}

func TestRun_ConcurrencySemaphoreLimit(t *testing.T) {
	// Create a temporary container list file with 5 repositories
	tmpFile, err := os.CreateTemp("", "container_list_*")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	repos := []string{
		"owner/repo1",
		"owner/repo2",
		"owner/repo3",
		"owner/repo4",
		"owner/repo5",
	}
	for _, r := range repos {
		if _, err := tmpFile.WriteString(r + "\n"); err != nil {
			t.Fatalf("failed to write to temp file: %v", err)
		}
	}
	tmpFile.Close()

	// Use --max-concurrency = 2
	cfg := &config{
		once:               true,
		containerList:      tmpFile.Name(),
		maxConcurrency:     2,
		maxIssueContainers: 5,
	}

	// Mock commandRunner with an artificial delay and trace concurrency
	originalCommandRunner := commandRunner
	defer func() { commandRunner = originalCommandRunner }()

	var activeRuns int32
	var maxActiveRuns int32

	commandRunner = func(name string, args ...string) ([]byte, error) {
		if name == devpodExe && len(args) > 0 && args[0] == "list" {
			// devcontainers list: return empty so it tries to start all of them
			return []byte("[]"), nil
		}
		if name == devpodExe && len(args) > 0 && args[0] == "up" {
			currentActive := atomic.AddInt32(&activeRuns, 1)
			defer atomic.AddInt32(&activeRuns, -1)

			// Store maximum active concurrency observed
			for {
				max := atomic.LoadInt32(&maxActiveRuns)
				if currentActive <= max {
					break
				}
				if atomic.CompareAndSwapInt32(&maxActiveRuns, max, currentActive) {
					break
				}
			}

			// Simulate container bring-up delay
			time.Sleep(50 * time.Millisecond)
			return []byte("success"), nil
		}
		return []byte("success"), nil
	}

	// Disable github client to avoid network calls and bypass issue scanning
	originalProvider := gitHubClientProvider
	defer func() { gitHubClientProvider = originalProvider }()
	gitHubClientProvider = func() (*github.Client, error) {
		return nil, fmt.Errorf("no gh client in this test")
	}

	err = run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}

	observedMax := atomic.LoadInt32(&maxActiveRuns)
	if observedMax != 1 {
		t.Errorf("expected max serialized devpod CLI runs to be 1, but observed %d concurrent runs", observedMax)
	}
}

func TestListOpenIssuesProvider_Success(t *testing.T) {
	originalCommandRunner := commandRunner
	defer func() { commandRunner = originalCommandRunner }()

	mockJSON := `[
		{
			"number": 169,
			"title": "Use gh tool to list open issues rather than pulling from http",
			"labels": [
				{"name": "container-ready"},
				{"name": "seraphine-bug"}
			]
		}
	]`

	var commandName string
	var commandArgs []string

	commandRunner = func(name string, args ...string) ([]byte, error) {
		commandName = name
		commandArgs = args
		return []byte(mockJSON), nil
	}

	issues, err := listOpenIssuesProvider(context.Background(), nil, "brotherlogic", "devcontainer-manager")
	if err != nil {
		t.Fatalf("listOpenIssuesProvider failed: %v", err)
	}

	if commandName != "gh" {
		t.Errorf("expected commandName 'gh', got '%s'", commandName)
	}

	expectedArgs := []string{"issue", "list", "-R", "brotherlogic/devcontainer-manager", "--state", "open", "--json", "number,title,labels,assignees,body"}
	if len(commandArgs) != len(expectedArgs) {
		t.Fatalf("expected %d args, got %d", len(expectedArgs), len(commandArgs))
	}
	for i, v := range expectedArgs {
		if commandArgs[i] != v {
			t.Errorf("arg %d: expected '%s', got '%s'", i, v, commandArgs[i])
		}
	}

	if len(issues) != 1 {
		t.Fatalf("expected 1 issue, got %d", len(issues))
	}
	if issues[0].GetNumber() != 169 {
		t.Errorf("expected issue number 169, got %d", issues[0].GetNumber())
	}
	if issues[0].GetTitle() != "Use gh tool to list open issues rather than pulling from http" {
		t.Errorf("unexpected title: %s", issues[0].GetTitle())
	}
	if len(issues[0].Labels) != 2 {
		t.Fatalf("expected 2 labels, got %d", len(issues[0].Labels))
	}
	if issues[0].Labels[0].GetName() != "container-ready" || issues[0].Labels[1].GetName() != "seraphine-bug" {
		t.Errorf("unexpected labels: %v", issues[0].Labels)
	}
}

func TestListOpenIssuesProvider_WithBody(t *testing.T) {
	originalCommandRunner := commandRunner
	defer func() { commandRunner = originalCommandRunner }()

	mockJSON := `[
		{
			"number": 169,
			"title": "Use gh tool to list open issues rather than pulling from http",
			"body": "This is the body of the issue",
			"labels": [
				{"name": "container-ready"}
			]
		}
	]`

	var commandName string
	var commandArgs []string

	commandRunner = func(name string, args ...string) ([]byte, error) {
		commandName = name
		commandArgs = args
		return []byte(mockJSON), nil
	}

	issues, err := listOpenIssuesProvider(context.Background(), nil, "brotherlogic", "devcontainer-manager")
	if err != nil {
		t.Fatalf("listOpenIssuesProvider failed: %v", err)
	}

	if commandName != "gh" {
		t.Errorf("expected commandName 'gh', got '%s'", commandName)
	}

	expectedArgs := []string{"issue", "list", "-R", "brotherlogic/devcontainer-manager", "--state", "open", "--json", "number,title,labels,assignees,body"}
	if len(commandArgs) != len(expectedArgs) {
		t.Fatalf("expected %d args, got %d", len(expectedArgs), len(commandArgs))
	}
	for i, v := range expectedArgs {
		if commandArgs[i] != v {
			t.Errorf("arg %d: expected '%s', got '%s'", i, v, commandArgs[i])
		}
	}

	if len(issues) != 1 {
		t.Fatalf("expected 1 issue, got %d", len(issues))
	}
	if issues[0].GetBody() != "This is the body of the issue" {
		t.Errorf("expected body 'This is the body of the issue', got '%s'", issues[0].GetBody())
	}
}


func TestPostLatencyComment_AlreadyExists(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	client := github.NewClient(nil)
	u, _ := url.Parse(server.URL + "/")
	client.BaseURL = u
	client.UploadURL = u

	mux.HandleFunc("/repos/test-owner/test-repo/issues/1/comments", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `[{"body": "This has devcontainer-startup-latency info"}]`)
			return
		}
		if r.Method == "POST" {
			t.Errorf("expected no POST request")
		}
	})

	err := postLatencyComment(context.Background(), client, "test-owner", "test-repo", 1, time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPostLatencyComment_CreatesNew(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	client := github.NewClient(nil)
	u, _ := url.Parse(server.URL + "/")
	client.BaseURL = u
	client.UploadURL = u

	postCalled := false
	mux.HandleFunc("/repos/test-owner/test-repo/issues/1/comments", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `[{"body": "Other comment"}]`)
			return
		}
		if r.Method == "POST" {
			postCalled = true
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id": 123}`)
			return
		}
	})

	err := postLatencyComment(context.Background(), client, "test-owner", "test-repo", 1, time.Now().Add(-5*time.Minute))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !postCalled {
		t.Errorf("expected POST request to create comment")
	}
}

func TestRun_ScanAndLaunchIssueContainer_LatencyCommentExists(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "container_list_*")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString("test-owner/test-repo\n"); err != nil {
		t.Fatalf("failed to write to temp file: %v", err)
	}
	tmpFile.Close()

	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	client := github.NewClient(nil)
	u, _ := url.Parse(server.URL + "/")
	client.BaseURL = u
	client.UploadURL = u

	originalProvider := gitHubClientProvider
	defer func() { gitHubClientProvider = originalProvider }()
	gitHubClientProvider = func() (*github.Client, error) {
		return client, nil
	}

	oldInterval := pollingInterval
	oldTimeout := pollingTimeout
	pollingInterval = 1 * time.Millisecond
	pollingTimeout = 100 * time.Millisecond
	defer func() {
		pollingInterval = oldInterval
		pollingTimeout = oldTimeout
	}()

	originalCommandRunner := commandRunner
	defer func() { commandRunner = originalCommandRunner }()

	commandRunner = func(name string, args ...string) ([]byte, error) {
		if name == devpodExe && len(args) > 0 && args[0] == "list" {
			return []byte(`[{"id": "test-repo"}]`), nil
		}
		return []byte("success"), nil
	}

	mux.HandleFunc("/repos/test-owner/test-repo/issues", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `[
			{
				"number": 42,
				"title": "My Awesome Feature",
				"labels": [{"name": "seraphine-feature"}],
				"assignees": [{"login": "user1"}],
				"created_at": "2023-01-01T00:00:00Z"
			}
		]`)
	})

	var postCalled bool
	mux.HandleFunc("/repos/test-owner/test-repo/issues/42/comments", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `[{"body": "This has devcontainer-startup-latency info"}]`)
		} else if r.Method == http.MethodPost {
			postCalled = true
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
		}
	})

	mux.HandleFunc("/repos/test-owner/test-repo/issues/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"number": 42, "title": "My Awesome Feature", "labels": [{"name": "seraphine-feature"}]}`)
		} else {
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `[]`)
		}
	})

	mux.HandleFunc("/repos/test-owner/test-repo/git/ref/heads/feature/my_awesome_feature_42", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"ref": "refs/heads/feature/my_awesome_feature_42", "object": {"sha": "latest_sha"}}`)
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

	// Wait a moment for background goroutine to complete
	time.Sleep(100 * time.Millisecond)

	if postCalled {
		t.Errorf("expected no POST request to create latency comment since one already existed")
	}
}

func TestRun_ScanAndLaunchIssueContainer_LatencyCommentError(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "container_list_*")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString("test-owner/test-repo\n"); err != nil {
		t.Fatalf("failed to write to temp file: %v", err)
	}
	tmpFile.Close()

	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	client := github.NewClient(nil)
	u, _ := url.Parse(server.URL + "/")
	client.BaseURL = u
	client.UploadURL = u

	originalProvider := gitHubClientProvider
	defer func() { gitHubClientProvider = originalProvider }()
	gitHubClientProvider = func() (*github.Client, error) {
		return client, nil
	}

	oldInterval := pollingInterval
	oldTimeout := pollingTimeout
	pollingInterval = 1 * time.Millisecond
	pollingTimeout = 100 * time.Millisecond
	defer func() {
		pollingInterval = oldInterval
		pollingTimeout = oldTimeout
	}()

	originalCommandRunner := commandRunner
	defer func() { commandRunner = originalCommandRunner }()

	var devpodUpCalled bool
	commandRunner = func(name string, args ...string) ([]byte, error) {
		if name == devpodExe && len(args) > 0 && args[0] == "up" {
			devpodUpCalled = true
		}
		if name == devpodExe && len(args) > 0 && args[0] == "list" {
			return []byte(`[{"id": "test-repo"}]`), nil
		}
		return []byte("success"), nil
	}

	mux.HandleFunc("/repos/test-owner/test-repo/issues", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `[
			{
				"number": 42,
				"title": "My Awesome Feature",
				"labels": [{"name": "seraphine-feature"}],
				"assignees": [{"login": "user1"}],
				"created_at": "2023-01-01T00:00:00Z"
			}
		]`)
	})

	mux.HandleFunc("/repos/test-owner/test-repo/issues/42/comments", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `[]`)
		} else if r.Method == http.MethodPost {
			// Simulate an error from GitHub API
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"message": "Internal Server Error"}`)
		}
	})

	mux.HandleFunc("/repos/test-owner/test-repo/issues/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"number": 42, "title": "My Awesome Feature", "labels": [{"name": "seraphine-feature"}]}`)
		} else {
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `[]`)
		}
	})

	mux.HandleFunc("/repos/test-owner/test-repo/git/ref/heads/feature/my_awesome_feature_42", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"ref": "refs/heads/feature/my_awesome_feature_42", "object": {"sha": "latest_sha"}}`)
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
		t.Fatalf("expected run() to succeed despite API error, but got: %v", err)
	}

	// Wait a moment for background goroutine to complete
	time.Sleep(100 * time.Millisecond)

	if !devpodUpCalled {
		t.Errorf("expected container to be successfully launched (devpod up) despite API error")
	}
}

func TestListDevpodWorkspaces_WithWarnings(t *testing.T) {
	originalCommandRunner := commandRunner
	defer func() { commandRunner = originalCommandRunner }()

	commandRunner = func(name string, args ...string) ([]byte, error) {
		output := "06:45:43 warn Couldn't load workspace dcrouter: unexpected end of JSON input\n" +
			"06:45:43 warn Couldn't load workspace gemclust: unexpected end of JSON input\n" +
			"[\n" +
			"  {\n" +
			"    \"id\": \"devcontainer-manager\",\n" +
			"    \"uid\": \"12345\",\n" +
			"    \"source\": {\n" +
			"      \"gitRepository\": \"git@github.com:brotherlogic/devcontainer-manager\"\n" +
			"    }\n" +
			"  }\n" +
			"]"
		return []byte(output), nil
	}

	workspaces, err := listDevpodWorkspaces()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(workspaces) != 1 {
		t.Fatalf("expected 1 workspace, got %d", len(workspaces))
	}

	if workspaces[0].ID != "devcontainer-manager" {
		t.Errorf("expected ID devcontainer-manager, got %s", workspaces[0].ID)
	}
}

func TestListDevpodWorkspaces_SingleLineJSONWithWarnings(t *testing.T) {
	originalCommandRunner := commandRunner
	defer func() { commandRunner = originalCommandRunner }()

	commandRunner = func(name string, args ...string) ([]byte, error) {
		output := "06:45:43 warn [Some log output]\n" +
			"[{\"id\":\"devcontainer-manager\",\"uid\":\"12345\",\"source\":{\"gitRepository\":\"git@github.com:brotherlogic/devcontainer-manager\"}}]"
		return []byte(output), nil
	}

	workspaces, err := listDevpodWorkspaces()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(workspaces) != 1 {
		t.Fatalf("expected 1 workspace, got %d", len(workspaces))
	}

	if workspaces[0].ID != "devcontainer-manager" {
		t.Errorf("expected ID devcontainer-manager, got %s", workspaces[0].ID)
	}
}

func TestReportStartupFailure_LogTooLong(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	client := github.NewClient(nil)
	u, _ := url.Parse(server.URL + "/")
	client.BaseURL = u
	client.UploadURL = u

	var createCalled bool

	mux.HandleFunc("/repos/test-owner/test-repo/issues", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			createCalled = true
			var req github.IssueRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("failed to decode request body: %v", err)
			}
			
			body := req.GetBody()
			if len(body) > 66000 {
				t.Errorf("expected body to be truncated to <= 66000 characters, got %d", len(body))
			}
			if !strings.Contains(body, "[logs truncated due to size limit] ...") {
				t.Errorf("expected body to contain truncation message")
			}
			
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"number": 100}`)
			return
		}
		
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `[]`)
	})

	longLog := strings.Repeat("a", 70000)
	reportStartupFailure(context.Background(), client, "test-owner", "test-repo", "feature/my-branch_42", 42, fmt.Errorf("startup err"), longLog)

	if !createCalled {
		t.Error("expected create issue to be called")
	}
}

func TestRun_ProcessManualUpRequestsInDcmReceivedState(t *testing.T) {
	// Create a temporary container list file with a dummy repo
	tmpFile, err := os.CreateTemp("", "container_list_*")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString("# comment line\n"); err != nil {
		t.Fatalf("failed to write to temp file: %v", err)
	}
	tmpFile.Close()

	// Inject nil gitHubClientProvider to avoid listing issues or doing GitHub calls
	originalProvider := gitHubClientProvider
	defer func() { gitHubClientProvider = originalProvider }()
	gitHubClientProvider = func() (*github.Client, error) {
		return nil, fmt.Errorf("no github client")
	}

	// Mock commandRunner
	originalCommandRunner := commandRunner
	defer func() { commandRunner = originalCommandRunner }()

	var capturedCommands [][]string
	commandRunner = func(name string, args ...string) ([]byte, error) {
		capturedCommands = append(capturedCommands, append([]string{name}, args...))
		if name == devpodExe && len(args) > 0 && args[0] == "list" {
			// Return empty list of running containers initially
			return []byte(`[]`), nil
		}
		return []byte("success"), nil
	}

	// Put a DCM_RECEIVED container in globalCache
	manualConfigID := "test-repo-manual-branch"
	manualConfig := &proto.DevcontainerConfig{
		Id: manualConfigID,
		Request: &proto.UpRequest{
			Repo:   "brotherlogic/test-repo",
			Branch: "manual-branch",
		},
		State: proto.State_DCM_RECEIVED,
	}
	globalCache.Update(manualConfigID, manualConfig)
	defer globalCache.Delete(manualConfigID)

	cfg := &config{
		once:               true,
		containerList:      tmpFile.Name(),
		maxIssueContainers: 5,
	}

	err = run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify that devpod up was called with the correct parameters for the manual container
	var devpodUpCalled bool
	for _, cmd := range capturedCommands {
		if len(cmd) > 3 && cmd[0] == devpodExe && cmd[1] == "up" {
			// Expected args: "devpod", "up", "git@github.com:brotherlogic/test-repo@manual-branch", "--id", "brotherlogic/test-repo-manual-branch", "--ide", "none"
			if cmd[2] == "git@github.com:brotherlogic/test-repo@manual-branch" && cmd[4] == manualConfigID {
				devpodUpCalled = true
			}
		}
	}

	if !devpodUpCalled {
		t.Errorf("expected devpod up to be called for manual container, captured commands: %v", capturedCommands)
	}

	// Verify cache state transitioned to DCM_READY
	cached, ok := globalCache.Get(manualConfigID)
	if !ok {
		t.Fatalf("expected manual container to remain in cache")
	}
	if cached.State != proto.State_DCM_READY {
		t.Errorf("expected cache state to transition to DCM_READY, got %v", cached.State)
	}
}

func TestCreateIssueWithDeduplication(t *testing.T) {
	t.Run("NormalRepo_Success", func(t *testing.T) {
		mux := http.NewServeMux()
		server := httptest.NewServer(mux)
		defer server.Close()

		client := github.NewClient(nil)
		u, _ := url.Parse(server.URL + "/")
		client.BaseURL = u
		client.UploadURL = u

		var createCalled bool
		var createdBody string
		var createdLabels []string

		mux.HandleFunc("/repos/dest-owner/dest-repo/issues", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodPost {
				createCalled = true
				var req struct {
					Title  string   `json:"title"`
					Body   string   `json:"body"`
					Labels []string `json:"labels"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
					createdBody = req.Body
					createdLabels = req.Labels
				}
				w.WriteHeader(http.StatusCreated)
				fmt.Fprint(w, `{"number": 101}`)
				return
			}
		})

		// Mock listOpenIssuesProvider
		originalListProvider := listOpenIssuesProvider
		defer func() { listOpenIssuesProvider = originalListProvider }()
		listOpenIssuesProvider = func(ctx context.Context, cl *github.Client, owner, repo string) ([]*github.Issue, error) {
			return []*github.Issue{}, nil
		}

		err := createIssueWithDeduplication(context.Background(), client, "dest-owner", "dest-repo", "target-owner", "target-repo", "my-branch", 123, fmt.Errorf("some error"), "my log")
		if err != nil {
			t.Errorf("expected no error, got %v", err)
		}
		if !createCalled {
			t.Error("expected issue creation to be called")
		}
		if !strings.Contains(createdBody, "some error") {
			t.Errorf("expected body to contain error message, got: %s", createdBody)
		}
		if !strings.Contains(createdBody, "**Branch:** `my-branch`") {
			t.Errorf("expected branch info, got: %s", createdBody)
		}
		if !strings.Contains(createdBody, "**Original Issue:** #123") {
			t.Errorf("expected original issue, got: %s", createdBody)
		}
		if len(createdLabels) != 1 || createdLabels[0] != "seraphine-bug" {
			t.Errorf("expected label seraphine-bug, got %v", createdLabels)
		}
	})

	t.Run("NormalRepo_Duplicate", func(t *testing.T) {
		client := github.NewClient(nil)

		// Mock listOpenIssuesProvider to return a duplicate issue
		originalListProvider := listOpenIssuesProvider
		defer func() { listOpenIssuesProvider = originalListProvider }()
		listOpenIssuesProvider = func(ctx context.Context, cl *github.Client, owner, repo string) ([]*github.Issue, error) {
			title := "Issue Container Startup Failed"
			num := 99
			return []*github.Issue{
				{
					Number: &num,
					Title:  &title,
				},
			}, nil
		}

		err := createIssueWithDeduplication(context.Background(), client, "dest-owner", "dest-repo", "target-owner", "target-repo", "my-branch", 123, fmt.Errorf("some error"), "my log")
		if err != nil {
			t.Errorf("expected no error, got %v", err)
		}
	})

	t.Run("FallbackRepo_Success", func(t *testing.T) {
		mux := http.NewServeMux()
		server := httptest.NewServer(mux)
		defer server.Close()

		client := github.NewClient(nil)
		u, _ := url.Parse(server.URL + "/")
		client.BaseURL = u
		client.UploadURL = u

		var createCalled bool
		mux.HandleFunc("/repos/dest-owner/devcontainer-manager/issues", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodPost {
				createCalled = true
				w.WriteHeader(http.StatusCreated)
				fmt.Fprint(w, `{"number": 102}`)
				return
			}
		})

		// Mock listOpenIssuesProvider to return an issue with title matching but NO target repo reference in body
		originalListProvider := listOpenIssuesProvider
		defer func() { listOpenIssuesProvider = originalListProvider }()
		listOpenIssuesProvider = func(ctx context.Context, cl *github.Client, owner, repo string) ([]*github.Issue, error) {
			title := "Issue Container Startup Failed"
			body := "Some other description without the target repo reference"
			num := 99
			return []*github.Issue{
				{
					Number: &num,
					Title:  &title,
					Body:   &body,
				},
			}, nil
		}

		err := createIssueWithDeduplication(context.Background(), client, "dest-owner", "devcontainer-manager", "target-owner", "target-repo", "my-branch", 123, fmt.Errorf("some error"), "my log")
		if err != nil {
			t.Errorf("expected no error, got %v", err)
		}
		if !createCalled {
			t.Error("expected new issue to be created in devcontainer-manager since target repo didn't match")
		}
	})

	t.Run("FallbackRepo_Duplicate", func(t *testing.T) {
		client := github.NewClient(nil)

		// Mock listOpenIssuesProvider to return an issue with title matching AND target repo reference in body
		originalListProvider := listOpenIssuesProvider
		defer func() { listOpenIssuesProvider = originalListProvider }()
		listOpenIssuesProvider = func(ctx context.Context, cl *github.Client, owner, repo string) ([]*github.Issue, error) {
			title := "Issue Container Startup Failed"
			body := "Stuff...\n**Target Repository:** target-owner/target-repo\nMore stuff..."
			num := 99
			return []*github.Issue{
				{
					Number: &num,
					Title:  &title,
					Body:   &body,
				},
			}, nil
		}

		err := createIssueWithDeduplication(context.Background(), client, "dest-owner", "devcontainer-manager", "target-owner", "target-repo", "my-branch", 123, fmt.Errorf("some error"), "my log")
		if err != nil {
			t.Errorf("expected no error, got %v", err)
		}
	})

	t.Run("Truncation", func(t *testing.T) {
		mux := http.NewServeMux()
		server := httptest.NewServer(mux)
		defer server.Close()

		client := github.NewClient(nil)
		u, _ := url.Parse(server.URL + "/")
		client.BaseURL = u
		client.UploadURL = u

		var createdBody string

		mux.HandleFunc("/repos/dest-owner/dest-repo/issues", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodPost {
				var req struct {
					Body string `json:"body"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
					createdBody = req.Body
				}
				w.WriteHeader(http.StatusCreated)
				fmt.Fprint(w, `{"number": 103}`)
				return
			}
		})

		originalListProvider := listOpenIssuesProvider
		defer func() { listOpenIssuesProvider = originalListProvider }()
		listOpenIssuesProvider = func(ctx context.Context, cl *github.Client, owner, repo string) ([]*github.Issue, error) {
			return []*github.Issue{}, nil
		}

		// Create a log that is 70,000 characters long
		longLog := strings.Repeat("A", 70000)
		err := createIssueWithDeduplication(context.Background(), client, "dest-owner", "dest-repo", "target-owner", "target-repo", "my-branch", 123, fmt.Errorf("some error"), longLog)
		if err != nil {
			t.Errorf("expected no error, got %v", err)
		}

		parts := strings.Split(createdBody, "```\n")
		if len(parts) < 2 {
			t.Fatalf("could not locate code block in created body: %s", createdBody)
		}
		codeBlock := parts[1]
		if !strings.Contains(codeBlock, "[logs truncated due to size limit] ...\n") {
			t.Errorf("expected code block to contain truncation message, got: %s", codeBlock)
		}

		logPart := strings.TrimPrefix(codeBlock, "Error: some error\n")
		logPart = strings.TrimSuffix(logPart, "\n")
		if len(logPart) != 65000 {
			t.Errorf("expected truncated log content to be exactly 65000 characters, got %d", len(logPart))
		}
		expectedPrefix := "[logs truncated due to size limit] ...\n" + strings.Repeat("A", 65000-len("[logs truncated due to size limit] ...\n"))
		if logPart != expectedPrefix {
			t.Errorf("truncated log content does not match expected prefix/format")
		}
	})
}

// TestReportStartupFailure_Fallback verifies that when reportStartupFailure fails to write to the
// target repository (e.g. due to a 403 Forbidden permission error), it correctly falls back
// to writing the startup failure issue to the devcontainer-manager repository.
func TestReportStartupFailure_Fallback(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	client := github.NewClient(nil)
	u, _ := url.Parse(server.URL + "/")
	client.BaseURL = u
	client.UploadURL = u

	var targetListCalled, targetCreateCalled bool
	var fallbackListCalled, fallbackCreateCalled bool

	// Mock target repo issues endpoint
	mux.HandleFunc("/repos/test-owner/test-repo/issues", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			targetCreateCalled = true
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message": "Resource not accessible by integration", "documentation_url": "https://docs.github.com"}`)
			return
		}
		targetListCalled = true
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `[]`)
	})

	// Mock fallback repo issues endpoint
	mux.HandleFunc("/repos/brotherlogic/devcontainer-manager/issues", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			fallbackCreateCalled = true
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"number": 287}`)
			return
		}
		fallbackListCalled = true
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `[]`)
	})

	// Mock listOpenIssuesProvider to fall back to the mock client endpoint
	originalListProvider := listOpenIssuesProvider
	defer func() { listOpenIssuesProvider = originalListProvider }()
	listOpenIssuesProvider = func(ctx context.Context, cl *github.Client, owner, repo string) ([]*github.Issue, error) {
		// Mock list behavior by fetching from client Issues service directly to match mock server
		issues, _, err := cl.Issues.ListByRepo(ctx, owner, repo, &github.IssueListByRepoOptions{State: "open"})
		return issues, err
	}

	reportStartupFailure(context.Background(), client, "test-owner", "test-repo", "feature/my-branch_42", 42, fmt.Errorf("some startup error"), "some log output")

	if !targetListCalled {
		t.Error("expected list issues on target repository to be called")
	}
	if !targetCreateCalled {
		t.Error("expected create issue on target repository to be called")
	}
	if !fallbackListCalled {
		t.Error("expected list issues on fallback repository to be called")
	}
	if !fallbackCreateCalled {
		t.Error("expected create issue on fallback repository to be called")
	}
}

func TestProcessManualUpRequest_FailureTriggersReportStartupFailure(t *testing.T) {
	// Create mock GitHub server
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	client := github.NewClient(nil)
	u, _ := url.Parse(server.URL + "/")
	client.BaseURL = u
	client.UploadURL = u

	// Inject gitHubClientProvider mock
	originalProvider := gitHubClientProvider
	defer func() { gitHubClientProvider = originalProvider }()
	gitHubClientProvider = func() (*github.Client, error) {
		return client, nil
	}

	// Mock listOpenIssuesProvider to fall back to the mock client endpoint
	originalListProvider := listOpenIssuesProvider
	defer func() { listOpenIssuesProvider = originalListProvider }()
	listOpenIssuesProvider = func(ctx context.Context, cl *github.Client, owner, repo string) ([]*github.Issue, error) {
		issues, _, err := cl.Issues.ListByRepo(ctx, owner, repo, &github.IssueListByRepoOptions{State: "open"})
		return issues, err
	}

	// Mock commandRunner
	originalCommandRunner := commandRunner
	defer func() { commandRunner = originalCommandRunner }()

	commandRunner = func(name string, args ...string) ([]byte, error) {
		if name == devpodExe && len(args) > 0 && args[0] == "up" {
			return []byte("failed to start container: port conflict"), fmt.Errorf("exit status 1")
		}
		if name == "docker" && len(args) > 0 && args[0] == "rm" {
			return []byte("removed"), nil
		}
		return []byte(""), nil
	}

	// Capture GitHub API requests
	var createCalled bool
	var capturedBody string
	mux.HandleFunc("/repos/test-owner/test-repo/issues", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			createCalled = true
			var reqBody struct {
				Title string `json:"title"`
				Body  string `json:"body"`
			}
			json.NewDecoder(r.Body).Decode(&reqBody)
			capturedBody = reqBody.Body
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"number": 100}`)
			return
		}
		// List issues
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `[]`)
	})

	// Setup DevcontainerConfig manual request
	manualConfigID := "test-repo-manual-failure"
	manualConfig := &proto.DevcontainerConfig{
		Id: manualConfigID,
		Request: &proto.UpRequest{
			Repo:   "https://github.com/test-owner/test-repo/issues/123",
			Branch: "my-manual-branch",
			Identifier: &proto.Identifier{
				Id: &proto.Identifier_IssueNumber{IssueNumber: 123},
			},
		},
		State: proto.State_DCM_RECEIVED,
	}
	globalCache.Update(manualConfigID, manualConfig)
	defer globalCache.Delete(manualConfigID)

	var wg sync.WaitGroup
	sem := make(chan struct{}, 1)
	sem <- struct{}{}
	wg.Add(1)

	processManualUpRequest(context.Background(), manualConfig, &wg, &config{}, sem)
	wg.Wait()
	time.Sleep(100 * time.Millisecond)

	// Verify GitHub issue creation was triggered with correct content
	if !createCalled {
		t.Error("expected reportStartupFailure to be triggered and create a GitHub issue, but it was not")
	}
	if !strings.Contains(capturedBody, "failed to start container: port conflict") {
		t.Errorf("expected issue body to contain startup log/error, got %q", capturedBody)
	}
	if !strings.Contains(capturedBody, "* **Branch:** `my-manual-branch`") {
		t.Errorf("expected issue body to contain branch, got %q", capturedBody)
	}
	if !strings.Contains(capturedBody, "* **Original Issue:** #123") {
		t.Errorf("expected issue body to contain original issue number, got %q", capturedBody)
	}
}

func TestProcessManualUpRequest_AdjustsIssueLabels(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	client := github.NewClient(nil)
	u, _ := url.Parse(server.URL + "/")
	client.BaseURL = u
	client.UploadURL = u

	originalProvider := gitHubClientProvider
	defer func() { gitHubClientProvider = originalProvider }()
	gitHubClientProvider = func() (*github.Client, error) {
		return client, nil
	}

	originalCommandRunner := commandRunner
	defer func() { commandRunner = originalCommandRunner }()
	commandRunner = func(name string, args ...string) ([]byte, error) {
		return []byte("success"), nil
	}

	var addedLabels []string
	var removedLabels []string
	var mu sync.Mutex

	mux.HandleFunc("/repos/test-owner/test-repo/issues/123", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"number": 123, "labels": [{"name": "seraphine-bug"}]}`)
	})
	mux.HandleFunc("/repos/test-owner/test-repo/issues/123/labels", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			var labels []string
			json.NewDecoder(r.Body).Decode(&labels)
			mu.Lock()
			addedLabels = append(addedLabels, labels...)
			mu.Unlock()
			fmt.Fprint(w, `[]`)
			return
		}
	})
	mux.HandleFunc("/repos/test-owner/test-repo/issues/123/labels/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodDelete {
			parts := strings.Split(r.URL.Path, "/")
			label := parts[len(parts)-1]
			mu.Lock()
			removedLabels = append(removedLabels, label)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
	})

	manualConfigID := "test-repo-manual-success"
	manualConfig := &proto.DevcontainerConfig{
		Id: manualConfigID,
		Request: &proto.UpRequest{
			Repo:   "https://github.com/test-owner/test-repo/issues/123",
			Branch: "my-manual-branch",
			Identifier: &proto.Identifier{
				Id: &proto.Identifier_IssueNumber{IssueNumber: 123},
			},
		},
		State: proto.State_DCM_RECEIVED,
	}
	globalCache.Update(manualConfigID, manualConfig)
	defer globalCache.Delete(manualConfigID)

	var wg sync.WaitGroup
	sem := make(chan struct{}, 1)
	sem <- struct{}{}
	wg.Add(1)

	processManualUpRequest(context.Background(), manualConfig, &wg, &config{}, sem)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	hasCreating := false
	hasReady := false
	for _, l := range addedLabels {
		if l == "container-creating" {
			hasCreating = true
		}
		if l == "container-ready" {
			hasReady = true
		}
	}
	if !hasCreating {
		t.Errorf("expected container-creating label to be added, got addedLabels: %v", addedLabels)
	}
	if !hasReady {
		t.Errorf("expected container-ready label to be added, got addedLabels: %v", addedLabels)
	}
}

func TestProcessManualUpRequest_InjectsPrompt(t *testing.T) {
	originalCommandRunner := commandRunner
	defer func() { commandRunner = originalCommandRunner }()

	var executedCommands []string
	var mu sync.Mutex
	commandRunner = func(name string, args ...string) ([]byte, error) {
		mu.Lock()
		cmdStr := name + " " + strings.Join(args, " ")
		executedCommands = append(executedCommands, cmdStr)
		mu.Unlock()
		return []byte("success"), nil
	}

	manualConfigID := "test-repo-manual-prompt"
	manualConfig := &proto.DevcontainerConfig{
		Id: manualConfigID,
		Request: &proto.UpRequest{
			Repo:   "https://github.com/test-owner/test-repo",
			Prompt: "Custom test prompt",
		},
		State: proto.State_DCM_RECEIVED,
	}
	globalCache.Update(manualConfigID, manualConfig)
	defer globalCache.Delete(manualConfigID)

	var wg sync.WaitGroup
	sem := make(chan struct{}, 1)
	sem <- struct{}{}
	wg.Add(1)

	processManualUpRequest(context.Background(), manualConfig, &wg, &config{}, sem)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()

	foundPrompt := false
	for _, cmd := range executedCommands {
		if strings.Contains(cmd, "Custom test prompt") {
			foundPrompt = true
			break
		}
	}
	if !foundPrompt {
		t.Errorf("expected commandRunner to execute command containing prompt 'Custom test prompt', got: %v", executedCommands)
	}
}

func TestParseOwnerRepo(t *testing.T) {
	tests := []struct {
		input         string
		expectedOwner string
		expectedRepo  string
		expectErr     bool
	}{
		{"git@github.com:brotherlogic/devcontainer-manager.git", "brotherlogic", "devcontainer-manager", false},
		{"https://github.com/brotherlogic/devcontainer-manager", "brotherlogic", "devcontainer-manager", false},
		{"https://github.com/brotherlogic/devcontainer-manager/issues/318", "brotherlogic", "devcontainer-manager", false},
		{"brotherlogic/devcontainer-manager", "brotherlogic", "devcontainer-manager", false},
		{"git@github.com:brotherlogic/devcontainer-manager@feature/mybranch", "brotherlogic", "devcontainer-manager", false},
		{"invalidrepo", "", "", true},
	}

	for _, tc := range tests {
		owner, repo, err := parseOwnerRepo(tc.input)
		if tc.expectErr {
			if err == nil {
				t.Errorf("expected error for input %q, got nil", tc.input)
			}
		} else {
			if err != nil {
				t.Errorf("unexpected error for input %q: %v", tc.input, err)
			}
			if owner != tc.expectedOwner || repo != tc.expectedRepo {
				t.Errorf("parseOwnerRepo(%q) = (%q, %q), expected (%q, %q)", tc.input, owner, repo, tc.expectedOwner, tc.expectedRepo)
			}
		}
	}
}

func TestGHGitClient(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	client := github.NewClient(nil)
	u, _ := url.Parse(server.URL + "/")
	client.BaseURL = u
	client.UploadURL = u

	originalProvider := gitHubClientProvider
	defer func() { gitHubClientProvider = originalProvider }()
	gitHubClientProvider = func() (*github.Client, error) {
		return client, nil
	}

	// Mock branch endpoint
	mux.HandleFunc("/repos/test-owner/test-repo/branches/existing-branch", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"name": "existing-branch"}`)
	})

	// Mock get repo endpoint
	mux.HandleFunc("/repos/test-owner/test-repo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"default_branch": "main"}`)
	})

	// Mock git ref endpoint for main branch
	mux.HandleFunc("/repos/test-owner/test-repo/git/ref/heads/main", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"ref": "refs/heads/main", "object": {"sha": "sha123"}}`)
	})

	// Mock git ref endpoint for new branch check
	mux.HandleFunc("/repos/test-owner/test-repo/git/ref/heads/new-branch", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
	})

	// Mock git refs POST for branch creation
	mux.HandleFunc("/repos/test-owner/test-repo/git/refs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"ref": "refs/heads/new-branch", "object": {"sha": "sha123"}}`)
	})

	ghClient := &ghGitClient{}

	// Test BranchExists
	exists, err := ghClient.BranchExists(context.Background(), "test-owner/test-repo", "existing-branch")
	if err != nil || !exists {
		t.Errorf("expected branch to exist, got exists=%v, err=%v", exists, err)
	}

	nonExists, err := ghClient.BranchExists(context.Background(), "test-owner/test-repo", "non-existing-branch")
	if err != nil || nonExists {
		t.Errorf("expected branch to not exist, got exists=%v, err=%v", nonExists, err)
	}

	// Test GetDefaultBranch
	defaultBranch, err := ghClient.GetDefaultBranch(context.Background(), "test-owner/test-repo")
	if err != nil || defaultBranch != "main" {
		t.Errorf("expected default branch 'main', got %q, err=%v", defaultBranch, err)
	}

	// Test CreateBranch
	err = ghClient.CreateBranch(context.Background(), "test-owner/test-repo", "new-branch", "main")
	if err != nil {
		t.Errorf("unexpected error creating branch: %v", err)
	}
}

func TestTriggerRunLoop(t *testing.T) {
	// Clear channel
	select {
	case <-triggerRunChan:
	default:
	}

	triggerRunLoop()

	select {
	case <-triggerRunChan:
		// Success
	default:
		t.Error("expected triggerRunChan to receive a signal")
	}
}

func TestProcessManualUpRequest_SSHURLConversion(t *testing.T) {
	originalCommandRunner := commandRunner
	defer func() { commandRunner = originalCommandRunner }()

	var capturedCommands [][]string
	commandRunner = func(name string, args ...string) ([]byte, error) {
		capturedCommands = append(capturedCommands, append([]string{name}, args...))
		return []byte("success"), nil
	}

	manualConfigID := "test-repo-manual-http-url"
	manualConfig := &proto.DevcontainerConfig{
		Id: manualConfigID,
		Request: &proto.UpRequest{
			Repo:   "https://github.com/brotherlogic/devcontainer-manager/issues/308",
			Branch: "feature/test-branch",
		},
		State: proto.State_DCM_RECEIVED,
	}
	globalCache.Update(manualConfigID, manualConfig)
	defer globalCache.Delete(manualConfigID)

	var wg sync.WaitGroup
	sem := make(chan struct{}, 1)
	sem <- struct{}{}
	wg.Add(1)

	processManualUpRequest(context.Background(), manualConfig, &wg, &config{}, sem)
	wg.Wait()

	expectedURL := "git@github.com:brotherlogic/devcontainer-manager@feature/test-branch"
	var found bool
	for _, cmd := range capturedCommands {
		if len(cmd) > 3 && cmd[0] == devpodExe && cmd[1] == "up" {
			if cmd[2] == expectedURL {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected devpod up command to be called with repo URL %q, captured commands: %v", expectedURL, capturedCommands)
	}
}

func TestManualIssueContainerNotCleanedUpWhenOpen(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	client := github.NewClient(nil)
	u, _ := url.Parse(server.URL + "/")
	client.BaseURL = u
	client.UploadURL = u

	origClientProvider := gitHubClientProvider
	gitHubClientProvider = func() (*github.Client, error) {
		return client, nil
	}
	defer func() { gitHubClientProvider = origClientProvider }()

	tmpFile, err := os.CreateTemp("", "container.list.*")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	content := "test-owner/test-repo\n"
	if _, err := tmpFile.WriteString(content); err != nil {
		t.Fatalf("failed to write to temp file: %v", err)
	}
	tmpFile.Close()

	origCommandRunner := commandRunner
	defer func() { commandRunner = origCommandRunner }()

	var capturedCommands [][]string
	commandRunner = func(name string, args ...string) ([]byte, error) {
		capturedCommands = append(capturedCommands, append([]string{name}, args...))
		if name == devpodExe && len(args) > 0 && args[0] == "list" {
			// devpod list returns test-repo-311 (an open manual issue container without seraphine label)
			return []byte(`[{"id":"test-repo-311","source":{"gitRepository":"git@github.com:test-owner/test-repo@main"}}]`), nil
		}
		return []byte("success"), nil
	}

	// Mock GitHub API: Issue 311 is open with label container-ready (no seraphine prefix)
	mux.HandleFunc("/repos/test-owner/test-repo/issues/311", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"number": 311, "state": "open", "updated_at": "2026-05-31T12:00:00Z", "labels": [{"name": "container-ready"}]}`)
	})

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

	manualConfigID := "test-repo-311"
	globalCache.SetManual(manualConfigID, true)
	defer globalCache.Delete(manualConfigID)

	cfg := &config{
		once:               true,
		containerList:      tmpFile.Name(),
		maxIssueContainers: 5,
	}

	err = run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify that devpod stop and devpod delete were NOT called for manual container test-repo-311
	for _, cmd := range capturedCommands {
		if len(cmd) >= 3 && cmd[0] == devpodExe && (cmd[1] == "stop" || cmd[1] == "delete") && cmd[2] == manualConfigID {
			t.Errorf("expected devpod %s NOT to be called for open manual issue container %s, but it was called", cmd[1], manualConfigID)
		}
	}
}

func TestProcessManualUpRequest_HarnessPi_Success(t *testing.T) {
	oldInterval := pollingInterval
	oldTimeout := pollingTimeout
	pollingInterval = 1 * time.Millisecond
	pollingTimeout = 100 * time.Millisecond
	defer func() {
		pollingInterval = oldInterval
		pollingTimeout = oldTimeout
	}()

	originalCommandRunner := commandRunner
	defer func() { commandRunner = originalCommandRunner }()

	var capturedCommands [][]string
	commandRunner = func(name string, args ...string) ([]byte, error) {
		capturedCommands = append(capturedCommands, append([]string{name}, args...))
		if name == devpodExe && len(args) >= 4 && args[0] == "ssh" && args[2] == "--command" {
			cmdStr := args[3]
			if strings.Contains(cmdStr, "command -v pi") {
				// Simulate pi missing initially
				return []byte(""), fmt.Errorf("pi not found")
			}
			if strings.Contains(cmdStr, "pi.dev") {
				// Simulate installation success
				return []byte("installed pi"), nil
			}
			if strings.Contains(cmdStr, "has-session") {
				return []byte("session exists"), nil
			}
		}
		return []byte("success"), nil
	}

	configID := "test-repo-pi-1"
	devConfig := &proto.DevcontainerConfig{
		Id: configID,
		Request: &proto.UpRequest{
			Repo:    "brotherlogic/test-repo",
			Branch:  "main",
			Prompt:  "Run pi task",
			Harness: proto.Harness_HARNESS_PI,
			Identifier: &proto.Identifier{
				Id: &proto.Identifier_IssueNumber{IssueNumber: 349},
			},
		},
		State: proto.State_DCM_RECEIVED,
	}
	globalCache.Update(configID, devConfig)
	defer globalCache.Delete(configID)

	var wg sync.WaitGroup
	sem := make(chan struct{}, 1)
	sem <- struct{}{}
	wg.Add(1)

	cfg := &config{
		startupCommand: "",
	}

	processManualUpRequest(context.Background(), devConfig, &wg, cfg, sem)
	wg.Wait()

	// Verify command -v pi check was called
	var foundPiCheck, foundPiInstall, foundPiSendKeys bool
	for _, cmd := range capturedCommands {
		if cmd[0] == devpodExe && cmd[1] == "ssh" && cmd[2] == configID && cmd[3] == "--command" {
			c := cmd[4]
			if strings.Contains(c, "command -v pi") {
				foundPiCheck = true
			}
			if strings.Contains(c, "pi.dev") {
				foundPiInstall = true
			}
			if strings.Contains(c, "send-keys") && strings.Contains(c, "pi --prompt") && strings.Contains(c, "Run pi task") {
				foundPiSendKeys = true
			}
		}
	}

	if !foundPiCheck {
		t.Errorf("expected command -v pi check to be run via devpod ssh, captured: %v", capturedCommands)
	}
	if !foundPiInstall {
		t.Errorf("expected pi.dev installation script to be run when pi was missing, captured: %v", capturedCommands)
	}
	if !foundPiSendKeys {
		t.Errorf("expected tmux send-keys with pi --prompt 'Run pi task' to be injected, captured: %v", capturedCommands)
	}

	cached, ok := globalCache.Get(configID)
	if !ok || cached.State != proto.State_DCM_READY {
		t.Errorf("expected container state DCM_READY, got: %v (exists: %v)", cached, ok)
	}
}

func TestProcessManualUpRequest_HarnessPi_InstallationFailure(t *testing.T) {
	oldInterval := pollingInterval
	oldTimeout := pollingTimeout
	pollingInterval = 1 * time.Millisecond
	pollingTimeout = 100 * time.Millisecond
	defer func() {
		pollingInterval = oldInterval
		pollingTimeout = oldTimeout
	}()

	originalCommandRunner := commandRunner
	defer func() { commandRunner = originalCommandRunner }()

	var capturedCommands [][]string
	commandRunner = func(name string, args ...string) ([]byte, error) {
		capturedCommands = append(capturedCommands, append([]string{name}, args...))
		if name == devpodExe && len(args) >= 4 && args[0] == "ssh" && args[2] == "--command" {
			cmdStr := args[3]
			if strings.Contains(cmdStr, "command -v pi") {
				return []byte(""), fmt.Errorf("pi not found")
			}
			if strings.Contains(cmdStr, "pi.dev") {
				return []byte("failed install"), fmt.Errorf("pi installation failed")
			}
			if strings.Contains(cmdStr, "has-session") {
				return []byte("session exists"), nil
			}
		}
		return []byte("success"), nil
	}

	configID := "test-repo-pi-fail"
	devConfig := &proto.DevcontainerConfig{
		Id: configID,
		Request: &proto.UpRequest{
			Repo:    "brotherlogic/test-repo",
			Branch:  "main",
			Prompt:  "Run pi task",
			Harness: proto.Harness_HARNESS_PI,
			Identifier: &proto.Identifier{
				Id: &proto.Identifier_IssueNumber{IssueNumber: 349},
			},
		},
		State: proto.State_DCM_RECEIVED,
	}
	globalCache.Update(configID, devConfig)
	defer globalCache.Delete(configID)

	var wg sync.WaitGroup
	sem := make(chan struct{}, 1)
	sem <- struct{}{}
	wg.Add(1)

	cfg := &config{
		startupCommand: "",
	}

	processManualUpRequest(context.Background(), devConfig, &wg, cfg, sem)
	wg.Wait()

	cached, ok := globalCache.Get(configID)
	if !ok {
		t.Fatalf("expected container in cache")
	}
	if cached.State != proto.State_DCM_FAILED {
		t.Errorf("expected state DCM_FAILED upon installation failure, got %v", cached.State)
	}

	var foundDelete bool
	for _, cmd := range capturedCommands {
		if cmd[0] == devpodExe && cmd[1] == "delete" && cmd[2] == configID {
			foundDelete = true
		}
	}
	if !foundDelete {
		t.Errorf("expected devpod delete to be called for failed container, captured: %v", capturedCommands)
	}
}

// TestProcessManualUpRequest_HarnessPi_InjectionFailure verifies that when HARNESS_PI command injection fails via tmux send-keys,
// the devcontainer state is transitioned to DCM_FAILED and the container cleanup is triggered.
func TestProcessManualUpRequest_HarnessPi_InjectionFailure(t *testing.T) {
	oldInterval := pollingInterval
	oldTimeout := pollingTimeout
	pollingInterval = 1 * time.Millisecond
	pollingTimeout = 100 * time.Millisecond
	defer func() {
		pollingInterval = oldInterval
		pollingTimeout = oldTimeout
	}()

	originalCommandRunner := commandRunner
	defer func() { commandRunner = originalCommandRunner }()

	var capturedCommands [][]string
	commandRunner = func(name string, args ...string) ([]byte, error) {
		capturedCommands = append(capturedCommands, append([]string{name}, args...))
		if name == devpodExe && len(args) >= 4 && args[0] == "ssh" && args[2] == "--command" {
			cmdStr := args[3]
			if strings.Contains(cmdStr, "command -v pi") {
				return []byte("pi"), nil
			}
			if strings.Contains(cmdStr, "has-session") {
				return []byte("session exists"), nil
			}
			if strings.Contains(cmdStr, "send-keys") {
				return []byte("injection failed"), fmt.Errorf("tmux send-keys failed")
			}
		}
		return []byte("success"), nil
	}

	configID := "test-repo-pi-inject-fail"
	devConfig := &proto.DevcontainerConfig{
		Id: configID,
		Request: &proto.UpRequest{
			Repo:    "brotherlogic/test-repo",
			Branch:  "main",
			Prompt:  "Run pi task",
			Harness: proto.Harness_HARNESS_PI,
			Identifier: &proto.Identifier{
				Id: &proto.Identifier_IssueNumber{IssueNumber: 349},
			},
		},
		State: proto.State_DCM_RECEIVED,
	}
	globalCache.Update(configID, devConfig)
	defer globalCache.Delete(configID)

	var wg sync.WaitGroup
	sem := make(chan struct{}, 1)
	sem <- struct{}{}
	wg.Add(1)

	cfg := &config{
		startupCommand: "",
	}

	processManualUpRequest(context.Background(), devConfig, &wg, cfg, sem)
	wg.Wait()

	cached, ok := globalCache.Get(configID)
	if !ok {
		t.Fatalf("expected container in cache")
	}
	if cached.State != proto.State_DCM_FAILED {
		t.Errorf("expected state DCM_FAILED upon injection failure, got %v", cached.State)
	}

	var foundDelete bool
	for _, cmd := range capturedCommands {
		if cmd[0] == devpodExe && cmd[1] == "delete" && cmd[2] == configID {
			foundDelete = true
		}
	}
	if !foundDelete {
		t.Errorf("expected devpod delete to be called for failed container, captured: %v", capturedCommands)
	}
}

// TestProcessManualUpRequest_HarnessUnspecified_ReturnsError verifies that an Up RPC request with HARNESS_UNSPECIFIED returns an InvalidArgument status error.
func TestProcessManualUpRequest_HarnessUnspecified_ReturnsError(t *testing.T) {
	mgrServer := srvPkg.NewServer(globalCache, nil)
	req := &proto.UpRequest{
		Repo:    "brotherlogic/test-repo",
		Branch:  "main",
		Harness: proto.Harness_HARNESS_UNSPECIFIED,
	}
	_, err := mgrServer.Up(context.Background(), req)
	if err == nil {
		t.Fatalf("expected error for HARNESS_UNSPECIFIED, got nil")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument status error, got status: %v (err: %v)", st.Code(), err)
	}
	expectedMsg := "harness must be explicitly specified"
	if !strings.Contains(st.Message(), expectedMsg) {
		t.Errorf("expected error message to contain %q, got %q", expectedMsg, st.Message())
	}
}

func TestDevpodCLISerialization(t *testing.T) {
	originalCommandRunner := commandRunner
	defer func() { commandRunner = originalCommandRunner }()

	var activeInvocations int32
	var maxActiveInvocations int32

	commandRunner = func(name string, args ...string) ([]byte, error) {
		current := atomic.AddInt32(&activeInvocations, 1)
		defer atomic.AddInt32(&activeInvocations, -1)

		for {
			max := atomic.LoadInt32(&maxActiveInvocations)
			if current <= max {
				break
			}
			if atomic.CompareAndSwapInt32(&maxActiveInvocations, max, current) {
				break
			}
		}

		time.Sleep(10 * time.Millisecond)
		return []byte("ok"), nil
	}

	var wg sync.WaitGroup
	goroutines := 10
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			_, _ = runCommandWithLog("test-repo", devpodExe, "up", fmt.Sprintf("id-%d", id))
		}(i)
	}
	wg.Wait()

	if maxActiveInvocations != 1 {
		t.Errorf("expected max active devpod CLI invocations to be 1 (serialized), got %d", maxActiveInvocations)
	}
}

func TestAdjustIssueLabels_ReasonLogging(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	client := github.NewClient(nil)
	u, _ := url.Parse(server.URL + "/")
	client.BaseURL = u
	client.UploadURL = u

	var addedLabels []string
	var removedLabels []string
	var mu sync.Mutex

	mux.HandleFunc("/repos/test-owner/test-repo/issues/373", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"number": 373, "labels": [{"name": "container-creating"}]}`)
	})
	mux.HandleFunc("/repos/test-owner/test-repo/issues/373/labels", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			var labels []string
			json.NewDecoder(r.Body).Decode(&labels)
			mu.Lock()
			addedLabels = append(addedLabels, labels...)
			mu.Unlock()
			fmt.Fprint(w, `[]`)
			return
		}
	})
	mux.HandleFunc("/repos/test-owner/test-repo/issues/373/labels/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodDelete {
			parts := strings.Split(r.URL.Path, "/")
			label := parts[len(parts)-1]
			mu.Lock()
			removedLabels = append(removedLabels, label)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
	})

	adjustIssueLabels(context.Background(), client, "test-owner", "test-repo", 373, "container-failed", []string{"container-creating", "container-ready"}, "startup command injection failed: pi installation error")

	mu.Lock()
	defer mu.Unlock()
	if len(addedLabels) != 1 || addedLabels[0] != "container-failed" {
		t.Errorf("expected added label 'container-failed', got: %v", addedLabels)
	}
	if len(removedLabels) != 1 || removedLabels[0] != "container-creating" {
		t.Errorf("expected removed label 'container-creating', got: %v", removedLabels)
	}
}









