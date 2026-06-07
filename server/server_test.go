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

	container := &proto.Container{
		Id:            "test-container",
		RepositoryUrl: "https://github.com/test/repo",
		BranchOrIssue: "main",
		Status:        proto.ContainerStatus_RUNNING,
	}

	cache.Update("test-container", container)

	list := cache.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 container, got %d", len(list))
	}

	if list[0].Id != "test-container" {
		t.Errorf("expected container ID 'test-container', got '%s'", list[0].Id)
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
				container := &proto.Container{
					Id:            fmt.Sprintf("container-%d-%d", id, j),
					RepositoryUrl: "https://github.com/test/repo",
					Status:        proto.ContainerStatus_STARTING,
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

func TestListContainersRPC(t *testing.T) {
	cache := NewCache()
	server := NewServer(cache)

	container := &proto.Container{
		Id:            "rpc-container",
		RepositoryUrl: "https://github.com/test/rpc",
		Status:        proto.ContainerStatus_FAILED,
		ErrorMessage:  "failed to start",
	}
	cache.Update("rpc-container", container)

	resp, err := server.ListContainers(context.Background(), &proto.ListContainersRequest{})
	if err != nil {
		t.Fatalf("unexpected error from ListContainers: %v", err)
	}

	if len(resp.Containers) != 1 {
		t.Fatalf("expected 1 container in RPC response, got %d", len(resp.Containers))
	}

	if resp.Containers[0].Id != "rpc-container" {
		t.Errorf("expected rpc-container, got %s", resp.Containers[0].Id)
	}
}
