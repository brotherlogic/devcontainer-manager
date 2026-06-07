package main

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/brotherlogic/devcontainer-manager/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestParseFlagsPort(t *testing.T) {
	// Verify that the -port command line flag exists and defaults to 50051
	cfg, err := parseFlags([]string{})
	if err != nil {
		t.Fatalf("unexpected error parsing flags: %v", err)
	}
	if cfg.port != 50051 {
		t.Errorf("expected default port to be 50051, got %d", cfg.port)
	}

	cfg, err = parseFlags([]string{"-port", "12345"})
	if err != nil {
		t.Fatalf("unexpected error parsing flags: %v", err)
	}
	if cfg.port != 12345 {
		t.Errorf("expected port to be 12345, got %d", cfg.port)
	}
}

func TestGRPCServerIntegration(t *testing.T) {
	// Spin up the server on a random port (0) to verify integration
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("failed to listen on random port: %v", err)
	}
	port := lis.Addr().(*net.TCPAddr).Port
	_ = lis.Close() // Close it so startGRPCServer can bind to it (or similar)

	// Since we are running the server in the background, let's test startGRPCServer directly
	// or through our custom initialization.
	cache := initCache()
	if cache == nil {
		t.Fatal("cache is nil")
	}

	// Add a test container
	testContainer := &proto.Container{
		Id:            "test-integration-container",
		RepositoryUrl: "git@github.com:foo/bar",
		Status:        proto.ContainerStatus_RUNNING,
	}
	cache.Update("test-integration-container", testContainer)

	srv, err := startGRPCServer(port, cache)
	if err != nil {
		t.Fatalf("failed to start gRPC server: %v", err)
	}
	defer srv.GracefulStop()

	// Wait a moment for server to start
	time.Sleep(100 * time.Millisecond)

	// Connect as client
	conn, err := grpc.Dial(fmt.Sprintf("localhost:%d", port), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to dial gRPC server: %v", err)
	}
	defer conn.Close()

	client := proto.NewDashboardServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	resp, err := client.ListContainers(ctx, &proto.ListContainersRequest{})
	if err != nil {
		t.Fatalf("failed to list containers: %v", err)
	}

	found := false
	for _, c := range resp.Containers {
		if c.Id == "test-integration-container" && c.Status == proto.ContainerStatus_RUNNING {
			found = true
			break
		}
	}

	if !found {
		t.Errorf("expected to find test-integration-container with RUNNING status in response, got: %v", resp.Containers)
	}
}
