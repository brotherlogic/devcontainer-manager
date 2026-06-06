# Devcontainer Manager

The manager periodically checks GitHub for updates. If it detects changes in the devcontainer configuration, it automatically deletes and recreates the container. By default, it uses `--ide none` to prevent the IDE from automatically launching, though this can be configured using the `--ide` command-line flag.

*It actively synchronizes the local `container.list` with the remote `container.list.template` configuration. Any local devcontainers removed from the template will be gracefully detected and removed from the active system.*

cli installed for managing devcontainers and running them. Project is written in golang, using the latest standards.

## Configuration Tracking & Caching
The daemon automatically tracks GitHub commits and content changes of the `.devcontainer` configuration files (`devcontainer.json` or `.devcontainer/devcontainer.json`), as well as any lifecycle scripts referenced inside them (such as `postCreateCommand`, `postStartCommand`, etc.). This tracking applies to both standard/non-issue containers and per-issue branch devcontainers (checking the configuration on their respective feature branches). This ensures existing devpod containers seamlessly restart and rebuild when configurations or their dependencies update, without unnecessary rebuilding.

Configurations are actively tracked via a state file (`~/.config/devcontainer-manager/tracked_shas.json`) containing deterministic, composite SHAs of all tracked files. To force a hard container rebuild, simply delete this JSON file to bypass the state. Rebuilds are otherwise automatic whenever remote repository devcontainer configuration files or referenced script files are updated.

## Installation

You can install the project and set it up as a systemd user service by running the provided `install.sh` script.

```bash
sudo ./install.sh
```

This script will:
1. Build the binary using your regular user's `go` environment
2. Move the built binary to `/usr/local/bin`
3. Configure a systemd user service based on `service-file`
4. Enable lingering for your user so that the service runs in the background even when you are not logged in
5. Enable and start the systemd service

## Robust Container Renaming
The daemon automatically ensures that the underlying Docker containers perfectly match their corresponding project names, even when multiple disjoint environments run simultaneously, by referencing their dedicated workspace IDs.

## Supported Projects
The managed projects are defined in `container.list.template`.

## Improved Observability
The manager now logs the full `devpod-cli up` command it executes when starting or recreating a container. This provides better visibility into the background operations and simplifies debugging of the container lifecycle.

## SSH for DevPod
The manager now uses SSH repository URLs (`git@github.com:...`) instead of HTTPS shorthand when calling `devpod-cli up`. This ensures that DevPod utilizes your local SSH credentials for repository operations.

## Port Mapping & Discovery Support
The manager automatically allocates a unique SSH port for each devcontainer (starting from 2222) and maps it to the container's SSH port (22) using `--provider-option DOCKER_RUN_ARGS=`. 

Allocated ports are advertised by pushing a `mappings.json` file to the `brotherlogic/devcontainer-manager` repository. The manager automatically generates and registers an SSH deploy key to bypass pull request requirements for these updates. This allows tools like `dcrouter` to discover and route SSH connections to the correct container.

## Version Tracking
The manager now prints the git SHA of the build on startup, allowing you to easily identify which version of the code is running. This information is automatically extracted from the build metadata.

## Container Prioritization
The manager dynamically orders container startup operations, prioritizing repositories that have been most recently updated (pushed) on GitHub. This ensures the projects you are actively working on are spun up first.
## Issue-Based Devcontainers & Label Tracking
The daemon supports automatically provisioning dedicated devcontainers for open issues labeled with `seraphine` (or prefixes thereof). When it provisions these containers, it updates the GitHub issue labels to track their state:
- `container-creating`: Added when container provisioning begins.
- `container-ready`: Added when the container is successfully launched and ready (with `container-creating` and `container-failed` labels removed).
- `container-failed`: Added if provisioning fails (with `container-creating` and `container-ready` labels removed).

When a container is hibernated due to hitting concurrent container limits, the labels are kept as-is. Similarly, when a container is cleaned up or deleted, the labels are left on the issue for historical record.

The detailed workflow and guidelines for collaborating on these issues are documented in [issues.md](file:///workspaces/devcontainer-manager/issues.md).

## Startup Failure Reporting
If a devcontainer for an issue branch fails to start (e.g. during branch creation, devpod launch, or container recreation), the manager automatically runs a background task to report the failure. It queries GitHub for any existing open issues titled `"Issue Container Startup Failed"`. If none exists, it creates a new issue with that title, applies the `seraphine-bug` label, and documents the branch name, original issue number, and startup logs/errors in the issue body.

## Dynamic Startup Commands
The manager supports dynamically injecting a startup command or prompt into the container once it starts up or is recreated. When the `-startup_command "<command>"` flag is provided, the manager will poll the container via SSH until a `tmux` session named exactly after the container ID is ready. Once ready, the command will be sent directly to that tmux session, running it dynamically inside the persistent tmux shell.

## Issue Closer Workflow
The repository includes an Issue Closer GitHub Action (`.github/workflows/issue-closer.yml`) which runs every 5 minutes. It automatically queries GitHub's native Sub-issues API for open issues and closes parent issues if all of their sub-issues are closed.

## Assign Reviewer Workflow
The repository includes an Assign Reviewer GitHub Action (`.github/workflows/assign-reviewer.yml`) which runs upon successful completion of the `Validate PR` workflow. It automatically assigns the repository owner (`brotherlogic`) as a reviewer on the pull request.

## GitHub API Rate Limit Retries
The manager wraps GitHub API client calls in a retry handler that performs exponential backoff when encountering rate limit responses (secondary rate limit: HTTP 403 or 429) or standard Rate Limit/Abuse Rate Limit errors. This ensures resilient synchronization when running concurrent operations or API-heavy tasks.


## Command-Line Flags


The daemon supports the following command-line flags:
* `-once`: Run the check loop once and then exit immediately (default: `false`).
* `-container_list <file>`: The path to the container template file to check (default: `container.list.template`).
* `-max_issue_containers <count>`: The maximum number of concurrent running issue containers (default: `5`).
* `-startup_command <command>`: A command/prompt to inject dynamically into the container's active tmux session once it is ready (default: `""`).
* `-max-concurrency <count>`: The maximum number of concurrent repository processing threads/goroutines (default: `10`).