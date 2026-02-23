# Grove

A local daemon + CLI that supervises AI coding agents running in isolated Git
worktrees and Docker containers on a single machine.

groved is **not** an AI model. It is a process supervisor and developer UX
layer around existing agent CLIs (e.g. `claude`, `aider`).

<img width="916" height="469" alt="Screenshot 2026-02-21 at 11 11 27 PM" src="https://github.com/user-attachments/assets/9ffbd86b-598d-4ba2-abc6-63d858b093de" />

---

## Requirements

- **Docker** — required, no fallback. Every agent instance runs inside a container.
  Install: https://docs.docker.com/get-docker/
- **macOS or Linux**

---

## Binaries

| Binary   | Role                                   |
|----------|----------------------------------------|
| `groved` | Background daemon (Unix socket server) |
| `grove`  | CLI client                             |

You may alias `grove` to `g` in your shell profile if you prefer a shorter
invocation.

---

## How it works

```
grove start my-project feat/dark-mode
```

1. Reads the project **registration** from `~/.grove/projects/my-project/project.yaml`
   to get the repo URL
2. Clones the repo (if needed) into `~/.grove/projects/my-project/main/`
3. Runs `git pull` to sync to the latest remote HEAD
4. Reads `grove.yaml` from inside the cloned repo — the project-owned
   config that defines container image, start commands, agent, and finish steps;
   if missing, prompts you to create it
5. Creates a Git worktree at `~/.grove/projects/my-project/worktrees/<id>/`
   on branch `feat/dark-mode`
6. Starts a Docker container with the worktree bind-mounted inside it; agent
   credentials (e.g. `~/.claude`) are mounted automatically
7. Runs the `start` commands inside the container
8. Allocates a PTY, runs the agent inside the container via `docker exec -it`,
   and attaches your terminal immediately (pass `-d` to skip)

Each instance gets its own container (or compose stack), so databases, ports,
and environment state are fully isolated between parallel instances.

**Worktrees are first-class citizens.** An instance record lives exactly as
long as its worktree. `drop` is the only operation that removes both.
Everything else — `stop`, `finish`, daemon restart — only changes state.

---

## Building

```bash
go build -o bin/groved ./cmd/groved
go build -o bin/grove  ./cmd/grove
```

---

## Agent credentials

Grove runs AI agents (like Claude) inside Docker containers. Since the
container can't access your host's credential store (e.g. macOS Keychain),
you need to provide an authentication token.

**First-time setup — grove prompts you automatically.** The first time you
`grove start` a project that uses `claude`, grove will ask you to generate
and paste a token:

```
Claude authentication required.

Generate a long-lived token by running:

    claude setup-token

Then paste the token below.

Token (or Enter to skip):
```

The token is saved to `~/.grove/env` and used for all future sessions.

**Manual setup** (if you prefer):

```bash
claude setup-token          # generates a token valid for 1 year
echo "CLAUDE_CODE_OAUTH_TOKEN=<paste-token>" >> ~/.grove/env
```

**Alternative — API key auth:**

```bash
echo "ANTHROPIC_API_KEY=sk-ant-api03-..." >> ~/.grove/env
```

The `~/.grove/env` file uses dotenv format (`KEY=VALUE`, one per line, `#`
comments). It is created with `0600` permissions (owner-only read/write).

---

## Project config

Project configuration has two parts: a **registration** on your machine and an
**in-repo config** owned by the project.

### Registration (per-machine)

Tells grove how to **find** the project. Created by `grove project create` and
stored in `~/.grove/projects/<name>/project.yaml`. Just a name and repo URL.

```yaml
name: my-app
repo: git@github.com:example/my-app.git
```

### In-repo config (`grove.yaml` in your project)

The **authoritative source** for how to set up and run the project. Committed
alongside your code so every grove user automatically gets the right container,
start commands, and agent — no per-machine setup required.

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

---

## Filesystem layout

```
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
└─ groved.sock          ← Unix domain socket
```

Instance IDs are short and human-friendly: single characters from `1`–`9` then
`a`–`z` (35 slots), expanding to two-character combinations as needed.

---

## CLI reference

### Project commands

```
grove project create <name> [--repo <url>]
                         Register a new project (name + repo URL)
grove project list     List registered projects (numbered)
grove project delete <name|#>
                      Remove a project and all its worktrees (prompts to confirm)
grove project dir <name|#>
                      Print the main checkout path for a project
```

### Instance commands

```
grove start <project|#> <branch> [-d]
                         Start a new agent instance on <branch>
                         <project> may be a name or the number from 'project list'
                         Attaches immediately; use -d to skip
grove attach <id>      Attach terminal to a running instance (detach: Ctrl-])
grove stop <id>        Kill the agent; instance stays in list as KILLED
grove restart <id> [-d]
                         Restart the agent in the existing worktree and container
                         Attaches immediately; use -d to skip
grove check <id>       Run check commands concurrently; instance returns to WAITING
grove finish <id>      Run finish commands; stop container; instance stays as FINISHED
grove drop <id>        Delete the worktree, container, and record permanently
grove list [--active]  List all instances (--active: exclude FINISHED)
grove watch            Live dashboard (refreshes every second, Ctrl-C to exit)
grove logs <id> [-f]   Print buffered output; -f to follow
grove dir <id>         Print the worktree path for an instance
grove prune [--finished]
                         Drop all EXITED/CRASHED/KILLED instances
                         (--finished: also include FINISHED)
```

### Daemon commands

```
grove daemon install    Register groved as a login LaunchAgent (macOS only)
grove daemon uninstall  Remove the LaunchAgent (macOS only)
grove daemon status     Show LaunchAgent status (macOS only)
grove daemon logs [-f] [-n N]
                         Print daemon log (-f follow, -n tail lines)
```

---

## Instance states

| State      | Meaning                                              |
|------------|------------------------------------------------------|
| `RUNNING`  | Agent process is alive                               |
| `WAITING`  | Agent is idle (no PTY output for >2 s)               |
| `ATTACHED` | A grove client is currently attached                 |
| `EXITED`   | Agent exited cleanly (code 0)                        |
| `CRASHED`  | Agent exited with a non-zero code                    |
| `KILLED`   | Agent was stopped with `grove stop`                  |
| `FINISHED` | Instance completed via `grove finish`                |

State transitions:

```
RUNNING/WAITING ←→ ATTACHED   (attach / detach)
RUNNING/WAITING/ATTACHED → EXITED    (agent exits 0)
RUNNING/WAITING/ATTACHED → CRASHED   (agent exits non-zero, or daemon was killed)
RUNNING/WAITING/ATTACHED → KILLED    (grove stop)
any live state           → FINISHED  (grove finish)
EXITED/CRASHED/KILLED/FINISHED → RUNNING  (grove restart)
```

Instances in any terminal state (`EXITED`, `CRASHED`, `KILLED`, `FINISHED`) are
still visible in `grove list` and their worktrees are intact on disk. Use
`grove drop <id>` to permanently delete a worktree, stop its container, and
remove its record.

---

## Container lifecycle

```
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

The container outlives individual agent sessions. `stop` + `restart` reuses the
same container without re-running `start` commands, so restarts are fast.

---

## Attach / detach

`grove attach` behaves like `tmux attach`:

- Your terminal is connected directly to the agent's PTY inside the container.
- All keystrokes are forwarded to the agent.
- Terminal resize events (SIGWINCH) are forwarded automatically.
- **Detach** with **Ctrl-]** — the agent keeps running in the background.

`grove start` and `grove restart` attach automatically after the agent
starts. Use `-d` to skip and leave the agent running in the background.

---

## Example workflow

```bash
# 0. Register the daemon (once). On macOS use the LaunchAgent; on Linux use systemd or let grove auto-start it.
grove daemon install   # macOS only; on Linux: start groved manually or via systemd

# 1. Register a project
grove project create my-app --repo git@github.com:you/my-app.git

# 2. Start two parallel instances on different branches
#    If the repo has no grove.yaml, grove prompts you to create one.
grove start my-app feat/dark-mode -d
grove start my-app feat/search    -d
# Each gets its own container — isolated databases, ports, dependencies.

# 3. Attach to one
grove attach 1

# … interact with the agent, then Ctrl-] to detach …

# 4. Check all instances
grove list

# 5. Open a live dashboard
grove watch

# 6. Run checks (tests, lint) inside the container
grove check 1

# 7. Read logs without attaching
grove logs 1 -f

# 8. Finish the work (runs finish commands inside container, then stops container)
grove finish 1

# 9. Clean up dead instances
grove prune

# 10. Permanently delete a worktree, container, and record
grove drop 2
```

---

## Daemon management

`grove` auto-starts `groved` on demand when you run any command that requires
it. For a persistent setup that survives reboots, register it with your init
system.

**macOS — LaunchAgent:**

```bash
grove daemon install    # writes ~/Library/LaunchAgents/com.grove.daemon.plist
grove daemon uninstall
grove daemon status
```

> On macOS the LaunchAgent is preferred over auto-start because it avoids PTY
> permission errors from launching a detached background process directly.

**Linux — systemd (example):**

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

Daemon output goes to `~/.grove/daemon.log` and is also accessible via:

```bash
grove daemon logs -n 100      # last 100 lines
grove daemon logs -f          # follow new lines
```

Instance metadata is persisted to `~/.grove/instances/<id>.json`. When the
daemon restarts, all instances reload with their last known state. Instances
that were live when the daemon was killed are marked `CRASHED` on reload.
Orphaned containers (from instances that were live at daemon kill time) remain
until `grove drop` is called.

---

## Platform support

Grove runs on macOS and Linux. Docker is required on both.

The `grove daemon install/uninstall/status` commands are **macOS-only** — they
manage a LaunchAgent via `launchctl`. On Linux, manage `groved` with systemd
or any other init system; `grove` will auto-start the daemon on demand for the
current session regardless.

## What works well with Grove

Grove is a good fit for any project where parallel instances are meaningful —
i.e. where you could run parallel CI jobs on GitHub Actions:

- Web apps with databases (Rails, Django, Laravel) — each instance gets its own isolated database via compose
- Node / Python / Go / Rust services
- Anything with a `docker-compose.yml`

Grove is **not** a good fit for:

- **iOS / macOS app development** — Xcode, the iOS Simulator, and code signing
  are macOS-only and cannot run inside a Linux container. Parallel instances
  also conflict on the simulator regardless of containers.
- **Projects that own global host resources** — daemon processes that bind
  fixed host ports or sockets outside the container.
