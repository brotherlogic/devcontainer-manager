package server

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/brotherlogic/devcontainer-manager/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// CommandRunner defines a function type for running external commands.
type CommandRunner func(name string, args ...string) ([]byte, error)

var defaultCommandRunner CommandRunner = func(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

// Cache is a thread-safe in-memory cache for storing container status.
type Cache struct {
	mu         sync.RWMutex
	containers map[string]*proto.DevcontainerConfig
	manualIDs  map[string]bool
}

// NewCache creates and initializes a new Cache.
func NewCache() *Cache {
	return &Cache{
		containers: make(map[string]*proto.DevcontainerConfig),
		manualIDs:  make(map[string]bool),
	}
}

// Update adds or updates a container status in the cache.
func (c *Cache) Update(id string, container *proto.DevcontainerConfig) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.containers[id] = container
}

// SetManual marks a container ID as manually requested via API.
func (c *Cache) SetManual(id string, manual bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.manualIDs == nil {
		c.manualIDs = make(map[string]bool)
	}
	if manual {
		c.manualIDs[id] = true
	} else {
		delete(c.manualIDs, id)
	}
}

// IsManual returns true if the container was manually requested via API.
func (c *Cache) IsManual(id string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.manualIDs == nil {
		return false
	}
	return c.manualIDs[id]
}

// Delete removes a container status from the cache by its ID.
func (c *Cache) Delete(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.containers, id)
	if c.manualIDs != nil {
		delete(c.manualIDs, id)
	}
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

// Get retrieves a DevcontainerConfig from the cache by its ID.
func (c *Cache) Get(id string) (*proto.DevcontainerConfig, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	cfg, ok := c.containers[id]
	return cfg, ok
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
	cache           *Cache
	gitClient       GitClient
	modelsMu        sync.RWMutex
	supportedModels map[string]bool
	commandRunner   CommandRunner
	onUpReceived    func()
}

// NewServer creates and initializes a new gRPC server implementation.
func NewServer(cache *Cache, gitClient GitClient) *Server {
	if gitClient == nil {
		log.Printf("WARN: NewServer: gitClient is nil - branch auto-creation in Up will be skipped")
	} else {
		log.Printf("NewServer: gitClient is initialized")
	}
	return &Server{
		cache:           cache,
		gitClient:       gitClient,
		supportedModels: make(map[string]bool),
		commandRunner:   defaultCommandRunner,
	}
}

// SetOnUpReceived sets a callback function to be executed whenever an Up request is successfully processed.
func (s *Server) SetOnUpReceived(fn func()) {
	s.onUpReceived = fn
}

// SetCommandRunner overrides the command runner for testing purposes.
func (s *Server) SetCommandRunner(runner CommandRunner) {
	s.commandRunner = runner
}

// SetSupportedModels manually sets the cached supported models.
func (s *Server) SetSupportedModels(models []string) {
	s.modelsMu.Lock()
	defer s.modelsMu.Unlock()
	s.supportedModels = make(map[string]bool, len(models))
	for _, m := range models {
		s.supportedModels[m] = true
	}
}

// GetSupportedModels returns the list of currently cached supported models.
func (s *Server) GetSupportedModels() []string {
	s.modelsMu.RLock()
	defer s.modelsMu.RUnlock()
	models := make([]string, 0, len(s.supportedModels))
	for m := range s.supportedModels {
		models = append(models, m)
	}
	return models
}

// FetchSupportedModels queries agy models and parses the output.
func (s *Server) FetchSupportedModels() ([]string, error) {
	out, err := s.commandRunner("agy", "models")
	if err != nil {
		return nil, fmt.Errorf("failed to query agy models: %w", err)
	}
	var models []string
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// agy models lines look like "gemini-3.6-flash-low     Gemini 3.6 Flash (Low)"
		fields := strings.Fields(line)
		if len(fields) > 0 {
			modelID := fields[0]
			// Filter out loading indicator text if present
			if !strings.HasPrefix(modelID, "Fetching") && !strings.HasPrefix(modelID, "⠋") && !strings.HasPrefix(modelID, "⠙") && !strings.HasPrefix(modelID, "⠹") && !strings.HasPrefix(modelID, "⠸") && !strings.HasPrefix(modelID, "⠼") && !strings.HasPrefix(modelID, "⠴") && !strings.HasPrefix(modelID, "⠦") && !strings.HasPrefix(modelID, "⠧") && !strings.HasPrefix(modelID, "⠇") && !strings.HasPrefix(modelID, "⠏") {
				models = append(models, modelID)
			}
		}
	}
	return models, nil
}

// RefreshSupportedModels updates the cached supported models by querying agy.
func (s *Server) RefreshSupportedModels() error {
	models, err := s.FetchSupportedModels()
	if err != nil {
		return err
	}
	s.SetSupportedModels(models)
	return nil
}

// StartModelValidationSync periodically queries agy models to update supported models cache.
func (s *Server) StartModelValidationSync(ctx context.Context, interval time.Duration) {
	if err := s.RefreshSupportedModels(); err != nil {
		log.Printf("Error initial refreshing supported models: %v", err)
	}
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := s.RefreshSupportedModels(); err != nil {
					log.Printf("Error refreshing supported models: %v", err)
				}
			}
		}
	}()
}

// IsModelSupported checks whether a given model string is in the cached supported models set.
func (s *Server) IsModelSupported(model string) bool {
	s.modelsMu.RLock()
	defer s.modelsMu.RUnlock()
	// If no models are cached yet (e.g. startup before first fetch), allow or strict?
	// If cache is populated, enforce validation.
	if len(s.supportedModels) == 0 {
		return true
	}
	return s.supportedModels[model]
}

var cleanStringRegex = regexp.MustCompile(`[^a-z0-9-]+`)

func cleanString(s string) string {
	return strings.Trim(cleanStringRegex.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

func getCleanID(repoURL, branchName string, issueNum int32) string {
	repoName := "container"

	u, err := url.Parse(repoURL)
	if err == nil && u.Path != "" && (strings.HasPrefix(repoURL, "http://") || strings.HasPrefix(repoURL, "https://")) {
		path := strings.TrimSuffix(u.Path, ".git")
		if idx := strings.Index(path, "/issues/"); idx != -1 {
			path = path[:idx]
		}

		parts := strings.Split(path, "/")
		for i := len(parts) - 1; i >= 0; i-- {
			part := parts[i]
			if part != "" {
				repoName = part
				break
			}
		}
	} else if strings.HasPrefix(repoURL, "git@") {
		if idx := strings.Index(repoURL, ":"); idx != -1 {
			path := strings.TrimSuffix(repoURL[idx+1:], ".git")
			if lastSlash := strings.LastIndex(path, "/"); lastSlash != -1 {
				repoName = path[lastSlash+1:]
			} else {
				repoName = path
			}
		}
	} else {
		parts := strings.Split(repoURL, "/")
		if len(parts) > 0 {
			repoName = parts[len(parts)-1]
		}
	}

	cleanRepo := cleanString(repoName)
	cleanBranch := cleanString(branchName)

	var id string
	if issueNum > 0 {
		id = fmt.Sprintf("%s-%d", cleanRepo, issueNum)
	} else {
		id = fmt.Sprintf("%s-%s", cleanRepo, cleanBranch)
	}

	if len(id) > 63 {
		id = id[:63]
	}
	return strings.TrimSuffix(id, "-")
}

// Up handles creating/starting a devcontainer workspace request with model validation and branch auto-creation.
func (s *Server) Up(ctx context.Context, req *proto.UpRequest) (*proto.UpResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.GetModel() != "" && !s.IsModelSupported(req.GetModel()) {
		return nil, status.Errorf(codes.InvalidArgument, "unsupported model: %s", req.GetModel())
	}

	if s.gitClient != nil && req.GetBranch() != "" {
		log.Printf("Up: checking if branch %q exists in repo %q", req.GetBranch(), req.GetRepo())
		exists, err := s.gitClient.BranchExists(ctx, req.GetRepo(), req.GetBranch())
		if err != nil {
			return nil, err
		}

		if !exists {
			log.Printf("Up: branch %q does not exist, attempting to auto-create from default branch", req.GetBranch())
			defaultBranch, err := s.gitClient.GetDefaultBranch(ctx, req.GetRepo())
			if err != nil || defaultBranch == "" {
				defaultBranch = "main"
			}

			if err := s.gitClient.CreateBranch(ctx, req.GetRepo(), req.GetBranch(), defaultBranch); err != nil {
				return nil, err
			}
		}
	} else {
		log.Printf("Up: skipping branch auto-creation (gitClient is nil: %v, branch is empty: %v)", s.gitClient == nil, req.GetBranch() == "")
	}

	var issueNum int32
	if req.GetIdentifier() != nil {
		issueNum = req.GetIdentifier().GetIssueNumber()
	}

	config := &proto.DevcontainerConfig{
		Id:      getCleanID(req.GetRepo(), req.GetBranch(), issueNum),
		Request: req,
		State:   proto.State_DCM_RECEIVED,
	}
	s.cache.Update(config.Id, config)
	s.cache.SetManual(config.Id, true)
	if s.onUpReceived != nil {
		s.onUpReceived()
	}
	return &proto.UpResponse{
		Config: config,
	}, nil
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

var devpodExe string

func init() {
	if path := os.Getenv("DEVPOD_CLI_PATH"); path != "" {
		devpodExe = path
	} else if _, err := exec.LookPath("devpod-cli"); err == nil {
		devpodExe = "devpod-cli"
	} else {
		devpodExe = "devpod"
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// PushPrompt dispatches prompt payload to target container if it exists and is in DCM_READY state.
func (s *Server) PushPrompt(ctx context.Context, req *proto.PushPromptRequest) (*proto.PushPromptResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	container, ok := s.cache.Get(req.GetId())
	if !ok {
		return nil, status.Errorf(codes.NotFound, "container %s not found", req.GetId())
	}

	if container.GetState() != proto.State_DCM_READY {
		return nil, status.Errorf(codes.FailedPrecondition, "container %s is not in DCM_READY state (current state: %s)", req.GetId(), container.GetState())
	}

	id := req.GetId()
	sessionName := id
	_, err := s.commandRunner(devpodExe, "ssh", id, "--command", fmt.Sprintf("tmux has-session -t %s", shellQuote(id)))
	if err != nil {
		// Fallback to base name if it is an issue container
		lastIdx := strings.LastIndex(id, "-")
		if lastIdx != -1 {
			if _, errNum := strconv.Atoi(id[lastIdx+1:]); errNum == nil {
				baseID := id[:lastIdx]
				_, fallbackErr := s.commandRunner(devpodExe, "ssh", id, "--command", fmt.Sprintf("tmux has-session -t %s", shellQuote(baseID)))
				if fallbackErr == nil {
					err = nil
					sessionName = baseID
				}
			}
		}
	}

	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "tmux session not active for container %s: %v", id, err)
	}

	cmd := fmt.Sprintf("tmux send-keys -t %s %s C-m", shellQuote(sessionName), shellQuote(req.GetPrompt()))
	_, err = s.commandRunner(devpodExe, "ssh", id, "--command", cmd)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to send keys to tmux session: %v", err)
	}

	return &proto.PushPromptResponse{}, nil
}

// Down removes container configuration from the cache.
func (s *Server) Down(ctx context.Context, req *proto.DownRequest) (*proto.DownResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	_, ok := s.cache.Get(req.GetId())
	if !ok {
		return nil, status.Errorf(codes.NotFound, "container %s not found", req.GetId())
	}

	s.cache.Delete(req.GetId())
	return &proto.DownResponse{}, nil
}


