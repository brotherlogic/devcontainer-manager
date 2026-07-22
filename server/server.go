package server

import (
	"context"
	"sync"

	"github.com/brotherlogic/devcontainer-manager/proto"
)

// Cache is a thread-safe in-memory cache for storing container status.
type Cache struct {
	mu         sync.RWMutex
	containers map[string]*proto.DevcontainerConfig
}

// NewCache creates and initializes a new Cache.
func NewCache() *Cache {
	return &Cache{
		containers: make(map[string]*proto.DevcontainerConfig),
	}
}

// Update adds or updates a container status in the cache.
func (c *Cache) Update(id string, container *proto.DevcontainerConfig) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.containers[id] = container
}

// Delete removes a container status from the cache by its ID.
func (c *Cache) Delete(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.containers, id)
}

// List returns a slice of all container statuses stored in the cache.
func (c *Cache) List() []*proto.DevcontainerConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	list := make([]*proto.DevcontainerConfig, 0, len(c.containers))
	for _, container := range c.containers {
		list = append(list, container)
	}
	return list
}

// GitClient abstracts git / github operations required for branch management.
type GitClient interface {
	BranchExists(ctx context.Context, repo, branch string) (bool, error)
	CreateBranch(ctx context.Context, repo, newBranch, baseBranch string) error
	GetDefaultBranch(ctx context.Context, repo string) (string, error)
}

// Server implements the proto.ManagerServiceServer interface.
type Server struct {
	proto.UnimplementedManagerServiceServer
	cache     *Cache
	gitClient GitClient
}

// NewServer creates and initializes a new gRPC server implementation.
func NewServer(cache *Cache, gitClient GitClient) *Server {
	return &Server{
		cache:     cache,
		gitClient: gitClient,
	}
}

// List returns the list of all containers stored in the cache.
func (s *Server) List(ctx context.Context, req *proto.ListRequest) (*proto.ListResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &proto.ListResponse{
		Configs: s.cache.List(),
	}, nil
}

// Up handles the Up RPC request, checking and auto-creating target branches if needed.
func (s *Server) Up(ctx context.Context, req *proto.UpRequest) (*proto.UpResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if s.gitClient != nil && req.GetBranch() != "" {
		exists, err := s.gitClient.BranchExists(ctx, req.GetRepo(), req.GetBranch())
		if err != nil {
			return nil, err
		}

		if !exists {
			defaultBranch, err := s.gitClient.GetDefaultBranch(ctx, req.GetRepo())
			if err != nil || defaultBranch == "" {
				defaultBranch = "main"
			}

			if err := s.gitClient.CreateBranch(ctx, req.GetRepo(), req.GetBranch(), defaultBranch); err != nil {
				return nil, err
			}
		}
	}

	config := &proto.DevcontainerConfig{
		Id:      req.GetRepo() + ":" + req.GetBranch(),
		Request: req,
		State:   proto.State_DCM_READY,
	}

	s.cache.Update(config.Id, config)

	return &proto.UpResponse{
		Config: config,
	}, nil
}

