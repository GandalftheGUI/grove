# Grove — Technical details

This document is the deeper, engineer-facing reference for Grove’s architecture, configuration, and command surface. If you just want the overview and a quick start, see `README.md`.

## How it works (detailed)

When you run:

```bash
grove start my-project feat/dark-mode
```

Grove:

1. Reads the project **registration** from `~/.grove/projects/my-project/project.yaml` to get the repo URL
2. Clones the repo (if needed) into `~/.grove/projects/my-project/main/`
3. Runs `git pull` to sync to the latest remote HEAD
4. Reads `grove.yaml` from inside the cloned repo — the project-owned config that defines container image, start commands, agent, and finish steps; if missing, prompts you to create it
5. Creates a Git worktree at `~/.grove/projects/my-project/worktrees/<id>/` on branch `feat/dark-mode`
6. Starts a Docker container (or a compose stack) with the worktree bind-mounted inside it
7. Runs the `start` commands inside the container
8. Allocates a PTY, runs the agent inside the container via `docker exec -it`, and attaches your terminal immediately (pass `-d` to skip)

Each instance gets its own container (or compose stack), so databases, ports, and environment state are fully isolated between parallel instances.

Worktrees are first-class citizens: an instance record lives exactly as long as its worktree. `drop` is the only operation that removes both. Everything else (`stop`, `finish`, daemon restart) only changes state.

## Binaries

| Binary   | Role                                   |
|----------|----------------------------------------|
| `groved` | Background daemon (Unix socket server) |
| `grove`  | CLI client                             |

## Agent credentials

Grove runs AI agents (like Claude) inside Docker containers. Since the container can’t access your host’s credential store (e.g. macOS Keychain), you need to provide an authentication token or API key via `~/.grove/env` (dotenv format).

- **Interactive setup (recommended)**: `grove token` prompts and writes `CLAUDE_CODE_OAUTH_TOKEN=...` to `~/.grove/env` (replacing any existing token line).
- **API key auth**:

```bash
echo "ANTHROPIC_API_KEY=sk-ant-api03-..." >> ~/.grove/env
```

## Project config

Project configuration has two parts: a **registration** on your machine and an **in-repo config** owned by the project.

### Registration (per-machine)

Tells Grove how to find the project. Created by `grove project create` and stored in `~/.grove/projects/<name>/project.yaml`. It’s just a name and repo URL.

```yaml
name: my-app
repo: git@github.com:example/my-app.git
```

### In-repo config (`grove.yaml`)

The authoritative source for how to set up and run the project. Committed alongside your code so every Grove user automatically gets the right container, start commands, and agent — no per-machine setup required.

```yaml
# ── Container ──────────────────────────────────────────────────────────────────
# Docker is required. Each instance gets its own container with the worktree
# bind-mounted inside at `workdir`.
#
# Option A – single image:
container:
  image: ruby:3.3
  workdir: /app         # default /app

# Option B – docker-compose.yml (for projects with databases, caches, etc.):
# container:
#   compose: docker-compose.yml
#   service: app        # service to exec into; default "app"
#   workdir: /app

# Agent credentials are injected automatically from ~/.grove/env.
# Config directories are also mounted:
#   claude → ~/.claude    aider → ~/.aider
#
# Mount additional host paths (~/... maps to /root/... in the container):
# container:
#   mounts:
#     - ~/.gitconfig
#     - ~/.ssh

# ── Start ──────────────────────────────────────────────────────────────────────
# Commands run once inside the container before the agent starts.
start:
  - bundle install
  - bin/rails db:create db:migrate

# ── Agent ──────────────────────────────────────────────────────────────────────
# The AI coding agent. Runs inside the container via `docker exec -it`.
# Grove auto-installs known agents if not present in the image:
#   claude → npm install -g @anthropic-ai/claude-code  (requires node in image)
#   aider  → pip install aider-chat                    (requires python in image)
# For other agents, add the install command to start: above.
agent:
  command: claude
  args: []

# ── Check ──────────────────────────────────────────────────────────────────────
# Commands run concurrently by `grove check`. Run inside the container.
# Instance returns to WAITING when all complete.
check:
  - bundle exec rspec

# ── Finish ─────────────────────────────────────────────────────────────────────
# Commands run by `grove finish` inside the container.
# Use {{branch}} as a placeholder for the branch name.
finish:
  - git push -u origin {{branch}}
  # - gh pr create --title "{{branch}}" --fill
```

## Filesystem layout

```text
~/.grove/                        ← data root (GROVE_ROOT)
├─ env                  ← agent credentials (dotenv format, 0600)
├─ projects/
│  └─ <project-name>/
│     ├─ project.yaml   ← registration (name + repo URL)
│     ├─ main/          ← canonical git clone
│     └─ worktrees/
│        └─ <id>/       ← one git worktree per instance (bind-mounted into container)
├─ instances/
│  └─ <id>.json         ← persisted instance metadata (survives daemon restart)
├─ logs/
│  └─ <id>.log          ← PTY output + start + finish command output
└─ groved.sock           ← Unix domain socket
```

Instance IDs are short and human-friendly: single characters from `1`–`9` then `a`–`z` (35 slots), expanding to two-character combinations as needed.

## CLI reference

### Project commands

```text
grove project create <name> [--repo <url>]  Register a new project (name + repo URL)
grove project list                         List registered projects (numbered)
grove project delete <name|#>              Remove a project and all its worktrees (prompts)
grove project dir <name|#>                 Print the main checkout path for a project
```

### Instance commands

```text
grove start <project|#> <branch> [-d]      Start a new agent instance on <branch> (attaches unless -d)
grove attach <id>                          Attach terminal to a running instance (detach: Ctrl-])
grove stop <id>                            Kill the agent; instance stays in list as KILLED
grove restart <id> [-d]                    Restart the agent in the existing worktree + container
grove check <id>                           Run check commands concurrently; instance returns to WAITING
grove finish <id>                          Run finish commands; stop container; instance stays as FINISHED
grove drop <id>                            Delete the worktree, container, and record permanently
grove list [--active]                      List all instances (--active: exclude FINISHED)
grove watch                                Live dashboard (refreshes every second, Ctrl-C to exit)
grove logs <id> [-f]                       Print buffered output; -f to follow
grove dir <id>                             Print the worktree path for an instance
grove shell <id> [shell]                   Open an interactive shell in the instance container (default: sh)
grove prune [--finished]                   Drop EXITED/CRASHED/KILLED instances (--finished includes FINISHED)
```

### Daemon commands

```text
grove daemon install                       Register groved as a login LaunchAgent (macOS only)
grove daemon uninstall                     Remove the LaunchAgent (macOS only)
grove daemon status                        Show LaunchAgent status (macOS only)
grove daemon logs [-f] [-n N]              Print daemon log (-f follow, -n tail lines)
```

### Token helper

```text
grove token                                Set/replace CLAUDE_CODE_OAUTH_TOKEN in ~/.grove/env
```

## Container lifecycle

```text
grove start   → docker run ... sleep infinity   (container starts)
              → docker exec  start commands     (setup inside container)
              → docker exec -it <agent>         (agent runs inside container)

grove stop    → kills docker exec session       (container keeps running)
grove restart → docker exec -it <agent>         (new session, same container)

grove finish  → docker exec  finish commands    (inside container)
              → docker compose down / docker stop+rm  (container stops)

grove drop    → docker compose down / docker stop+rm  (container stops)
              → git worktree remove
```

The container outlives individual agent sessions. `stop` + `restart` reuses the same container without re-running `start` commands, so restarts are fast.

## Attach / detach

`grove attach` behaves like `tmux attach`:

- Your terminal is connected directly to the agent’s PTY inside the container.
- All keystrokes are forwarded to the agent.
- Terminal resize events (SIGWINCH) are forwarded automatically.
- Detach with **Ctrl-]** — the agent keeps running in the background.

`grove start` and `grove restart` attach automatically after the agent starts. Use `-d` to skip and leave the agent running in the background.

## Daemon management

`grove` auto-starts `groved` on demand when you run any command that requires it. For a persistent setup that survives reboots, register it with your init system.

### macOS — LaunchAgent

```bash
grove daemon install    # writes ~/Library/LaunchAgents/com.grove.daemon.plist
grove daemon uninstall
grove daemon status
```

On macOS the LaunchAgent is preferred over auto-start because it avoids PTY permission errors from launching a detached background process directly.

### Linux — systemd (example)

```ini
# ~/.config/systemd/user/groved.service
[Unit]
Description=Grove daemon

[Service]
ExecStart=/usr/local/bin/groved
Restart=on-failure

[Install]
WantedBy=default.target
```

```bash
systemctl --user enable --now groved
```

Daemon output goes to `~/.grove/daemon.log` and is also accessible via `grove daemon logs`.

Instance metadata is persisted to `~/.grove/instances/<id>.json`. When the daemon restarts, all instances reload with their last known state. Instances that were live when the daemon was killed are marked `CRASHED` on reload. Orphaned containers (from instances that were live at daemon kill time) remain until `grove drop` is called.

## Platform support and fit

Grove runs on macOS and Linux. Docker is required on both.

The `grove daemon install/uninstall/status` commands are macOS-only — they manage a LaunchAgent via `launchctl`. On Linux, manage `groved` with systemd (or any init system); `grove` will auto-start the daemon on demand for the current session regardless.

Grove is a good fit for any project where parallel instances are meaningful — i.e. where you could run parallel CI jobs:

- Web apps with databases (Rails, Django, Laravel)
- Node / Python / Go / Rust services
- Anything with a `docker-compose.yml`

Grove is not a good fit for:

- iOS / macOS app development (Xcode + simulator + code signing don’t work in Linux containers, and the simulator conflicts across instances)
- Projects that own global host resources (fixed host ports/sockets outside the container)

