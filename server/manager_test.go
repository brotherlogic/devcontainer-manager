package server

import (
	"context"
	"strings"
	"testing"

	"github.com/brotherlogic/devcontainer-manager/proto"
	pstore_pb "github.com/brotherlogic/pstore/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

type fakePStore struct {
	data map[string]*anypb.Any
}

func (f *fakePStore) Read(ctx context.Context, req *pstore_pb.ReadRequest) (*pstore_pb.ReadResponse, error) {
	val, ok := f.data[req.Key]
	if !ok {
		return nil, context.DeadlineExceeded // simulate error
	}
	return &pstore_pb.ReadResponse{Value: val}, nil
}

func (f *fakePStore) Write(ctx context.Context, req *pstore_pb.WriteRequest) (*pstore_pb.WriteResponse, error) {
	f.data[req.Key] = req.Value
	return &pstore_pb.WriteResponse{}, nil
}

func (f *fakePStore) GetKeys(ctx context.Context, req *pstore_pb.GetKeysRequest) (*pstore_pb.GetKeysResponse, error) {
	var keys []string
	for k := range f.data {
		if strings.HasPrefix(k, req.Prefix) {
			keys = append(keys, k)
		}
	}
	return &pstore_pb.GetKeysResponse{Keys: keys}, nil
}

func (f *fakePStore) Delete(ctx context.Context, req *pstore_pb.DeleteRequest) (*pstore_pb.DeleteResponse, error) {
	delete(f.data, req.Key)
	return &pstore_pb.DeleteResponse{}, nil
}

func (f *fakePStore) Count(ctx context.Context, req *pstore_pb.CountRequest) (*pstore_pb.CountResponse, error) {
	return &pstore_pb.CountResponse{Count: int64(len(f.data))}, nil
}

func TestManagerUp(t *testing.T) {
	fakeStore := &fakePStore{data: make(map[string]*anypb.Any)}
	s := NewManagerServer(fakeStore)

	req := &proto.UpRequest{
		Repo:   "test-repo",
		Branch: "main",
	}

	resp, err := s.Up(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.Config == nil {
		t.Fatalf("expected config in response, got nil")
	}

	if resp.Config.State != proto.State_DCM_RECEIVED {
		t.Errorf("expected state DCM_RECEIVED, got %v", resp.Config.State)
	}

	select {
	case <-s.JobQueue:
	default:
		t.Errorf("expected job to be enqueued")
	}
}

func TestManagerDown(t *testing.T) {
	fakeStore := &fakePStore{data: make(map[string]*anypb.Any)}
	s := NewManagerServer(fakeStore)

	upReq := &proto.UpRequest{Repo: "test-repo"}
	upResp, err := s.Up(context.Background(), upReq)
	if err != nil {
		t.Fatalf("unexpected error during up: %v", err)
	}

	req := &proto.DownRequest{
		Id: upResp.Config.Id,
	}

	_, err = s.Down(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error during down: %v", err)
	}
}

func TestManagerList(t *testing.T) {
	fakeStore := &fakePStore{data: make(map[string]*anypb.Any)}
	s := NewManagerServer(fakeStore)

	upReq := &proto.UpRequest{Repo: "test-repo"}
	_, err := s.Up(context.Background(), upReq)
	if err != nil {
		t.Fatalf("unexpected error during up: %v", err)
	}

	req := &proto.ListRequest{}

	resp, err := s.List(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error during list: %v", err)
	}

	if resp.Configs == nil || len(resp.Configs) == 0 {
		t.Fatalf("expected configs in response")
	}
}
