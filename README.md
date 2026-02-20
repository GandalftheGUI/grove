# CatHerdD

A local daemon + CLI that supervises AI coding agents running in isolated Git
worktrees on a single macOS machine.

catherdd is **not** an AI model. It is a process supervisor and developer UX
layer around existing agent CLIs (e.g. `claude`, `aider`).

---

## Binaries

| Binary     | Role                                   |
|------------|----------------------------------------|
| `catherdd` | Background daemon (Unix socket server) |
| `catherd`  | CLI client                             |

You may alias `catherd` to `ch` in your shell profile if you prefer a shorter
invocation.

---

## How it works

```
catherd start my-project feat/dark-mode
```

1. Reads the project config from `projects.local/` or `projects/` in the repo
2. Clones the repo (if needed) into `~/.catherdd/projects/my-project/main/`
3. Runs `git pull` to sync to the latest remote HEAD
4. Creates a Git worktree at `~/.catherdd/projects/my-project/worktrees/<id>/`
   on branch `feat/dark-mode`
5. Runs the `bootstrap` commands in the worktree (output streams to your terminal)
6. Allocates a real PTY, starts the agent process inside it, and attaches your
   terminal immediately (pass `-d` to skip auto-attach)

**Worktrees are first-class citizens.** An instance record lives exactly as
long as its worktree. `drop` is the only operation that removes both.
Everything else — `stop`, `finish`, daemon restart — only changes state.

---

## Building

```bash
go build -o bin/catherdd ./cmd/catherdd
go build -o bin/catherd  ./cmd/catherd
```

---

## Filesystem layout

```
~/.catherdd/
├─ projects/
│  └─ <project-name>/
│     ├─ main/          ← canonical git clone
│     └─ worktrees/
│        └─ <id>/       ← one git worktree per instance
├─ instances/
│  └─ <id>.json         ← persisted instance metadata (survives daemon restart)
├─ logs/
│  └─ <id>.log          ← PTY output + bootstrap + complete command output
└─ catherdd.sock        ← Unix domain socket
```

Project config lives in the catherdd repo itself:

```
<repo>/
├─ projects/             ← shared, tracked by git
│  └─ <name>/project.yaml
└─ projects.local/       ← personal, git-ignored
   └─ <name>/project.yaml
```

---

## Project definition (`project.yaml`)

```yaml
name: my-app
repo: git@github.com:example/my-app.git

bootstrap:
  - npm install

agent:
  command: claude
  args: []

dev:
  start:
    - npx expo start

# complete: commands run by `catherd finish`. Use {{branch}} for the branch name.
complete:
  - git push -u origin {{branch}}
  # - gh pr create --title "{{branch}}" --fill
```

Use `catherd project create <name>` to scaffold a new project file.

---

## CLI reference

### Project commands

```
catherd project create <name> [--global] [--repo <url>] [--agent <cmd>]
                         Define a new project (personal by default, --global for shared)
catherd project list     List defined projects
catherd main <project>   Print the main checkout path for a project
```

### Instance commands

```
catherd start <project> <branch> [-d]
                         Start a new agent instance on <branch>
                         Attaches immediately; use -d to skip
catherd attach <id>      Attach terminal to a running instance (detach: Ctrl-])
catherd stop <id>        Kill the agent; instance stays in list as KILLED
catherd restart <id> [-d]
                         Restart the agent in the existing worktree
                         Attaches immediately; use -d to skip
catherd finish <id>      Run complete commands; instance stays as FINISHED
catherd drop <id>        Delete the worktree and branch permanently (prompts first)
catherd list [--active]  List all instances (--active: exclude FINISHED)
catherd watch            Live dashboard (refreshes every second, Ctrl-C to exit)
catherd logs <id> [-f]   Print buffered output; -f to follow
catherd worktree <id>    Print the worktree path for an instance
catherd prune [--finished]
                         Drop all EXITED/CRASHED/KILLED instances
                         (--finished: also include FINISHED)
```

### Daemon commands

```
catherd daemon install    Register catherdd as a login LaunchAgent
catherd daemon uninstall  Remove the LaunchAgent
catherd daemon status     Show whether the LaunchAgent is installed and running
```

---

## Instance states

| State      | Meaning                                              |
|------------|------------------------------------------------------|
| `RUNNING`  | Agent process is alive                               |
| `WAITING`  | Agent is idle (no PTY output for >2 s)               |
| `ATTACHED` | A catherd client is currently attached               |
| `EXITED`   | Agent exited cleanly (code 0)                        |
| `CRASHED`  | Agent exited with a non-zero code                    |
| `KILLED`   | Agent was stopped with `catherd stop`                |
| `FINISHED` | Instance completed via `catherd finish`              |

State transitions:

```
RUNNING/WAITING ←→ ATTACHED   (attach / detach)
RUNNING/WAITING/ATTACHED → EXITED    (agent exits 0)
RUNNING/WAITING/ATTACHED → CRASHED   (agent exits non-zero, or daemon was killed)
RUNNING/WAITING/ATTACHED → KILLED    (catherd stop)
any live state           → FINISHED  (catherd finish)
EXITED/CRASHED/KILLED/FINISHED → RUNNING  (catherd restart)
```

Instances in any terminal state (`EXITED`, `CRASHED`, `KILLED`, `FINISHED`) are
still visible in `catherd list` and their worktrees are intact on disk. Use
`catherd drop <id>` to permanently delete a worktree and its record.

---

## Attach / detach

`catherd attach` behaves like `tmux attach`:

- Your terminal is connected directly to the agent's PTY.
- All keystrokes are forwarded to the agent.
- Terminal resize events (SIGWINCH) are forwarded automatically.
- **Detach** with **Ctrl-]** — the agent keeps running in the background.

`catherd start` and `catherd restart` attach automatically after the agent
starts. Use `-d` to skip and leave the agent running in the background.

---

## Example workflow

```bash
# 0. Register the daemon (once, on macOS)
catherd daemon install

# 1. Define a project
catherd project create my-app --repo git@github.com:you/my-app.git
# Edit projects.local/my-app/project.yaml to add bootstrap steps

# 2. Start an agent on a branch (bootstrap output streams here; auto-attaches)
catherd start my-app feat/dark-mode

# … interact with the agent, then Ctrl-] to detach …

# 3. Check all instances
catherd list

# 4. Open a live dashboard
catherd watch

# 5. Read logs without attaching
catherd logs 1
catherd logs 1 -f   # follow

# 6. Stop the agent (keeps worktree and record)
catherd stop 1

# 7. Restart it in the same worktree
catherd restart 1

# 8. Finish the work (runs complete commands: git push, gh pr create, etc.)
catherd finish 1

# 9. Clean up dead instances
catherd prune

# 10. Permanently delete a worktree and its record
catherd drop 1
```

---

## Daemon management (macOS LaunchAgent)

On macOS, PTY allocation requires running inside a full user login session.
Register `catherdd` as a LaunchAgent so it starts automatically at login with
the correct privileges:

```bash
catherd daemon install
```

This writes `~/Library/LaunchAgents/com.catherd.daemon.plist` and starts the
daemon immediately. Daemon output is written to `~/.catherdd/daemon.log`.

> **Note:** `catherd` also auto-starts the daemon on demand when you run any
> command that requires it. The LaunchAgent approach is preferred on macOS
> because it avoids PTY permission errors that occur when launching a detached
> background process directly.

Instance metadata is persisted to `~/.catherdd/instances/<id>.json`. When the
daemon restarts, all instances reload with their last known state. Instances
that were live when the daemon was killed are marked `CRASHED` on reload.

---

## macOS only

catherdd targets macOS. It uses:

- Unix domain sockets for IPC
- `posix_openpt` / `openpty` (via `github.com/creack/pty`) for PTY allocation
- `SIGKILL` on the process group for clean teardown
- `launchd` for daemon lifecycle management
