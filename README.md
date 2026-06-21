# Devcontainer Manager

**Devcontainer Manager (DCM)** is a daemon written in Go designed to automatically manage, run, and sync [DevPod](https://devpod.sh/) devcontainers on a Linux environment. It continuously monitors remote Git repository templates and coordinates the lifecycle of active workspaces, handling configurations, port mapping, and GitHub-integrated issue environments.

---

## 🚀 Key Capabilities

*   **Continuous Synchronization:** Detects configuration changes in remote templates and aligns local devcontainers by rebuilding or cleaning them up.
*   **GitHub Issue Devcontainers:** Automatically provisions dedicated devcontainers for open issues containing `seraphine` labels, handling state labels (`container-creating`, `container-ready`, `container-failed`) dynamically.
*   **Container Prioritization:** Dynamically orders container startup operations, prioritizing repositories that have been most recently updated (pushed) on GitHub.
*   **Deterministic Caching:** Minimizes rebuild times by storing composite SHAs of configurations and script dependencies in a state cache.
*   **Automatic SSH Mapping:** Assigns unique SSH ports to workspaces, facilitating reverse-proxy routing via systems like `dcrouter`.
*   **Startup Command Injection:** Polls containers via SSH until they are ready, then automatically injects execution commands into the container's active tmux session.
*   **Robust Command Timeouts:** Prevents standard output pipe leaks from background tasks from deadlocking the issue provisioning loops.
*   **Robust Observability & Prefixed Logging:** Prepends all log messages and command outputs with a `[owner/repo]` prefix for concurrent readability. Reports startup failure logs back to GitHub issues.
*   **GitHub API Rate Limit Retries:** Wraps GitHub API calls in a retry handler that performs exponential backoff when encountering rate limit responses (HTTP 403 or 429).
*   **Latency Metric Tracking:** Automatically calculates and logs startup latency metrics for GitHub issues by recording `devcontainer-startup-latency` to GitHub comments after successful container provisioning. Includes robust test cases to verify error handling and prevent duplicate postings.
---

## 🛠️ Architecture & Workflow

```mermaid
graph TD
    A[Remote Repository] -->|1. Poll Config Changes| B(Devcontainer Manager)
    B -->|2. Check Template & Shas| C{Rebuild Needed?}
    C -->|Yes| D[Recreate DevPod Container]
    C -->|No| E[Maintain State]
    B -->|3. Read Open GitHub Issues| F{Has Seraphine Label?}
    F -->|Yes| G[Provision Branch Devcontainer]
    F -->|No| H[Ignore]
    D -->|4. Map Ports & Advertise| I[mappings.json]
    G -->|5. Inject startup_command| J[Active tmux Session]
```

---

## 📋 Prerequisites

Before running the Devcontainer Manager, ensure that the following dependencies are installed and available in the daemon host's environment:

*   **Go** (v1.20+)
*   **Docker** / **Podman** (underlying container provider)
*   **DevPod CLI** (`devpod` or `devpod-cli` installed in your path)
*   **GitHub CLI** (`gh`, authenticated to list issues, write pull requests, and manage issue labels)
*   **tmux** (installed inside target devcontainers to support startup command injections)

---

## ⚙️ Installation & Setup

### 1. Automatic systemd User Service Setup (Recommended)
Use the included automated install script to build, configure, and register the manager as a persistent systemd user service.

```bash
./install.sh
```

> [!NOTE]
> The `install.sh` script does not require root privileges for standard setups. Running it executes the following steps:
> 1. Compiles the binary using the local Go environment.
> 2. Places the compiled binary under `~/.local/bin/devcontainer-manager`.
> 3. Creates a systemd unit file at `~/.config/systemd/user/devcontainer-manager.service`.
> 4. Configures lingering (`loginctl enable-linger`) so that the daemon runs in the background even when you are not logged in.
> 5. Reloads systemd and enables the service to start automatically on system boot.

To verify that the service is running successfully:
```bash
systemctl --user status devcontainer-manager
```

### 2. Manual Startup
For manual runs or local debugging, you can build and run the binary directly:

```bash
go build -o devcontainer-manager main.go
./devcontainer-manager -container_list container.list.template
```

---

If no custom startup command is supplied via the flag, issue-specific containers will default to injecting a prompt that dynamically references the GitHub issue/bug number they were created for.

## ⌨️ Command-Line Interface (CLI)

The manager supports configuration via the following command-line flags:

| Flag | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `-once` | `bool` | `false` | Runs the synchronization check once and exits immediately instead of polling in a loop. |
| `-container_list` | `string` | `container.list.template` | The template file specifying the target Git repositories to track and manage. |
| `-max_issue_containers`| `int` | `5` | The maximum number of concurrent issue-based devcontainers allowed to run simultaneously. |
| `-startup_command` | `string` | `""` | A shell command to inject dynamically into the container's primary tmux session once it is active. |
| `-max-concurrency` | `int` | `10` | The maximum number of concurrent repository processing threads/goroutines. |
| `-port` | `int` | `50051` | The port to run the gRPC dashboard service on. |

---

## 💾 Caching & State Persistence

DCM keeps track of the active configurations it processes to prevent redundant rebuilds:
*   **Location:** Caches are persisted at `~/.config/devcontainer-manager/tracked_shas.json`.
*   **Behavior:** Stores deterministic composite hashes of tracking files (`devcontainer.json`, `postCreateCommand`, etc.).
*   **Manual Override:** If you need to force a complete rebuild of all managed containers, delete the state file:
    ```bash
    rm ~/.config/devcontainer-manager/tracked_shas.json
    ```

## 📊 gRPC Dashboard Service

DCM hosts a gRPC service implementing `DashboardService` defined in `proto/dashboard.proto`. This service provides:
*   **ListContainers RPC:** Retrieves a list of active devcontainers with their metadata, including ID, Repository URL, Branch or Issue number, current Lifecycle Status (`STARTING`, `RUNNING`, `FAILED`), last active timestamp, and error messages (if applicable).
*   **Thread-Safe In-Memory Status Cache:** The state is updated in real-time as devcontainers transition through different lifecycle stages, and is periodically synchronized with the actual running containers on the host.

---

## 🤖 Automated GitHub Workflows

The repository includes pre-configured automation workflows under `.github/workflows/`:
*   **Issue Closer Workflow (`issue-closer.yml`):** Runs every 5 minutes to automatically query GitHub's Sub-issues API, closing parent issues once all related child sub-issues are marked closed.
*   **Assign Reviewer Workflow (`assign-reviewer.yml`):** Automatically assigns the repository owner (`brotherlogic`) as a reviewer on new pull requests once validation checks pass.

---

## 🔗 Project Workflows & Contributions
For details on the issue lifecycle, label transitions, and the AI-driven development workflows utilized in this project, refer to the [Issues Workflow Guide (issues.md)](file:///workspaces/devcontainer-manager/issues.md).
