package server

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/brotherlogic/devcontainer-manager/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestCacheUpdateAndList(t *testing.T) {
	cache := NewCache()

	container := &proto.DevcontainerConfig{
		Id:    "test-container",
		State: proto.State_DCM_READY,
	}

	cache.Update("test-container", container)

	list := cache.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 container, got %d", len(list))
	}

	if list[0].Id != "test-container" {
		t.Errorf("expected container ID 'test-container', got '%s'", list[0].Id)
	}

	// Test Delete
	cache.Delete("test-container")
	list = cache.List()
	if len(list) != 0 {
		t.Errorf("expected 0 containers after deletion, got %d", len(list))
	}
}

func TestCacheConcurrency(t *testing.T) {
	cache := NewCache()
	var wg sync.WaitGroup

	numGoroutines := 50
	numOperations := 100

	wg.Add(numGoroutines * 2)

	// Concurrent writers
	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < numOperations; j++ {
				container := &proto.DevcontainerConfig{
					Id:    fmt.Sprintf("container-%d-%d", id, j),
					State: proto.State_DCM_CREATING,
				}
				cache.Update(container.Id, container)
			}
		}(i)
	}

	// Concurrent readers
	for i := 0; i < numGoroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < numOperations; j++ {
				_ = cache.List()
			}
		}()
	}

	wg.Wait()
}

func TestListRPC(t *testing.T) {
	cache := NewCache()
	server := NewServer(cache, nil)

	container := &proto.DevcontainerConfig{
		Id:    "rpc-container",
		State: proto.State_DCM_FAILED,
	}
	cache.Update("rpc-container", container)

	resp, err := server.List(context.Background(), &proto.ListRequest{})
	if err != nil {
		t.Fatalf("unexpected error from List: %v", err)
	}

	if len(resp.Configs) != 1 {
		t.Fatalf("expected 1 container in RPC response, got %d", len(resp.Configs))
	}

	if resp.Configs[0].Id != "rpc-container" {
		t.Errorf("expected rpc-container, got %s", resp.Configs[0].Id)
	}
}

type mockGitClient struct {
	existingBranches map[string]bool
	createdBranches  map[string]string // newBranch -> baseBranch
	defaultBranch    string
}

func (m *mockGitClient) BranchExists(ctx context.Context, repo, branch string) (bool, error) {
	return m.existingBranches[branch], nil
}

func (m *mockGitClient) CreateBranch(ctx context.Context, repo, newBranch, baseBranch string) error {
	if m.createdBranches == nil {
		m.createdBranches = make(map[string]string)
	}
	m.createdBranches[newBranch] = baseBranch
	m.existingBranches[newBranch] = true
	return nil
}

func (m *mockGitClient) GetDefaultBranch(ctx context.Context, repo string) (string, error) {
	if m.defaultBranch != "" {
		return m.defaultBranch, nil
	}
	return "main", nil
}

func TestUpRPCExistingBranch(t *testing.T) {
	cache := NewCache()
	mockGit := &mockGitClient{
		existingBranches: map[string]bool{"feature/test": true},
	}
	server := NewServer(cache, mockGit)

	req := &proto.UpRequest{
		Repo:    "brotherlogic/test-repo",
		Branch:  "feature/test",
		Harness: proto.Harness_HARNESS_ANTIGRAVITY,
	}

	resp, err := server.Up(context.Background(), req)
	if err != nil {
		t.Fatalf("Up failed: %v", err)
	}

	if resp.Config.Id != "test-repo-feature-test" {
		t.Errorf("unexpected config ID: got %s", resp.Config.Id)
	}

	if len(mockGit.createdBranches) > 0 {
		t.Errorf("expected no branch creation, but created: %v", mockGit.createdBranches)
	}
}

func TestUpRPCAutoCreateBranch(t *testing.T) {
	cache := NewCache()
	mockGit := &mockGitClient{
		existingBranches: map[string]bool{},
		defaultBranch:    "main",
	}
	server := NewServer(cache, mockGit)

	req := &proto.UpRequest{
		Repo:    "brotherlogic/test-repo",
		Branch:  "feature/new-branch",
		Harness: proto.Harness_HARNESS_ANTIGRAVITY,
	}

	resp, err := server.Up(context.Background(), req)
	if err != nil {
		t.Fatalf("Up failed: %v", err)
	}

	if resp.Config.Id != "test-repo-feature-new-branch" {
		t.Errorf("unexpected config ID: got %s", resp.Config.Id)
	}

	if base, created := mockGit.createdBranches["feature/new-branch"]; !created || base != "main" {
		t.Errorf("expected branch feature/new-branch created off main, got base %s (created=%v)", base, created)
	}
}

func TestUpRPCIssueCleanID(t *testing.T) {
	cache := NewCache()
	server := NewServer(cache, nil)

	req := &proto.UpRequest{
		Repo:   "https://github.com/brotherlogic/devcontainer-manager/issues/275",
		Branch: "feature/test-38b30f88-4354-48d5-89a0-3d0148797d3a",
		Identifier: &proto.Identifier{
			Id: &proto.Identifier_IssueNumber{IssueNumber: 275},
		},
		Harness: proto.Harness_HARNESS_ANTIGRAVITY,
	}

	resp, err := server.Up(context.Background(), req)
	if err != nil {
		t.Fatalf("Up failed: %v", err)
	}

	expectedID := "devcontainer-manager-275"
	if resp.Config.Id != expectedID {
		t.Errorf("expected config ID %q, got %q", expectedID, resp.Config.Id)
	}
}

func TestUpRPC_ModelValidation(t *testing.T) {
	cache := NewCache()
	srv := NewServer(cache, nil)
	srv.SetSupportedModels([]string{"gemini-3.6-flash-low", "claude-sonnet-4-6"})

	// Test valid model
	reqValid := &proto.UpRequest{
		Repo:    "test/repo",
		Model:   "gemini-3.6-flash-low",
		Harness: proto.Harness_HARNESS_ANTIGRAVITY,
	}
	resp, err := srv.Up(context.Background(), reqValid)
	if err != nil {
		t.Fatalf("unexpected error for valid model: %v", err)
	}
	if resp == nil || resp.Config == nil {
		t.Fatalf("expected non-nil config in response")
	}

	// Test invalid model
	reqInvalid := &proto.UpRequest{
		Repo:    "test/repo",
		Model:   "invalid-model-name",
		Harness: proto.Harness_HARNESS_ANTIGRAVITY,
	}
	_, err = srv.Up(context.Background(), reqInvalid)
	if err == nil {
		t.Fatalf("expected error for invalid model, got nil")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument status error, got: %v", err)
	}
}

func TestUpRPC_HarnessValidation(t *testing.T) {
	cache := NewCache()
	srv := NewServer(cache, nil)

	// Unspecified harness should fail with InvalidArgument
	reqUnspecified := &proto.UpRequest{
		Repo:    "brotherlogic/test-repo",
		Branch:  "main",
		Harness: proto.Harness_HARNESS_UNSPECIFIED,
	}
	_, err := srv.Up(context.Background(), reqUnspecified)
	if err == nil {
		t.Fatalf("expected error for HARNESS_UNSPECIFIED, got nil")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument status error, got: %v", err)
	}
	expectedMsg := "harness must be explicitly specified (HARNESS_ANTIGRAVITY or HARNESS_PI)"
	if !strings.Contains(st.Message(), expectedMsg) {
		t.Errorf("expected error message to contain %q, got %q", expectedMsg, st.Message())
	}

	// HARNESS_ANTIGRAVITY harness should succeed
	reqAntigravity := &proto.UpRequest{
		Repo:    "brotherlogic/test-repo",
		Branch:  "main",
		Harness: proto.Harness_HARNESS_ANTIGRAVITY,
	}
	respAnti, err := srv.Up(context.Background(), reqAntigravity)
	if err != nil {
		t.Fatalf("unexpected error for HARNESS_ANTIGRAVITY: %v", err)
	}
	if respAnti == nil || respAnti.Config == nil {
		t.Fatalf("expected non-nil response config for HARNESS_ANTIGRAVITY")
	}

	// HARNESS_PI harness should succeed
	reqPi := &proto.UpRequest{
		Repo:    "brotherlogic/test-repo",
		Branch:  "main",
		Harness: proto.Harness_HARNESS_PI,
	}
	respPi, err := srv.Up(context.Background(), reqPi)
	if err != nil {
		t.Fatalf("unexpected error for HARNESS_PI: %v", err)
	}
	if respPi == nil || respPi.Config == nil {
		t.Fatalf("expected non-nil response config for HARNESS_PI")
	}
}

func TestFetchAndRefreshSupportedModels(t *testing.T) {
	cache := NewCache()
	srv := NewServer(cache, nil)
	srv.SetCommandRunner(func(name string, args ...string) ([]byte, error) {
		output := "gemini-3.6-flash-low     Gemini 3.6 Flash (Low)\nclaude-sonnet-4-6         Claude Sonnet 4.6 (Thinking)\n"
		return []byte(output), nil
	})

	err := srv.RefreshSupportedModels()
	if err != nil {
		t.Fatalf("unexpected error refreshing models: %v", err)
	}

	models := srv.GetSupportedModels()
	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(models))
	}

	if !srv.IsModelSupported("gemini-3.6-flash-low") {
		t.Errorf("expected gemini-3.6-flash-low to be supported")
	}
	if !srv.IsModelSupported("claude-sonnet-4-6") {
		t.Errorf("expected claude-sonnet-4-6 to be supported")
	}
	if srv.IsModelSupported("unknown-model") {
		t.Errorf("expected unknown-model to NOT be supported")
	}
}

func TestPushPrompt_NotFound(t *testing.T) {
	cache := NewCache()
	srv := NewServer(cache, nil)

	req := &proto.PushPromptRequest{
		Id:     "non-existent-container",
		Prompt: "hello",
	}

	_, err := srv.PushPrompt(context.Background(), req)
	if err == nil {
		t.Fatalf("expected error for non-existent container, got nil")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.NotFound {
		t.Errorf("expected NotFound status error, got: %v", err)
	}
}

func TestPushPrompt_NotReadyState(t *testing.T) {
	cache := NewCache()
	srv := NewServer(cache, nil)

	container := &proto.DevcontainerConfig{
		Id:    "building-container",
		State: proto.State_DCM_CREATING,
	}
	cache.Update("building-container", container)

	req := &proto.PushPromptRequest{
		Id:     "building-container",
		Prompt: "hello",
	}

	_, err := srv.PushPrompt(context.Background(), req)
	if err == nil {
		t.Fatalf("expected error for container not in DCM_READY state, got nil")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.FailedPrecondition {
		t.Errorf("expected FailedPrecondition status error, got: %v", err)
	}
}

func TestPushPrompt_Success(t *testing.T) {
	cache := NewCache()
	srv := NewServer(cache, nil)

	container := &proto.DevcontainerConfig{
		Id:    "ready-container",
		State: proto.State_DCM_READY,
	}
	cache.Update("ready-container", container)

	var executedCmds [][]string
	srv.SetCommandRunner(func(name string, args ...string) ([]byte, error) {
		executedCmds = append(executedCmds, append([]string{name}, args...))
		return []byte("success"), nil
	})

	req := &proto.PushPromptRequest{
		Id:     "ready-container",
		Prompt: "hello prompt",
	}

	resp, err := srv.PushPrompt(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil {
		t.Fatalf("expected non-nil response")
	}

	// Verify command execution
	if len(executedCmds) != 2 {
		t.Fatalf("expected 2 commands to be executed, got %d: %v", len(executedCmds), executedCmds)
	}

	// First command: tmux has-session -t 'ready-container'
	expectedCmd1 := []string{devpodExe, "ssh", "ready-container", "--command", "tmux has-session -t 'ready-container'"}
	for i, v := range expectedCmd1 {
		if executedCmds[0][i] != v {
			t.Errorf("expected cmd1[%d] to be %q, got %q", i, v, executedCmds[0][i])
		}
	}

	// Second command: tmux send-keys -t 'ready-container' 'hello prompt' C-m
	expectedCmd2 := []string{devpodExe, "ssh", "ready-container", "--command", "tmux send-keys -t 'ready-container' 'hello prompt' C-m"}
	for i, v := range expectedCmd2 {
		if executedCmds[1][i] != v {
			t.Errorf("expected cmd2[%d] to be %q, got %q", i, v, executedCmds[1][i])
		}
	}

	// Verify container remains in DCM_READY state after prompt execution
	updated, ok := cache.Get("ready-container")
	if !ok || updated.GetState() != proto.State_DCM_READY {
		t.Errorf("expected container state to remain DCM_READY after PushPrompt, got %v", updated.GetState())
	}
}

func TestPushPrompt_FallbackSuccess(t *testing.T) {
	cache := NewCache()
	srv := NewServer(cache, nil)

	// Container ID with trailing issue number
	container := &proto.DevcontainerConfig{
		Id:    "ready-container-42",
		State: proto.State_DCM_READY,
	}
	cache.Update("ready-container-42", container)

	var executedCmds [][]string
	srv.SetCommandRunner(func(name string, args ...string) ([]byte, error) {
		executedCmds = append(executedCmds, append([]string{name}, args...))
		// Fail the direct has-session check, succeed the rest
		if len(args) >= 4 && args[0] == "ssh" && args[3] == "tmux has-session -t 'ready-container-42'" {
			return nil, fmt.Errorf("session not found")
		}
		return []byte("success"), nil
	})

	req := &proto.PushPromptRequest{
		Id:     "ready-container-42",
		Prompt: "hello prompt",
	}

	resp, err := srv.PushPrompt(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil {
		t.Fatalf("expected non-nil response")
	}

	// Verify command execution: 1. check main session (fail), 2. check base session (success), 3. send-keys (success)
	if len(executedCmds) != 3 {
		t.Fatalf("expected 3 commands to be executed, got %d: %v", len(executedCmds), executedCmds)
	}

	expectedCmd1 := []string{devpodExe, "ssh", "ready-container-42", "--command", "tmux has-session -t 'ready-container-42'"}
	for i, v := range expectedCmd1 {
		if executedCmds[0][i] != v {
			t.Errorf("expected cmd1[%d] to be %q, got %q", i, v, executedCmds[0][i])
		}
	}

	expectedCmd2 := []string{devpodExe, "ssh", "ready-container-42", "--command", "tmux has-session -t 'ready-container'"}
	for i, v := range expectedCmd2 {
		if executedCmds[1][i] != v {
			t.Errorf("expected cmd2[%d] to be %q, got %q", i, v, executedCmds[1][i])
		}
	}

	expectedCmd3 := []string{devpodExe, "ssh", "ready-container-42", "--command", "tmux send-keys -t 'ready-container' 'hello prompt' C-m"}
	for i, v := range expectedCmd3 {
		if executedCmds[2][i] != v {
			t.Errorf("expected cmd3[%d] to be %q, got %q", i, v, executedCmds[2][i])
		}
	}
}

func TestPushPrompt_SessionNotFound(t *testing.T) {
	cache := NewCache()
	srv := NewServer(cache, nil)

	container := &proto.DevcontainerConfig{
		Id:    "ready-container-42",
		State: proto.State_DCM_READY,
	}
	cache.Update("ready-container-42", container)

	srv.SetCommandRunner(func(name string, args ...string) ([]byte, error) {
		return nil, fmt.Errorf("session not found")
	})

	req := &proto.PushPromptRequest{
		Id:     "ready-container-42",
		Prompt: "hello prompt",
	}

	_, err := srv.PushPrompt(context.Background(), req)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.FailedPrecondition {
		t.Errorf("expected FailedPrecondition, got: %v", err)
	}
}

func TestPushPrompt_SendCommandFailure(t *testing.T) {
	cache := NewCache()
	srv := NewServer(cache, nil)

	container := &proto.DevcontainerConfig{
		Id:    "ready-container",
		State: proto.State_DCM_READY,
	}
	cache.Update("ready-container", container)

	srv.SetCommandRunner(func(name string, args ...string) ([]byte, error) {
		if len(args) >= 4 && args[0] == "ssh" && strings.HasPrefix(args[3], "tmux send-keys") {
			return nil, fmt.Errorf("ssh connection failed")
		}
		return []byte("success"), nil
	})

	req := &proto.PushPromptRequest{
		Id:     "ready-container",
		Prompt: "hello prompt",
	}

	_, err := srv.PushPrompt(context.Background(), req)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.Internal {
		t.Errorf("expected Internal, got: %v", err)
	}
}

func TestDown_NotFound(t *testing.T) {
	cache := NewCache()
	srv := NewServer(cache, nil)

	req := &proto.DownRequest{
		Id: "non-existent-container",
	}

	_, err := srv.Down(context.Background(), req)
	if err == nil {
		t.Fatalf("expected error for non-existent container, got nil")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.NotFound {
		t.Errorf("expected NotFound status error, got: %v", err)
	}
}

func TestDown_Success(t *testing.T) {
	cache := NewCache()
	srv := NewServer(cache, nil)

	container := &proto.DevcontainerConfig{
		Id:    "active-container",
		State: proto.State_DCM_READY,
	}
	cache.Update("active-container", container)

	req := &proto.DownRequest{
		Id: "active-container",
	}

	resp, err := srv.Down(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error on Down: %v", err)
	}
	if resp == nil {
		t.Fatalf("expected non-nil response on Down")
	}

	// Verify container is cleaned up / deleted from cache
	_, ok := cache.Get("active-container")
	if ok {
		t.Errorf("expected container active-container to be removed from cache on Down RPC call")
	}
}

func TestTokenUsageSerializationAndServerResponses(t *testing.T) {
	cache := NewCache()
	srv := NewServer(cache, nil)

	tokenUsage := &proto.TokenUsage{
		TotalTokens:   15420,
		Status:        proto.ExtractionStatus_EXTRACTION_SUCCESS,
		FailureReason: "",
	}

	container := &proto.DevcontainerConfig{
		Id:         "token-container",
		State:      proto.State_DCM_READY,
		TokenUsage: tokenUsage,
	}

	cache.Update("token-container", container)

	// Verify retrieval from cache preserves TokenUsage
	retrieved, ok := cache.Get("token-container")
	if !ok {
		t.Fatalf("expected container 'token-container' in cache")
	}

	if retrieved.GetTokenUsage() == nil {
		t.Fatalf("expected non-nil TokenUsage")
	}

	if retrieved.GetTokenUsage().GetTotalTokens() != 15420 {
		t.Errorf("expected TotalTokens 15420, got %d", retrieved.GetTokenUsage().GetTotalTokens())
	}

	if retrieved.GetTokenUsage().GetStatus() != proto.ExtractionStatus_EXTRACTION_SUCCESS {
		t.Errorf("expected status EXTRACTION_SUCCESS, got %v", retrieved.GetTokenUsage().GetStatus())
	}

	// Verify gRPC List RPC includes TokenUsage
	resp, err := srv.List(context.Background(), &proto.ListRequest{})
	if err != nil {
		t.Fatalf("unexpected error from List: %v", err)
	}

	if len(resp.GetConfigs()) != 1 {
		t.Fatalf("expected 1 config, got %d", len(resp.GetConfigs()))
	}

	config := resp.GetConfigs()[0]
	if config.GetTokenUsage() == nil {
		t.Fatalf("expected non-nil TokenUsage in List RPC response")
	}

	if config.GetTokenUsage().GetTotalTokens() != 15420 {
		t.Errorf("expected TotalTokens 15420 in List RPC response, got %d", config.GetTokenUsage().GetTotalTokens())
	}

	if config.GetTokenUsage().GetStatus() != proto.ExtractionStatus_EXTRACTION_SUCCESS {
		t.Errorf("expected status EXTRACTION_SUCCESS in List RPC response, got %v", config.GetTokenUsage().GetStatus())
	}

	// Test update with nil TokenUsage preserving existing TokenUsage in cache
	updatedNilTokenUsage := &proto.DevcontainerConfig{
		Id:    "token-container",
		State: proto.State_DCM_READY,
	}
	cache.Update("token-container", updatedNilTokenUsage)

	retrievedNilUpdate, ok := cache.Get("token-container")
	if !ok || retrievedNilUpdate.GetTokenUsage() == nil {
		t.Fatalf("expected cached TokenUsage to be preserved after update with nil TokenUsage")
	}

	if retrievedNilUpdate.GetTokenUsage().GetTotalTokens() != 15420 {
		t.Errorf("expected preserved TotalTokens 15420, got %d", retrievedNilUpdate.GetTokenUsage().GetTotalTokens())
	}
}



