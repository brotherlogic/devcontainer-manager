# Devcontainer Manager

The manager periodically checks GitHub for updates. If it detects changes in the devcontainer configuration, it automatically deletes and recreates the container. By default, it uses `--ide none` to prevent the IDE from automatically launching, though this can be configured using the `--ide` command-line flag.

*It actively synchronizes the local `container.list` with the remote `container.list.template` configuration. Any local devcontainers removed from the template will be gracefully detected and removed from the active system.*

cli installed for managing devcontainers and running them. Project is written in golang, using the latest standards.

## Build Consistency
The daemon enforces configuration consistency by invoking DevPod with the `--recreate` parameter. As a result, restarting or updating a devcontainer will always result in a fresh build drawn from the latest configuration rather than an outdated container state.

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
Adding support for `brotherlogic/recordcollection`, `brotherlogic/sale-description-generator`, `brotherlogic/recordalerting`, and `brotherlogic/focus` and their specific environment needs.
Adding support for `brotherlogic/recordcollection`, `brotherlogic/sale-description-generator`, `brotherlogic/recordalerting`, and `brotherlogic/godiscogs` and their specific environment needs.

## Improved Observability
The manager now logs the full `devpod-cli up` command it executes when starting or recreating a container. This provides better visibility into the background operations and simplifies debugging of the container lifecycle.
