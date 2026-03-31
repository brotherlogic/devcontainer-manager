# Devcontainer Manager

This project pulls in a list of github sourced devcontainers, stored in the container.list file. It then periodically checks github for updates
and if it detects that the devcontainer configuration has changed, it will delete and recreate the new container with the updated configuration.

*It actively synchronizes the local `container.list` with the remote `container.list.template` configuration. Any local devcontainers removed from the template will be gracefully detected and removed from the active system.*

cli installed for managing devcontainers and running them. Project is written in golang, using the latest standards.

## Build Consistency
The daemon enforces absolute build consistency by invoking DevPod with the `--build-no-cache` parameter. As a result, restarting or updating a devcontainer will always result in a completely fresh build drawn from the latest source image rather than an improperly cached Docker layer.

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
5. Enable and start the systemd service\n## Robust Container Renaming\nThe daemon automatically ensures that the underlying Docker containers perfectly match their corresponding project names, even when multiple disjoint environments run simultaneously, by referencing their dedicated workspace IDs.
