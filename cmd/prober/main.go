package main

import (
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/brotherlogic/devcontainer-manager/proto"
	"github.com/google/go-github/v50/github"
	"golang.org/x/oauth2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type ProberConfig struct {
	Server    string
	Repo      string
	Prompt1   string
	Prompt2   string
	Timeout   time.Duration
}

type githubClient interface {
	CreateIssue(ctx context.Context, owner, repo string, req *github.IssueRequest) (*github.Issue, error)
	ListComments(ctx context.Context, owner, repo string, number int) ([]*github.IssueComment, error)
	CloseIssue(ctx context.Context, owner, repo string, number int) error
}

type realGitHubClient struct {
	client *github.Client
}

func (r *realGitHubClient) CreateIssue(ctx context.Context, owner, repo string, req *github.IssueRequest) (*github.Issue, error) {
	issue, _, err := r.client.Issues.Create(ctx, owner, repo, req)
	return issue, err
}

func (r *realGitHubClient) ListComments(ctx context.Context, owner, repo string, number int) ([]*github.IssueComment, error) {
	comments, _, err := r.client.Issues.ListComments(ctx, owner, repo, number, nil)
	return comments, err
}

func (r *realGitHubClient) CloseIssue(ctx context.Context, owner, repo string, number int) error {
	state := "closed"
	_, _, err := r.client.Issues.Edit(ctx, owner, repo, number, &github.IssueRequest{
		State: &state,
	})
	return err
}

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40 // Version 4
	b[8] = (b[8] & 0x3f) | 0x80 // Variant is 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func getGitHubClient() (*github.Client, error) {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		cmd := exec.Command("gh", "auth", "token")
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("failed to get token from gh cli: %w", err)
		}
		token = strings.TrimSpace(string(out))
	}

	if token == "" {
		return nil, fmt.Errorf("GITHUB_TOKEN is not set and could not be retrieved from gh cli")
	}

	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: token},
	)
	tc := oauth2.NewClient(context.Background(), ts)
	return github.NewClient(tc), nil
}

var pollInterval = 5 * time.Second

func pollForComment(ctx context.Context, ghClient githubClient, owner, repo string, issueNum int, targetComment string) error {
	// Check once immediately
	comments, err := ghClient.ListComments(ctx, owner, repo, issueNum)
	if err == nil {
		for _, c := range comments {
			if c.GetBody() == targetComment {
				return nil
			}
		}
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout/cancelled waiting for comment %q: %w", targetComment, ctx.Err())
		case <-ticker.C:
			comments, err := ghClient.ListComments(ctx, owner, repo, issueNum)
			if err != nil {
				log.Printf("Warning: failed to list comments during polling: %v", err)
				continue
			}
			for _, c := range comments {
				if c.GetBody() == targetComment {
					return nil
				}
			}
		}
	}
}

func pollForContainerDeletion(ctx context.Context, managerClient proto.ManagerServiceClient, containerID string) error {
	// Check once immediately
	resp, err := managerClient.List(ctx, &proto.ListRequest{})
	if err == nil {
		found := false
		for _, cfg := range resp.GetConfigs() {
			if cfg.GetId() == containerID {
				found = true
				break
			}
		}
		if !found {
			return nil
		}
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout/cancelled waiting for container %q to be deleted: %w", containerID, ctx.Err())
		case <-ticker.C:
			resp, err := managerClient.List(ctx, &proto.ListRequest{})
			if err != nil {
				log.Printf("Warning: failed to list containers during polling: %v", err)
				continue
			}
			found := false
			for _, cfg := range resp.GetConfigs() {
				if cfg.GetId() == containerID {
					found = true
					break
				}
			}
			if !found {
				return nil
			}
		}
	}
}

func RunProber(ctx context.Context, cfg ProberConfig, ghClient githubClient, managerClient proto.ManagerServiceClient) (err error) {
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	parts := strings.Split(cfg.Repo, "/")
	if len(parts) != 2 {
		return fmt.Errorf("invalid repo format, expected owner/repo, got %s", cfg.Repo)
	}
	owner, repoName := parts[0], parts[1]

	// 1. Create a temporary GitHub issue in the target repo
	title := fmt.Sprintf("[test] %s", newUUID())
	body := "Temporary issue created by integration test prober."
	issueReq := &github.IssueRequest{
		Title: &title,
		Body:  &body,
	}

	issue, err := ghClient.CreateIssue(ctx, owner, repoName, issueReq)
	if err != nil {
		return fmt.Errorf("failed to create issue: %w", err)
	}
	issueNum := int32(issue.GetNumber())
	issueURL := issue.GetHTMLURL()

	// Defer cleanup: ensure Down is called and the issue is closed
	var containerID string
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 1*time.Minute)
		defer cleanupCancel()

		if containerID != "" {
			_, downErr := managerClient.Down(cleanupCtx, &proto.DownRequest{Id: containerID})
			if downErr != nil {
				log.Printf("Warning: failed to call Down on cleanup: %v", downErr)
			}
		}

		closeErr := ghClient.CloseIssue(cleanupCtx, owner, repoName, int(issueNum))
		if closeErr != nil {
			log.Printf("Warning: failed to close issue on cleanup: %v", closeErr)
		}
	}()

	// 4. Connect to the manager gRPC server and call the Up RPC with the newly created issue URL
	branchName := "feature/test-" + strings.TrimPrefix(title, "[test] ")
	upResp, err := managerClient.Up(ctx, &proto.UpRequest{
		Repo:       issueURL,
		Branch:     branchName,
		Identifier: &proto.Identifier{Id: &proto.Identifier_IssueNumber{IssueNumber: issueNum}},
		Prompt:     cfg.Prompt1,
	})
	if err != nil {
		return fmt.Errorf("failed calling Up: %w", err)
	}
	containerID = upResp.GetConfig().GetId()

	// 5. Poll the GitHub issue comments until the first comment matches --prompt-1
	err = pollForComment(ctx, ghClient, owner, repoName, int(issueNum), cfg.Prompt1)
	if err != nil {
		printRunningContainers(managerClient)
		return err
	}

	// 6. Call PushPrompt on the manager with --prompt-2
	_, err = managerClient.PushPrompt(ctx, &proto.PushPromptRequest{
		Id:     containerID,
		Prompt: cfg.Prompt2,
	})
	if err != nil {
		return fmt.Errorf("failed calling PushPrompt: %w", err)
	}

	// Poll comments until matched prompt-2
	err = pollForComment(ctx, ghClient, owner, repoName, int(issueNum), cfg.Prompt2)
	if err != nil {
		printRunningContainers(managerClient)
		return err
	}

	// 7. Call Down on the manager to delete the container
	_, err = managerClient.Down(ctx, &proto.DownRequest{Id: containerID})
	if err != nil {
		return fmt.Errorf("failed calling Down: %w", err)
	}

	// Clear containerID so defer doesn't call Down again
	cID := containerID
	containerID = ""

	// 8. Poll the List RPC until the container is no longer returned in the list
	err = pollForContainerDeletion(ctx, managerClient, cID)
	if err != nil {
		return err
	}

	return nil
}

func main() {
	server := flag.String("server", "localhost:50051", "gRPC server address")
	repo := flag.String("repo", "brotherlogic/devcontainer-manager", "GitHub repository")
	prompt1 := flag.String("prompt-1", "hello", "First prompt comment check")
	prompt2 := flag.String("prompt-2", "goodbye", "Second prompt to send and check")
	timeout := flag.Duration("timeout", 5*time.Minute, "Timeout duration")
	flag.Parse()

	cfg := ProberConfig{
		Server:  *server,
		Repo:    *repo,
		Prompt1: *prompt1,
		Prompt2: *prompt2,
		Timeout: *timeout,
	}

	gh, err := getGitHubClient()
	if err != nil {
		log.Fatalf("failed to create github client: %v", err)
	}
	realGH := &realGitHubClient{client: gh}

	conn, err := grpc.Dial(cfg.Server, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("failed to connect to manager server: %v", err)
	}
	defer conn.Close()
	managerClient := proto.NewManagerServiceClient(conn)

	fmt.Println("Running integration prober...")
	if err := RunProber(context.Background(), cfg, realGH, managerClient); err != nil {
		fmt.Fprintf(os.Stderr, "Prober run failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Prober run completed successfully!")
}

func printRunningContainers(managerClient proto.ManagerServiceClient) {
	listCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := managerClient.List(listCtx, &proto.ListRequest{})
	if err != nil {
		log.Printf("Failed to list containers: %v", err)
		return
	}
	if resp == nil {
		log.Printf("No containers returned (list response is nil)")
		return
	}

	log.Printf("Currently running devcontainers:")
	for _, cfg := range resp.GetConfigs() {
		repo := ""
		if cfg.GetRequest() != nil {
			repo = cfg.GetRequest().GetRepo()
		}
		// Redact any sensitive user info/tokens from the repo URL
		if parsed, err := url.Parse(repo); err == nil && parsed.User != nil {
			parsed.User = url.User("redacted")
			repo = parsed.String()
		}
		log.Printf("  - Container ID: %s, State: %v, Repo: %s, Error: %s",
			cfg.GetId(), cfg.GetState(), repo, cfg.GetErrorMessage())
	}
}
