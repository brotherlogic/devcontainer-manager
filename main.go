package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
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

	trackedSHAs := loadTrackedSHAs()
	client, clientErr := getGHClient()
	if clientErr != nil {
		log.Printf("Warning: failed to get GitHub client: %v. Change detection will be bypassed.", clientErr)
	}

	for _, repo := range repos {
		id := strings.ReplaceAll(repo, "/", "-")
		if !running[id] {
			log.Printf("Starting devcontainer for %s", repo)
			cmd := exec.Command("devpod", "up", fmt.Sprintf("https://github.com/%s", repo), "--id", id, "--detach")
			out, err := cmd.CombinedOutput()
			if err != nil {
				log.Printf("Failed to start devcontainer for %s: %v (output: %s)", repo, err, string(out))
			} else {
				if client != nil {
					compositeSHA, err := getRepoCompositeSHA(ctx, client, repo)
					if err == nil {
						trackedSHAs[repo] = compositeSHA
						saveTrackedSHAs(trackedSHAs)
					} else {
						log.Printf("Warning: failed to get composite SHA for %s: %v", repo, err)
					}
				}
			}
		} else {
			if client != nil {
				compositeSHA, err := getRepoCompositeSHA(ctx, client, repo)
				if err != nil {
					log.Printf("Warning: failed to get composite SHA for %s: %v", repo, err)
					continue
				}

				lastSeen, exists := trackedSHAs[repo]
				if !exists {
					log.Printf("Initial tracking established for %s at %s", repo, compositeSHA)
					trackedSHAs[repo] = compositeSHA
					saveTrackedSHAs(trackedSHAs)
				} else if lastSeen != compositeSHA {
					log.Printf("Detected devcontainer configuration/script change in %s! Recreating container...", repo)
					log.Printf("Old tracking: %s", lastSeen)
					log.Printf("New tracking: %s", compositeSHA)

					err := recreateContainer(repo, id)
					if err != nil {
						log.Printf("Failed to recreate devcontainer for %s: %v", repo, err)
					} else {
						trackedSHAs[repo] = compositeSHA
						saveTrackedSHAs(trackedSHAs)
					}
				}
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

func getConfigDir() string {
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
	if err == nil {
		json.Unmarshal(bytes, &shas)
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
	r := strings.NewReplacer("&&", " ", "||", " ", ";", " ", "|", " ", "\"", " ", "'", " ")
	cmdCleaned := r.Replace(cmd)
	return strings.Fields(cmdCleaned)
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
		"python": true, "pip": true, "npm": true, "yarn": true, "node": true, "echo": true,
		"cat": true, "mkdir": true, "cd": true, "rm": true, "mv": true, "cp": true,
		"chmod": true, "chown": true, "env": true, "export": true, "git": true, "make": true,
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
	if (strings.HasPrefix(token, "./") || strings.Contains(token, "/")) && !strings.Contains(token, "://") {
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
	scriptKeys := []string{
		"initializeCommand",
		"onCreateCommand",
		"updateContentCommand",
		"postCreateCommand",
		"postStartCommand",
		"postAttachCommand",
	}

	for _, key := range scriptKeys {
		re := regexp.MustCompile(fmt.Sprintf(`"%s"\s*:\s*"([^"]+)"`, key))
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

func fetchFileContent(ctx context.Context, client *github.Client, owner, repo, path string) (string, string, error) {
	fileContent, _, _, err := client.Repositories.GetContents(ctx, owner, repo, path, nil)
	if err != nil {
		return "", "", err
	}
	content, err := fileContent.GetContent()
	if err != nil {
		return "", "", err
	}
	return content, fileContent.GetSHA(), nil
}

func getFileSHA(ctx context.Context, client *github.Client, owner, repoName, path string) (string, bool) {
	fileContent, _, resp, err := client.Repositories.GetContents(ctx, owner, repoName, path, nil)
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

func getRepoCompositeSHA(ctx context.Context, client *github.Client, repo string) (string, error) {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 {
		return "", fmt.Errorf("invalid repository format: %s", repo)
	}
	owner, repoName := parts[0], parts[1]

	var devcontainerJSON string
	var devcontainerSHA string
	var found bool

	content, sha, err := fetchFileContent(ctx, client, owner, repoName, ".devcontainer/devcontainer.json")
	if err == nil {
		devcontainerJSON = content
		devcontainerSHA = sha
		found = true
	} else {
		content, sha, err = fetchFileContent(ctx, client, owner, repoName, "devcontainer.json")
		if err == nil {
			devcontainerJSON = content
			devcontainerSHA = sha
			found = true
		}
	}

	if !found {
		return "no-devcontainer", nil
	}

	shaMap := map[string]string{
		"devcontainer.json": devcontainerSHA,
	}

	scripts := extractScriptsFromJSON(devcontainerJSON)
	for _, scriptPath := range scripts {
		if sha, ok := getFileSHA(ctx, client, owner, repoName, scriptPath); ok {
			shaMap[scriptPath] = sha
		} else {
			fallbackPath := path.Join(".devcontainer", scriptPath)
			if sha, ok := getFileSHA(ctx, client, owner, repoName, fallbackPath); ok {
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

	return strings.Join(partsSHA, "|"), nil
}

func recreateContainer(repo string, id string) error {
	log.Printf("Recreating devcontainer for %s...", repo)
	if err := deleteContainer(id); err != nil {
		log.Printf("Warning: failed to delete container %s before recreating: %v", id, err)
	}
	cmd := exec.Command("devpod", "up", fmt.Sprintf("https://github.com/%s", repo), "--id", id, "--detach")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("devpod up failed: %w (output: %s)", err, string(out))
	}
	log.Printf("Successfully recreated devcontainer for %s", repo)
	return nil
}
