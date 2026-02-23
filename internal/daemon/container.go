package daemon

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// validateDocker checks that Docker is available by running "docker info".
func validateDocker() error {
	cmd := exec.Command("docker", "info")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker is not available (%w)\nInstall Docker: https://docs.docker.com/get-docker/", err)
	}
	return nil
}

// startContainer dispatches to the single-container or compose variant.
// Returns the exec target container name.
func startContainer(p *Project, instanceID, worktreeDir string, w io.Writer) (string, error) {
	if p.Container.Compose != "" {
		return startComposeContainer(p, instanceID, worktreeDir, w)
	}
	if p.Container.Image == "" {
		groveYAML := filepath.Join(p.MainDir(), "grove.yaml")
		return "", fmt.Errorf("no container configured in %s\nadd a 'container:' section, e.g.:\n\n  container:\n    image: ubuntu:24.04\n", groveYAML)
	}
	return startSingleContainer(p, instanceID, worktreeDir, w)
}

// startSingleContainer runs:
//
//	docker run -d --name grove-<id> -v <worktreeDir>:<workdir> -w <workdir> [mounts...] <image> sleep infinity
func startSingleContainer(p *Project, instanceID, worktreeDir string, w io.Writer) (string, error) {
	name := "grove-" + instanceID
	workdir := p.containerWorkdir()
	image := p.Container.Image

	args := []string{"run", "-d",
		"--name", name,
		"-v", worktreeDir + ":" + workdir,
		"-w", workdir,
	}
	for _, m := range buildMounts(p, w) {
		args = append(args, "-v", m[0]+":"+m[1])
	}
	args = append(args, image, "sleep", "infinity")

	fmt.Fprintf(w, "Starting container %s (image: %s) …\n", name, image)
	cmd := exec.Command("docker", args...)
	out, err := cmd.CombinedOutput()
	if len(out) > 0 {
		w.Write(out)
	}
	if err != nil {
		return "", fmt.Errorf("docker run: %w", err)
	}
	return name, nil
}

// startComposeContainer writes a temporary override YAML that bind-mounts the
// worktree (and any extra mounts) into the app service, then runs:
//
//	docker compose -p grove-<id> -f <composefile> -f <overridefile> up -d
//
// Returns "grove-<id>-<service>-1" as the exec target.
func startComposeContainer(p *Project, instanceID, worktreeDir string, w io.Writer) (string, error) {
	project := "grove-" + instanceID
	service := p.containerService()
	workdir := p.containerWorkdir()
	composeFile := p.Container.Compose

	// Build the volumes block: worktree first, then any extra mounts.
	volumes := fmt.Sprintf("      - type: bind\n        source: %s\n        target: %s\n", worktreeDir, workdir)
	for _, m := range buildMounts(p, w) {
		volumes += fmt.Sprintf("      - type: bind\n        source: %s\n        target: %s\n", m[0], m[1])
	}
	overrideContent := fmt.Sprintf("services:\n  %s:\n    volumes:\n%s", service, volumes)

	overrideFile, err := os.CreateTemp("", "grove-compose-override-*.yml")
	if err != nil {
		return "", fmt.Errorf("create compose override: %w", err)
	}
	overridePath := overrideFile.Name()
	if _, err := overrideFile.WriteString(overrideContent); err != nil {
		overrideFile.Close()
		os.Remove(overridePath)
		return "", fmt.Errorf("write compose override: %w", err)
	}
	overrideFile.Close()
	defer os.Remove(overridePath)

	fmt.Fprintf(w, "Starting compose stack %s (compose: %s, service: %s) …\n", project, composeFile, service)
	cmd := exec.Command("docker", "compose",
		"-p", project,
		"-f", composeFile,
		"-f", overridePath,
		"up", "-d",
	)
	cmd.Stdout = w
	cmd.Stderr = w
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker compose up: %w", err)
	}

	// Exec target: "grove-<id>-<service>-1"
	return project + "-" + service + "-1", nil
}

// stopContainer tears down the container or compose stack for an instance.
// If composeProject is non-empty, tears down the compose stack; otherwise
// stops and removes the single container.
func stopContainer(containerName, composeProject string) {
	if composeProject != "" {
		exec.Command("docker", "compose", "-p", composeProject, "down", "-v").Run()
		return
	}
	exec.Command("docker", "stop", containerName).Run()
	exec.Command("docker", "rm", containerName).Run()
}

// execInContainer runs cmd inside the named container using "docker exec".
func execInContainer(containerName, cmd string, w io.Writer) error {
	c := exec.Command("docker", "exec", containerName, "sh", "-c", cmd)
	c.Stdout = w
	c.Stderr = w
	if err := c.Run(); err != nil {
		return fmt.Errorf("exec in container %s: %w", containerName, err)
	}
	return nil
}

// ensureAgentInstalled checks whether agentCmd is present in the container and,
// if not, attempts to install it automatically for known agents.
// All output (install progress, errors) is written to w so it appears in the
// instance log and in the user's terminal during "grove start".
func ensureAgentInstalled(agentCmd, containerName string, w io.Writer) error {
	// Fast path: agent already installed.
	check := exec.Command("docker", "exec", containerName,
		"sh", "-c", "command -v "+agentCmd+" >/dev/null 2>&1")
	if check.Run() == nil {
		return nil
	}

	// Auto-install for known agents.
	var installScript, startSnippet string
	switch agentCmd {
	case "claude":
		// Claude Code requires Node.js 18+.  We download a pre-built binary
		// from nodejs.org — no package manager, no setup scripts, no GPG keys.
		// Only curl (or wget) and tar are required, which are present in
		// virtually every container image.  Alpine is handled separately via
		// apk because its packages are already modern.
		installScript = `set -e
node_ok() {
  command -v node >/dev/null 2>&1 || return 1
  major=$(node --version 2>/dev/null | sed 's/v\([0-9]*\).*/\1/')
  [ "${major:-0}" -ge 18 ]
}
if ! node_ok; then
  echo "Installing Node.js 20 LTS..."
  ARCH=$(uname -m)
  case "$ARCH" in
    x86_64)       NODE_ARCH=x64 ;;
    aarch64|arm64) NODE_ARCH=arm64 ;;
    *) echo "unsupported CPU architecture: $ARCH" >&2; exit 1 ;;
  esac
  NODE_URL="https://nodejs.org/dist/v20.11.0/node-v20.11.0-linux-${NODE_ARCH}.tar.gz"
  if command -v apk >/dev/null 2>&1; then
    apk add --no-cache nodejs npm
  elif command -v curl >/dev/null 2>&1; then
    curl -fsSL "$NODE_URL" | tar -xz -C /usr/local --strip-components=1
  elif command -v wget >/dev/null 2>&1; then
    wget -qO- "$NODE_URL" | tar -xz -C /usr/local --strip-components=1
  elif command -v apt-get >/dev/null 2>&1; then
    apt-get update -qq && apt-get install -y -qq curl
    curl -fsSL "$NODE_URL" | tar -xz -C /usr/local --strip-components=1
  else
    echo "Cannot install Node.js: no curl, wget, or apk found in this container." >&2
    echo "Add node installation to 'start:' in grove.yaml" >&2
    exit 1
  fi
fi
npm install -g @anthropic-ai/claude-code
# Symlink into ~/.local/bin so claude can find itself at the path stored in ~/.claude.json
mkdir -p /root/.local/bin
CLAUDE_BIN=$(command -v claude 2>/dev/null || true)
if [ -n "$CLAUDE_BIN" ] && [ ! -e /root/.local/bin/claude ]; then
  ln -sf "$CLAUDE_BIN" /root/.local/bin/claude
fi`
		startSnippet = `  start:
    - curl -fsSL https://deb.nodesource.com/setup_lts.x | bash -
    - apt-get install -y nodejs
    - npm install -g @anthropic-ai/claude-code`
	case "aider":
		installScript = `set -e
if ! command -v pip >/dev/null 2>&1 && ! command -v pip3 >/dev/null 2>&1; then
  if command -v apt-get >/dev/null 2>&1; then
    apt-get update -qq && apt-get install -y -qq python3 python3-pip
  elif command -v apk >/dev/null 2>&1; then
    apk add --no-cache python3 py3-pip
  else
    echo "pip not found and no supported package manager available" >&2
    exit 1
  fi
fi
pip install aider-chat 2>/dev/null || pip3 install aider-chat`
		startSnippet = `  start:
    - pip install aider-chat`
	default:
		return fmt.Errorf("agent command %q not found in container %s\n"+
			"install it in your container image or add it to 'start:' in grove.yaml",
			agentCmd, containerName)
	}

	fmt.Fprintf(w, "Agent %q not found — auto-installing (this runs once per container)…\n", agentCmd)
	c := exec.Command("docker", "exec", containerName, "sh", "-c", installScript)
	c.Stdout = w
	c.Stderr = w
	if err := c.Run(); err != nil {
		return fmt.Errorf("auto-install of %q failed: %w\n"+
			"to install it yourself, add to grove.yaml:\n%s",
			agentCmd, err, startSnippet)
	}

	// Verify the install actually made the binary available.
	verify := exec.Command("docker", "exec", containerName,
		"sh", "-c", "command -v "+agentCmd+" >/dev/null 2>&1")
	if err := verify.Run(); err != nil {
		return fmt.Errorf("auto-install of %q appeared to succeed but the command is still not in PATH\n"+
			"check that the install placed the binary in a directory on $PATH inside the container",
			agentCmd)
	}

	fmt.Fprintf(w, "Agent %q installed successfully.\n", agentCmd)
	return nil
}

// restoreClaudeConfigIfMissing checks whether ~/.claude.json exists and, if not,
// restores the latest backup from ~/.claude/backups/ so the file can be
// bind-mounted into the container as /root/.claude.json.
func restoreClaudeConfigIfMissing(home string, w io.Writer) {
	configPath := filepath.Join(home, ".claude.json")
	if _, err := os.Stat(configPath); err == nil {
		return // already exists
	}
	backupsDir := filepath.Join(home, ".claude", "backups")
	entries, err := os.ReadDir(backupsDir)
	if err != nil || len(entries) == 0 {
		return // no backups to restore from
	}
	// Backups use timestamp suffixes; the last entry alphabetically is the newest.
	latest := entries[len(entries)-1]
	src := filepath.Join(backupsDir, latest.Name())
	data, err := os.ReadFile(src)
	if err != nil {
		return
	}
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		return
	}
	fmt.Fprintf(w, "Restored Claude config from backup: %s\n", latest.Name())
}

// buildMounts returns all (source, target) mount pairs for the container:
// auto-detected agent credentials followed by user-configured mounts.
// Each applied mount is logged to w. User-configured paths that don't exist
// on the host produce a warning; missing credential dirs are silently skipped
// (the agent may not be installed yet).
func buildMounts(p *Project, w io.Writer) [][2]string {
	home, _ := os.UserHomeDir()
	var mounts [][2]string

	// For claude: ensure ~/.claude.json exists on the host before mounting.
	// Claude stores its main config (including auth) at ~/.claude.json, separate
	// from the ~/.claude/ session directory. If only the directory was backed up,
	// restore from the latest backup so the bind mount can apply it.
	if p.Agent.Command == "claude" {
		restoreClaudeConfigIfMissing(home, w)
	}

	// Auto-mount credentials for known agents.
	var credsMounted int
	for _, pair := range agentCredentialMounts(p.Agent.Command, home) {
		if _, err := os.Stat(pair[0]); err == nil {
			fmt.Fprintf(w, "Mounting credentials: %s → %s\n", pair[0], pair[1])
			mounts = append(mounts, pair)
			credsMounted++
		}
	}
	if p.Agent.Command == "claude" && credsMounted == 0 {
		fmt.Fprintf(w, "Warning: no Claude credentials found on host (~/.claude or ~/.claude.json). Agent will show welcome/login.\n")
	}

	// User-configured extra mounts from grove.yaml.
	for _, m := range p.Container.Mounts {
		src, tgt := resolveMountPath(m, home)
		if _, err := os.Stat(src); err == nil {
			fmt.Fprintf(w, "Mounting: %s → %s\n", src, tgt)
			mounts = append(mounts, [2]string{src, tgt})
		} else {
			fmt.Fprintf(w, "Warning: skipping mount %q — path not found on host\n", m)
		}
	}

	return mounts
}

// agentCredentialMounts returns (source, target) pairs for known agent CLIs.
func agentCredentialMounts(agentCmd, home string) [][2]string {
	switch agentCmd {
	case "claude":
		return [][2]string{
			{filepath.Join(home, ".claude"), "/root/.claude"},
			{filepath.Join(home, ".claude.json"), "/root/.claude.json"},
		}
	case "aider":
		return [][2]string{
			{filepath.Join(home, ".aider"), "/root/.aider"},
		}
	}
	return nil
}

// resolveMountPath expands a user-specified mount path to (source, target).
// ~/foo  →  (/home/user/foo, /root/foo)
// /abs   →  (/abs, /abs)
func resolveMountPath(m, home string) (source, target string) {
	if m == "~" {
		return home, "/root"
	}
	if strings.HasPrefix(m, "~/") {
		rel := m[2:]
		return filepath.Join(home, rel), "/root/" + rel
	}
	return m, m
}

// loadEnvFile reads a dotenv-style file at <rootDir>/env and returns the
// key-value pairs. Lines starting with # and blank lines are ignored.
// Returns an empty map (not an error) if the file does not exist.
func loadEnvFile(rootDir string) map[string]string {
	env := map[string]string{}
	f, err := os.Open(filepath.Join(rootDir, "env"))
	if err != nil {
		return env
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		env[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return env
}

