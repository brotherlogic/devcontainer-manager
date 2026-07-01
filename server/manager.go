package server

import (
	"context"
	"fmt"
	"time"

	"github.com/brotherlogic/devcontainer-manager/proto"
	pstore_client "github.com/brotherlogic/pstore/client"
	pstore_pb "github.com/brotherlogic/pstore/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

// ManagerServer implements the proto.ManagerServiceServer interface.
type ManagerServer struct {
	proto.UnimplementedManagerServiceServer
	pstoreClient pstore_client.PStoreClient
	JobQueue     chan *proto.DevcontainerConfig
}

// NewManagerServer creates and initializes a new ManagerServiceServer implementation.
func NewManagerServer(pstoreClient pstore_client.PStoreClient) *ManagerServer {
	return &ManagerServer{
		pstoreClient: pstoreClient,
		JobQueue:     make(chan *proto.DevcontainerConfig, 100),
	}
}

func (s *ManagerServer) saveConfig(ctx context.Context, config *proto.DevcontainerConfig) error {
	anyConfig, err := anypb.New(config)
	if err != nil {
		return err
	}
	_, err = s.pstoreClient.Write(ctx, &pstore_pb.WriteRequest{
		Key:   fmt.Sprintf("devcontainer-manager/%s", config.Id),
		Value: anyConfig,
	})
	return err
}

func (s *ManagerServer) Up(ctx context.Context, req *proto.UpRequest) (*proto.UpResponse, error) {
	// Validate
	if req.Repo == "" {
		return nil, fmt.Errorf("repo is required")
	}

	id := fmt.Sprintf("%s-%d", req.Repo, time.Now().UnixNano())

	config := &proto.DevcontainerConfig{
		Id:      id,
		Request: req,
		State:   proto.State_DCM_RECEIVED,
	}

	if err := s.saveConfig(ctx, config); err != nil {
		return nil, err
	}

	// Enqueue job
	select {
	case s.JobQueue <- config:
	default:
		return nil, fmt.Errorf("job queue is full")
	}

	return &proto.UpResponse{Config: config}, nil
}

func (s *ManagerServer) Down(ctx context.Context, req *proto.DownRequest) (*proto.DownResponse, error) {
	readRes, err := s.pstoreClient.Read(ctx, &pstore_pb.ReadRequest{
		Key: fmt.Sprintf("devcontainer-manager/%s", req.Id),
	})
	if err != nil {
		return nil, err
	}

	config := &proto.DevcontainerConfig{}
	if err := readRes.Value.UnmarshalTo(config); err != nil {
		return nil, err
	}

	// Update state
	config.State = proto.State_DCM_HARD_FAILED

	if err := s.saveConfig(ctx, config); err != nil {
		return nil, err
	}

	return &proto.DownResponse{}, nil
}

func (s *ManagerServer) List(ctx context.Context, req *proto.ListRequest) (*proto.ListResponse, error) {
	keysRes, err := s.pstoreClient.GetKeys(ctx, &pstore_pb.GetKeysRequest{
		Prefix: "devcontainer-manager/",
	})
	if err != nil {
		return nil, err
	}

	var configs []*proto.DevcontainerConfig
	for _, key := range keysRes.Keys {
		readRes, err := s.pstoreClient.Read(ctx, &pstore_pb.ReadRequest{Key: key})
		if err != nil {
			continue
		}
		config := &proto.DevcontainerConfig{}
		if err := readRes.Value.UnmarshalTo(config); err == nil {
			configs = append(configs, config)
		}
	}

	return &proto.ListResponse{Configs: configs}, nil
}
