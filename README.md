# Grove

Grove is a local orchestration system for running and supervising multiple AI
coding agents in parallel, with strong isolation and a fast, debuggable
developer experience.

Each agent runs in its own Git worktree and Docker container, preventing
conflicts across dependencies, services, ports, and working state.

> Think: tmux + git worktree + Docker, purpose-built for AI agents.

<img width="924" height="360" alt="image" src="https://github.com/user-attachments/assets/4c2e7d54-c75e-4114-87d1-2d39ba11ebd4" />

---

## Why Grove exists

Running AI coding agents in parallel is hard because they tend to:

- Share global state (ports, databases, node_modules, env vars)
- Mutate the same working tree
- Leave behind half-configured environments
- Be painful to stop, restart, or resume

*Grove makes isolation and lifecycle management first-class, so parallel agent
workflows are safe and repeatable on a single machine.*

---

## What Grove gives you

- **True isolation** — agents run in dedicated worktrees and containers
- **Fast iteration** — restarts reuse the existing container and worktree
- **Deterministic setup** — project-owned grove.yaml defines containers, setup,
  checks, and finish steps
- **Process supervision** — restartable, attachable agents with durable state
- **Low ceremony** — one command to start, attach, check, finish, or drop

*If you want to go deeper, see [TECHNICAL.md](./docs/TECHNICAL.md)*

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

# Quick start

### Requirements

- **Docker** (required): [Get Docker](https://docs.docker.com/get-docker/)
- **macOS or Linux**
- **Go 1.22+** (to build from source)

### Build

```bash
go build -o bin/groved ./cmd/groved
go build -o bin/grove  ./cmd/grove
export PATH="$PWD/bin:$PATH"
```

### Start the daemon

```bash
# macOS (recommended)
grove daemon install

# Linux: run groved via systemd (see docs/TECHNICAL.md) or start it manually:
# groved
```

### Run your first instance

```bash
grove project create my-app --repo git@github.com:you/my-app.git
grove start my-app feat/dark-mode
```

Detach with **Ctrl-]**. Reattach with:

```bash
grove attach 1
```

---

## Credentials

Grove injects env vars from `~/.grove/env` (dotenv format) into the container. For Claude, the easiest path is:

```bash
grove token
```

Full details (and alternatives like API keys) are in `docs/TECHNICAL.md`.

---

## Docs

- [TECHNICAL.md](./docs/TECHNICAL.md) — architecture, `grove.yaml` reference, filesystem layout, full CLI reference, daemon management, lifecycle details

---

## Fit / non-goals

Grove is a good fit for projects where parallel instances are meaningful — i.e. where you’d also benefit from parallel CI jobs:

- Web apps with databases (Rails, Django, Laravel)
- Node / Python / Go / Rust services
- Anything with a `docker-compose.yml`

Grove is not a good fit for:

- iOS / macOS app development (Xcode + simulator + code signing don’t work in Linux containers, and the simulator conflicts across instances)
- Projects that own global host resources (fixed host ports/sockets outside the container)
