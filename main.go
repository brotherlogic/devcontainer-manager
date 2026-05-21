package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/go-github/v50/github"
	"golang.org/x/oauth2"
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

func main() {
	var runOnce = flag.Bool("once", false, "Run once and exit")
	var containerList = flag.String("container_list", "container.list.template", "The list of containers to run")
	flag.Parse()

	for {
		err := run(context.Background(), *containerList)
		if err != nil {
			log.Printf("Error: %v", err)
		}

		if *runOnce {
			break
		}

		time.Sleep(time.Minute * 5)
	}
}

func run(ctx context.Context, containerList string) error {
	data, err := os.ReadFile(containerList)
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

	// Get running devcontainers
	cmd := exec.Command("devpod", "list")
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("failed to list devcontainers: %w", err)
	}

	running := make(map[string]bool)
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 {
			running[fields[0]] = true
		}
	}

	for _, repo := range repos {
		id := strings.ReplaceAll(repo, "/", "-")
		if !running[id] {
			log.Printf("Starting devcontainer for %s", repo)
			cmd := exec.Command("devpod", "up", fmt.Sprintf("https://github.com/%s", repo), "--id", id, "--detach")
			out, err := cmd.CombinedOutput()
			if err != nil {
				log.Printf("Failed to start devcontainer for %s: %v (output: %s)", repo, err, string(out))
			}
		}
	}

	return nil
}

const (
	devpodExe = "devpod"
)

func stopContainer(id string) error {
	stopCmd := exec.Command(devpodExe, "stop", id)
	stopOut, err := stopCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s stop failed: %w (output: %s)", devpodExe, err, string(stopOut))
	}

	log.Printf("Successfully stopped devcontainer %s", id)
	return nil
}

func deleteContainer(id string) error {
	deleteCmd := exec.Command(devpodExe, "delete", id)
	deleteOut, err := deleteCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s delete failed: %w (output: %s)", devpodExe, err, string(deleteOut))
	}

	log.Printf("Successfully deleted devcontainer %s", id)
	return nil
}

func sortReposByLastUpdated(repos []string) {
	log.Printf("Sorting repositories by most recently touched...")
	type RepoUpdate struct {
		Name     string
<<<<<<< HEAD
		PushedAt string
=======
		PushedAt time.Time
>>>>>>> origin/main
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
<<<<<<< HEAD
				updates[index].PushedAt = strings.TrimSpace(string(out))
			} else {
				// Keep it empty on error so it falls to the bottom
				updates[index].PushedAt = ""
=======
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
>>>>>>> origin/main
			}
		}(i, repo)
	}

	wg.Wait()

	sort.SliceStable(updates, func(i, j int) bool {
<<<<<<< HEAD
		return updates[i].PushedAt > updates[j].PushedAt
=======
		return updates[i].PushedAt.After(updates[j].PushedAt)
>>>>>>> origin/main
	})

	for i, update := range updates {
		repos[i] = update.Name
	}
}
