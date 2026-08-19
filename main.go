package main

// Trigger PR review for assign reviewer
// Trigger PR review for issue closer

import (
	"context"
	"errors"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/brotherlogic/devcontainer-manager/proto"
	"github.com/brotherlogic/devcontainer-manager/server"
	"github.com/google/go-github/v50/github"
	"golang.org/x/oauth2"
	"google.golang.org/grpc"
)

func getGHClient() (*github.Client, error) {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		// Try to get token from gh cli
		cmd := exec.Command("gh", "auth", "token")
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("failed to get token from gh cli: %w", err)
		}
		token = strings.TrimSpace(string(out))
	}

	if token == "" {
		return nil, fmt.Errorf("GITHUB_TOKEN is not set")
	}

	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: token},
	)
	tc := oauth2.NewClient(context.Background(), ts)

	return github.NewClient(tc), nil
}

var devpodExe = "devpod"

// devpodMutex serializes all DevPod CLI operations across goroutines to prevent read/write race conditions on ~/.devpod/config.yaml.
var devpodMutex sync.Mutex

func isDevpodCommand(name string) bool {
	return name == devpodExe || name == "devpod" || name == "devpod-cli"
}

// runDevpodCommand executes commandRunner with mutex locking when calling devpod CLI commands.
func runDevpodCommand(name string, args ...string) ([]byte, error) {
	if isDevpodCommand(name) {
		devpodMutex.Lock()
		defer devpodMutex.Unlock()
	}
	return commandRunner(name, args...)
}

func init() {
	if _, err := exec.LookPath("devpod-cli"); err == nil {
		devpodExe = "devpod-cli"
	}
}

var commandRunner = func(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	return cmd.CombinedOutput()
}

type DevpodWorkspace struct {
	ID     string `json:"id"`
	UID    string `json:"uid"`
	Source struct {
		GitRepository string `json:"gitRepository"`
	} `json:"source"`
}

func listDevpodWorkspaces() ([]DevpodWorkspace, error) {
	out, err := runDevpodCommand(devpodExe, "list", "--output", "json")
	if err != nil {
		return nil, err
	}

	outStr := string(out)
	// Strip ANSI escape codes
	re := regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)
	outStr = re.ReplaceAllString(outStr, "")

	var workspaces []DevpodWorkspace
	if err := json.Unmarshal([]byte(outStr), &workspaces); err != nil {
		lines := strings.Split(outStr, "\n")
		var found bool
		for i, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), "[") {
				if err2 := json.Unmarshal([]byte(strings.Join(lines[i:], "\n")), &workspaces); err2 == nil {
					found = true
					break
				}
			}
		}
		if !found {
			start := strings.Index(outStr, "[")
			end := strings.LastIndex(outStr, "]")
			if start != -1 && end != -1 && end >= start {
				if err3 := json.Unmarshal([]byte(outStr[start:end+1]), &workspaces); err3 == nil {
					found = true
				}
			}
		}
		if !found {
			return nil, fmt.Errorf("failed to parse devpod list json: %w", err)
		}
	}
	return workspaces, nil
}

var gitHubClientProvider = getGHClient

var listOpenIssuesProvider = func(ctx context.Context, client *github.Client, owner, repoName string) ([]*github.Issue, error) {
	repoPath := fmt.Sprintf("%s/%s", owner, repoName)
	out, err := commandRunner("gh", "issue", "list", "-R", repoPath, "--state", "open", "--json", "number,title,labels,assignees,body")
	if err == nil {
		var rawIssues []struct {
			Number int    `json:"number"`
			Title  string `json:"title"`
			Body   string `json:"body"`
			Labels []struct {
				Name string `json:"name"`
			} `json:"labels"`
			Assignees []struct {
				Login string `json:"login"`
			} `json:"assignees"`
		}
		if errUnmarshal := json.Unmarshal(out, &rawIssues); errUnmarshal == nil {
			var issues []*github.Issue
			for _, raw := range rawIssues {
				num := raw.Number
				title := raw.Title
				body := raw.Body
				var labels []*github.Label
				for _, lbl := range raw.Labels {
					name := lbl.Name
					labels = append(labels, &github.Label{
						Name: &name,
					})
				}
				var assignees []*github.User
				for _, a := range raw.Assignees {
					login := a.Login
					assignees = append(assignees, &github.User{
						Login: &login,
					})
				}
				var assignee *github.User
				if len(assignees) > 0 {
					assignee = assignees[0]
				}
				issues = append(issues, &github.Issue{
					Number:    &num,
					Title:     &title,
					Body:      &body,
					Labels:    labels,
					Assignee:  assignee,
					Assignees: assignees,
				})
			}
			return issues, nil
		} else {
			log.Printf("Warning: failed to parse JSON from gh issue list for %s: %v. Raw output: %s", repoPath, errUnmarshal, string(out))
		}
	} else {
		log.Printf("Warning: gh issue list command failed for %s: %v. Output: %s", repoPath, err, string(out))
	}

	if client != nil {
		log.Printf("Falling back to GitHub HTTP API to list open issues for %s", repoPath)
		opts := &github.IssueListByRepoOptions{State: "open"}
		issues, _, errAPI := client.Issues.ListByRepo(ctx, owner, repoName, opts)
		return issues, errAPI
	}
	return nil, fmt.Errorf("gh command failed (%w) and github client is nil", err)
}

var (
	pollingInterval = 5 * time.Second
	pollingTimeout  = 5 * time.Minute
	shaMutex        sync.RWMutex
)

// We live dangerously
const (
	agyInteractivePrefix       = "agy --dangerously-skip-permissions --prompt-interactive"
	defaultIssueStartupCommand = `agy --dangerously-skip-permissions --prompt-interactive "Take a look at the status of this issue - if the label matches any of the workflows in the brotherlogic/seraphine project's .agent/workflows list then you should follow that workflow. Otherwise just suggest a path forward for the issue - do not undertake any implementation work"`
	defaultBranchRef           = ""
	DevpodLabelPrefix          = "sh.loft.devpod.workspace.id="
	VscLabelPrefix             = "dev.containers.id="
)

type config struct {
	once               bool
	containerList      string
	maxIssueContainers int
	startupCommand     string
	maxConcurrency     int
	port               int
}

// globalCache is a thread-safe in-memory cache protected by sync.RWMutex inside package server.
var globalCache = server.NewCache()

func initCache() *server.Cache {
	return globalCache
}

// parseOwnerRepo extracts the repository owner and name from various Git URL formats
// (e.g. SSH URLs, HTTP URLs, owner/repo strings, or branch-suffixed URLs).
func parseOwnerRepo(repoStr string) (string, string, error) {
	s := repoStr
	s = strings.TrimPrefix(s, "git@")
	s = strings.TrimSuffix(s, ".git")
	if idx := strings.Index(s, "@"); idx != -1 {
		s = s[:idx]
	}
	if idx := strings.Index(s, ":"); idx != -1 {
		s = s[idx+1:]
	}
	if u, err := url.Parse(s); err == nil && u.Path != "" {
		s = u.Path
	}
	s = strings.TrimPrefix(s, "/")
	// Strip subpaths like /issues/, /pull/, /discussions/ if present
	for _, subpath := range []string{"/issues/", "/pull/", "/discussions/"} {
		if idx := strings.Index(s, subpath); idx != -1 {
			s = s[:idx]
		}
	}
	parts := strings.Split(s, "/")
	if len(parts) >= 2 {
		return parts[len(parts)-2], parts[len(parts)-1], nil
	}
	return "", "", fmt.Errorf("invalid repo string: %s", repoStr)
}

type ghGitClient struct{}

func (g *ghGitClient) BranchExists(ctx context.Context, repo, branch string) (bool, error) {
	owner, repoName, err := parseOwnerRepo(repo)
	if err != nil {
		return false, err
	}
	client, err := gitHubClientProvider()
	if err != nil {
		return false, err
	}
	_, _, err = client.Repositories.GetBranch(ctx, owner, repoName, branch, false)
	if err == nil {
		return true, nil
	}
	return false, nil
}

func (g *ghGitClient) CreateBranch(ctx context.Context, repo, newBranch, baseBranch string) error {
	owner, repoName, err := parseOwnerRepo(repo)
	if err != nil {
		return err
	}
	client, err := gitHubClientProvider()
	if err != nil {
		return err
	}
	return ensureIssueBranchExists(ctx, client, owner, repoName, newBranch)
}

func (g *ghGitClient) GetDefaultBranch(ctx context.Context, repo string) (string, error) {
	owner, repoName, err := parseOwnerRepo(repo)
	if err != nil {
		return "main", err
	}
	client, err := gitHubClientProvider()
	if err != nil {
		return "main", err
	}
	r, _, err := client.Repositories.Get(ctx, owner, repoName)
	if err != nil {
		return "main", err
	}
	return r.GetDefaultBranch(), nil
}

var triggerRunChan = make(chan struct{}, 1)

func triggerRunLoop() {
	select {
	case triggerRunChan <- struct{}{}:
	default:
	}
}

func startGRPCServer(port int, cache *server.Cache) (*grpc.Server, error) {
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return nil, fmt.Errorf("failed to listen: %w", err)
	}
	s := grpc.NewServer()
	srv := server.NewServer(cache, &ghGitClient{})
	srv.SetOnUpReceived(triggerRunLoop)
	proto.RegisterManagerServiceServer(s, srv)
	go func() {
		if err := s.Serve(lis); err != nil {
			log.Printf("gRPC server stopped: %v", err)
		}
	}()
	return s, nil
}

func syncCacheWithRunning(running map[string]bool) {
	// 1. For each running container, make sure it is in the cache as RUNNING (if not already STARTING/RUNNING)
	for cid := range running {
		existing := globalCache.List()
		var found *proto.DevcontainerConfig
		for _, c := range existing {
			if c.Id == cid {
				found = c
				break
			}
		}

		if found != nil {
			if found.State != proto.State_DCM_READY && found.State != proto.State_DCM_CREATING {
				found.State = proto.State_DCM_READY
				globalCache.Update(cid, found)
			}
		} else {
			// Not in cache, let's try to infer repo url and branch
			repoURL := ""
			branchOrIssue := ""
			var issueNum int32
			if idx := strings.LastIndex(cid, "-"); idx != -1 {
				if val, err := strconv.ParseInt(cid[idx+1:], 10, 32); err == nil {
					branchOrIssue = cid[idx+1:]
					issueNum = int32(val)
				}
			}
			var identifier *proto.Identifier
			if issueNum > 0 {
				identifier = &proto.Identifier{
					Id: &proto.Identifier_IssueNumber{IssueNumber: issueNum},
				}
			}
			globalCache.Update(cid, &proto.DevcontainerConfig{
				Id:      cid,
				Request: &proto.UpRequest{Repo: repoURL, Branch: branchOrIssue, Identifier: identifier},
				State:   proto.State_DCM_READY,
			})
		}
	}

	// 2. Remove containers from cache that are NOT in devpod list and not in STARTING/RECEIVED status
	for _, c := range globalCache.List() {
		if !running[c.Id] && c.State != proto.State_DCM_CREATING && c.State != proto.State_DCM_RECEIVED {
			globalCache.Delete(c.Id)
		}
	}
}

func parseFlags(args []string) (*config, error) {
	fs := flag.NewFlagSet("devcontainer-manager", flag.ContinueOnError)
	once := fs.Bool("once", false, "Run once and exit")
	containerList := fs.String("container_list", "container.list.template", "The list of containers to run")
	maxIssueContainers := fs.Int("max_issue_containers", 5, "Maximum number of concurrent running issue containers")
	startupCommand := fs.String("startup_command", "", "Command to inject into the container's tmux session on startup")
	maxConcurrency := fs.Int("max-concurrency", 10, "Maximum concurrency limit")
	port := fs.Int("port", 50051, "The port to run the gRPC server on")

	err := fs.Parse(args)
	if err != nil {
		return nil, err
	}

	var startupCmdVal string
	if startupCommand != nil {
		startupCmdVal = *startupCommand
	}

	return &config{
		once:               *once,
		containerList:      *containerList,
		maxIssueContainers: *maxIssueContainers,
		startupCommand:     startupCmdVal,
		maxConcurrency:     *maxConcurrency,
		port:               *port,
	}, nil
}

func main() {
	cfg, err := parseFlags(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing flags: %v\n", err)
		os.Exit(1)
	}

	port := cfg.port
	if port <= 0 {
		port = 50051
	}
	srv, err := startGRPCServer(port, globalCache)
	if err != nil {
		log.Fatalf("Failed to start gRPC server: %v", err)
	}
	defer srv.GracefulStop()

	go func() {
		for {
			log.Printf("Running periodic docker system prune...")
			cmd := exec.Command("docker", "system", "prune", "-af", "--volumes")
			output, err := cmd.CombinedOutput()
			if err != nil {
				log.Printf("Docker prune failed: %v\nOutput: %s", err, string(output))
			} else {
				log.Printf("Docker prune succeeded")
			}
			time.Sleep(24 * time.Hour)
		}
	}()

	for {
		err := run(context.Background(), cfg)
		if err != nil {
			log.Printf("Error: %v", err)
		}

		if cfg.once {
			break
		}

		select {
		case <-triggerRunChan:
		case <-time.After(time.Minute * 5):
		}
	}
}

func run(ctx context.Context, cfg *config) error {
	// wg is used to coordinate and wait for all concurrent background goroutines
	// that handle the readiness-polling and startup-command injection for newly
	// started or recreated devcontainers, preventing goroutine leaks before run() exits.
	var wg sync.WaitGroup
	data, err := os.ReadFile(cfg.containerList)
	if err != nil {
		return fmt.Errorf("failed to read container list: %w", err)
	}

	lines := strings.Split(string(data), "\n")
	var repos []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			repos = append(repos, line)
		}
	}

	sortReposByLastUpdated(repos)

	projectRepoMap := make(map[string]string)
	for _, repo := range repos {
		parts := strings.Split(repo, "/")
		pID := parts[len(parts)-1]
		projectRepoMap[pID] = repo
	}

	// Get running devcontainers
	workspaces, err := listDevpodWorkspaces()
	if err != nil {
		return fmt.Errorf("failed to list devcontainers: %w", err)
	}

	running := make(map[string]bool)
	for _, w := range workspaces {
		running[w.ID] = true
	}
	syncCacheWithRunning(running)

	// Process manual Up requests that are in DCM_RECEIVED state
	maxConcurrencyLimit := cfg.maxConcurrency
	if maxConcurrencyLimit <= 0 {
		maxConcurrencyLimit = 10
	}
	manualSem := make(chan struct{}, maxConcurrencyLimit)

	for _, c := range globalCache.List() {
		if c.State == proto.State_DCM_RECEIVED {
			globalCache.SetManual(c.Id, true)
			// Update state to DCM_CREATING to lock it
			c.State = proto.State_DCM_CREATING
			globalCache.Update(c.Id, c)

			manualSem <- struct{}{}
			wg.Add(1)
			go processManualUpRequest(ctx, c, &wg, cfg, manualSem)
		}
	}

	trackedSHAs := loadTrackedSHAs()
	trackedSHAsChanged := false
	client, clientErr := gitHubClientProvider()
	if clientErr != nil {
		log.Printf("Warning: failed to get GitHub client: %v. Change detection will be bypassed.", clientErr)
	}

	maxConcurrency := cfg.maxConcurrency
	if maxConcurrency <= 0 {
		maxConcurrency = 10
	}
	sem := make(chan struct{}, maxConcurrency)

	validIssueContainers := make(map[string]bool)
	var stateMu sync.Mutex
	var loopWg sync.WaitGroup

	for _, repo := range repos {
		sem <- struct{}{}
		loopWg.Add(1)
		go func(repo string) {
			defer func() { <-sem }()
			defer loopWg.Done()
			defer func() {
				if r := recover(); r != nil {
					logWithPrefix(repo, "Recovered from panic: %v", r)
				}
			}()

			parts := strings.Split(repo, "/")
			id := parts[len(parts)-1]
			log.Printf("DEBUG: repo is %s, client is nil? %v", repo, client == nil)

			stateMu.Lock()
			isAlreadyRunning := running[id]
			stateMu.Unlock()

			if !isAlreadyRunning {
				logWithPrefix(repo, "Starting devcontainer")
				container := &proto.DevcontainerConfig{
					Id:      id,
					Request: &proto.UpRequest{Repo: fmt.Sprintf("git@github.com:%s", repo), Branch: defaultBranchRef},
					State:   proto.State_DCM_CREATING,
				}
				globalCache.Update(id, container)

				out, err := runCommandWithLog(repo, devpodExe, "up", fmt.Sprintf("git@github.com:%s", repo), "--id", id, "--ide", "none")
				if err != nil {
					logWithPrefix(repo, "Failed to start devcontainer: %v (output: %s)", err, string(out))
					container.State = proto.State_DCM_FAILED
					container.ErrorMessage = err.Error()
					globalCache.Update(id, container)
					// Delete container so it can be re-provisioned from head next time
					if delErr := deleteContainer(repo, id); delErr != nil {
						logWithPrefix(repo, "Warning: failed to delete failed devcontainer %s: %v", id, delErr)
					}
					// Ensure the failed status remains in the cache for dashboard visibility despite the container deletion
					globalCache.Update(id, container)
				} else {
					container.State = proto.State_DCM_READY
					globalCache.Update(id, container)
					if cfg.startupCommand != "" {
						wg.Add(1)
						go func(cid string) {
							defer wg.Done()
							if err := injectStartupCommand(ctx, repo, cid, cfg.startupCommand, proto.Harness_HARNESS_ANTIGRAVITY); err != nil {
								logWithPrefix(repo, "ERROR: Failed to inject startup command for container %s: %v", cid, err)
							}
						}(id)
					}
					if client != nil {
						compositeSHA, found, err := getRepoCompositeSHA(ctx, client, repo, defaultBranchRef)
						if err == nil && found {
							updateAndSaveRepoSHA(repo, compositeSHA, trackedSHAs)
						} else if err != nil {
							logWithPrefix(repo, "Warning: failed to get composite SHA: %v", err)
						}
					}
					renameDockerContainer(id)
				}
			} else {
				if client != nil {
					compositeSHA, found, err := getRepoCompositeSHA(ctx, client, repo, defaultBranchRef)
					if err != nil {
						logWithPrefix(repo, "Warning: failed to get composite SHA: %v", err)
					} else if found {
						lastSeen, exists := getTrackedSHA(repo, trackedSHAs)
						if !exists {
							logWithPrefix(repo, "Initial tracking established at %s", compositeSHA)
							updateAndSaveRepoSHA(repo, compositeSHA, trackedSHAs)
						} else if lastSeen != compositeSHA {
							logWithPrefix(repo, "Detected devcontainer configuration/script change! Recreating container...")
							logWithPrefix(repo, "Old tracking: %s", lastSeen)
							logWithPrefix(repo, "New tracking: %s", compositeSHA)

							err := recreateContainer(ctx, repo, id, cfg.startupCommand, &wg)
							if err != nil {
								logWithPrefix(repo, "Failed to recreate devcontainer: %v", err)
							} else {
								updateAndSaveRepoSHA(repo, compositeSHA, trackedSHAs)
							}
						}
					}
				}
			}

			// Issue-scanning and launching loop for per-issue devcontainers
			if client != nil {
				parts := strings.Split(repo, "/")
				if len(parts) == 2 {
					owner, repoName := parts[0], parts[1]
					issues, err := listOpenIssuesProvider(ctx, client, owner, repoName)
					if err != nil {
						log.Printf("Warning: failed to list issues for %s: %v", repo, err)
					} else {
						log.Printf("DEBUG: Successfully fetched %d issues for %s", len(issues), repo)
						for _, issue := range issues {
							hasSeraphineLabel := false
							for _, label := range issue.Labels {
								if strings.HasPrefix(label.GetName(), "seraphine") {
									hasSeraphineLabel = true
									break
								}
							}
							if !hasSeraphineLabel {
								continue
							}

							if len(issue.Assignees) == 0 && issue.GetAssignee() == nil {
								continue
							}

							issueNumber := issue.GetNumber()
							containerID := fmt.Sprintf("%s-%d", id, issueNumber)

							stateMu.Lock()
							validIssueContainers[containerID] = true
							isIssueRunning := running[containerID]
							stateMu.Unlock()

							if !isIssueRunning {
								log.Printf("Discovered new issue #%d labeled 'seraphine' in %s. Provisioning container...", issueNumber, repo)
								adjustIssueLabels(ctx, client, owner, repoName, issueNumber, "container-creating", []string{"container-ready", "container-failed", "container-asleep"}, "provisioning container for issue")

								slug, err := deriveFeatureSlug(issue.GetTitle())
								if err != nil {
									log.Printf("Failed to derive branch slug for issue %d: %v", issueNumber, err)
									adjustIssueLabels(ctx, client, owner, repoName, issueNumber, "container-failed", []string{"container-creating", "container-ready", "container-asleep"}, fmt.Sprintf("failed to derive branch slug: %v", err))
									go reportStartupFailure(ctx, client, owner, repoName, "", issueNumber, err, "")
									continue
								}

								branchName := fmt.Sprintf("feature/%s_%d", slug, issueNumber)
								err = ensureIssueBranchExists(ctx, client, owner, repoName, branchName)
								if err != nil {
									log.Printf("Failed to ensure issue branch %s exists: %v", branchName, err)
									adjustIssueLabels(ctx, client, owner, repoName, issueNumber, "container-failed", []string{"container-creating", "container-ready", "container-asleep"}, fmt.Sprintf("failed to ensure issue branch %s exists: %v", branchName, err))
									go reportStartupFailure(ctx, client, owner, repoName, branchName, issueNumber, err, "")
									continue
								}

								repoURL := fmt.Sprintf("git@github.com:%s/%s@%s", owner, repoName, branchName)
								logWithPrefix(repo, "Launching issue container %s on branch %s", containerID, branchName)
								container := &proto.DevcontainerConfig{
									Id: containerID,
									Request: &proto.UpRequest{
										Repo:   repoURL,
										Branch: branchName,
										Identifier: &proto.Identifier{
											Id: &proto.Identifier_IssueNumber{IssueNumber: int32(issueNumber)},
										},
									},
									State: proto.State_DCM_CREATING,
								}
								globalCache.Update(containerID, container)

								// Use --recreate to ensure we pull the freshest version of the container from head, avoiding stale caches.
								out, err := runCommandWithLog(repo, devpodExe, "up", repoURL, "--id", containerID, "--ide", "none", "--recreate")
								if err != nil {
									logWithPrefix(repo, "Failed to launch devcontainer for issue %d: %v (output: %s)", issueNumber, err, string(out))
									container.State = proto.State_DCM_FAILED
									container.ErrorMessage = err.Error()
									globalCache.Update(containerID, container)
									// Delete container so it can be re-provisioned from head next time
									if delErr := deleteContainer(repo, containerID); delErr != nil {
										logWithPrefix(repo, "Warning: failed to delete failed devcontainer %s: %v", containerID, delErr)
									}
									// Ensure the failed status remains in the cache for dashboard visibility despite the container deletion
									globalCache.Update(containerID, container)
									adjustIssueLabels(ctx, client, owner, repoName, issueNumber, "container-failed", []string{"container-creating", "container-ready", "container-asleep"}, fmt.Sprintf("devpod up failed: %v", err))
									go reportStartupFailure(ctx, client, owner, repoName, branchName, issueNumber, err, string(out))
								} else {
									container.State = proto.State_DCM_READY
									globalCache.Update(containerID, container)
									stateMu.Lock()
									running[containerID] = true
									stateMu.Unlock()

									adjustIssueLabels(ctx, client, owner, repoName, issueNumber, "container-ready", []string{"container-creating", "container-failed", "container-asleep"}, "devpod up succeeded")

									if issue.CreatedAt != nil {
										wg.Add(1)
										go func(iNum int, t time.Time) {
											defer wg.Done()
											if err := postLatencyComment(ctx, client, owner, repoName, iNum, t); err != nil {
												logWithPrefix(repo, "Warning: failed to post latency comment for issue %d: %v", iNum, err)
											}
										}(issueNumber, issue.CreatedAt.Time)
									}
									renameDockerContainer(containerID)
									cmdToInject := cfg.startupCommand
									if cmdToInject == "" {
										cmdToInject = fmt.Sprintf(`agy --dangerously-skip-permissions --prompt-interactive "Take a look at the status of issue #%d - if the label matches any of the workflows in the brotherlogic/seraphine project's .agent/workflows list then you should follow that workflow. Otherwise just suggest a path forward for the issue - do not undertake any implementation work"`, issueNumber)
									}
									wg.Add(1)
									go func(cid string, iNum int, bName string) {
										defer wg.Done()
										if err := injectStartupCommand(ctx, repo, cid, cmdToInject, proto.Harness_HARNESS_ANTIGRAVITY); err != nil {
											logWithPrefix(repo, "ERROR: Failed to inject startup command for container %s: %v", cid, err)
											container.State = proto.State_DCM_FAILED
											container.ErrorMessage = err.Error()
											globalCache.Update(cid, container)

											if client != nil && owner != "" && repoName != "" && iNum > 0 {
												adjustIssueLabels(ctx, client, owner, repoName, iNum, "container-failed", []string{"container-creating", "container-ready", "container-asleep"}, fmt.Sprintf("startup command injection failed: %v", err))
											}
											if delErr := deleteContainer(repo, cid); delErr != nil {
												logWithPrefix(repo, "Warning: failed to delete failed devcontainer %s: %v", cid, delErr)
											}
											globalCache.Update(cid, container)
											if client != nil && owner != "" && repoName != "" {
												go reportStartupFailure(ctx, client, owner, repoName, bName, iNum, err, "")
											}
										}
									}(containerID, issueNumber, branchName)

									compositeSHA, found, err := getRepoCompositeSHA(ctx, client, repo, branchName)
									if err == nil && found {
										updateAndSaveRepoSHA(containerID, compositeSHA, trackedSHAs)
									} else if err != nil {
										logWithPrefix(repo, "Warning: failed to get composite SHA for issue container %s on branch %s: %v", containerID, branchName, err)
									}
								}
							} else {
								slug, err := deriveFeatureSlug(issue.GetTitle())
								if err != nil {
									log.Printf("Failed to derive branch slug for issue %d: %v", issueNumber, err)
									continue
								}

								branchName := fmt.Sprintf("feature/%s_%d", slug, issueNumber)
								compositeSHA, found, err := getRepoCompositeSHA(ctx, client, repo, branchName)
								if err != nil {
									log.Printf("Warning: failed to get composite SHA for issue container %s: %v", containerID, err)
								} else if found {
									lastSeen, exists := getTrackedSHA(containerID, trackedSHAs)
									if !exists {
										log.Printf("Initial tracking established for issue container %s at %s", containerID, compositeSHA)
										updateAndSaveRepoSHA(containerID, compositeSHA, trackedSHAs)
									} else if lastSeen != compositeSHA {
										log.Printf("Detected devcontainer configuration/script change in issue container %s! Recreating container...", containerID)
										log.Printf("Old tracking: %s", lastSeen)
										log.Printf("New tracking: %s", compositeSHA)

										err := recreateIssueContainer(ctx, owner, repoName, branchName, containerID, cfg.startupCommand, &wg)
										if err != nil {
											log.Printf("Failed to recreate devcontainer for issue %d: %v", issueNumber, err)
											go reportStartupFailure(ctx, client, owner, repoName, branchName, issueNumber, err, "")
										} else {
											updateAndSaveRepoSHA(containerID, compositeSHA, trackedSHAs)
										}
									}
								}
							}
						}
					}
				}
			}
		}(repo)
	}
	loopWg.Wait()

	// Resource Management & Cleanup Logic for issue containers
	if client != nil {
		projectRepoMap := make(map[string]string)
		for _, repo := range repos {
			parts := strings.Split(repo, "/")
			pID := parts[len(parts)-1]
			projectRepoMap[pID] = repo
		}

		workspaces, errList := listDevpodWorkspaces()
		if errList == nil {
			// 1. Hibernation Logic
			type issueContainer struct {
				id          string
				repo        string
				owner       string
				repoName    string
				issueNumber int
				updatedAt   time.Time
			}
			var runningIssues []issueContainer

			for _, w := range workspaces {
				id := w.ID
				lastIdx := strings.LastIndex(id, "-")
				if lastIdx == -1 {
					log.Printf("Debug: Skipping devpod %s: ID does not contain a hyphen for issue number separation", id)
					continue
				}
				projectID := id[:lastIdx]
				repo := projectRepoMap[projectID]
				if repo == "" {
					continue
				}
				issueNumber, errNum := strconv.Atoi(id[lastIdx+1:])
				if errNum != nil {
					log.Printf("Debug: Skipping devpod %s: Failed to parse issue number from ID '%s': %v", id, id[lastIdx+1:], errNum)
					continue
				}
				if repo != "" {
					partsRepo := strings.Split(repo, "/")
					if len(partsRepo) == 2 {
						owner, repoName := partsRepo[0], partsRepo[1]
						issue, _, errGet := client.Issues.Get(ctx, owner, repoName, issueNumber)
						if errGet == nil {
							runningIssues = append(runningIssues, issueContainer{
								id:          id,
								repo:        repo,
								owner:       owner,
								repoName:    repoName,
								issueNumber: issueNumber,
								updatedAt:   issue.GetUpdatedAt().Time,
							})
						} else {
							log.Printf("Debug: Skipping devpod %s: Failed to get issue %d from %s: %v", id, issueNumber, repo, errGet)
						}
					}
				}
			}

			if len(runningIssues) > cfg.maxIssueContainers {
				sort.Slice(runningIssues, func(i, j int) bool {
					return runningIssues[i].updatedAt.Before(runningIssues[j].updatedAt)
				})
				excess := len(runningIssues) - cfg.maxIssueContainers
				for i := 0; i < excess; i++ {
					cRepo := runningIssues[i].repo
					logWithPrefix(cRepo, "Hibernating oldest running issue container: %s", runningIssues[i].id)
					errStop := stopContainer(cRepo, runningIssues[i].id)
					if errStop != nil {
						logWithPrefix(cRepo, "Warning: failed to stop container %s during hibernation: %v", runningIssues[i].id, errStop)
					}
					if client != nil && runningIssues[i].owner != "" && runningIssues[i].repoName != "" && runningIssues[i].issueNumber > 0 {
						// Mark issue container as asleep when hibernating to satisfy maximum concurrent running container limit.
						adjustIssueLabels(ctx, client, runningIssues[i].owner, runningIssues[i].repoName, runningIssues[i].issueNumber, "container-asleep", []string{"container-creating", "container-ready", "container-failed"}, "hibernating issue container")
					}
				}
			}

			// 2. Cleanup Logic
			for _, w := range workspaces {
				id := w.ID
				lastIdx := strings.LastIndex(id, "-")
				if lastIdx != -1 {
					projectID := id[:lastIdx]
					issueNumber, errNum := strconv.Atoi(id[lastIdx+1:])
					repo := projectRepoMap[projectID]
					if errNum == nil && repo != "" {
						partsRepo := strings.Split(repo, "/")
						if len(partsRepo) == 2 {
							owner, repoName := partsRepo[0], partsRepo[1]
							issue, resp, errGet := client.Issues.Get(ctx, owner, repoName, issueNumber)

							shouldCleanup := false
							if errGet == nil {
								if issue.GetState() == "closed" {
									shouldCleanup = true
								} else if !globalCache.IsManual(id) {
									hasSeraphineLabel := false
									for _, label := range issue.Labels {
										if strings.HasPrefix(label.GetName(), "seraphine") {
											hasSeraphineLabel = true
											break
										}
									}
									if !hasSeraphineLabel {
										shouldCleanup = true
									}
								}
							} else if resp != nil && resp.StatusCode == http.StatusNotFound {
								shouldCleanup = true
							}

							if shouldCleanup {
								logWithPrefix(repo, "Cleaning up finished/unlabeled issue container: %s", id)
								errStop := stopContainer(repo, id)
								if errStop != nil {
									logWithPrefix(repo, "Warning: failed to stop container %s during cleanup: %v", id, errStop)
								}
								errDelete := deleteContainer(repo, id)
								if errDelete != nil {
									logWithPrefix(repo, "Failed to delete container %s during cleanup: %v", id, errDelete)
								} else {
									if _, exists := getTrackedSHA(id, trackedSHAs); exists {
										deleteTrackedSHA(id, trackedSHAs)
										trackedSHAsChanged = true
									}
								}
							}
						}
					}
				}
			}
		}
	}

	// 3. Extra Cleanup Logic for (a) not in template list (accounting for issues), and (b) use HTTP source
	workspaces, listErr := listDevpodWorkspaces()
	if listErr == nil {
		validProjectNames := make(map[string]bool)
		for _, r := range repos {
			rParts := strings.Split(r, "/")
			validProjectNames[rParts[len(rParts)-1]] = true
		}

		for _, w := range workspaces {
			cName := w.ID
			cSource := w.Source.GitRepository
			if cName != "" {
				// Check (b): Uses HTTP source
				u, errURL := url.Parse(cSource)
				isHTTPSource := errURL == nil && (u.Scheme == "http" || u.Scheme == "https")

				// Check (a): Not in the container list (accounting for issues)
				inList := validProjectNames[cName] || validIssueContainers[cName] || globalCache.IsManual(cName)

				if !inList || isHTTPSource {
					cRepo := getRepoForID(cName, projectRepoMap)
					logWithPrefix(cRepo, "Cleaning up container %s (inList: %v, isHTTPSource: %v)", cName, inList, isHTTPSource)
					errStop := stopContainer(cRepo, cName)
					if errStop != nil {
						logWithPrefix(cRepo, "Warning: failed to stop container %s during extra cleanup: %v", cName, errStop)
					}
					errDel := deleteContainer(cRepo, cName)
					if errDel != nil {
						logWithPrefix(cRepo, "Warning: failed to delete container %s during extra cleanup: %v", cName, errDel)
					} else {
						if _, exists := getTrackedSHA(cName, trackedSHAs); exists {
							deleteTrackedSHA(cName, trackedSHAs)
							trackedSHAsChanged = true
						}
					}
				}
			}
		}
	}

	if trackedSHAsChanged {
		if errSave := saveTrackedSHAs(trackedSHAs); errSave != nil {
			log.Printf("Warning: failed to save tracked SHAs: %v", errSave)
		}
	}

	wg.Wait()
	return nil
}

func processManualUpRequest(ctx context.Context, config *proto.DevcontainerConfig, wg *sync.WaitGroup, cfg *config, sem chan struct{}) {
	defer wg.Done()
	defer func() { <-sem }()

	req := config.Request
	repoURL := req.GetRepo()
	if idx := strings.Index(repoURL, "/issues/"); idx != -1 {
		repoURL = repoURL[:idx]
	}
	repoURL = strings.TrimPrefix(repoURL, "https://github.com/")
	repoURL = strings.TrimPrefix(repoURL, "http://github.com/")
	if idx := strings.Index(repoURL, "github.com:"); idx != -1 {
		repoURL = repoURL[idx+len("github.com:"):]
	}
	if !strings.HasPrefix(repoURL, "git@") {
		repoURL = fmt.Sprintf("git@github.com:%s", repoURL)
	}
	if req.GetBranch() != "" && !strings.HasSuffix(repoURL, "@"+req.GetBranch()) {
		repoURL = fmt.Sprintf("%s@%s", repoURL, req.GetBranch())
	}

	owner, repoName, _ := parseOwnerRepo(req.GetRepo())
	var issueNum int
	if req.GetIdentifier() != nil && req.GetIdentifier().GetIssueNumber() > 0 {
		issueNum = int(req.GetIdentifier().GetIssueNumber())
	}
	client, _ := gitHubClientProvider()

	if client != nil && owner != "" && repoName != "" && issueNum > 0 {
		adjustIssueLabels(ctx, client, owner, repoName, issueNum, "container-creating", []string{"container-ready", "container-failed", "container-asleep"}, "manual container launch initiated")
	}

	// Execute container provisioning logic
	logWithPrefix(config.Id, "Manually launching container %s on repo %s", config.Id, repoURL)
	out, err := runCommandWithLog(config.Id, devpodExe, "up", repoURL, "--id", config.Id, "--ide", "none")

	if client != nil && owner != "" && repoName != "" && issueNum > 0 {
		if err != nil {
			adjustIssueLabels(ctx, client, owner, repoName, issueNum, "container-failed", []string{"container-creating", "container-ready", "container-asleep"}, fmt.Sprintf("manual container devpod up failed: %v", err))
		} else {
			adjustIssueLabels(ctx, client, owner, repoName, issueNum, "container-ready", []string{"container-creating", "container-failed", "container-asleep"}, "manual container devpod up succeeded")
		}
	}

	if err != nil {
		logWithPrefix(config.Id, "Failed to manually launch devcontainer: %v (output: %s)", err, string(out))
		config.State = proto.State_DCM_FAILED
		config.ErrorMessage = err.Error()
		globalCache.Update(config.Id, config)

		// Delete container so it can be re-provisioned next time
		if delErr := deleteContainer(req.GetRepo(), config.Id); delErr != nil {
			logWithPrefix(config.Id, "Warning: failed to delete failed devcontainer %s: %v", config.Id, delErr)
		}

		if client != nil && owner != "" && repoName != "" {
			go reportStartupFailure(ctx, client, owner, repoName, req.GetBranch(), issueNum, err, string(out))
		}
	} else {
		config.State = proto.State_DCM_READY
		globalCache.Update(config.Id, config)

		renameDockerContainer(config.Id)

		cmdToInject := cfg.startupCommand
		if req.GetHarness() == proto.Harness_HARNESS_PI {
			prompt := req.GetPrompt()
			if prompt == "" {
				if issueNum > 0 {
					prompt = fmt.Sprintf("Take a look at the status of issue #%d - if the label matches any of the workflows in the brotherlogic/seraphine project's .agent/workflows list then you should follow that workflow. Otherwise just suggest a path forward for the issue - do not undertake any implementation work", issueNum)
				} else {
					prompt = "Take a look at the status of this issue - if the label matches any of the workflows in the brotherlogic/seraphine project's .agent/workflows list then you should follow that workflow. Otherwise just suggest a path forward for the issue - do not undertake any implementation work"
				}
			}
			cmdToInject = fmt.Sprintf("pi --prompt %s", shellQuote(prompt))
		} else if req.GetPrompt() != "" {
			cmdToInject = fmt.Sprintf("%s %s", agyInteractivePrefix, shellQuote(req.GetPrompt()))
		} else if cmdToInject == "" {
			if issueNum > 0 {
				cmdToInject = fmt.Sprintf(`%s "Take a look at the status of issue #%d - if the label matches any of the workflows in the brotherlogic/seraphine project's .agent/workflows list then you should follow that workflow. Otherwise just suggest a path forward for the issue - do not undertake any implementation work"`, agyInteractivePrefix, issueNum)
			} else {
				cmdToInject = defaultIssueStartupCommand
			}
		}

		if cmdToInject != "" {
			wg.Add(1)
			go func(cid string) {
				defer wg.Done()
				if err := injectStartupCommand(ctx, req.GetRepo(), cid, cmdToInject, req.GetHarness()); err != nil {
					logWithPrefix(cid, "ERROR: Failed to inject startup command for container %s: %v", cid, err)
					config.State = proto.State_DCM_FAILED
					config.ErrorMessage = err.Error()
					globalCache.Update(cid, config)

					if client != nil && owner != "" && repoName != "" && issueNum > 0 {
						adjustIssueLabels(ctx, client, owner, repoName, issueNum, "container-failed", []string{"container-creating", "container-ready", "container-asleep"}, fmt.Sprintf("startup command injection failed: %v", err))
					}
					if delErr := deleteContainer(req.GetRepo(), cid); delErr != nil {
						logWithPrefix(cid, "Warning: failed to delete failed devcontainer %s: %v", cid, delErr)
					}
					// Ensure the failed status remains in the cache for dashboard visibility despite the container deletion
					globalCache.Update(cid, config)
					if client != nil && owner != "" && repoName != "" {
						go reportStartupFailure(ctx, client, owner, repoName, req.GetBranch(), issueNum, err, "")
					}
				}
			}(config.Id)
		}
	}
}

func stopContainer(repo, id string) error {
	stopOut, err := runCommandWithLog(repo, devpodExe, "stop", id)
	if err != nil {
		return fmt.Errorf("%s stop failed: %w (output: %s)", devpodExe, err, string(stopOut))
	}

	logWithPrefix(repo, "Successfully stopped devcontainer %s", id)
	return nil
}

func extractTokenUsage(ctx context.Context, repo, containerID string) *proto.TokenUsage {
	out, err := runCommandWithLog(repo, devpodExe, "ssh", containerID, "--command", "cat /tmp/token_usage.json")
	if err != nil {
		return &proto.TokenUsage{
			Status:        proto.ExtractionStatus_EXTRACTION_FAILED,
			FailureReason: err.Error(),
		}
	}

	var data struct {
		TotalTokens int64 `json:"total_tokens"`
	}
	if jsonErr := json.Unmarshal(out, &data); jsonErr == nil && data.TotalTokens > 0 {
		return &proto.TokenUsage{
			TotalTokens: data.TotalTokens,
			Status:      proto.ExtractionStatus_EXTRACTION_SUCCESS,
		}
	}

	if val, parseErr := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); parseErr == nil {
		return &proto.TokenUsage{
			TotalTokens: val,
			Status:      proto.ExtractionStatus_EXTRACTION_SUCCESS,
		}
	}

	return &proto.TokenUsage{
		Status:        proto.ExtractionStatus_EXTRACTION_FAILED,
		FailureReason: fmt.Sprintf("failed to parse token usage output: %s", string(out)),
	}
}

func postTokenUsageReport(ctx context.Context, client *github.Client, owner, repoName string, issueNumber int, containerID string, usage *proto.TokenUsage) error {
	if client == nil {
		return fmt.Errorf("github client is nil")
	}

	statusStr := usage.GetStatus().String()
	tokensStr := "N/A"
	if usage.GetStatus() == proto.ExtractionStatus_EXTRACTION_SUCCESS {
		tokensStr = fmt.Sprintf("%d", usage.GetTotalTokens())
	}
	reasonStr := "N/A"
	if usage.GetFailureReason() != "" {
		reasonStr = usage.GetFailureReason()
	}

	body := fmt.Sprintf("### 📊 Devcontainer Closure Token Usage Report\n"+
		"- **Container ID:** `%s`\n"+
		"- **Extraction Status:** `%s`\n"+
		"- **Total Tokens Consumed:** `%s`\n"+
		"- **Notes / Failure Reason:** `%s`",
		containerID, statusStr, tokensStr, reasonStr)

	newComment := &github.IssueComment{Body: &body}
	_, _, err := client.Issues.CreateComment(ctx, owner, repoName, issueNumber, newComment)
	return err
}

func deleteContainer(repo, id string) error {
	ctx := context.Background()

	usage := extractTokenUsage(ctx, repo, id)

	var issueNumber int
	var owner, repoName string

	if config, ok := globalCache.Get(id); ok {
		if usage != nil {
			config.TokenUsage = usage
			globalCache.Update(id, config)
		}
		if req := config.GetRequest(); req != nil {
			if req.GetRepo() != "" {
				parts := strings.Split(req.GetRepo(), "/")
				if len(parts) == 2 {
					owner = parts[0]
					repoName = parts[1]
				}
			}
			if req.GetIdentifier() != nil {
				issueNumber = int(req.GetIdentifier().GetIssueNumber())
			}
		}
	}

	if owner == "" && repo != "" {
		parts := strings.Split(repo, "/")
		if len(parts) == 2 {
			owner = parts[0]
			repoName = parts[1]
		}
	}

	if issueNumber > 0 && owner != "" && repoName != "" {
		client, err := gitHubClientProvider()
		if err == nil && client != nil {
			if commentErr := postTokenUsageReport(ctx, client, owner, repoName, issueNumber, id, usage); commentErr != nil {
				logWithPrefix(repo, "Warning: failed to post token usage report for issue %d: %v", issueNumber, commentErr)
			}
		} else {
			logWithPrefix(repo, "Warning: could not get GitHub client for token usage comment: %v", err)
		}
	} else {
		logWithPrefix(repo, "Token usage for non-issue container %s: Status=%s, TotalTokens=%d, Reason=%s",
			id, usage.GetStatus(), usage.GetTotalTokens(), usage.GetFailureReason())
	}

	deleteOut, err := runCommandWithLog(repo, devpodExe, "delete", id)
	if err != nil {
		return fmt.Errorf("%s delete failed: %w (output: %s)", devpodExe, err, string(deleteOut))
	}

	globalCache.Delete(id)
	logWithPrefix(repo, "Successfully deleted devcontainer %s", id)
	return nil
}

func sortReposByLastUpdated(repos []string) {
	log.Printf("Sorting repositories by most recently touched...")
	type RepoUpdate struct {
		Name     string
		PushedAt time.Time
	}

	updates := make([]RepoUpdate, len(repos))
	var wg sync.WaitGroup

	for i, repo := range repos {
		wg.Add(1)
		go func(index int, r string) {
			defer wg.Done()
			updates[index].Name = r

			cmd := exec.Command("gh", "api", fmt.Sprintf("repos/%s", r), "--jq", ".pushed_at")
			out, err := cmd.Output()
			if err == nil {
				pushedAtStr := strings.TrimSpace(string(out))
				// Remove quotes if present from jq output
				pushedAtStr = strings.Trim(pushedAtStr, "\"")
				t, err := time.Parse(time.RFC3339, pushedAtStr)
				if err == nil {
					updates[index].PushedAt = t
				} else {
					log.Printf("Warning: failed to parse pushed_at for %s: %v", r, err)
				}
			} else {
				log.Printf("Warning: failed to fetch pushed_at for %s: %v", r, err)
			}
		}(i, repo)
	}

	wg.Wait()

	sort.SliceStable(updates, func(i, j int) bool {
		return updates[i].PushedAt.After(updates[j].PushedAt)
	})

	for i, update := range updates {
		repos[i] = update.Name
	}
}

var getConfigDir = func() string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("failed to get home directory: %v", err)
	}
	configDir := filepath.Join(homeDir, ".config", "devcontainer-manager")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		log.Fatalf("failed to create config directory: %v", err)
	}
	return configDir
}

func loadTrackedSHAs() map[string]string {
	shaMutex.RLock()
	defer shaMutex.RUnlock()
	return loadTrackedSHAsLocked()
}

func loadTrackedSHAsLocked() map[string]string {
	trackerPath := filepath.Join(getConfigDir(), "tracked_shas.json")
	shas := make(map[string]string)

	bytes, err := os.ReadFile(trackerPath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("Warning: failed to read tracked SHAs file %s: %v", trackerPath, err)
		}
		return shas
	}

	if err := json.Unmarshal(bytes, &shas); err != nil {
		log.Printf("Warning: failed to unmarshal tracked SHAs from %s: %v", trackerPath, err)
	}
	return shas
}

func saveTrackedSHAs(shas map[string]string) error {
	shaMutex.Lock()
	defer shaMutex.Unlock()
	return saveTrackedSHAsLocked(shas)
}

func saveTrackedSHAsLocked(shas map[string]string) error {
	trackerPath := filepath.Join(getConfigDir(), "tracked_shas.json")
	bytes, err := json.MarshalIndent(shas, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(trackerPath, bytes, 0644)
}

func updateAndSaveRepoSHA(repo string, sha string, trackedSHAs map[string]string) {
	shaMutex.Lock()
	defer shaMutex.Unlock()
	trackedSHAs[repo] = sha
	if err := saveTrackedSHAsLocked(trackedSHAs); err != nil {
		log.Printf("Warning: failed to save tracked SHAs: %v", err)
	}
}

func getTrackedSHA(repo string, trackedSHAs map[string]string) (string, bool) {
	shaMutex.RLock()
	defer shaMutex.RUnlock()
	val, ok := trackedSHAs[repo]
	return val, ok
}

func deleteTrackedSHA(repo string, trackedSHAs map[string]string) {
	shaMutex.Lock()
	defer shaMutex.Unlock()
	delete(trackedSHAs, repo)
}

func stripComments(jsonStr string) string {
	var result strings.Builder
	inString := false
	inLineComment := false
	inBlockComment := false

	runes := []rune(jsonStr)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if inLineComment {
			if r == '\n' || r == '\r' {
				inLineComment = false
				result.WriteRune(r)
			}
			continue
		}
		if inBlockComment {
			if r == '*' && i+1 < len(runes) && runes[i+1] == '/' {
				inBlockComment = false
				i++
			}
			continue
		}
		if inString {
			if r == '"' {
				escaped := false
				for j := i - 1; j >= 0; j-- {
					if runes[j] == '\\' {
						escaped = !escaped
					} else {
						break
					}
				}
				if !escaped {
					inString = false
				}
			}
			result.WriteRune(r)
			continue
		}

		if r == '"' {
			inString = true
			result.WriteRune(r)
			continue
		}
		if r == '/' && i+1 < len(runes) && runes[i+1] == '/' {
			inLineComment = true
			i++
			continue
		}
		if r == '/' && i+1 < len(runes) && runes[i+1] == '*' {
			inBlockComment = true
			i++
			continue
		}
		result.WriteRune(r)
	}
	return result.String()
}

func cleanJSON(jsonStr string) string {
	cleaned := stripComments(jsonStr)
	reTrailingComma := regexp.MustCompile(`,(\s*[}\]])`)
	cleaned = reTrailingComma.ReplaceAllString(cleaned, "$1")
	return cleaned
}

func extractTokens(cmd string) []string {
	var tokens []string
	var current strings.Builder
	inDoubleQuote := false
	inSingleQuote := false
	escaped := false

	for _, r := range cmd {
		if escaped {
			current.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if r == '"' && !inSingleQuote {
			inDoubleQuote = !inDoubleQuote
			continue
		}
		if r == '\'' && !inDoubleQuote {
			inSingleQuote = !inSingleQuote
			continue
		}
		if (r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == ';' || r == '&' || r == '|') && !inDoubleQuote && !inSingleQuote {
			if current.Len() > 0 {
				tokens = append(tokens, current.String())
				current.Reset()
			}
			continue
		}
		current.WriteRune(r)
	}
	if current.Len() > 0 {
		tokens = append(tokens, current.String())
	}
	return tokens
}

func isScriptCandidate(token string) bool {
	token = strings.TrimSpace(token)
	if token == "" {
		return false
	}
	if strings.HasPrefix(token, "-") {
		return false
	}
	builtins := map[string]bool{
		"bash": true, "sh": true, "zsh": true, "sudo": true, "apt": true, "go": true,
		"python": true, "python3": true, "pip": true, "npm": true, "yarn": true, "node": true,
		"echo": true, "cat": true, "mkdir": true, "cd": true, "rm": true, "mv": true,
		"cp": true, "chmod": true, "chown": true, "env": true, "export": true, "git": true,
		"make": true, "docker": true, "kubectl": true, "az": true, "gcloud": true, "curl": true,
		"wget": true, "tar": true, "unzip": true, "grep": true, "sed": true, "awk": true,
	}
	if builtins[token] {
		return false
	}
	extensions := []string{".sh", ".bash", ".zsh", ".py", ".pl", ".rb", ".js", ".ts"}
	for _, ext := range extensions {
		if strings.HasSuffix(token, ext) {
			return true
		}
	}
	if (strings.HasPrefix(token, "/") || strings.HasPrefix(token, "./") || strings.HasPrefix(token, "../") || strings.Contains(token, "/")) && !strings.Contains(token, "://") {
		return true
	}
	return false
}

func normalizePath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "./")
	p = path.Clean(p)
	return p
}

func extractScriptsFromJSON(jsonContent string) []string {
	cleaned := cleanJSON(jsonContent)
	var data map[string]interface{}
	if err := json.Unmarshal([]byte(cleaned), &data); err != nil {
		log.Printf("Warning: failed to parse devcontainer JSON: %v. Falling back to regex.", err)
		return extractScriptsViaRegex(jsonContent)
	}

	scriptKeys := []string{
		"initializeCommand",
		"onCreateCommand",
		"updateContentCommand",
		"postCreateCommand",
		"postStartCommand",
		"postAttachCommand",
	}

	var candidates []string
	for _, key := range scriptKeys {
		val, ok := data[key]
		if !ok {
			continue
		}

		switch v := val.(type) {
		case string:
			for _, token := range extractTokens(v) {
				if isScriptCandidate(token) {
					candidates = append(candidates, normalizePath(token))
				}
			}
		case []interface{}:
			for _, item := range v {
				if s, ok := item.(string); ok {
					for _, token := range extractTokens(s) {
						if isScriptCandidate(token) {
							candidates = append(candidates, normalizePath(token))
						}
					}
				}
			}
		}
	}

	unique := make(map[string]bool)
	var result []string
	for _, c := range candidates {
		if !unique[c] {
			unique[c] = true
			result = append(result, c)
		}
	}
	return result
}

func extractScriptsViaRegex(content string) []string {
	var result []string
	re := regexp.MustCompile(`"([^"]+)"`)
	matches := re.FindAllStringSubmatch(content, -1)
	for _, match := range matches {
		if len(match) > 1 {
			for _, token := range extractTokens(match[1]) {
				if isScriptCandidate(token) {
					result = append(result, normalizePath(token))
				}
			}
		}
	}

	unique := make(map[string]bool)
	var finalResult []string
	for _, r := range result {
		if !unique[r] {
			unique[r] = true
			finalResult = append(finalResult, r)
		}
	}
	return finalResult
}

func fetchFileContent(ctx context.Context, client *github.Client, owner, repo, path string, ref string) (string, string, error) {
	var opts *github.RepositoryContentGetOptions
	if ref != "" {
		opts = &github.RepositoryContentGetOptions{Ref: ref}
	}
	fileContent, _, _, err := client.Repositories.GetContents(ctx, owner, repo, path, opts)
	if err != nil {
		return "", "", err
	}
	content, err := fileContent.GetContent()
	if err != nil {
		return "", "", err
	}
	return content, fileContent.GetSHA(), nil
}

func getFileSHA(ctx context.Context, client *github.Client, owner, repoName, path string, ref string) (string, bool) {
	var opts *github.RepositoryContentGetOptions
	if ref != "" {
		opts = &github.RepositoryContentGetOptions{Ref: ref}
	}
	fileContent, _, resp, err := client.Repositories.GetContents(ctx, owner, repoName, path, opts)
	if err != nil {
		if resp != nil && resp.StatusCode == 404 {
			return "", false
		}
		log.Printf("Warning: failed to get contents for %s/%s at %s: %v", owner, repoName, path, err)
		return "", false
	}
	if fileContent == nil {
		return "", false
	}
	return fileContent.GetSHA(), true
}

func getRepoCompositeSHA(ctx context.Context, client *github.Client, repo string, ref string) (string, bool, error) {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 {
		return "", false, fmt.Errorf("invalid repository format: %s", repo)
	}
	owner, repoName := parts[0], parts[1]

	var devcontainerJSON string
	var devcontainerSHA string
	var found bool

	content, sha, err := fetchFileContent(ctx, client, owner, repoName, ".devcontainer/devcontainer.json", ref)
	if err == nil {
		devcontainerJSON = content
		devcontainerSHA = sha
		found = true
	} else {
		content, sha, err = fetchFileContent(ctx, client, owner, repoName, "devcontainer.json", ref)
		if err == nil {
			devcontainerJSON = content
			devcontainerSHA = sha
			found = true
		}
	}

	if !found {
		return "", false, nil
	}

	shaMap := map[string]string{
		"devcontainer.json": devcontainerSHA,
	}

	scripts := extractScriptsFromJSON(devcontainerJSON)
	for _, scriptPath := range scripts {
		if sha, ok := getFileSHA(ctx, client, owner, repoName, scriptPath, ref); ok {
			shaMap[scriptPath] = sha
		} else {
			fallbackPath := path.Join(".devcontainer", scriptPath)
			if sha, ok := getFileSHA(ctx, client, owner, repoName, fallbackPath, ref); ok {
				shaMap[fallbackPath] = sha
			}
		}
	}

	var keys []string
	for k := range shaMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var partsSHA []string
	for _, k := range keys {
		partsSHA = append(partsSHA, fmt.Sprintf("%s:%s", k, shaMap[k]))
	}

	return strings.Join(partsSHA, "|"), true, nil
}

func recreateContainer(ctx context.Context, repo string, id string, startupCmd string, wg *sync.WaitGroup) error {
	logWithPrefix(repo, "Recreating devcontainer...")
	if err := deleteContainer(repo, id); err != nil {
		logWithPrefix(repo, "Warning: failed to delete container %s before recreating: %v", id, err)
	}

	container := &proto.DevcontainerConfig{
		Id:      id,
		Request: &proto.UpRequest{Repo: fmt.Sprintf("git@github.com:%s", repo), Branch: defaultBranchRef},
		State:   proto.State_DCM_CREATING,
	}
	globalCache.Update(id, container)

	out, err := runCommandWithLog(repo, devpodExe, "up", fmt.Sprintf("git@github.com:%s", repo), "--id", id, "--ide", "none")
	if err != nil {
		container.State = proto.State_DCM_FAILED
		container.ErrorMessage = err.Error()
		globalCache.Update(id, container)
		return fmt.Errorf("%s up failed: %w (output: %s)", devpodExe, err, string(out))
	}

	container.State = proto.State_DCM_READY
	globalCache.Update(id, container)

	logWithPrefix(repo, "Successfully recreated devcontainer")
	renameDockerContainer(id)
	if startupCmd != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := injectStartupCommand(ctx, repo, id, startupCmd, proto.Harness_HARNESS_ANTIGRAVITY); err != nil {
				logWithPrefix(repo, "ERROR: Failed to inject startup command for container %s: %v", id, err)
			}
		}()
	}
	return nil
}

func recreateIssueContainer(ctx context.Context, owner, repoName, branchName, containerID, startupCmd string, wg *sync.WaitGroup) error {
	repo := fmt.Sprintf("%s/%s", owner, repoName)
	logWithPrefix(repo, "Recreating devcontainer for issue container %s on branch %s...", containerID, branchName)
	if err := deleteContainer(repo, containerID); err != nil {
		logWithPrefix(repo, "Warning: failed to delete container %s before recreating: %v", containerID, err)
	}
	repoURL := fmt.Sprintf("git@github.com:%s/%s@%s", owner, repoName, branchName)

	var issueNum int32
	if idx := strings.LastIndex(containerID, "-"); idx != -1 {
		if val, err := strconv.ParseInt(containerID[idx+1:], 10, 32); err == nil {
			issueNum = int32(val)
		}
	}
	var identifier *proto.Identifier
	if issueNum > 0 {
		identifier = &proto.Identifier{
			Id: &proto.Identifier_IssueNumber{IssueNumber: issueNum},
		}
	}
	container := &proto.DevcontainerConfig{
		Id:      containerID,
		Request: &proto.UpRequest{Repo: repoURL, Branch: branchName, Identifier: identifier},
		State:   proto.State_DCM_CREATING,
	}
	globalCache.Update(containerID, container)

	// Use --recreate to ensure we pull the freshest version of the container from head, avoiding stale caches.
	out, err := runCommandWithLog(repo, devpodExe, "up", repoURL, "--id", containerID, "--ide", "none", "--recreate")
	if err != nil {
		container.State = proto.State_DCM_FAILED
		container.ErrorMessage = err.Error()
		globalCache.Update(containerID, container)
		return fmt.Errorf("%s up failed: %w (output: %s)", devpodExe, err, string(out))
	}

	container.State = proto.State_DCM_READY
	globalCache.Update(containerID, container)

	logWithPrefix(repo, "Successfully recreated devcontainer for issue container %s", containerID)
	renameDockerContainer(containerID)

	cmdToInject := startupCmd
	if cmdToInject == "" {
		issueNum := 0
		parts := strings.Split(containerID, "-")
		if len(parts) > 0 {
			if num, err := strconv.Atoi(parts[len(parts)-1]); err == nil {
				issueNum = num
			}
		}
		if issueNum > 0 {
			cmdToInject = fmt.Sprintf(`agy --dangerously-skip-permissions --prompt-interactive "Take a look at the status of issue #%d - if the issue matches any of the labels in the issues.md file, follow the referenced workflow immediately. Otherwise, if there's associated information in the issues.md file, follow those instructions and begin work immediately. Otherwise just suggest a path forward for the issue - do not undertake any implementation work"`, issueNum)
		} else {
			cmdToInject = defaultIssueStartupCommand
		}
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := injectStartupCommand(ctx, repo, containerID, cmdToInject, proto.Harness_HARNESS_ANTIGRAVITY); err != nil {
			logWithPrefix(repo, "ERROR: Failed to inject startup command for container %s: %v", containerID, err)
		}
	}()
	return nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// injectStartupCommand polls a devcontainer tmux session for readiness and injects the specified startup command into tmux.
// If harness is HARNESS_PI, it checks if `pi` is installed via `command -v pi` and executes the pi.dev installation script if missing.
// All injected prompt strings and commands are safely quoted using shellQuote to sanitize metacharacters and prevent shell command injection.
func injectStartupCommand(ctx context.Context, repo string, id string, startupCmd string, harness proto.Harness) error {
	if startupCmd == "" {
		return nil
	}
	logWithPrefix(repo, "Starting tmux readiness polling for container %s", id)

	ticker := time.NewTicker(pollingInterval)
	defer ticker.Stop()

	timeout := time.After(pollingTimeout)

	for {
		select {
		case <-ctx.Done():
			logWithPrefix(repo, "Context cancelled while waiting for container %s", id)
			return fmt.Errorf("context cancelled while waiting for container %s", id)
		case <-timeout:
			logWithPrefix(repo, "Timeout reached waiting for container %s tmux session to be ready", id)
			return fmt.Errorf("timeout reached waiting for container %s tmux session to be ready", id)
		case <-ticker.C:
			out, err := runCommandWithLog(repo, devpodExe, "ssh", id, "--command", fmt.Sprintf("tmux has-session -t %s", id))
			sessionName := id
			if err != nil {
				// Fallback to base name if it is an issue container
				lastIdx := strings.LastIndex(id, "-")
				if lastIdx != -1 {
					if _, errNum := strconv.Atoi(id[lastIdx+1:]); errNum == nil {
						baseID := id[:lastIdx]
						fallbackOut, fallbackErr := runCommandWithLog(repo, devpodExe, "ssh", id, "--command", fmt.Sprintf("tmux has-session -t %s", baseID))
						if fallbackErr == nil {
							err = nil
							sessionName = baseID
							out = fallbackOut
						}
					}
				}
			}

			if err == nil {
				logWithPrefix(repo, "Container %s tmux session %q is ready.", id, sessionName)

				if harness == proto.Harness_HARNESS_PI {
					logWithPrefix(repo, "Checking if pi is installed in container %s...", id)
					_, checkErr := runCommandWithLog(repo, devpodExe, "ssh", id, "--command", "command -v pi")
					if checkErr != nil {
						logWithPrefix(repo, "pi missing in container %s. Running installation script (pi.dev)...", id)
						installOut, installErr := runCommandWithLog(repo, devpodExe, "ssh", id, "--command", "curl -fsSL https://pi.dev | sh")
						if installErr != nil {
							logWithPrefix(repo, "Failed to install pi in container %s: %v (output: %s)", id, installErr, string(installOut))
							return fmt.Errorf("failed to install pi: %w (output: %s)", installErr, string(installOut))
						}
					}
				}

				logWithPrefix(repo, "Injecting startup command...")
				injectOut, injectErr := runCommandWithLog(repo, devpodExe, "ssh", id, "--command", fmt.Sprintf("tmux send-keys -t %s %s C-m", sessionName, shellQuote(startupCmd)))
				if injectErr != nil {
					logWithPrefix(repo, "Failed to inject startup command for %s: %v (output: %s)", id, injectErr, string(injectOut))
					return fmt.Errorf("failed to inject startup command: %w (output: %s)", injectErr, string(injectOut))
				}
				logWithPrefix(repo, "Successfully injected startup command for %s", id)
				return nil
			}
			logWithPrefix(repo, "Polling container %s: tmux session not ready yet: %v (output: %s)", id, err, strings.TrimSpace(string(out)))
		}
	}
}

func deriveFeatureSlug(title string) (string, error) {
	title = strings.ToLower(title)
	var sb strings.Builder
	for _, r := range title {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
		} else {
			sb.WriteRune(' ')
		}
	}

	words := strings.Fields(sb.String())
	var filtered []string
	stopWords := map[string]bool{
		"the": true, "a": true, "an": true, "to": true, "of": true,
		"in": true, "for": true, "on": true, "with": true, "and": true,
		"is": true, "it": true, "use": true, "add": true, "fix": true,
	}
	for _, w := range words {
		if !stopWords[w] {
			filtered = append(filtered, w)
		}
	}

	if len(filtered) == 0 {
		filtered = words
	}

	if len(filtered) > 3 {
		filtered = filtered[:3]
	}

	for len(filtered) < 3 {
		filtered = append(filtered, "feature")
	}

	return strings.Join(filtered, "_"), nil
}

func ensureIssueBranchExists(ctx context.Context, client *github.Client, owner, repoName, branchName string) error {
	refName := "heads/" + branchName
	_, resp, err := client.Git.GetRef(ctx, owner, repoName, refName)
	if err == nil {
		return nil
	}

	if resp == nil || resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("failed to check if branch %s exists: %w", branchName, err)
	}

	// 1. Retrieve the default branch name dynamically
	repo, _, err := client.Repositories.Get(ctx, owner, repoName)
	if err != nil {
		return fmt.Errorf("failed to get repository info: %w", err)
	}
	defaultBranch := repo.GetDefaultBranch()
	if defaultBranch == "" {
		defaultBranch = "main"
	}

	// 2. Fetch the latest commit SHA of the default branch
	defaultRefName := "heads/" + defaultBranch
	defaultRef, _, err := client.Git.GetRef(ctx, owner, repoName, defaultRefName)
	if err != nil {
		return fmt.Errorf("failed to get default branch ref %s: %w", defaultBranch, err)
	}
	latestSHA := defaultRef.GetObject().GetSHA()
	if latestSHA == "" {
		return fmt.Errorf("failed to get commit SHA for default branch %s", defaultBranch)
	}

	// 3. Create the new branch reference
	targetRef := "refs/heads/" + branchName
	ref := &github.Reference{
		Ref:    github.String(targetRef),
		Object: &github.GitObject{SHA: github.String(latestSHA)},
	}
	_, _, err = client.Git.CreateRef(ctx, owner, repoName, ref)
	if err != nil {
		return fmt.Errorf("failed to create branch ref: %w", err)
	}

	return nil
}

func adjustIssueLabels(ctx context.Context, client *github.Client, owner, repo string, issueNumber int, addLabel string, removeLabels []string, reason string) {
	if client == nil {
		return
	}
	log.Printf("Adjusting labels for issue #%d in %s/%s: add='%s', remove=%v (reason: %s)", issueNumber, owner, repo, addLabel, removeLabels, reason)
	// Fetch the issue first to get current labels and avoid redundant API calls
	issue, _, err := client.Issues.Get(ctx, owner, repo, issueNumber)
	if err != nil {
		log.Printf("Warning: failed to fetch issue %d in %s/%s for label adjustment: %v", issueNumber, owner, repo, err)
		return
	}

	hasAddLabel := false
	var existingRemoveLabels []string
	for _, l := range issue.Labels {
		name := l.GetName()
		if name == addLabel {
			hasAddLabel = true
		}
		for _, r := range removeLabels {
			if name == r {
				existingRemoveLabels = append(existingRemoveLabels, name)
			}
		}
	}

	// Remove labels that shouldn't be there
	for _, r := range existingRemoveLabels {
		log.Printf("Removing label '%s' from issue #%d in %s/%s (reason: %s)", r, issueNumber, owner, repo, reason)
		_, err := client.Issues.RemoveLabelForIssue(ctx, owner, repo, issueNumber, r)
		if err != nil {
			log.Printf("Warning: failed to remove label %s from issue %d in %s/%s: %v", r, issueNumber, owner, repo, err)
		}
	}

	// Add the new label if not present
	if !hasAddLabel && addLabel != "" {
		log.Printf("Adding label '%s' to issue #%d in %s/%s (reason: %s)", addLabel, issueNumber, owner, repo, reason)
		_, _, err := client.Issues.AddLabelsToIssue(ctx, owner, repo, issueNumber, []string{addLabel})
		if err != nil {
			log.Printf("Warning: failed to add label %s to issue %d in %s/%s: %v", addLabel, issueNumber, owner, repo, err)
		}
	}
}

func renameDockerContainer(workspaceID string) {
	log.Printf("Attempting to rename docker container to %s...", workspaceID)

	workspaces, err := listDevpodWorkspaces()
	if err != nil {
		log.Printf("Error fetching devpod workspaces: %v", err)
		return
	}

	var targetUid string
	for _, w := range workspaces {
		if w.ID == workspaceID {
			targetUid = w.UID
			break
		}
	}

	if targetUid == "" {
		log.Printf("Could not find devpod workspace uid for %s", workspaceID)
		return
	}

	out, err := commandRunner("docker", "ps", "--format", "{{.ID}}|{{.Names}}|{{.Labels}}")
	if err != nil {
		log.Printf("Error running docker ps: %v", err)
		return
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var targetID, currentName string

	for _, line := range lines {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) >= 3 {
			id, name, labels := parts[0], parts[1], parts[2]
			if strings.Contains(labels, fmt.Sprintf("dev.containers.id=%s", targetUid)) {
				targetID, currentName = id, name
				break
			}
		}
	}

	if targetID != "" {
		if currentName == workspaceID {
			log.Printf("Container %s is already named %s", targetID, workspaceID)
			return
		}

		log.Printf("Renaming container %s (currently %s) to %s", targetID, currentName, workspaceID)
		if _, err := commandRunner("docker", "rename", targetID, workspaceID); err != nil {
			log.Printf("Failed to rename container: %v", err)
		} else {
			log.Printf("Successfully renamed container to %s", workspaceID)
		}
	} else {
		log.Printf("Could not identify which docker container corresponds to devpod uid %s for %s", targetUid, workspaceID)
	}
}

// reportStartupFailure reports startup failure logs back to GitHub issues.
// It first attempts to write to the target repository using createIssueWithDeduplication,
// and falls back to writing to brotherlogic/devcontainer-manager if it encounters a permission error.
func reportStartupFailure(ctx context.Context, client *github.Client, owner, repo, branch string, originalIssueNum int, startupErr error, outputLog string) {
	if client == nil {
		return
	}

	err := createIssueWithDeduplication(ctx, client, owner, repo, owner, repo, branch, originalIssueNum, startupErr, outputLog)
	if err != nil {
		isPermissionError := false
		var errResponse *github.ErrorResponse
		if errors.As(err, &errResponse) && errResponse.Response != nil {
			if errResponse.Response.StatusCode == http.StatusForbidden || errResponse.Response.StatusCode == http.StatusNotFound {
				isPermissionError = true
			}
		}

		if isPermissionError {
			fallbackErr := createIssueWithDeduplication(ctx, client, "brotherlogic", "devcontainer-manager", owner, repo, branch, originalIssueNum, startupErr, outputLog)
			if fallbackErr != nil {
				log.Printf("Warning: failed to create startup failure issue in fallback repository brotherlogic/devcontainer-manager: %v", fallbackErr)
			}
		} else {
			log.Printf("Warning: failed to create startup failure issue in %s/%s: %v", owner, repo, err)
		}
	}
}

func createIssueWithDeduplication(ctx context.Context, client *github.Client, destOwner, destRepo, targetOwner, targetRepo, branch string, originalIssueNum int, startupErr error, outputLog string) error {
	if client == nil {
		return fmt.Errorf("github client is nil")
	}

	issues, err := listOpenIssuesProvider(ctx, client, destOwner, destRepo)
	if err != nil {
		return fmt.Errorf("failed to list open issues: %w", err)
	}

	for _, issue := range issues {
		if issue.GetTitle() == "Issue Container Startup Failed" {
			if destRepo == "devcontainer-manager" {
				targetRef := fmt.Sprintf("**Target Repository:** %s/%s", targetOwner, targetRepo)
				if strings.Contains(issue.GetBody(), targetRef) {
					log.Printf("An open issue 'Issue Container Startup Failed' for %s/%s already exists in %s/%s. Skipping creation.", targetOwner, targetRepo, destOwner, destRepo)
					return nil
				}
			} else {
				log.Printf("An open issue 'Issue Container Startup Failed' already exists in %s/%s. Skipping creation.", destOwner, destRepo)
				return nil
			}
		}
	}

	const (
		GitHubIssueBodyLimit = 65000
		TruncMsg             = "[logs truncated due to size limit] ...\n"
	)

	if len(outputLog) > GitHubIssueBodyLimit {
		outputLog = TruncMsg + outputLog[:GitHubIssueBodyLimit-len(TruncMsg)]
	}

	var bodyBuilder strings.Builder
	bodyBuilder.WriteString("### Devcontainer Startup Failure Report\n\n")
	bodyBuilder.WriteString(fmt.Sprintf("* **Target Repository:** %s/%s\n", targetOwner, targetRepo))
	bodyBuilder.WriteString(fmt.Sprintf("* **Branch:** `%s`\n", branch))
	bodyBuilder.WriteString(fmt.Sprintf("* **Original Issue:** #%d\n\n", originalIssueNum))
	bodyBuilder.WriteString("#### Startup Log / Error Message\n")
	bodyBuilder.WriteString("```\n")
	if startupErr != nil {
		bodyBuilder.WriteString(fmt.Sprintf("Error: %v\n", startupErr))
	}
	if outputLog != "" {
		bodyBuilder.WriteString(outputLog)
		bodyBuilder.WriteString("\n")
	}
	bodyBuilder.WriteString("```\n")

	bodyStr := bodyBuilder.String()

	req := &github.IssueRequest{
		Title:  github.String("Issue Container Startup Failed"),
		Body:   github.String(bodyStr),
		Labels: &[]string{"seraphine-bug"},
	}

	_, _, err = client.Issues.Create(ctx, destOwner, destRepo, req)
	if err != nil {
		return fmt.Errorf("failed to create issue: %w", err)
	}

	return nil
}

func logWithPrefix(repo string, format string, args ...interface{}) {
	prefix := fmt.Sprintf("[%s] ", repo)
	log.Printf(prefix+format, args...)
}

func runCommandWithLog(repo string, name string, args ...string) ([]byte, error) {
	out, err := runDevpodCommand(name, args...)
	if len(out) > 0 {
		for _, line := range strings.Split(string(out), "\n") {
			if strings.TrimSpace(line) != "" {
				logWithPrefix(repo, "%s", line)
			}
		}
	}
	return out, err
}

func getRepoForID(id string, projectRepoMap map[string]string) string {
	lastIdx := strings.LastIndex(id, "-")
	projectID := id
	if lastIdx != -1 {
		projectID = id[:lastIdx]
	}
	if repo, ok := projectRepoMap[projectID]; ok {
		return repo
	}
	return id
}

func withGitHubRetry(ctx context.Context, f func() (*github.Response, error)) error {
	backoff := 100 * time.Millisecond
	maxBackoff := 10 * time.Second
	maxRetries := 5

	for i := 0; i < maxRetries; i++ {
		resp, err := f()
		if err == nil {
			return nil
		}

		isRateLimit := false
		if resp != nil && resp.Response != nil {
			status := resp.Response.StatusCode
			if status == http.StatusForbidden || status == http.StatusTooManyRequests {
				isRateLimit = true
			}
		}

		if _, ok := err.(*github.RateLimitError); ok {
			isRateLimit = true
		}
		if _, ok := err.(*github.AbuseRateLimitError); ok {
			isRateLimit = true
		}

		if !isRateLimit {
			return err
		}

		log.Printf("GitHub API rate limit encountered (attempt %d/%d). Retrying in %v...", i+1, maxRetries, backoff)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}

		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}

	_, err := f()
	return err
}

func postLatencyComment(ctx context.Context, client *github.Client, owner, repo string, issueNum int, createdAt time.Time) error {
	latency := time.Since(createdAt)

	comments, _, err := client.Issues.ListComments(ctx, owner, repo, issueNum, nil)
	if err != nil {
		log.Printf("Error listing comments for issue %d: %v", issueNum, err)
		return err
	}

	for _, comment := range comments {
		if comment.Body != nil && strings.Contains(*comment.Body, "devcontainer-startup-latency") {
			return nil
		}
	}

	body := fmt.Sprintf("devcontainer-startup-latency: %v", latency)
	newComment := &github.IssueComment{Body: &body}
	_, _, err = client.Issues.CreateComment(ctx, owner, repo, issueNum, newComment)
	if err != nil {
		log.Printf("Error creating comment for issue %d: %v", issueNum, err)
		return err
	}

	log.Printf("Logged metric devcontainer-startup-latency for issue %d: %v", issueNum, latency)
	return nil
}

func runDockerPrune() {
	pruneCmds := [][]string{
		{"container", "prune", "-f"},
		{"image", "prune", "-af"},
		{"builder", "prune", "-f"},
	}

	for _, args := range pruneCmds {
		out, err := commandRunner("docker", args...)
		if err != nil {
			log.Printf("Warning: docker %s failed: %v. Output: %s", strings.Join(args, " "), err, string(out))
		} else {
			log.Printf("Successfully executed docker %s: %s", strings.Join(args, " "), string(out))
		}
	}
}

var diskUsageRegexp = regexp.MustCompile(`(\d+)%`)

// parseDiskUsage parses the output of `df -h /` and extracts the disk usage percentage integer.
func parseDiskUsage(output string) (int, error) {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) == 0 || (len(lines) == 1 && lines[0] == "") {
		return 0, fmt.Errorf("empty df output")
	}

	for _, line := range lines {
		matches := diskUsageRegexp.FindStringSubmatch(line)
		if len(matches) > 1 {
			val, err := strconv.Atoi(matches[1])
			if err == nil {
				return val, nil
			}
		}
	}

	return 0, fmt.Errorf("could not parse disk usage percentage from output: %q", output)
}

// isDiskUsageAboveThreshold returns true if the parsed usage percentage strictly exceeds the given threshold percentage.
func isDiskUsageAboveThreshold(usagePercent int, threshold int) bool {
	return usagePercent > threshold
}

const defaultDiskUsageThreshold = 85
const defaultAlertRepository = "brotherlogic/devcontainer-manager"

// createHighDiskUsageIssue creates a GitHub issue on brotherlogic/devcontainer-manager with disk status details when usagePercent exceeds 85%.
func createHighDiskUsageIssue(usagePercent int, diskDetails string) error {
	if !isDiskUsageAboveThreshold(usagePercent, defaultDiskUsageThreshold) {
		return nil
	}

	title := fmt.Sprintf("High Disk Usage Alert: %d%%", usagePercent)
	body := fmt.Sprintf("Disk usage has exceeded the 85%% threshold.\n\nCurrent disk usage: %d%%\n\nDisk Details:\n```\n%s\n```", usagePercent, diskDetails)

	_, err := commandRunner("gh", "issue", "create", "-R", defaultAlertRepository, "--title", title, "--body", body)
	if err != nil {
		return fmt.Errorf("failed to create high disk usage issue: %w", err)
	}

	return nil
}


