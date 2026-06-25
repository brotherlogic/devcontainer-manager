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

func TestManagerProtoTypesExist(t *testing.T) {
	var upReq proto.UpRequest
	var upResp proto.UpResponse
	var downReq proto.DownRequest
	var downResp proto.DownResponse
	var listReq proto.ListRequest
	var listResp proto.ListResponse
	var state proto.State = proto.State_DCM_RECEIVED
	var config proto.DevcontainerConfig
	var identifier proto.Identifier

	t.Logf("Manager proto types loaded: %T %T %T %T %T %T %v %T %T", 
		&upReq, &upResp, &downReq, &downResp, &listReq, &listResp, state, &config, &identifier)
}
