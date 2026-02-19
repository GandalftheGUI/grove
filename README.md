# catherdd

A local daemon + CLI that supervises AI coding agents running in isolated Git
worktrees on a single macOS machine.

catherdd is **not** an AI model.  It is a process supervisor and developer UX
layer around existing agent CLIs (e.g. `claude`, `aider`).

---

## Binaries

| Binary     | Role                                    |
|------------|-----------------------------------------|
| `catherdd` | Background daemon (Unix socket server)  |
| `catherd`  | CLI client                              |

You may alias `catherd` to `ch` in your shell profile if you prefer a shorter
invocation.

---

## How it works

```
catherd start my-project "add dark mode"
```

1. Reads `~/.catherdd/projects/my-project/project.yaml`
2. Clones the repo (if needed) into `~/.catherdd/projects/my-project/main/`
3. Creates a Git worktree at `~/.catherdd/projects/my-project/worktrees/<id>/`
   on branch `agent/<id>`
4. Runs the `bootstrap` commands once in the worktree
5. Allocates a real PTY and starts the agent process inside it
6. The agent runs in the background; you can attach at any time

---

## Building

```bash
go build -o bin/catherdd ./cmd/catherdd
go build -o bin/catherd  ./cmd/catherd
```

Or with `make` (if you add a Makefile):

```bash
make
```

---

## Filesystem layout

```
~/.catherdd/
├─ projects/
│  └─ <project-name>/
│     ├─ project.yaml
│     ├─ main/          ← canonical git clone
│     └─ worktrees/
│        └─ <id>/       ← one git worktree per instance
├─ instances/
│  └─ <id>.json         ← persisted instance metadata
├─ logs/
│  └─ <id>.log          ← rolling PTY output
└─ catherdd.sock        ← Unix domain socket
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
```

Place this file at `~/.catherdd/projects/my-app/project.yaml`.

---

## CLI reference

```
catherd start <project> "<task>"   Create and start a new agent instance
catherd list                       List all instances and their states
catherd attach <id>                Attach your terminal to the instance PTY
catherd logs <id>                  Print buffered output for an instance
catherd destroy <id>               Stop and remove an instance
```

### Attach / detach

`catherd attach` behaves like `tmux attach`:

- Your terminal is connected directly to the agent's PTY.
- All keystrokes are forwarded to the agent.
- Terminal resize events (SIGWINCH) are forwarded automatically.
- **Detach** with **Ctrl-]** — the agent keeps running in the background.

---

## Example workflow

```bash
# 1. Define a project
mkdir -p ~/.catherdd/projects/my-app
cat > ~/.catherdd/projects/my-app/project.yaml <<'EOF'
name: my-app
repo: git@github.com:you/my-app.git
bootstrap:
  - npm install
agent:
  command: claude
  args: []
EOF

# 2. Start an agent on a task
catherd start my-app "add a dark mode toggle to the settings page"
# → started instance a1b2c3d4

# 3. Watch what it's doing
catherd attach a1b2c3d4
# … interact or just observe …
# Ctrl-] to detach

# 4. Check all instances
catherd list

# 5. Read logs without attaching
catherd logs a1b2c3d4

# 6. Clean up
catherd destroy a1b2c3d4
```

---

## Auto-start

`catherd` automatically starts `catherdd` in the background when needed.  You
do not need to run the daemon manually.

---

## Instance states

| State      | Meaning                                   |
|------------|-------------------------------------------|
| `RUNNING`  | Agent process is alive                    |
| `WAITING`  | Agent is idle / waiting for human input   |
| `ATTACHED` | A catherd client is currently attached    |
| `EXITED`   | Agent exited with code 0                  |
| `CRASHED`  | Agent exited with a non-zero code         |

---

## macOS only

catherdd targets macOS.  It uses:

- Unix domain sockets for IPC
- `posix_openpt` / `openpty` (via `github.com/creack/pty`) for PTY allocation
- `SIGKILL` on the process group for clean teardown
