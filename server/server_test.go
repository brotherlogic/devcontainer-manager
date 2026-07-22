package server

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/brotherlogic/devcontainer-manager/proto"
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
		Repo:   "brotherlogic/test-repo",
		Branch: "feature/test",
	}

	resp, err := server.Up(context.Background(), req)
	if err != nil {
		t.Fatalf("Up failed: %v", err)
	}

	if resp.Config.Id != "brotherlogic/test-repo:feature/test" {
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
		Repo:   "brotherlogic/test-repo",
		Branch: "feature/new-branch",
	}

	resp, err := server.Up(context.Background(), req)
	if err != nil {
		t.Fatalf("Up failed: %v", err)
	}

	if resp.Config.Id != "brotherlogic/test-repo:feature/new-branch" {
		t.Errorf("unexpected config ID: got %s", resp.Config.Id)
	}

	if base, created := mockGit.createdBranches["feature/new-branch"]; !created || base != "main" {
		t.Errorf("expected branch feature/new-branch created off main, got base %s (created=%v)", base, created)
	}
}

