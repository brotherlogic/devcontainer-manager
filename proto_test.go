package main

import (
	"testing"

	"github.com/brotherlogic/devcontainer-manager/proto"
)

func TestProtoTypesExist(t *testing.T) {
	// Try to instantiate proto messages to verify they are compiled and available
	var req proto.ListContainersRequest
	var resp proto.ListContainersResponse
	var container proto.Container
	var status proto.ContainerStatus = proto.ContainerStatus_RUNNING

	t.Logf("Proto types loaded successfully: req=%T, resp=%T, container=%T, status=%v", &req, &resp, &container, status)
}
