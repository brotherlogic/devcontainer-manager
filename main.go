package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

var devpodExe string

func init() {
	flag.StringVar(&devpodExe, "devpod-exe", "devpod-cli", "The executable name for devpod")
}

type Commit struct {
	Sha string `json:"sha"`
}

func main() {
	flag.Parse()
	log.Println("Starting devcontainer manager...")

	if err := checkGHAuth(); err != nil {
		log.Fatalf("GitHub CLI authentication failed: %v", err)
	}

	// Track the last seen commit for each repo
	trackedSHAs := make(map[string]string)

	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	// Run initial check immediately
	checkRepos(trackedSHAs)
	
	// Loop to check periodically
	for range ticker.C {
		checkRepos(trackedSHAs)
	}
}

func checkGHAuth() error {
	cmd := exec.Command("gh", "auth", "status")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("GitHub CLI is not authenticated. Please run 'gh auth login' to authenticate.\nDetails: %s", strings.TrimSpace(string(output)))
	}
	return nil
}

func checkRepos(trackedSHAs map[string]string) {
	repos, err := readContainerList()
	if err != nil {
		log.Printf("Error reading container.list: %v", err)
		return
	}

	for _, repo := range repos {
		checkRepo(repo, trackedSHAs)
	}
}

func readContainerList() ([]string, error) {
	file, err := os.Open("container.list")
	if err != nil {
		if os.IsNotExist(err) {
			log.Println("container.list not found, pulling template from GitHub...")
			if pullErr := pullTemplateFromGitHub(); pullErr != nil {
				return nil, fmt.Errorf("failed to pull template: %w", pullErr)
			}
			file, err = os.Open("container.list")
			if err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}
	defer file.Close()

	var repos []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// Skip empty lines and comments
		if line != "" && !strings.HasPrefix(line, "#") {
			repos = append(repos, line)
		}
	}

	return repos, scanner.Err()
}

func pullTemplateFromGitHub() error {
	cmd := exec.Command("gh", "api", "repos/brotherlogic/devcontainer-manager/contents/container.list.template", "-H", "Accept: application/vnd.github.v3.raw")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh api error: %w (output: %s)", err, strings.TrimSpace(string(output)))
	}
	return os.WriteFile("container.list", output, 0644)
}

func checkRepo(repo string, trackedSHAs map[string]string) {
	log.Printf("Checking %s for devcontainer updates...", repo)

	latestSHA, err := getLatestDevcontainerCommit(repo)
	if err != nil {
		log.Printf("Error checking commits for %s: %v", repo, err)
		return
	}

	if latestSHA == "-" {
		log.Printf("No devcontainer configuration found or no commits for %s", repo)
		return
	}

	lastSeen, exists := trackedSHAs[repo]
	if !exists {
		log.Printf("Initial tracking for %s at commit state %s. Bringing up container...", repo, latestSHA)
		if err := bringUpDevcontainer(repo); err != nil {
			log.Printf("Failed to bring up devcontainer for %s: %v", repo, err)
			return
		}
		trackedSHAs[repo] = latestSHA
		return
	}

	if lastSeen != latestSHA {
		log.Printf("Detected devcontainer change in %s! Updating from %s to %s", repo, lastSeen, latestSHA)

		err := recreateDevcontainer(repo)
		if err != nil {
			log.Printf("Failed to recreate devcontainer for %s: %v", repo, err)
			return // Don't update tracked SHA if recreation failed so it retries next time
		}

		trackedSHAs[repo] = latestSHA
		log.Printf("Successfully updated devcontainer for %s", repo)
	} else {
		log.Printf("No new updates for %s", repo)
	}
}

func getLatestDevcontainerCommit(repo string) (string, error) {
	// Check .devcontainer updates
	shaDot, errDot := getLatestCommitForPath(repo, ".devcontainer")
	// Check devcontainer.json updates
	shaFile, errFile := getLatestCommitForPath(repo, "devcontainer.json")

	if errDot != nil && errFile != nil {
		// Log errors but we might just return empty strings instead of failing
		log.Printf("Warnings getting commits: .devcontainer(%v), devcontainer.json(%v)", errDot, errFile)
	}

	// We'll combine the SHAs as a composite key. If either changes, the composite changes.
	return fmt.Sprintf("%s-%s", shaDot, shaFile), nil
}

func getLatestCommitForPath(repo, path string) (string, error) {
	cmd := exec.Command("gh", "api", fmt.Sprintf("repos/%s/commits?path=%s&per_page=1", repo, path))
	output, err := cmd.Output()
	if err != nil {
		// gh api returns failure on 404s (e.g., path not found or repo not found)
		return "", err
	}

	var commits []Commit
	if err := json.Unmarshal(output, &commits); err != nil {
		return "", err
	}

	if len(commits) > 0 {
		return commits[0].Sha, nil
	}

	return "", nil
}

func recreateDevcontainer(repo string) error {
	log.Printf("Deleting previous devcontainer for %s...", repo)
	deleteCmd := exec.Command(devpodExe, "delete", fmt.Sprintf("github.com/%s", repo))
	deleteOut, _ := deleteCmd.CombinedOutput()
	log.Printf("%s delete output: %s", devpodExe, string(deleteOut))

	log.Printf("Creating new devcontainer for %s...", repo)
	upCmd := exec.Command(devpodExe, "up", fmt.Sprintf("github.com/%s", repo))
	upOut, err := upCmd.CombinedOutput()
	log.Printf("%s up output: %s", devpodExe, string(upOut))

	if err != nil {
		return fmt.Errorf("%s up failed: %w", devpodExe, err)
	}

	return nil
}

func bringUpDevcontainer(repo string) error {
	log.Printf("Bringing up devcontainer for %s...", repo)
	upCmd := exec.Command(devpodExe, "up", fmt.Sprintf("github.com/%s", repo))
	upOut, err := upCmd.CombinedOutput()
	log.Printf("%s up output: %s", devpodExe, string(upOut))

	if err != nil {
		return fmt.Errorf("%s up failed: %w", devpodExe, err)
	}

	return nil
}
