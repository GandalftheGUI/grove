# Grove

A local daemon + CLI that supervises AI coding agents running in isolated Git
worktrees on a single macOS machine.

groved is **not** an AI model. It is a process supervisor and developer UX
layer around existing agent CLIs (e.g. `claude`, `aider`).

---

## Binaries

| Binary     | Role                                   |
|------------|----------------------------------------|
| `groved` | Background daemon (Unix socket server) |
| `grove`  | CLI client                             |

You may alias `grove` to `g` in your shell profile if you prefer a shorter
invocation.

---

## How it works

```
grove start my-project feat/dark-mode
```

1. Reads the project **registration** from `projects.local/` or `projects/` in
   the grove repo to get the repo URL
2. Clones the repo (if needed) into `~/.grove/projects/my-project/main/`
3. Runs `git pull` to sync to the latest remote HEAD
4. Reads `.grove/project.yaml` from inside the cloned repo (if it exists) and
   overlays the bootstrap, agent, and complete settings; if the file is missing
   and no agent is configured in the registration, prompts you to create it
5. Creates a Git worktree at `~/.grove/projects/my-project/worktrees/<id>/`
   on branch `feat/dark-mode`
6. Runs the `bootstrap` commands in the worktree (output streams to your terminal)
7. Allocates a real PTY, starts the agent process inside it, and attaches your
   terminal immediately (pass `-d` to skip auto-attach)

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

## Two-layer project config

Project configuration is split into two files that overlay each other:

### 1. Registration (in the grove repo)

Tells grove how to **find** the project. Stored in the grove repo itself and
read by the `grove` client at install time.

```
<grove-repo>/
├─ projects/             ← shared, tracked by git
│  └─ <name>/project.yaml
└─ projects.local/       ← personal, git-ignored
   └─ <name>/project.yaml
```

A minimal registration only needs a name and repo URL:

```yaml
name: my-app
repo: git@github.com:example/my-app.git
```

Run `grove project create <name>` to scaffold one.

### 2. In-repo config (committed in the target project)

Tells grove how to **set up and run** the project. Committed at
`.grove/project.yaml` inside the project's own repository so every
groved user automatically gets the right bootstrap, agent, and completion
steps — no per-machine setup required.

```
<project-repo>/
└─ .grove/
   └─ project.yaml      ← committed alongside your code
```

**The in-repo config takes precedence** over the registration for every field
it defines (`bootstrap`, `agent`, `complete`, `dev`). The registration's
`repo` URL is always used for cloning.

If `grove start` finds no `.grove/project.yaml` and the registration has no
agent command, it prompts you to create a boilerplate file and commit it.

---

## Filesystem layout

```
~/.grove/                        ← runtime data root (GROVE_ROOT)
├─ projects/
│  └─ <project-name>/
│     ├─ main/          ← canonical git clone
│     └─ worktrees/
│        └─ <id>/       ← one git worktree per instance
├─ instances/
│  └─ <id>.json         ← persisted instance metadata (survives daemon restart)
├─ logs/
│  └─ <id>.log          ← PTY output + bootstrap + complete command output
└─ groved.sock          ← Unix domain socket
```

Instance IDs are short and human-friendly: single characters from `1`–`9` then
`a`–`z` (35 slots), expanding to two-character combinations as needed.

---

## Project definition

### Registration (`projects.local/<name>/project.yaml`)

```yaml
name: my-app
repo: git@github.com:example/my-app.git

# Optional: set an agent here if the repo has no .grove/project.yaml yet.
# agent:
#   command: claude
#   args: []
```

### In-repo config (`.grove/project.yaml` in your project)

```yaml
# ── Bootstrap ──────────────────────────────────────────────────────────────────
# Commands run once in each fresh worktree before the agent starts.
# Working directory: worktree root. Delegate to a script when possible.
bootstrap:
  - ./scripts/bootstrap.sh
  # - npm install
  # - pip install -r requirements.txt

# ── Agent ──────────────────────────────────────────────────────────────────────
# The AI coding agent to run inside each worktree PTY.
# Common values: claude, aider, sh (plain shell, useful for testing)
agent:
  command: claude
  args: []

# ── Complete ───────────────────────────────────────────────────────────────────
# Commands run by `grove finish`. Use {{branch}} for the branch name.
# The daemon runs these to completion even if you close your terminal.
complete:
  - git push -u origin {{branch}}
  # - gh pr create --title "{{branch}}" --fill

# ── Dev servers (reserved, not yet implemented) ────────────────────────────────
dev:
  start: []
```

---

## CLI reference

### Project commands

```
grove project create <name> [--global] [--repo <url>] [--agent <cmd>]
                         Define a new project registration (personal by default, --global for shared)
grove project list     List defined projects
grove main <project>   Print the main checkout path for a project
```

### Instance commands

```
grove start <project> <branch> [-d]
                         Start a new agent instance on <branch>
                         Attaches immediately; use -d to skip
grove attach <id>      Attach terminal to a running instance (detach: Ctrl-])
grove stop <id>        Kill the agent; instance stays in list as KILLED
grove restart <id> [-d]
                         Restart the agent in the existing worktree
                         Attaches immediately; use -d to skip
grove finish <id>      Run complete commands; instance stays as FINISHED
grove drop <id>        Delete the worktree and branch permanently (prompts first)
grove list [--active]  List all instances (--active: exclude FINISHED)
grove watch            Live dashboard (refreshes every second, Ctrl-C to exit)
grove logs <id> [-f]   Print buffered output; -f to follow
grove worktree <id>    Print the worktree path for an instance
grove prune [--finished]
                         Drop all EXITED/CRASHED/KILLED instances
                         (--finished: also include FINISHED)
```

### Daemon commands

```
grove daemon install    Register groved as a login LaunchAgent
grove daemon uninstall  Remove the LaunchAgent
grove daemon status     Show whether the LaunchAgent is installed and running
```

---

## Instance states

| State      | Meaning                                              |
|------------|------------------------------------------------------|
| `RUNNING`  | Agent process is alive                               |
| `WAITING`  | Agent is idle (no PTY output for >2 s)               |
| `ATTACHED` | A grove client is currently attached               |
| `EXITED`   | Agent exited cleanly (code 0)                        |
| `CRASHED`  | Agent exited with a non-zero code                    |
| `KILLED`   | Agent was stopped with `grove stop`                |
| `FINISHED` | Instance completed via `grove finish`              |

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
`grove drop <id>` to permanently delete a worktree and its record.

---

## Attach / detach

`grove attach` behaves like `tmux attach`:

- Your terminal is connected directly to the agent's PTY.
- All keystrokes are forwarded to the agent.
- Terminal resize events (SIGWINCH) are forwarded automatically.
- **Detach** with **Ctrl-]** — the agent keeps running in the background.

`grove start` and `grove restart` attach automatically after the agent
starts. Use `-d` to skip and leave the agent running in the background.

---

## Example workflow

```bash
# 0. Register the daemon (once, on macOS)
grove daemon install

# 1. Register a project (creates projects.local/my-app/project.yaml)
grove project create my-app --repo git@github.com:you/my-app.git

# 2. Start an agent on a branch
#    If the repo has no .grove/project.yaml, grove will prompt you to create one.
grove start my-app feat/dark-mode

# … interact with the agent, then Ctrl-] to detach …

# 3. Check all instances
grove list

# 4. Open a live dashboard
grove watch

# 5. Read logs without attaching
grove logs 1
grove logs 1 -f   # follow

# 6. Stop the agent (keeps worktree and record)
grove stop 1

# 7. Restart it in the same worktree
grove restart 1

# 8. Finish the work (runs complete commands: git push, gh pr create, etc.)
grove finish 1

# 9. Clean up dead instances
grove prune

# 10. Permanently delete a worktree and its record
grove drop 1
```

---

## Daemon management (macOS LaunchAgent)

On macOS, PTY allocation requires running inside a full user login session.
Register `groved` as a LaunchAgent so it starts automatically at login with
the correct privileges:

```bash
grove daemon install
```

This writes `~/Library/LaunchAgents/com.grove.daemon.plist` and starts the
daemon immediately. Daemon output is written to `~/.grove/daemon.log`.

> **Note:** `grove` also auto-starts the daemon on demand when you run any
> command that requires it. The LaunchAgent approach is preferred on macOS
> because it avoids PTY permission errors that occur when launching a detached
> background process directly.

Instance metadata is persisted to `~/.grove/instances/<id>.json`. When the
daemon restarts, all instances reload with their last known state. Instances
that were live when the daemon was killed are marked `CRASHED` on reload.

---

## macOS only

groved targets macOS. It uses:

- Unix domain sockets for IPC
- `posix_openpt` / `openpty` (via `github.com/creack/pty`) for PTY allocation
- `SIGKILL` on the process group for clean teardown
- `launchd` for daemon lifecycle management
