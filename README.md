# Devcontainer Manager

The manager periodically checks GitHub for updates. If it detects changes in the devcontainer configuration, it automatically deletes and recreates the container. By default, it uses `--ide none` to prevent the IDE from automatically launching, though this can be configured using the `--ide` command-line flag.

*It actively synchronizes the local `container.list` with the remote `container.list.template` configuration. Any local devcontainers removed from the template will be gracefully detected and removed from the active system.*

cli installed for managing devcontainers and running them. Project is written in golang, using the latest standards.

## Configuration Tracking & Caching
The daemon automatically tracks GitHub commits of the `.devcontainer` configuration files to seamlessly restart existing devpod containers without unnecessarily rebuilding them.

Configurations are actively tracked via a state file (`~/.config/devcontainer-manager/tracked_shas.json`). To force a hard container rebuild, simply delete this JSON file to bypass the state. Rebuilds are otherwise automatic whenever remote repository devcontainer files are updated.

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
Adding support for `brotherlogic/focus`, `brotherlogic/gramophile`, `brotherlogic/recordalerting`, `brotherlogic/recordcollection`, `brotherlogic/recordgetter`, `brotherlogic/sale-description-generator`, and `brotherlogic/seraphine` and their specific environment needs.

## Improved Observability
The manager now logs the full `devpod-cli up` command it executes when starting or recreating a container. This provides better visibility into the background operations and simplifies debugging of the container lifecycle.

## SSH for DevPod
The manager now uses SSH repository URLs (`git@github.com:...`) instead of HTTPS shorthand when calling `devpod-cli up`. This ensures that DevPod utilizes your local SSH credentials for repository operations.


## Version Tracking
The manager now prints the git SHA of the build on startup, allowing you to easily identify which version of the code is running. This information is automatically extracted from the build metadata.

## Hooray

Hooray