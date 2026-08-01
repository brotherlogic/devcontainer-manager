package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/brotherlogic/devcontainer-manager/proto"
	"github.com/google/go-github/v50/github"
	"google.golang.org/grpc"
)

type mockGitHubClient struct {
	createIssueFunc  func(ctx context.Context, owner, repo string, req *github.IssueRequest) (*github.Issue, error)
	listCommentsFunc func(ctx context.Context, owner, repo string, number int) ([]*github.IssueComment, error)
	closeIssueFunc   func(ctx context.Context, owner, repo string, number int) error
}

func (m *mockGitHubClient) CreateIssue(ctx context.Context, owner, repo string, req *github.IssueRequest) (*github.Issue, error) {
	if m.createIssueFunc != nil {
		return m.createIssueFunc(ctx, owner, repo, req)
	}
	return nil, nil
}

func (m *mockGitHubClient) ListComments(ctx context.Context, owner, repo string, number int) ([]*github.IssueComment, error) {
	if m.listCommentsFunc != nil {
		return m.listCommentsFunc(ctx, owner, repo, number)
	}
	return nil, nil
}

func (m *mockGitHubClient) CloseIssue(ctx context.Context, owner, repo string, number int) error {
	if m.closeIssueFunc != nil {
		return m.closeIssueFunc(ctx, owner, repo, number)
	}
	return nil
}

type mockManagerServiceClient struct {
	proto.ManagerServiceClient
	upFunc         func(ctx context.Context, in *proto.UpRequest) (*proto.UpResponse, error)
	downFunc       func(ctx context.Context, in *proto.DownRequest) (*proto.DownResponse, error)
	listFunc       func(ctx context.Context, in *proto.ListRequest) (*proto.ListResponse, error)
	pushPromptFunc func(ctx context.Context, in *proto.PushPromptRequest) (*proto.PushPromptResponse, error)
}

func (m *mockManagerServiceClient) Up(ctx context.Context, in *proto.UpRequest, opts ...grpc.CallOption) (*proto.UpResponse, error) {
	if m.upFunc != nil {
		return m.upFunc(ctx, in)
	}
	return nil, nil
}

func (m *mockManagerServiceClient) Down(ctx context.Context, in *proto.DownRequest, opts ...grpc.CallOption) (*proto.DownResponse, error) {
	if m.downFunc != nil {
		return m.downFunc(ctx, in)
	}
	return nil, nil
}

func (m *mockManagerServiceClient) List(ctx context.Context, in *proto.ListRequest, opts ...grpc.CallOption) (*proto.ListResponse, error) {
	if m.listFunc != nil {
		return m.listFunc(ctx, in)
	}
	return nil, nil
}

func (m *mockManagerServiceClient) PushPrompt(ctx context.Context, in *proto.PushPromptRequest, opts ...grpc.CallOption) (*proto.PushPromptResponse, error) {
	if m.pushPromptFunc != nil {
		return m.pushPromptFunc(ctx, in)
	}
	return nil, nil
}

func TestRunProber_Success(t *testing.T) {
	pollInterval = 50 * time.Millisecond
	cfg := ProberConfig{
		Server:  "localhost:50051",
		Repo:    "brotherlogic/devcontainer-manager",
		Prompt1: "hello",
		Prompt2: "goodbye",
		Timeout: 2 * time.Second,
	}

	issueNum := int32(456)
	issueURL := "https://github.com/brotherlogic/devcontainer-manager/issues/456"

	// Track function calls
	var (
		issueCreated  bool
		issueClosed   bool
		upCalled      bool
		downCalled    bool
		pushCalled    bool
		commentsCount int
		listCount     int
	)

	ghMock := &mockGitHubClient{
		createIssueFunc: func(ctx context.Context, owner, repo string, req *github.IssueRequest) (*github.Issue, error) {
			if owner != "brotherlogic" || repo != "devcontainer-manager" {
				t.Errorf("expected brotherlogic/devcontainer-manager, got %s/%s", owner, repo)
			}
			issueCreated = true
			num := int(issueNum)
			return &github.Issue{
				Number:  &num,
				HTMLURL: &issueURL,
			}, nil
		},
		listCommentsFunc: func(ctx context.Context, owner, repo string, number int) ([]*github.IssueComment, error) {
			if number != int(issueNum) {
				t.Errorf("expected issue number %d, got %d", issueNum, number)
			}
			commentsCount++
			if !pushCalled {
				// Polling prompt 1: return mismatch first, then match
				if commentsCount == 1 {
					body := "other"
					return []*github.IssueComment{{Body: &body}}, nil
				}
				body := "hello"
				return []*github.IssueComment{{Body: &body}}, nil
			} else {
				// Polling prompt 2: return match
				body := "goodbye"
				return []*github.IssueComment{{Body: &body}}, nil
			}
		},
		closeIssueFunc: func(ctx context.Context, owner, repo string, number int) error {
			if number != int(issueNum) {
				t.Errorf("expected issue number %d, got %d", issueNum, number)
			}
			issueClosed = true
			return nil
		},
	}

	mgrMock := &mockManagerServiceClient{
		upFunc: func(ctx context.Context, in *proto.UpRequest) (*proto.UpResponse, error) {
			if in.GetRepo() != issueURL {
				t.Errorf("expected repo to be issueURL %s, got %s", issueURL, in.GetRepo())
			}
			if in.GetIdentifier().GetIssueNumber() != issueNum {
				t.Errorf("expected issue number %d, got %d", issueNum, in.GetIdentifier().GetIssueNumber())
			}
			expectedPrompt := buildIssueCommentPrompt(issueNum)
			if in.GetPrompt() != expectedPrompt {
				t.Errorf("expected prompt %s, got %s", expectedPrompt, in.GetPrompt())
			}
			upCalled = true
			return &proto.UpResponse{
				Config: &proto.DevcontainerConfig{
					Id: "brotherlogic-devcontainer-manager-456",
				},
			}, nil
		},
		pushPromptFunc: func(ctx context.Context, in *proto.PushPromptRequest) (*proto.PushPromptResponse, error) {
			if in.GetId() != "brotherlogic-devcontainer-manager-456" {
				t.Errorf("expected container id brotherlogic-devcontainer-manager-456, got %s", in.GetId())
			}
			if in.GetPrompt() != "goodbye" {
				t.Errorf("expected prompt goodbye, got %s", in.GetPrompt())
			}
			pushCalled = true
			return &proto.PushPromptResponse{}, nil
		},
		downFunc: func(ctx context.Context, in *proto.DownRequest) (*proto.DownResponse, error) {
			if in.GetId() != "brotherlogic-devcontainer-manager-456" {
				t.Errorf("expected container id brotherlogic-devcontainer-manager-456, got %s", in.GetId())
			}
			downCalled = true
			return &proto.DownResponse{}, nil
		},
		listFunc: func(ctx context.Context, in *proto.ListRequest) (*proto.ListResponse, error) {
			listCount++
			if listCount == 1 {
				// return container still in list
				return &proto.ListResponse{
					Configs: []*proto.DevcontainerConfig{
						{Id: "brotherlogic-devcontainer-manager-456"},
					},
				}, nil
			}
			// container no longer in list
			return &proto.ListResponse{}, nil
		},
	}

	err := RunProber(context.Background(), cfg, ghMock, mgrMock)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if !issueCreated {
		t.Error("expected issue to be created")
	}
	if !upCalled {
		t.Error("expected Up RPC to be called")
	}
	if !pushCalled {
		t.Error("expected PushPrompt RPC to be called")
	}
	if !downCalled {
		t.Error("expected Down RPC to be called")
	}
	if !issueClosed {
		t.Error("expected issue to be closed")
	}
}

func TestRunProber_FailureCleanup(t *testing.T) {
	pollInterval = 50 * time.Millisecond
	cfg := ProberConfig{
		Server:  "localhost:50051",
		Repo:    "brotherlogic/devcontainer-manager",
		Prompt1: "hello",
		Prompt2: "goodbye",
		Timeout: 2 * time.Second,
	}

	issueNum := int32(456)
	issueURL := "https://github.com/brotherlogic/devcontainer-manager/issues/456"

	var (
		downCalled  bool
		issueClosed bool
		listCalled  bool
	)

	ghMock := &mockGitHubClient{
		createIssueFunc: func(ctx context.Context, owner, repo string, req *github.IssueRequest) (*github.Issue, error) {
			num := int(issueNum)
			return &github.Issue{
				Number:  &num,
				HTMLURL: &issueURL,
			}, nil
		},
		listCommentsFunc: func(ctx context.Context, owner, repo string, number int) ([]*github.IssueComment, error) {
			// Simulate failure/error during comment polling
			return nil, errors.New("github rate limit or some other error")
		},
		closeIssueFunc: func(ctx context.Context, owner, repo string, number int) error {
			issueClosed = true
			return nil
		},
	}

	mgrMock := &mockManagerServiceClient{
		upFunc: func(ctx context.Context, in *proto.UpRequest) (*proto.UpResponse, error) {
			expectedPrompt := buildIssueCommentPrompt(issueNum)
			if in.GetPrompt() != expectedPrompt {
				t.Errorf("expected prompt %s, got %s", expectedPrompt, in.GetPrompt())
			}
			return &proto.UpResponse{
				Config: &proto.DevcontainerConfig{
					Id: "brotherlogic-devcontainer-manager-456",
				},
			}, nil
		},
		downFunc: func(ctx context.Context, in *proto.DownRequest) (*proto.DownResponse, error) {
			downCalled = true
			return &proto.DownResponse{}, nil
		},
		listFunc: func(ctx context.Context, in *proto.ListRequest) (*proto.ListResponse, error) {
			listCalled = true
			return &proto.ListResponse{
				Configs: []*proto.DevcontainerConfig{
					{
						Id: "brotherlogic-devcontainer-manager-456",
						Request: &proto.UpRequest{
							Repo: "https://user:pass@github.com/brotherlogic/devcontainer-manager/issues/456",
						},
						State: proto.State_DCM_HARNESS,
					},
				},
			}, nil
		},
	}

	err := RunProber(context.Background(), cfg, ghMock, mgrMock)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if !downCalled {
		t.Error("expected Down RPC to be called on failure")
	}
	if !issueClosed {
		t.Error("expected issue to be closed on failure")
	}
	if !listCalled {
		t.Error("expected List RPC to be called on failure")
	}
}

func TestBuildIssueCommentPrompt(t *testing.T) {
	got := buildIssueCommentPrompt(123)
	want := "Please post a comment containing strictly \"hello\" to issue #123 in this repository using the gh CLI tool."
	if got != want {
		t.Errorf("buildIssueCommentPrompt(123) = %q; want %q", got, want)
	}
}

func TestRunProber_UpRPCInvocation(t *testing.T) {
	pollInterval = 10 * time.Millisecond
	cfg := ProberConfig{
		Server:  "localhost:50051",
		Repo:    "brotherlogic/devcontainer-manager",
		Prompt1: "hello",
		Prompt2: "goodbye",
		Timeout: 2 * time.Second,
	}

	issueNum := int32(789)
	issueURL := "https://github.com/brotherlogic/devcontainer-manager/issues/789"

	var upReq *proto.UpRequest

	ghMock := &mockGitHubClient{
		createIssueFunc: func(ctx context.Context, owner, repo string, req *github.IssueRequest) (*github.Issue, error) {
			num := int(issueNum)
			return &github.Issue{
				Number:  &num,
				HTMLURL: &issueURL,
			}, nil
		},
		listCommentsFunc: func(ctx context.Context, owner, repo string, number int) ([]*github.IssueComment, error) {
			body1 := "hello"
			body2 := "goodbye"
			return []*github.IssueComment{{Body: &body1}, {Body: &body2}}, nil
		},
	}

	mgrMock := &mockManagerServiceClient{
		upFunc: func(ctx context.Context, in *proto.UpRequest) (*proto.UpResponse, error) {
			upReq = in
			return &proto.UpResponse{
				Config: &proto.DevcontainerConfig{
					Id: "container-789",
				},
			}, nil
		},
		pushPromptFunc: func(ctx context.Context, in *proto.PushPromptRequest) (*proto.PushPromptResponse, error) {
			return &proto.PushPromptResponse{}, nil
		},
		downFunc: func(ctx context.Context, in *proto.DownRequest) (*proto.DownResponse, error) {
			return &proto.DownResponse{}, nil
		},
		listFunc: func(ctx context.Context, in *proto.ListRequest) (*proto.ListResponse, error) {
			return &proto.ListResponse{}, nil
		},
	}

	err := RunProber(context.Background(), cfg, ghMock, mgrMock)
	if err != nil {
		t.Fatalf("unexpected error running prober: %v", err)
	}

	if upReq == nil {
		t.Fatal("expected Up RPC to be invoked, but it was not")
	}

	if upReq.GetRepo() != issueURL {
		t.Errorf("expected Repo %q, got %q", issueURL, upReq.GetRepo())
	}

	if upReq.GetIdentifier().GetIssueNumber() != issueNum {
		t.Errorf("expected IssueNumber %d, got %d", issueNum, upReq.GetIdentifier().GetIssueNumber())
	}

	expectedPrompt := buildIssueCommentPrompt(issueNum)
	if upReq.GetPrompt() != expectedPrompt {
		t.Errorf("expected Prompt %q, got %q", expectedPrompt, upReq.GetPrompt())
	}

	if !strings.HasPrefix(upReq.GetBranch(), "feature/test-") {
		t.Errorf("expected Branch to have prefix 'feature/test-', got %q", upReq.GetBranch())
	}
}

