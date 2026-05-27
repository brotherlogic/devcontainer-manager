# Devcontainer Manager

The manager periodically checks GitHub for updates. If it detects changes in the devcontainer configuration, it automatically deletes and recreates the container. By default, it uses `--ide none` to prevent the IDE from automatically launching, though this can be configured using the `--ide` command-line flag.

*It actively synchronizes the local `container.list` with the remote `container.list.template` configuration. Any local devcontainers removed from the template will be gracefully detected and removed from the active system.*

cli installed for managing devcontainers and running them. Project is written in golang, using the latest standards.

## Configuration Tracking & Caching
The daemon automatically tracks GitHub commits and content changes of the `.devcontainer` configuration files (`devcontainer.json` or `.devcontainer/devcontainer.json`), as well as any lifecycle scripts referenced inside them (such as `postCreateCommand`, `postStartCommand`, etc.). This ensures existing devpod containers seamlessly restart and rebuild when configurations or their dependencies update, without unnecessary rebuilding.

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

## Hooray

Hooray

// dummy comment for recordcleaner CI validation