package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
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

var devpodExe = "devpod"

func init() {
	if _, err := exec.LookPath("devpod-cli"); err == nil {
		devpodExe = "devpod-cli"
	}
}

var commandRunner = func(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	return cmd.CombinedOutput()
}

var gitHubClientProvider = getGHClient

var (
	pollingInterval = 5 * time.Second
	pollingTimeout  = 5 * time.Minute
)

// We live dangerously
const (
	defaultIssueStartupCommand = `agy --dangerously-skip-permissions --prompt-interactive "Take a look at the status of this issue - if there's associated information in the ISSUE.md file, use that, otherwise just suggest a path forward for the issue. Do not undertake any implementation work"`
	defaultBranchRef           = ""
	DevpodLabelPrefix          = "sh.loft.devpod.workspace.id="
	VscLabelPrefix             = "dev.containers.id="
)

type config struct {
	once               bool
	containerList      string
	maxIssueContainers int
	startupCommand     string
}

func parseFlags(args []string) (*config, error) {
	fs := flag.NewFlagSet("devcontainer-manager", flag.ContinueOnError)
	once := fs.Bool("once", false, "Run once and exit")
	containerList := fs.String("container_list", "container.list.template", "The list of containers to run")
	maxIssueContainers := fs.Int("max_issue_containers", 5, "Maximum number of concurrent running issue containers")
	startupCommand := fs.String("startup_command", "", "Command to inject into the container's tmux session on startup")

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
	}, nil
}

func main() {
	cfg, err := parseFlags(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing flags: %v\n", err)
		os.Exit(1)
	}

	for {
		err := run(context.Background(), cfg)
		if err != nil {
			log.Printf("Error: %v", err)
		}

		if cfg.once {
			break
		}

		time.Sleep(time.Minute * 5)
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

	// Get running devcontainers
	out, err := commandRunner(devpodExe, "list")
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

	trackedSHAs := loadTrackedSHAs()
	trackedSHAsChanged := false
	client, clientErr := gitHubClientProvider()
	if clientErr != nil {
		log.Printf("Warning: failed to get GitHub client: %v. Change detection will be bypassed.", clientErr)
	}

	validIssueContainers := make(map[string]bool)
	for _, repo := range repos {
		parts := strings.Split(repo, "/")
		id := parts[len(parts)-1]
		log.Printf("DEBUG: repo is %s, client is nil? %v", repo, client == nil)
		if !running[id] {
			log.Printf("Starting devcontainer for %s", repo)
			out, err := commandRunner(devpodExe, "up", fmt.Sprintf("git@github.com:%s", repo), "--id", id, "--ide", "none")
			if err != nil {
				log.Printf("Failed to start devcontainer for %s: %v (output: %s)", repo, err, string(out))
			} else {
				if cfg.startupCommand != "" {
					wg.Add(1)
					go func(cid string) {
						defer wg.Done()
						if err := injectStartupCommand(ctx, cid, cfg.startupCommand); err != nil {
							log.Printf("ERROR: Failed to inject startup command for container %s: %v", cid, err)
						}
					}(id)
				}
				if client != nil {
					compositeSHA, found, err := getRepoCompositeSHA(ctx, client, repo, defaultBranchRef)
					if err == nil && found {
						updateAndSaveRepoSHA(repo, compositeSHA, trackedSHAs)
					} else if err != nil {
						log.Printf("Warning: failed to get composite SHA for %s: %v", repo, err)
					}
				}
				renameDockerContainer(id)
			}
		} else {
			if client != nil {
				compositeSHA, found, err := getRepoCompositeSHA(ctx, client, repo, defaultBranchRef)
				if err != nil {
					log.Printf("Warning: failed to get composite SHA for %s: %v", repo, err)
				} else if found {
					lastSeen, exists := trackedSHAs[repo]
					if !exists {
						log.Printf("Initial tracking established for %s at %s", repo, compositeSHA)
						updateAndSaveRepoSHA(repo, compositeSHA, trackedSHAs)
					} else if lastSeen != compositeSHA {
						log.Printf("Detected devcontainer configuration/script change in %s! Recreating container...", repo)
						log.Printf("Old tracking: %s", lastSeen)
						log.Printf("New tracking: %s", compositeSHA)

						err := recreateContainer(ctx, repo, id, cfg.startupCommand, &wg)
						if err != nil {
							log.Printf("Failed to recreate devcontainer for %s: %v", repo, err)
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
				opts := &github.IssueListByRepoOptions{State: "open"}
				issues, _, err := client.Issues.ListByRepo(ctx, owner, repoName, opts)
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

						issueNumber := issue.GetNumber()
						containerID := fmt.Sprintf("%s_%d", id, issueNumber)
						validIssueContainers[containerID] = true
						if !running[containerID] {
							log.Printf("Discovered new issue #%d labeled 'seraphine' in %s. Provisioning container...", issueNumber, repo)
							adjustIssueLabels(ctx, client, owner, repoName, issueNumber, "container-creating", []string{"container-ready", "container-failed"})

							slug, err := deriveFeatureSlug(ctx, issue.GetTitle())
							if err != nil {
								log.Printf("Failed to derive branch slug for issue %d: %v", issueNumber, err)
								adjustIssueLabels(ctx, client, owner, repoName, issueNumber, "container-failed", []string{"container-creating", "container-ready"})
								go reportStartupFailure(ctx, client, owner, repoName, "", issueNumber, err, "")
								continue
							}

							branchName := fmt.Sprintf("feature/%s_%d", slug, issueNumber)
							err = ensureIssueBranchExists(ctx, client, owner, repoName, branchName)
							if err != nil {
								log.Printf("Failed to ensure issue branch %s exists: %v", branchName, err)
								adjustIssueLabels(ctx, client, owner, repoName, issueNumber, "container-failed", []string{"container-creating", "container-ready"})
								go reportStartupFailure(ctx, client, owner, repoName, branchName, issueNumber, err, "")
								continue
							}

							repoURL := fmt.Sprintf("git@github.com:%s/%s@%s", owner, repoName, branchName)
							log.Printf("Launching issue container %s on branch %s", containerID, branchName)
							out, err := commandRunner(devpodExe, "up", repoURL, "--id", containerID, "--ide", "none")
							if err != nil {
								log.Printf("Failed to launch devcontainer for issue %d: %v (output: %s)", issueNumber, err, string(out))
								adjustIssueLabels(ctx, client, owner, repoName, issueNumber, "container-failed", []string{"container-creating", "container-ready"})
								go reportStartupFailure(ctx, client, owner, repoName, branchName, issueNumber, err, string(out))
							} else {
								running[containerID] = true
								adjustIssueLabels(ctx, client, owner, repoName, issueNumber, "container-ready", []string{"container-creating", "container-failed"})
								renameDockerContainer(containerID)
								cmdToInject := cfg.startupCommand
								if cmdToInject == "" {
									cmdToInject = defaultIssueStartupCommand
								}
								wg.Add(1)
								go func(cid string) {
									defer wg.Done()
									if err := injectStartupCommand(ctx, cid, cmdToInject); err != nil {
										log.Printf("ERROR: Failed to inject startup command for container %s: %v", cid, err)
									}
								}(containerID)

								compositeSHA, found, err := getRepoCompositeSHA(ctx, client, repo, branchName)
								if err == nil && found {
									updateAndSaveRepoSHA(containerID, compositeSHA, trackedSHAs)
								} else if err != nil {
									log.Printf("Warning: failed to get composite SHA for issue container %s on branch %s: %v", containerID, branchName, err)
								}
							}
						} else {
							slug, err := deriveFeatureSlug(ctx, issue.GetTitle())
							if err != nil {
								log.Printf("Failed to derive branch slug for issue %d: %v", issueNumber, err)
								continue
							}

							branchName := fmt.Sprintf("feature/%s_%d", slug, issueNumber)
							compositeSHA, found, err := getRepoCompositeSHA(ctx, client, repo, branchName)
							if err != nil {
								log.Printf("Warning: failed to get composite SHA for issue container %s: %v", containerID, err)
							} else if found {
								lastSeen, exists := trackedSHAs[containerID]
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
	}

	// Resource Management & Cleanup Logic for issue containers
	if client != nil {
		projectRepoMap := make(map[string]string)
		for _, repo := range repos {
			parts := strings.Split(repo, "/")
			pID := parts[len(parts)-1]
			projectRepoMap[pID] = repo
		}

		outList, errList := commandRunner(devpodExe, "list")
		if errList == nil {
			containerStates := make(map[string]string)
			for _, line := range strings.Split(string(outList), "\n") {
				fields := strings.Fields(line)
				if len(fields) > 1 {
					containerStates[fields[0]] = fields[1]
				}
			}

			// 1. Hibernation Logic
			type issueContainer struct {
				id        string
				updatedAt time.Time
			}
			var runningIssues []issueContainer

			for id, state := range containerStates {
				if state == "Running" {
					lastIdx := strings.LastIndex(id, "_")
					if lastIdx != -1 {
						projectID := id[:lastIdx]
						issueNumber, errNum := strconv.Atoi(id[lastIdx+1:])
						repo := projectRepoMap[projectID]
						if errNum == nil && repo != "" {
							partsRepo := strings.Split(repo, "/")
							if len(partsRepo) == 2 {
								owner, repoName := partsRepo[0], partsRepo[1]
								issue, _, errGet := client.Issues.Get(ctx, owner, repoName, issueNumber)
								if errGet == nil {
									runningIssues = append(runningIssues, issueContainer{
										id:        id,
										updatedAt: issue.GetUpdatedAt().Time,
									})
								}
							}
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
					log.Printf("Hibernating oldest running issue container: %s", runningIssues[i].id)
					_, errStop := commandRunner(devpodExe, "stop", runningIssues[i].id)
					if errStop != nil {
						log.Printf("Warning: failed to stop container %s during hibernation: %v", runningIssues[i].id, errStop)
					}
				}
			}

			// 2. Cleanup Logic
			for id := range containerStates {
				lastIdx := strings.LastIndex(id, "_")
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
									} else {
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
									log.Printf("Cleaning up finished/unlabeled issue container: %s", id)
									_, errStop := commandRunner(devpodExe, "stop", id)
									if errStop != nil {
										log.Printf("Warning: failed to stop container %s during cleanup: %v", id, errStop)
									}
									_, errDelete := commandRunner(devpodExe, "delete", id)
									if errDelete != nil {
										log.Printf("Failed to delete container %s during cleanup: %v", id, errDelete)
									} else {
										if _, exists := trackedSHAs[id]; exists {
											delete(trackedSHAs, id)
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
	listOut, listErr := commandRunner(devpodExe, "list")
	if listErr == nil {
		validProjectNames := make(map[string]bool)
		for _, r := range repos {
			rParts := strings.Split(r, "/")
			validProjectNames[rParts[len(rParts)-1]] = true
		}

		for _, line := range strings.Split(string(listOut), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "-") || strings.Contains(line, "NAME") {
				continue
			}
			parts := strings.Split(line, "|")
			if len(parts) >= 2 {
				cName := strings.TrimSpace(parts[0])
				cSource := strings.TrimSpace(parts[1])
				if cName != "" {
					// Check (b): Uses HTTP source
					isHTTPSource := strings.Contains(cSource, "https://github.com/") || strings.Contains(cSource, "http://") || strings.Contains(cSource, "https://")

					// Check (a): Not in the container list (accounting for issues)
					inList := validProjectNames[cName] || validIssueContainers[cName]

					if !inList || isHTTPSource {
						log.Printf("Cleaning up container %s (inList: %v, isHTTPSource: %v)", cName, inList, isHTTPSource)
						errStop := stopContainer(cName)
						if errStop != nil {
							log.Printf("Warning: failed to stop container %s during extra cleanup: %v", cName, errStop)
						}
						errDel := deleteContainer(cName)
						if errDel != nil {
							log.Printf("Warning: failed to delete container %s during extra cleanup: %v", cName, errDel)
						} else {
							if _, exists := trackedSHAs[cName]; exists {
								delete(trackedSHAs, cName)
								trackedSHAsChanged = true
							}
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


func stopContainer(id string) error {
	stopOut, err := commandRunner(devpodExe, "stop", id)
	if err != nil {
		return fmt.Errorf("%s stop failed: %w (output: %s)", devpodExe, err, string(stopOut))
	}

	log.Printf("Successfully stopped devcontainer %s", id)
	return nil
}

func deleteContainer(id string) error {
	deleteOut, err := commandRunner(devpodExe, "delete", id)
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
	trackerPath := filepath.Join(getConfigDir(), "tracked_shas.json")
	bytes, err := json.MarshalIndent(shas, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(trackerPath, bytes, 0644)
}

func updateAndSaveRepoSHA(repo string, sha string, trackedSHAs map[string]string) {
	trackedSHAs[repo] = sha
	if err := saveTrackedSHAs(trackedSHAs); err != nil {
		log.Printf("Warning: failed to save tracked SHAs: %v", err)
	}
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
	log.Printf("Recreating devcontainer for %s...", repo)
	if err := deleteContainer(id); err != nil {
		log.Printf("Warning: failed to delete container %s before recreating: %v", id, err)
	}
	out, err := commandRunner(devpodExe, "up", fmt.Sprintf("git@github.com:%s", repo), "--id", id, "--ide", "none")
	if err != nil {
		return fmt.Errorf("%s up failed: %w (output: %s)", devpodExe, err, string(out))
	}
	log.Printf("Successfully recreated devcontainer for %s", repo)
	renameDockerContainer(id)
	if startupCmd != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := injectStartupCommand(ctx, id, startupCmd); err != nil {
				log.Printf("ERROR: Failed to inject startup command for container %s: %v", id, err)
			}
		}()
	}
	return nil
}

func recreateIssueContainer(ctx context.Context, owner, repoName, branchName, containerID, startupCmd string, wg *sync.WaitGroup) error {
	log.Printf("Recreating devcontainer for issue container %s on branch %s...", containerID, branchName)
	if err := deleteContainer(containerID); err != nil {
		log.Printf("Warning: failed to delete container %s before recreating: %v", containerID, err)
	}
	repoURL := fmt.Sprintf("git@github.com:%s/%s@%s", owner, repoName, branchName)
	out, err := commandRunner(devpodExe, "up", repoURL, "--id", containerID, "--ide", "none")
	if err != nil {
		return fmt.Errorf("%s up failed: %w (output: %s)", devpodExe, err, string(out))
	}
	log.Printf("Successfully recreated devcontainer for issue container %s", containerID)
	renameDockerContainer(containerID)

	cmdToInject := startupCmd
	if cmdToInject == "" {
		cmdToInject = defaultIssueStartupCommand
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := injectStartupCommand(ctx, containerID, cmdToInject); err != nil {
			log.Printf("ERROR: Failed to inject startup command for container %s: %v", containerID, err)
		}
	}()
	return nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func injectStartupCommand(ctx context.Context, id string, startupCmd string) error {
	if startupCmd == "" {
		return nil
	}
	log.Printf("Starting tmux readiness polling for container %s", id)

	ticker := time.NewTicker(pollingInterval)
	defer ticker.Stop()

	timeout := time.After(pollingTimeout)

	for {
		select {
		case <-ctx.Done():
			log.Printf("Context cancelled while waiting for container %s", id)
			return fmt.Errorf("context cancelled while waiting for container %s", id)
		case <-timeout:
			log.Printf("Timeout reached waiting for container %s tmux session to be ready", id)
			return fmt.Errorf("timeout reached waiting for container %s tmux session to be ready", id)
		case <-ticker.C:
			out, err := commandRunner(devpodExe, "ssh", id, "--command", fmt.Sprintf("tmux has-session -t %s", id))
			if err == nil {
				log.Printf("Container %s tmux session is ready. Injecting startup command...", id)
				injectOut, injectErr := commandRunner(devpodExe, "ssh", id, "--command", fmt.Sprintf("tmux send-keys -t %s %s C-m", id, shellQuote(startupCmd)))
				if injectErr != nil {
					log.Printf("Failed to inject startup command for %s: %v (output: %s)", id, injectErr, string(injectOut))
					return fmt.Errorf("failed to inject startup command: %w (output: %s)", injectErr, string(injectOut))
				}
				log.Printf("Successfully injected startup command for %s", id)
				return nil
			}
			log.Printf("Polling container %s: tmux session not ready yet: %v (output: %s)", id, err, strings.TrimSpace(string(out)))
		}
	}
}

type slugDeriver struct {
	runAgy func(ctx context.Context, prompt string) ([]byte, error)
}

func (sd *slugDeriver) derive(ctx context.Context, title string) (string, error) {
	prompt := fmt.Sprintf("Given the GitHub issue title: '%s', generate a 3-word slug summarizing the feature. Output exactly three lowercase words separated by underscores, with no other text, punctuation, or explanation.", title)
	output, err := sd.runAgy(ctx, prompt)
	if err != nil {
		return "", err
	}

	slug := strings.TrimSpace(string(output))
	slug = strings.ToLower(slug)

	var sb strings.Builder
	for _, r := range slug {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			sb.WriteRune(r)
		}
	}
	cleaned := sb.String()

	parts := strings.Split(cleaned, "_")
	var words []string
	for _, part := range parts {
		if part != "" {
			words = append(words, part)
		}
	}

	if len(words) != 3 {
		return "", fmt.Errorf("derived slug %q is invalid: must have exactly 3 words, got %d", cleaned, len(words))
	}

	return strings.Join(words, "_"), nil
}

var defaultDeriver = &slugDeriver{
	runAgy: func(ctx context.Context, prompt string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "agy", "--prompt", prompt)
		return cmd.Output()
	},
}

func deriveFeatureSlug(ctx context.Context, title string) (string, error) {
	return defaultDeriver.derive(ctx, title)
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

func adjustIssueLabels(ctx context.Context, client *github.Client, owner, repo string, issueNumber int, addLabel string, removeLabels []string) {
	if client == nil {
		return
	}
	// Fetch the issue first to get current labels and avoid redundant API calls
	issue, _, err := client.Issues.Get(ctx, owner, repo, issueNumber)
	if err != nil {
		log.Printf("Warning: failed to fetch issue %d for label adjustment: %v", issueNumber, err)
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
		_, err := client.Issues.RemoveLabelForIssue(ctx, owner, repo, issueNumber, r)
		if err != nil {
			log.Printf("Warning: failed to remove label %s from issue %d: %v", r, issueNumber, err)
		}
	}

	// Add the new label if not present
	if !hasAddLabel && addLabel != "" {
		_, _, err := client.Issues.AddLabelsToIssue(ctx, owner, repo, issueNumber, []string{addLabel})
		if err != nil {
			log.Printf("Warning: failed to add label %s to issue %d: %v", addLabel, issueNumber, err)
		}
	}
}

func renameDockerContainer(containerID string) {
	log.Printf("Attempting to rename docker container to %s...", containerID)

	out, err := commandRunner("docker", "ps", "--format", "{{.ID}}|{{.Names}}|{{.Image}}|{{.Labels}}")
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
		if len(parts) >= 4 {
			id, name, labels := parts[0], parts[1], parts[3]
			if strings.Contains(labels, fmt.Sprintf("%s%s", DevpodLabelPrefix, containerID)) ||
				strings.Contains(labels, fmt.Sprintf("%s%s", VscLabelPrefix, containerID)) {
				targetID, currentName = id, name
				break
			}
		}
	}

	if targetID == "" {
		for _, line := range lines {
			if line == "" {
				continue
			}
			parts := strings.Split(line, "|")
			if len(parts) >= 4 {
				id, name, labels := parts[0], parts[1], parts[3]
				if name == containerID {
					// Only use name match if it doesn't explicitly belong to another workspace
					if !strings.Contains(labels, DevpodLabelPrefix) && !strings.Contains(labels, VscLabelPrefix) {
						targetID, currentName = id, name
						break
					}
				}
			}
		}
	}

	if targetID == "" {
		for _, line := range lines {
			if line == "" {
				continue
			}
			parts := strings.Split(line, "|")
			if len(parts) >= 4 {
				id, name, image, labels := parts[0], parts[1], parts[2], parts[3]
				if strings.Contains(labels, DevpodLabelPrefix) || strings.Contains(labels, VscLabelPrefix) {
					continue
				}
				if strings.Contains(image, "devpod-") && name != containerID {
					targetID, currentName = id, name
					break
				}
			}
		}
	}

	if targetID == "" {
		for _, line := range lines {
			if line == "" {
				continue
			}
			parts := strings.Split(line, "|")
			if len(parts) >= 4 {
				id, name, image, labels := parts[0], parts[1], parts[2], parts[3]
				if strings.Contains(labels, DevpodLabelPrefix) || strings.Contains(labels, VscLabelPrefix) {
					continue
				}
				if strings.Contains(image, "vsc-content") && name != containerID {
					targetID, currentName = id, name
					break
				}
			}
		}
	}

	if targetID != "" {
		if currentName == containerID {
			log.Printf("Container %s is already named %s", targetID, containerID)
			return
		}

		log.Printf("Renaming container %s (currently %s) to %s", targetID, currentName, containerID)
		if _, err := commandRunner("docker", "rename", targetID, containerID); err != nil {
			log.Printf("Failed to rename container: %v", err)
		} else {
			log.Printf("Successfully renamed container to %s", containerID)
		}
	} else {
		log.Printf("Could not identify which container to rename for %s", containerID)
	}
}

// No-op change to trigger CI for issue 98

func reportStartupFailure(ctx context.Context, client *github.Client, owner, repo, branch string, originalIssueNum int, startupErr error, outputLog string) {
	if client == nil {
		return
	}

	opts := &github.IssueListByRepoOptions{
		State: "open",
	}
	issues, _, err := client.Issues.ListByRepo(ctx, owner, repo, opts)
	if err != nil {
		log.Printf("Warning: failed to list issues for %s/%s during startup failure reporting: %v", owner, repo, err)
		return
	}

	for _, issue := range issues {
		if issue.GetTitle() == "Issue Container Startup Failed" {
			log.Printf("An open issue 'Issue Container Startup Failed' already exists in %s/%s. Skipping creation.", owner, repo)
			return
		}
	}

	var bodyBuilder strings.Builder
	bodyBuilder.WriteString("### Devcontainer Startup Failure Report\n\n")
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

	req := &github.IssueRequest{
		Title:  github.String("Issue Container Startup Failed"),
		Body:   github.String(bodyBuilder.String()),
		Labels: &[]string{"seraphine-bug"},
	}

	_, _, err = client.Issues.Create(ctx, owner, repo, req)
	if err != nil {
		log.Printf("Warning: failed to create startup failure issue in %s/%s: %v", owner, repo, err)
	}
}


