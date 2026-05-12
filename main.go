package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	DevpodLabelPrefix = "sh.loft.devpod.workspace.id="
	VscLabelPrefix    = "dev.containers.id="
	DockerRunArgs     = "DOCKER_RUN_ARGS"
)

var (
	devpodExe string
	ideChoice string
)

func init() {
	flag.StringVar(&devpodExe, "devpod-exe", "devpod-cli", "The executable name for devpod")
	flag.StringVar(&ideChoice, "ide", "none", "The IDE to use for devcontainers")
}

type Commit struct {
	Sha string `json:"sha"`
}

type PortMapping struct {
	Containers map[string]ContainerMapping `json:"containers"`
}

type ContainerMapping struct {
	HostPort int `json:"hostPort"`
}

type Workspace struct {
	ID     string `json:"id"`  // Human-readable ID (project-name)
	UID    string `json:"uid"` // Devpod internal unique ID (matches dev.containers.id label)
	Source Source `json:"source"`
}

type Source struct {
	GitRepository string `json:"gitRepository"`
}

func main() {
	flag.Parse()
	log.Printf("Starting devcontainer manager (version: %s)...", getVersion())

	if err := checkGHAuth(); err != nil {
		notifyFatal("GitHub CLI authentication failed: %v", err)
	}

	if err := checkDevPodProvider(); err != nil {
		notifyFatal("DevPod provider configuration failed: %v", err)
	}

	// Track the last seen commit for each repo
	trackedSHAs := loadTrackedSHAs()

	// Load port mappings
	mappings := loadPortMappings()

	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	// Run initial check immediately
	checkRepos(trackedSHAs, mappings)

	for range ticker.C {
		checkRepos(trackedSHAs, mappings)
	}
}

func loadPortMappings() PortMapping {
	mappingPath := filepath.Join(getConfigDir(), "mappings.json")
	var mappings PortMapping
	mappings.Containers = make(map[string]ContainerMapping)

	bytes, err := os.ReadFile(mappingPath)
	if err == nil {
		json.Unmarshal(bytes, &mappings)
	}
	return mappings
}

func saveAndPushMappings(mappings PortMapping) error {
	mappingPath := filepath.Join(getConfigDir(), "mappings.json")
	bytes, err := json.MarshalIndent(mappings, "", "  ")
	if err != nil {
		return err
	}
	err = os.WriteFile(mappingPath, bytes, 0644)
	if err != nil {
		return err
	}

	// Ensure we have a deploy key and a repo to push from
	keyPath, err := ensureDeployKey()
	if err != nil {
		return fmt.Errorf("failed to ensure deploy key: %w", err)
	}

	repoPath, err := setupPushRepo()
	if err != nil {
		return fmt.Errorf("failed to setup push repo: %w", err)
	}

	// Copy mappings.json to the repo
	repoMappingPath := filepath.Join(repoPath, "mappings.json")
	if err := os.WriteFile(repoMappingPath, bytes, 0644); err != nil {
		return fmt.Errorf("failed to copy mappings to repo: %w", err)
	}

	// Commit and push
	log.Printf("Pushing updated mappings to GitHub using deploy key...")
	
	// Add and commit
	commands := [][]string{
		{"git", "add", "mappings.json"},
		{"git", "commit", "-m", "Update port mappings [cli-skip]"},
	}

	for _, args := range commands {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = repoPath
		if out, err := cmd.CombinedOutput(); err != nil {
			// Ignore if there's nothing to commit
			if !strings.Contains(string(out), "nothing to commit") {
				return fmt.Errorf("git command %v failed: %w (output: %s)", args, err, string(out))
			}
		}
	}

	// Push
	sshCmd := fmt.Sprintf("ssh -i %s -o StrictHostKeyChecking=no", keyPath)
	pushCmd := exec.Command("git", "push", "origin", "main")
	pushCmd.Dir = repoPath
	pushCmd.Env = append(os.Environ(), fmt.Sprintf("GIT_SSH_COMMAND=%s", sshCmd))
	
	if out, err := pushCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to push mappings to GitHub: %w (output: %s)", err, string(out))
	}

	return nil
}

func ensureDeployKey() (string, error) {
	configDir := getConfigDir()
	keyPath := filepath.Join(configDir, "deploy_key")
	pubPath := keyPath + ".pub"

	if _, err := os.Stat(keyPath); os.IsNotExist(err) {
		log.Printf("Generating new deploy key...")
		cmd := exec.Command("ssh-keygen", "-t", "ed25519", "-C", "devcontainer-manager-deploy-key", "-N", "", "-f", keyPath)
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("failed to generate ssh key: %w (output: %s)", err, string(out))
		}

		log.Printf("Registering deploy key with GitHub...")
		regCmd := exec.Command("gh", "repo", "deploy-key", "add", pubPath, "-w", "-t", "devcontainer-manager-deploy-key")
		if out, err := regCmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("failed to register deploy key: %w (output: %s)", err, string(out))
		}
	}

	return keyPath, nil
}

func setupPushRepo() (string, error) {
	configDir := getConfigDir()
	repoPath := filepath.Join(configDir, "push-repo")

	if _, err := os.Stat(filepath.Join(repoPath, ".git")); os.IsNotExist(err) {
		log.Printf("Setting up push repository...")
		os.MkdirAll(repoPath, 0755)

		// Initialize and add remote
		cmds := [][]string{
			{"git", "init"},
			{"git", "remote", "add", "origin", "git@github.com:brotherlogic/devcontainer-manager.git"},
			{"git", "config", "user.email", "devcontainer-manager@brotherlogic-systems.com"},
			{"git", "config", "user.name", "Devcontainer Manager"},
			{"git", "fetch", "origin", "main"},
			{"git", "checkout", "main"},
		}

		keyPath := filepath.Join(configDir, "deploy_key")
		sshCmd := fmt.Sprintf("ssh -i %s -o StrictHostKeyChecking=no", keyPath)

		for _, args := range cmds {
			cmd := exec.Command(args[0], args[1:]...)
			cmd.Dir = repoPath
			if args[0] == "git" && (args[1] == "fetch" || args[1] == "push") {
				cmd.Env = append(os.Environ(), fmt.Sprintf("GIT_SSH_COMMAND=%s", sshCmd))
			}
			if out, err := cmd.CombinedOutput(); err != nil {
				return "", fmt.Errorf("git command %v failed: %w (output: %s)", args, err, string(out))
			}
		}
	}

	return repoPath, nil
}

func allocatePort(projectName string, mappings *PortMapping) int {
	if m, ok := mappings.Containers[projectName]; ok {
		return m.HostPort
	}

	maxPort := 2221
	for _, m := range mappings.Containers {
		if m.HostPort > maxPort {
			maxPort = m.HostPort
		}
	}

	newPort := maxPort + 1
	mappings.Containers[projectName] = ContainerMapping{HostPort: newPort}
	return newPort
}

func getVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}

	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" {
			// Found the git SHA
			if len(setting.Value) > 7 {
				return setting.Value[:7]
			}
			return setting.Value
		}
	}

	return "dev"
}

func checkGHAuth() error {
	cmd := exec.Command("gh", "auth", "status")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("GitHub CLI authentication failed: %w (details: %s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func checkDevPodProvider() error {
	cmd := exec.Command(devpodExe, "provider", "list", "--output", "json")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to list devpod providers: %w (output: %s)", err, strings.TrimSpace(string(output)))
	}

	var providers map[string]interface{}
	if err := json.Unmarshal(output, &providers); err != nil {
		return fmt.Errorf("failed to parse devpod providers: %w", err)
	}

	if len(providers) == 0 {
		return fmt.Errorf("no DevPod provider found. Please add a provider (e.g., '%s provider add docker')", devpodExe)
	}

	return nil
}

func checkRepos(trackedSHAs map[string]string, mappings PortMapping) {
	configPath := filepath.Join(getConfigDir(), "container.list")

	log.Printf("Syncing container list from remote template...")
	if err := pullTemplateFromGitHub(configPath); err != nil {
		log.Printf("Warning: failed to sync container.list from template: %v", err)
		// We'll continue with the local container.list if it exists
	}

	repos, err := readContainerList()
	if err != nil {
		notifyError("Error reading container.list: %v", err)
		return
	}

	currentWorkspaces, err := getExistingWorkspaces()
	if err != nil {
		log.Printf("Error: failed to get existing devpod workspaces: %v. Skipping sync interval for safety.", err)
		return
	}

	sortReposByLastUpdated(repos)

	currentRepos := make(map[string]bool)
	for _, repo := range repos {
		currentRepos[repo] = true
	}

	// Deleting workspaces that are not in the template anymore
	// Pre-calculate mapping of IDs to repos in template for faster lookup
	templateRepoIDs := make(map[string]string)
	for _, repo := range repos {
		templateRepoIDs[filepath.Base(repo)] = repo
	}

	dirtyMappings := false
	for id := range currentWorkspaces {
		if _, exists := templateRepoIDs[id]; !exists {
			log.Printf("Workspace %s is no longer in the template. Deleting...", id)
			if err := deleteDevcontainerByID(id); err != nil {
				log.Printf("Failed to delete workspace %s: %v", id, err)
			} else {
				// Efficiently remove from trackedSHAs
				for repo := range trackedSHAs {
					if filepath.Base(repo) == id {
						delete(trackedSHAs, repo)
					}
				}
				saveTrackedSHAs(trackedSHAs)

				// Remove from mappings
				if _, ok := mappings.Containers[id]; ok {
					delete(mappings.Containers, id)
					dirtyMappings = true
				}
			}
		}
	}

	for _, repo := range repos {
		if checkRepo(repo, trackedSHAs, currentWorkspaces, &mappings) {
			dirtyMappings = true
		}
	}

	if dirtyMappings {
		if err := saveAndPushMappings(mappings); err != nil {
			log.Printf("Warning: failed to push mappings: %v", err)
		}
	}
}

func readContainerList() ([]string, error) {
	configPath := filepath.Join(getConfigDir(), "container.list")
	file, err := os.Open(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("%s not found, pulling template from GitHub...", configPath)
			if pullErr := pullTemplateFromGitHub(configPath); pullErr != nil {
				return nil, fmt.Errorf("failed to pull template: %w", pullErr)
			}
			file, err = os.Open(configPath)
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

func pullTemplateFromGitHub(configPath string) error {
	cmd := exec.Command("gh", "api", "repos/brotherlogic/devcontainer-manager/contents/container.list.template", "-H", "Accept: application/vnd.github.v3.raw")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh api error: %w (output: %s)", err, strings.TrimSpace(string(output)))
	}
	if len(output) == 0 {
		return fmt.Errorf("empty response from GitHub template API")
	}
	return os.WriteFile(configPath, output, 0644)
}

func getExistingWorkspaces() (map[string]Workspace, error) {
	cmd := exec.Command(devpodExe, "list", "--output", "json")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to list devpod workspaces: %w (output: %s)", err, strings.TrimSpace(string(output)))
	}

	var workspaces []Workspace
	if err := json.Unmarshal(output, &workspaces); err != nil {
		return nil, fmt.Errorf("failed to parse devpod workspaces json: %w", err)
	}

	wsMap := make(map[string]Workspace)
	for _, ws := range workspaces {
		if _, exists := wsMap[ws.ID]; exists {
			log.Printf("Warning: duplicate workspace ID '%s' found in devpod list. Overwriting for represented mapping.", ws.ID)
		}
		wsMap[ws.ID] = ws
	}
	return wsMap, nil
}

func getConfigDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	configDir := filepath.Join(home, ".config", "devcontainer-manager")
	os.MkdirAll(configDir, 0755)
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

func checkRepo(repo string, trackedSHAs map[string]string, currentWorkspaces map[string]Workspace, mappings *PortMapping) bool {
	log.Printf("Checking %s for devcontainer updates...", repo)

	latestSHA, err := getLatestDevcontainerCommit(repo)
	if err != nil {
		notifyError("Error checking commits for %s: %v", repo, err)
		return false
	}

	if latestSHA == "-" {
		log.Printf("No devcontainer configuration found or no commits for %s", repo)
		return false
	}

	projectName := filepath.Base(repo)
	initialMappingsCount := len(mappings.Containers)
	port := allocatePort(projectName, mappings)
	dirty := len(mappings.Containers) > initialMappingsCount

	lastSeen, exists := trackedSHAs[repo]
	if !exists {
		log.Printf("Initial tracking (or cache invalidated) for %s at commit state %s. Recreating container...", repo, latestSHA)
		if err := recreateDevcontainer(repo, currentWorkspaces, port); err != nil {
			notifyError("Failed to recreate devcontainer for %s: %v", repo, err)
			return dirty
		}
		trackedSHAs[repo] = latestSHA
		saveTrackedSHAs(trackedSHAs)
		return dirty
	}

	if lastSeen != latestSHA {
		log.Printf("Detected devcontainer change in %s! Updating from %s to %s", repo, lastSeen, latestSHA)

		err := recreateDevcontainer(repo, currentWorkspaces, port)
		if err != nil {
			notifyError("Failed to recreate devcontainer for %s: %v", repo, err)
			return dirty // Don't update tracked SHA if recreation failed so it retries next time
		}

		trackedSHAs[repo] = latestSHA
		saveTrackedSHAs(trackedSHAs)
		log.Printf("Successfully updated devcontainer for %s", repo)
	} else {
		if !isContainerRunning(projectName, currentWorkspaces) {
			log.Printf("No new updates for %s, but container is not running. Bringing it up...", repo)
			if err := bringUpDevcontainer(repo, currentWorkspaces, port); err != nil {
				notifyError("Failed to bring up devcontainer for %s: %v", repo, err)
				return dirty
			}
		} else {
			log.Printf("No new updates for %s, and container is running", repo)
		}
	}

	return dirty
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

func recreateDevcontainer(repo string, currentWorkspaces map[string]Workspace, port int) error {
	projectName := filepath.Base(repo)

	if err := deleteDevcontainer(repo); err != nil {
		log.Printf("Warning: delete error ignored during recreation for %s: %v", repo, err)
	}

	log.Printf("Creating new devcontainer for %s with id %s on port %d...", repo, projectName, port)
	upCmd := exec.Command(devpodExe, "up", fmt.Sprintf("git@github.com:%s.git", repo), "--id", projectName, "--reset", "--ide", ideChoice, "--provider-option", fmt.Sprintf("%s=-p %d:22", DockerRunArgs, port))
	log.Printf("Running command: %s", strings.Join(upCmd.Args, " "))
	upOut, err := upCmd.CombinedOutput()
	log.Printf("%s up output: %s", devpodExe, string(upOut))

	if err != nil {
		return fmt.Errorf("%s up failed: %w", devpodExe, err)
	}

	// Refresh workspaces to get the new UID if it changed (it shouldn't for the same ID, but safe)
	newWorkspaces, err := getExistingWorkspaces()
	if err != nil {
		return fmt.Errorf("failed to refresh workspaces after up: %w", err)
	}
	renameDockerContainer(projectName, newWorkspaces)

	return nil
}

func bringUpDevcontainer(repo string, currentWorkspaces map[string]Workspace, port int) error {
	projectName := filepath.Base(repo)

	log.Printf("Bringing up devcontainer for %s with id %s on port %d...", repo, projectName, port)
	upCmd := exec.Command(devpodExe, "up", fmt.Sprintf("git@github.com:%s.git", repo), "--id", projectName, "--ide", ideChoice, "--provider-option", fmt.Sprintf("%s=-p %d:22", DockerRunArgs, port))
	log.Printf("Running command: %s", strings.Join(upCmd.Args, " "))
	upOut, err := upCmd.CombinedOutput()
	log.Printf("%s up output: %s", devpodExe, string(upOut))

	if err != nil {
		return fmt.Errorf("%s up failed: %w", devpodExe, err)
	}

	// Refresh workspaces to get the new UID
	newWorkspaces, err := getExistingWorkspaces()
	if err != nil {
		return fmt.Errorf("failed to refresh workspaces after up: %w", err)
	}
	renameDockerContainer(projectName, newWorkspaces)

	return nil
}

// notifyError uses the Linux notify-send command to show a critical desktop notification,
// instead of just printing to the console, so the user doesn't miss errors from this background process.
func notifyError(format string, v ...interface{}) {
	msg := fmt.Sprintf(format, v...)
	log.Print(msg)
	exec.Command("notify-send", "-u", "critical", "Devcontainer Manager Error", msg).Run()
}

// notifyFatal shows a critical desktop notification and then exits the program.
func notifyFatal(format string, v ...interface{}) {
	notifyError(format, v...)
	os.Exit(1)
}

func renameDockerContainer(projectName string, currentWorkspaces map[string]Workspace) {
	log.Printf("Attempting to rename docker container to %s...", projectName)

	targetUID := ""
	if ws, ok := currentWorkspaces[projectName]; ok {
		targetUID = ws.UID
	}

	out, err := exec.Command("docker", "ps", "--format", "{{.ID}}|{{.Names}}|{{.Image}}|{{.Labels}}").Output()
	if err != nil {
		log.Printf("Error running docker ps: %v", err)
		return
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var targetID, currentName string

	// 1. Try to find by UID match
	if targetUID != "" {
		for _, line := range lines {
			if line == "" {
				continue
			}
			parts := strings.Split(line, "|")
			if len(parts) >= 4 {
				id, name, labels := parts[0], parts[1], parts[3]
				// Devpod uses dev.containers.id for its internal UID
				if strings.Contains(labels, fmt.Sprintf("%s%s", VscLabelPrefix, targetUID)) {
					targetID, currentName = id, name
					break
				}
			}
		}
	}

	// 2. Fallback to name match if we haven't found it and it's not already named correctly
	if targetID == "" {
		for _, line := range lines {
			if line == "" {
				continue
			}
			parts := strings.Split(line, "|")
			if len(parts) >= 2 {
				id, name := parts[0], parts[1]
				if name == projectName {
					targetID, currentName = id, name
					break
				}
			}
		}
	}

	// 3. Fallback to image name heuristics for un-renamed containers
	if targetID == "" {
		for _, line := range lines {
			if line == "" {
				continue
			}
			parts := strings.Split(line, "|")
			if len(parts) >= 4 {
				id, name, image, labels := parts[0], parts[1], parts[2], parts[3]
				// Skip if it definitely belongs to another known workspace UID
				belongsToOther := false
				for _, ws := range currentWorkspaces {
					if ws.ID != projectName && strings.Contains(labels, ws.UID) {
						belongsToOther = true
						break
					}
				}
				if belongsToOther {
					continue
				}

				if strings.Contains(image, "devpod-") || strings.Contains(image, "vsc-") {
					if name != projectName {
						targetID, currentName = id, name
						break
					}
				}
			}
		}
	}

	if targetID != "" {
		if currentName == projectName {
			log.Printf("Container %s is already named %s", targetID, projectName)
			return
		}

		log.Printf("Renaming container %s (currently %s) to %s", targetID, currentName, projectName)
		if err := exec.Command("docker", "rename", targetID, projectName).Run(); err != nil {
			log.Printf("Failed to rename container: %v", err)
		} else {
			log.Printf("Successfully renamed container to %s", projectName)
		}
	} else {
		log.Printf("Could not identify which container to rename for %s", projectName)
	}
}

func isContainerRunning(projectName string, currentWorkspaces map[string]Workspace) bool {
	targetUID := ""
	if ws, ok := currentWorkspaces[projectName]; ok {
		targetUID = ws.UID
	}

	out, err := exec.Command("docker", "ps", "--format", "{{.Names}}|{{.Labels}}").Output()
	if err != nil {
		return false
	}

	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) >= 2 {
			name, labels := parts[0], parts[1]
			if name == projectName {
				return true
			}
			if targetUID != "" && strings.Contains(labels, fmt.Sprintf("%s%s", VscLabelPrefix, targetUID)) {
				return true
			}
			// Fallback: also check DevpodLabelPrefix for old versions or specific provider metadata
			if strings.Contains(labels, fmt.Sprintf("%s%s", DevpodLabelPrefix, projectName)) {
				return true
			}
		}
	}

	return false
}

func deleteDevcontainer(repo string) error {
	projectName := filepath.Base(repo)
	log.Printf("Initiating devcontainer deletion for repository '%s' (project ID: '%s')...", repo, projectName)
	return deleteDevcontainerByID(projectName)
}

func deleteDevcontainerByID(id string) error {
	log.Printf("Deleting devcontainer with id: %s...", id)
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
		PushedAt string
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
				updates[index].PushedAt = strings.TrimSpace(string(out))
			} else {
				// Keep it empty on error so it falls to the bottom
				updates[index].PushedAt = ""
			}
		}(i, repo)
	}

	wg.Wait()

	sort.SliceStable(updates, func(i, j int) bool {
		return updates[i].PushedAt > updates[j].PushedAt
	})

	for i, update := range updates {
		repos[i] = update.Name
	}
}
