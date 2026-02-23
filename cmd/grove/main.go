// grove – the CLI client for the groved daemon.
//
// Usage:
//
//	grove project create <name>      – define a new project
//	grove project list               – list defined projects
//	grove start <project> "<task>"   – create and start a new agent instance
//	grove list                       – list all instances
//	grove attach <instance-id>       – attach your terminal to an instance PTY
//	grove logs <instance-id>         – print buffered logs for an instance
//	grove destroy <instance-id>      – stop and remove an instance
//
// grove will start the daemon automatically if it is not already running.
// Detach from an attached session with Ctrl-] (0x1D).
package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ianremillard/grove/internal/proto"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "project":
		cmdProject()
	case "start":
		cmdStart()
	case "list":
		cmdList()
	case "attach":
		cmdAttach()
	case "watch":
		cmdWatch()
	case "logs":
		cmdLogs()
	case "stop":
		cmdStop()
	case "restart":
		cmdRestart()
	case "drop":
		cmdDrop()
	case "finish":
		cmdFinish()
	case "check":
		cmdCheck()
	case "prune":
		cmdPrune()
	case "dir":
		cmdDir()
	case "daemon":
		cmdDaemon()
	default:
		fmt.Fprintf(os.Stderr, "grove: unknown command %q\n", os.Args[1])
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `grove – supervise AI coding agent instances

Project commands:
  project create <name> [--repo <url>]
                           Register a new project (name + repo URL)
  project list             List registered projects (numbered)
  project delete <name|#>  Remove a project and all its worktrees
  project dir <name|#>     Print the main checkout path for a project

Instance commands:
  start <project|#> <branch> [-d]
                                 Start a new agent instance on <branch> (attaches immediately; -d to skip)
                                 <project> may be a name or the number from 'project list'
  attach <instance-id>           Attach terminal to an instance (detach: Ctrl-])
  stop <instance-id>             Kill the agent; instance stays in list as KILLED
  restart <instance-id> [-d]     Restart agent in existing worktree (attaches immediately; -d to skip)
  check <instance-id>            Run check commands concurrently; instance returns to WAITING
  finish <instance-id>           Run finish steps; instance stays as FINISHED
  drop <instance-id>             Delete the worktree and branch permanently
  list [--active]                List all instances (--active: exclude FINISHED)
  logs <instance-id> [-f]        Print buffered output for an instance
  watch                          Live dashboard (refreshes every second, Ctrl-C to exit)
  prune [--finished]             Drop all exited/crashed instances (--finished: also FINISHED)
  dir <instance-id>              Print the worktree path for an instance

Daemon commands:
  daemon install           Register groved as a login LaunchAgent
  daemon uninstall         Remove the LaunchAgent
  daemon status            Show whether the LaunchAgent is installed and running
  daemon logs [-f] [-n N]  Print daemon log (-f follow, -n tail lines)`)
}

// ─── Subcommand implementations ───────────────────────────────────────────────

func cmdProject() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: grove project <create|list|delete|dir>")
		os.Exit(1)
	}
	switch os.Args[2] {
	case "create":
		cmdProjectCreate()
	case "list":
		cmdProjectList()
	case "delete":
		cmdProjectDelete()
	case "dir":
		cmdProjectDir()
	default:
		fmt.Fprintf(os.Stderr, "grove: unknown project subcommand %q\n", os.Args[2])
		os.Exit(1)
	}
}

// cmdProjectCreate handles: grove project create <name> [--repo <url>]
//
// Writes a minimal registration (name + repo URL) to
// ~/.grove/projects/<name>/project.yaml. All other config (container, agent,
// start, finish, check) belongs in grove.yaml in the project repo.
func cmdProjectCreate() {
	if len(os.Args) < 4 || os.Args[3] == "" || os.Args[3][0] == '-' {
		fmt.Fprintln(os.Stderr, "usage: grove project create <name> [--repo <url>]")
		os.Exit(1)
	}
	name := os.Args[3]

	fs := flag.NewFlagSet("project create", flag.ExitOnError)
	repo := fs.String("repo", "", "git remote URL (can be added later)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: grove project create <name> [--repo <url>]")
		fs.PrintDefaults()
	}
	fs.Parse(os.Args[4:])

	projectDir := filepath.Join(rootDir(), "projects", name)
	if _, err := os.Stat(filepath.Join(projectDir, "project.yaml")); err == nil {
		fmt.Fprintf(os.Stderr, "grove: project %q already exists at %s\n", name, projectDir)
		os.Exit(1)
	}
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "grove: %v\n", err)
		os.Exit(1)
	}

	yamlPath := filepath.Join(projectDir, "project.yaml")
	content := fmt.Sprintf("name: %s\nrepo: %s\n", name, *repo)
	if err := os.WriteFile(yamlPath, []byte(content), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "grove: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("\n%s✓  Created project%s %s%q%s\n\n", colorGreen+colorBold, colorReset, colorCyan, name, colorReset)
	fmt.Printf("%sConfig:%s %s%s%s\n\n", colorBold, colorReset, colorCyan, yamlPath, colorReset)
	fmt.Printf("%sNext step:%s\n\n", colorBold, colorReset)
	if *repo == "" {
		fmt.Printf("  %s1.%s Edit the file to set your repo URL\n", colorBold, colorReset)
		fmt.Printf("  %s2.%s Start an instance\n", colorBold, colorReset)
	} else {
		fmt.Printf("  %s1.%s Start an instance\n", colorBold, colorReset)
	}
	fmt.Printf("     %sgrove start %s <branch>%s\n\n", colorDim, name, colorReset)
}

// projectEntry holds the parsed fields grove cares about from a registration.
type projectEntry struct {
	name string
	repo string
}

// loadProjectEntries scans ~/.grove/projects/ and returns all registered
// projects in directory order (alphabetical by folder name).
func loadProjectEntries() []projectEntry {
	projectsDir := filepath.Join(rootDir(), "projects")
	dirEntries, err := os.ReadDir(projectsDir)
	if err != nil {
		return nil
	}

	var entries []projectEntry
	for _, e := range dirEntries {
		if !e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(projectsDir, e.Name(), "project.yaml"))
		if err != nil {
			continue
		}
		var p struct {
			Name string `yaml:"name"`
			Repo string `yaml:"repo"`
		}
		if err := yaml.Unmarshal(data, &p); err != nil {
			continue
		}
		name := p.Name
		if name == "" {
			name = e.Name()
		}
		repo := p.Repo
		if repo == "" {
			repo = "(no repo)"
		}
		entries = append(entries, projectEntry{name, repo})
	}
	return entries
}

// resolveProject resolves a project argument that may be a 1-based index
// (e.g. "1", "2") or a literal project name. Exits with an error message
// if a numeric index is out of range.
func resolveProject(arg string) string {
	n, err := strconv.Atoi(arg)
	if err != nil {
		return arg // not a number — use as-is
	}
	entries := loadProjectEntries()
	if n < 1 || n > len(entries) {
		fmt.Fprintf(os.Stderr, "grove: project index %d out of range (have %d project(s))\n", n, len(entries))
		os.Exit(1)
	}
	return entries[n-1].name
}

// cmdProjectList handles: grove project list
//
// Scans ~/.grove/projects/ and prints a numbered summary table.
// This is a pure filesystem operation — no daemon required.
func cmdProjectList() {
	entries := loadProjectEntries()
	if len(entries) == 0 {
		fmt.Printf("%sno projects defined%s\n", colorDim, colorReset)
		return
	}

	fmt.Printf("%s%-4s  %-20s  %s%s\n", colorBold, "#", "NAME", "REPO", colorReset)
	fmt.Printf("%s%-4s  %-20s  %s%s\n", colorDim, "----", "--------------------", "----", colorReset)
	for i, e := range entries {
		fmt.Printf("%-4d  %-20s  %s\n", i+1, e.name, e.repo)
	}
}

// cmdProjectDelete handles: grove project delete <name>
//
// Prompts for confirmation (project and all worktrees are removed), then
// deletes the entire project directory under ~/.grove/projects/<name>/.
func cmdProjectDelete() {
	if len(os.Args) < 4 || os.Args[3] == "" {
		fmt.Fprintln(os.Stderr, "usage: grove project delete <name|#>")
		os.Exit(1)
	}
	name := resolveProject(os.Args[3])

	projectDir := filepath.Join(rootDir(), "projects", name)
	yamlPath := filepath.Join(projectDir, "project.yaml")
	if _, err := os.Stat(yamlPath); err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "grove: project %q not found\n", name)
		} else {
			fmt.Fprintf(os.Stderr, "grove: %v\n", err)
		}
		os.Exit(1)
	}

	// Count live instances so the warning can be specific.
	var instanceCount int
	if resp, err := tryRequest(proto.Request{Type: proto.ReqList}); err == nil {
		for _, inst := range resp.Instances {
			if inst.Project == name {
				instanceCount++
			}
		}
	}

	fmt.Printf("\n%s⚠  Remove project%s %s%q%s\n\n", colorYellow+colorBold, colorReset, colorCyan, name, colorReset)
	if instanceCount > 0 {
		fmt.Printf("  This will %sstop and remove %d instance(s)%s, delete all worktrees,\n", colorBold, instanceCount, colorReset)
		fmt.Printf("  and remove the project.\n\n")
	} else {
		fmt.Printf("  This will delete the project and %sall its worktrees%s.\n\n", colorBold, colorReset)
	}
	fmt.Printf("%sContinue?%s [y/N] ", colorBold, colorReset)

	reader := bufio.NewReader(os.Stdin)
	answer, _ := reader.ReadString('\n')
	answer = strings.TrimSpace(answer)
	if answer != "y" && answer != "Y" {
		fmt.Printf("%saborted%s\n", colorDim, colorReset)
		return
	}

	// Drop all instances belonging to this project before removing the
	// project directory, so they don't linger in watch/list.
	if resp, err := tryRequest(proto.Request{Type: proto.ReqList}); err == nil {
		for _, inst := range resp.Instances {
			if inst.Project == name {
				tryRequest(proto.Request{Type: proto.ReqDrop, InstanceID: inst.ID})
			}
		}
	}

	if err := os.RemoveAll(projectDir); err != nil {
		fmt.Fprintf(os.Stderr, "grove: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("\n%s✓  Deleted project%s %s%q%s\n\n", colorGreen+colorBold, colorReset, colorCyan, name, colorReset)
}

// stripBoolFlag removes every occurrence of the given short/long flag from
// args and returns (filtered, found). This lets the flag appear anywhere —
// before or after positional arguments — regardless of flag.Parse stopping at
// the first non-flag argument.
func stripBoolFlag(args []string, short, long string) ([]string, bool) {
	out := make([]string, 0, len(args))
	found := false
	for _, a := range args {
		if a == "-"+short || a == "--"+short || a == "-"+long || a == "--"+long {
			found = true
		} else {
			out = append(out, a)
		}
	}
	return out, found
}

func cmdStart() {
	rawArgs, detach := stripBoolFlag(os.Args[2:], "d", "detach")
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: grove start <project|#> <branch> [-d]")
	}
	fs.Parse(rawArgs)
	args := fs.Args()
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: grove start <project|#> <branch> [-d]")
		os.Exit(1)
	}
	project := resolveProject(args[0])
	branch := args[1]

	agentEnv := ensureAgentCredentials(project)

	socketPath := daemonSocket()
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "grove: %v\n", err)
		os.Exit(1)
	}

	if err := writeRequest(conn, proto.Request{
		Type:     proto.ReqStart,
		Project:  project,
		Branch:   branch,
		AgentEnv: agentEnv,
	}); err != nil {
		conn.Close()
		fmt.Fprintf(os.Stderr, "grove: %v\n", err)
		os.Exit(1)
	}

	resp, err := readResponse(conn)
	if err != nil {
		conn.Close()
		fmt.Fprintf(os.Stderr, "grove: %v\n", err)
		os.Exit(1)
	}
	if !resp.OK {
		conn.Close()
		if resp.InitPath != "" {
			// Project exists but has no grove.yaml — prompt the user to create one.
			promptCreateProjectConfig(resp.InitPath, project)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "grove: %s\n", resp.Error)
		fmt.Fprintf(os.Stderr, "grove: check daemon logs with: grove daemon logs -n 100\n")
		os.Exit(1)
	}

	// Stream any setup output (clone, pull, bootstrap) the daemon buffered.
	io.Copy(os.Stdout, conn)
	conn.Close()

	fmt.Printf("\n%s✓  Started instance%s %s%s%s\n\n", colorGreen+colorBold, colorReset, colorCyan, resp.InstanceID, colorReset)

	if !detach {
		doAttach(resp.InstanceID)
	}
}

// promptCreateProjectConfig is called when the daemon reports that the project
// has no .grove/project.yaml in its repository.  It asks the user whether to
// create a boilerplate file, writes it if they agree, then exits with
// instructions to edit, commit, and re-run.
const (
	colorBold   = "\033[1m"
	colorDim    = "\033[2m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorCyan   = "\033[36m"
	colorReset  = "\033[0m"
)

// ensureAgentCredentials checks whether the required credentials for the
// project's agent are available. If not, it prompts the user interactively
// and saves the token to ~/.grove/env. Returns env vars to pass through the
// request for this session.
func ensureAgentCredentials(project string) map[string]string {
	agentCmd := detectAgentCommand(project)
	if agentCmd != "claude" {
		return nil
	}

	root := rootDir()
	envFile := loadCLIEnvFile(root)

	// Check all possible sources for a Claude token.
	if envFile["CLAUDE_CODE_OAUTH_TOKEN"] != "" ||
		envFile["ANTHROPIC_API_KEY"] != "" ||
		os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != "" ||
		os.Getenv("ANTHROPIC_API_KEY") != "" {
		return nil
	}

	// No token found — prompt the user.
	fmt.Printf("\n%sClaude authentication required.%s\n\n", colorYellow+colorBold, colorReset)
	fmt.Printf("Generate a long-lived token by running:\n\n")
	fmt.Printf("    %sclaude setup-token%s\n\n", colorCyan, colorReset)
	fmt.Printf("Then paste the token below.\n\n")
	fmt.Printf("%sToken%s (or Enter to skip): ", colorBold, colorReset)

	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return nil
	}
	token := strings.TrimSpace(scanner.Text())
	if token == "" {
		return nil
	}

	// Save to ~/.grove/env so the user never has to do this again.
	envPath := filepath.Join(root, "env")
	f, err := os.OpenFile(envPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err == nil {
		fmt.Fprintf(f, "CLAUDE_CODE_OAUTH_TOKEN=%s\n", token)
		f.Close()
		fmt.Printf("\n%s✓  Saved to %s%s\n\n", colorGreen, envPath, colorReset)
	}

	return map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": token}
}

// detectAgentCommand reads the project's grove.yaml to determine the agent
// command. Returns "" if the file doesn't exist or has no agent configured.
func detectAgentCommand(project string) string {
	root := rootDir()
	groveYAML := filepath.Join(root, "projects", project, "main", "grove.yaml")
	data, err := os.ReadFile(groveYAML)
	if err != nil {
		return ""
	}
	var cfg struct {
		Agent struct {
			Command string `yaml:"command"`
		} `yaml:"agent"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return ""
	}
	return cfg.Agent.Command
}

// loadCLIEnvFile is a simple dotenv parser matching the daemon's loadEnvFile.
func loadCLIEnvFile(root string) map[string]string {
	env := map[string]string{}
	data, err := os.ReadFile(filepath.Join(root, "env"))
	if err != nil {
		return env
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
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

// warnIfDockerUnavailable prints a human-readable error to stderr when Docker
// is not running or not installed.  Called after a daemon startup failure so
// the user knows why, without having to dig through daemon.log.
func warnIfDockerUnavailable() {
	cmd := exec.Command("docker", "info")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if cmd.Run() != nil {
		fmt.Fprintf(os.Stderr, "%sgrove requires Docker.%s Docker does not appear to be running.\n", colorRed+colorBold, colorReset)
		fmt.Fprintf(os.Stderr, "  Start Docker Desktop or install it: https://docs.docker.com/get-docker/\n")
	}
}

func promptCreateProjectConfig(mainDir, projectName string) {
	configPath := filepath.Join(mainDir, "grove.yaml")

	fmt.Printf("\n%s⚠  No grove.yaml found in %s%s\n\n", colorYellow+colorBold, projectName, colorReset)
	fmt.Printf("  This file tells grove how to set up the container, run the agent,\n")
	fmt.Printf("  and finish the work. Commit it once and every grove user gets the\n")
	fmt.Printf("  same setup automatically — no per-machine configuration needed.\n\n")

	fmt.Printf("%sCreate a boilerplate now?%s [Y/n] ", colorBold, colorReset)

	reader := bufio.NewReader(os.Stdin)
	answer, _ := reader.ReadString('\n')
	answer = strings.TrimSpace(answer)
	if answer != "" && answer != "y" && answer != "Y" {
		fmt.Printf("%saborted%s\n", colorDim, colorReset)
		return
	}

	if err := os.WriteFile(configPath, []byte(projectConfigBoilerplate), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "grove: %v\n", err)
		return
	}

	fmt.Printf("\n%s✓  Created%s %s%s%s\n\n", colorGreen+colorBold, colorReset, colorCyan, configPath, colorReset)
	fmt.Printf("%sNext steps:%s\n\n", colorBold, colorReset)
	fmt.Printf("  %s1.%s Edit the file to match your project\n", colorBold, colorReset)
	fmt.Printf("     %s%s%s\n\n", colorDim, configPath, colorReset)
	fmt.Printf("  %s2.%s Commit it\n", colorBold, colorReset)
	fmt.Printf("     %sgit -C %s add grove.yaml%s\n", colorDim, mainDir, colorReset)
	fmt.Printf("     %sgit -C %s commit -m 'Add grove.yaml'%s\n\n", colorDim, mainDir, colorReset)
	fmt.Printf("  %s3.%s Re-run\n", colorBold, colorReset)
	fmt.Printf("     %sgrove start %s <branch>%s\n\n", colorDim, projectName, colorReset)
}

// projectConfigBoilerplate is written to grove.yaml (repo root) when a project
// has none.  It is designed to be self-explanatory with enough comments and
// examples that a developer can configure it without reading external docs.
const projectConfigBoilerplate = `# grove.yaml
# ─────────────────────────────────────────────────────────────────────────────
# Grove project configuration.
# Commit this file so everyone using Grove gets the same setup automatically.
# https://github.com/ianremillard/grove
# ─────────────────────────────────────────────────────────────────────────────

# ── Container ─────────────────────────────────────────────────────────────────
# Docker is required.  Each agent instance runs in its own container with the
# git worktree bind-mounted inside.
#
# Option A – single image (no external services):
#   container:
#     image: ruby:3.3      # any Docker image
#     workdir: /app        # working directory inside the container (default /app)
#
# Option B – docker-compose.yml (databases, caches, etc.):
#   container:
#     compose: docker-compose.yml   # path relative to repo root
#     service: app                  # service to exec into (default: app)
#     workdir: /app
#
container:
  image: ubuntu:24.04

# ── Start ─────────────────────────────────────────────────────────────────────
# Commands run once in each fresh worktree before the agent starts.
# The working directory is the worktree root.
#
# Best practice: delegate to an existing setup script so the logic lives in one
# place and can be run and tested independently of groved.
#
# Examples:
#   - ./scripts/bootstrap.sh        ← recommended if you have one
#   - make setup
#   - npm install
#   - pip install -r requirements.txt && pre-commit install
#   - bundle install
start:

# ── Agent ─────────────────────────────────────────────────────────────────────
# The AI coding agent to run inside each worktree PTY.
# 'grove attach' and 'grove start' connect your terminal directly to it.
#
# Common values:
#   claude   – Claude Code  (https://claude.ai/code)
#   aider    – Aider        (https://aider.chat)
#   sh       – plain shell  (useful for testing without an agent)
agent:
  command: claude
  args: []

# ── Check ─────────────────────────────────────────────────────────────────────
# Commands run concurrently by 'grove check <id>' inside the worktree directory.
# The daemon executes these while the agent stays alive; the instance returns to
# WAITING when all commands complete.
#
# Use these for verification steps: running tests, linting, type-checking, or
# starting a dev server to inspect the agent's work.
#
# Examples:
#   - npm test
#   - go test ./...
#   - make lint
check:

# ── Finish ────────────────────────────────────────────────────────────────────
# Commands run by 'grove finish <id>' inside the worktree directory.
# The daemon executes these — they complete even if you close your terminal.
# Use {{branch}} as a placeholder for the instance's branch name.
#
# The instance is marked FINISHED before these run, so a disconnection mid-way
# does not leave it in a broken state; output is preserved in the instance log.
#
# Tip: for anything beyond a simple push, delegate to a script so you can test
# the finish flow independently.
#
#   - ./scripts/finish.sh {{branch}}
#
finish:
  # Push the branch to the remote.
  - git push -u origin {{branch}}

  # Open a pull request (requires GitHub CLI: https://cli.github.com).
  # - gh pr create --title "{{branch}}" --fill

  # Or push, open a PR, squash-merge, and delete the branch in one step.
  # - git push -u origin {{branch}} && gh pr create --title "{{branch}}" --fill && gh pr merge --squash --delete-branch
`

func cmdList() {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	activeOnly := fs.Bool("active", false, "show only active instances (exclude FINISHED)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: grove list [--active]")
	}
	fs.Parse(os.Args[2:])

	resp := mustRequest(proto.Request{Type: proto.ReqList})

	var instances []proto.InstanceInfo
	for _, inst := range resp.Instances {
		if *activeOnly && inst.State == proto.StateFinished {
			continue
		}
		instances = append(instances, inst)
	}

	if len(instances) == 0 {
		fmt.Printf("%sno instances%s\n", colorDim, colorReset)
		return
	}

	fmt.Printf("%s%-10s  %-12s  %-10s  %s%s\n", colorBold, "ID", "PROJECT", "STATE", "BRANCH", colorReset)
	fmt.Printf("%s%-10s  %-12s  %-10s  %s%s\n", colorDim, "----------", "------------", "----------", "------", colorReset)
	for _, inst := range instances {
		color := colorState(inst.State)
		reset := ""
		if color != "" {
			reset = "\033[0m"
		}
		fmt.Printf("%-10s  %-12s  %s%-10s%s  %s\n", inst.ID, inst.Project, color, inst.State, reset, inst.Branch)
	}
}

func cmdAttach() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: grove attach <instance-id>")
		os.Exit(1)
	}
	doAttach(os.Args[2])
}

// doAttach connects the terminal to the instance PTY and blocks until the
// user detaches (Ctrl-]) or the agent exits.
func doAttach(instanceID string) {
	socketPath := daemonSocket()
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "grove: cannot connect to daemon: %v\n", err)
		os.Exit(1)
	}
	// Note: conn is NOT deferred-closed here; the attach loop owns its lifetime.

	if err := writeRequest(conn, proto.Request{
		Type:       proto.ReqAttach,
		InstanceID: instanceID,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "grove: %v\n", err)
		os.Exit(1)
	}

	resp, err := readResponse(conn)
	if err != nil || !resp.OK {
		msg := "attach failed"
		if err != nil {
			msg = err.Error()
		} else if resp.Error != "" {
			msg = resp.Error
		}
		fmt.Fprintf(os.Stderr, "grove: %s\n", msg)
		conn.Close()
		os.Exit(1)
	}

	fd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "grove: cannot set raw mode: %v\n", err)
		conn.Close()
		os.Exit(1)
	}

	restore := func() {
		term.Restore(fd, oldState)
	}
	defer restore()

	fmt.Fprintf(os.Stdout, "\r\n[grove] attached to %s  (detach: Ctrl-])\r\n", instanceID)

	done := make(chan struct{}, 1)

	// Goroutine 1: copy PTY output (server → client) to stdout.
	go func() {
		io.Copy(os.Stdout, conn)
		select {
		case done <- struct{}{}:
		default:
		}
	}()

	// Goroutine 2: read stdin, watch for Ctrl-], frame and send to server.
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				for i := 0; i < n; i++ {
					if buf[i] == 0x1D {
						sendFrame(conn, proto.AttachFrameDetach, nil)
						select {
						case done <- struct{}{}:
						default:
						}
						return
					}
				}
				sendFrame(conn, proto.AttachFrameData, buf[:n])
			}
			if err != nil {
				select {
				case done <- struct{}{}:
				default:
				}
				return
			}
		}
	}()

	// Forward terminal resize events.
	winchCh := make(chan os.Signal, 1)
	signal.Notify(winchCh, syscall.SIGWINCH)
	go func() {
		for range winchCh {
			cols, rows, err := term.GetSize(fd)
			if err == nil {
				payload := make([]byte, 4)
				binary.BigEndian.PutUint16(payload[0:2], uint16(cols))
				binary.BigEndian.PutUint16(payload[2:4], uint16(rows))
				sendFrame(conn, proto.AttachFrameResize, payload)
			}
		}
	}()

	// Send initial window size.
	if cols, rows, err := term.GetSize(fd); err == nil {
		payload := make([]byte, 4)
		binary.BigEndian.PutUint16(payload[0:2], uint16(cols))
		binary.BigEndian.PutUint16(payload[2:4], uint16(rows))
		sendFrame(conn, proto.AttachFrameResize, payload)
	}

	<-done
	signal.Stop(winchCh)
	conn.Close()

	restore()
	defer func() {}() // suppress second restore() from defer
	fmt.Fprintf(os.Stdout, "\n[grove] detached from %s\n", instanceID)
}

func cmdLogs() {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	follow := fs.Bool("f", false, "follow log output")
	fs.BoolVar(follow, "follow", false, "follow log output")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: grove logs <instance-id> [-f]")
	}
	fs.Parse(os.Args[2:])
	remaining := fs.Args()
	if len(remaining) < 1 {
		fmt.Fprintln(os.Stderr, "usage: grove logs <instance-id> [-f]")
		os.Exit(1)
	}
	instanceID := remaining[0]

	reqType := proto.ReqLogs
	if *follow {
		reqType = proto.ReqLogsFollow
	}

	socketPath := daemonSocket()
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "grove: cannot connect to daemon: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	if err := writeRequest(conn, proto.Request{Type: reqType, InstanceID: instanceID}); err != nil {
		fmt.Fprintf(os.Stderr, "grove: %v\n", err)
		os.Exit(1)
	}
	resp, err := readResponse(conn)
	if err != nil || !resp.OK {
		msg := "logs failed"
		if resp.Error != "" {
			msg = resp.Error
		}
		fmt.Fprintf(os.Stderr, "grove: %s\n", msg)
		os.Exit(1)
	}
	io.Copy(os.Stdout, conn)
}

func cmdWatch() {
	socketPath := daemonSocket()

	fd := int(os.Stdout.Fd())

	// Hide cursor; restore on exit.
	fmt.Print("\033[?25l")
	defer fmt.Print("\033[?25h")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	winchCh := make(chan os.Signal, 1)
	signal.Notify(winchCh, syscall.SIGWINCH)
	defer signal.Stop(sigCh)
	defer signal.Stop(winchCh)

	drawWatch(fd, socketPath)

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-sigCh:
			fmt.Print("\033[?25h")
			os.Exit(0)
		case <-winchCh:
			drawWatch(fd, socketPath)
		case <-ticker.C:
			drawWatch(fd, socketPath)
		}
	}
}

func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}

func drawWatch(fd int, socketPath string) {
	width, _, err := term.GetSize(fd)
	if err != nil || width < 40 {
		width = 120
	}

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		fmt.Printf("\033[H\033[2Jdaemon not reachable: %v\n", err)
		return
	}
	defer conn.Close()

	if err := writeRequest(conn, proto.Request{Type: proto.ReqList}); err != nil {
		fmt.Printf("\033[H\033[2Jdaemon not reachable: %v\n", err)
		return
	}
	resp, err := readResponse(conn)
	if err != nil || !resp.OK {
		fmt.Printf("\033[H\033[2Jdaemon not reachable: %v\n", err)
		return
	}

	// Compute dynamic column widths based on actual content.
	const idW, stateW, uptimeW = 10, 10, 10
	projW := 14 // minimum width
	for _, inst := range resp.Instances {
		if l := len(inst.Project); l > projW {
			projW = l
		}
	}
	if projW > 30 {
		projW = 30
	}

	const separators = 4 * 2 // 4 column gaps of 2 spaces
	branchW := width - (idW + projW + stateW + uptimeW + separators)
	if branchW < 15 {
		branchW = 15
	}

	var buf strings.Builder
	buf.WriteString("\033[H\033[2J")

	// ASCII art grove header — banner with tree on either side.
	treeLines1 := []string{
		`     ccee88oo`,
		`  C8O8O8Q8PoOb o8oo`,
		` dOB69QO8PdUOpugoO9bD`,
		`CgggbU8OU qOp qOdoUOdcb`,
		`    6OuU  /p u gcoUodpP`,
		`      \\\//  /douUP`,
		`        \\\////`,
		`         |||/\`,
		`         |||\/`,
		`         |||||`,
		`   .....//||||\....`,
	}
	treeLines2 := []string{
		`        ccee88oo`,
		`  C8O8O8Q8PoOb o8oo`,
		` dOB9_GandalftheGUI_O9bD`,
		`CgggbU8OU qOp qOdoUOdcb`,
		`    6OuU6 /p IRgcoUodpP`,
		`      \dou/  /douUP`,
		`        \\\\///`,
		`         |||||`,
		`         |ILR|`,
		`         |||||`,
		`   .....//||||\....`,
	}
	bannerLines := []string{
		"      _,---.                 _,.---._           ,-.-.    ,----. ",
		"  _.='.'-,  \\  .-.,.---.   ,-.' , -  `.  ,--.-./=/ ,/ ,-.--` , \\",
		" /==.'-     / /==/  `   \\ /==/_,  ,  - \\/==/, ||=| -||==|-  _.-`",
		"/==/ -   .-' |==|-, .=., |==|   .=.     \\==\\,  \\ / ,||==|   `.-.",
		"|==|_   /_,-.|==|   '='  /==|_ : ;=:  - |\\==\\ - ' - /==/_ ,    /",
		"|==|  , \\_.' )==|- ,   .'|==| , '='     | \\==\\ ,   ||==|    .-' ",
		"\\==\\-  ,    (|==|_  . ,'. \\==\\ -    ,_ /  |==| -  ,/|==|_  ,`-._",
		" /==/ _  ,  //==/  /\\ ,  ) '.='. -   .'   \\==\\  _ / /==/ ,     /",
		" `--`------' `--`-`--`--'    `--`--''      `--`--'  `--`-----`` ",
	}
	const treeGap = 2
	maxTreeW := 0
	for _, l := range treeLines1 {
		if len(l) > maxTreeW {
			maxTreeW = len(l)
		}
	}
	for _, l := range treeLines2 {
		if len(l) > maxTreeW {
			maxTreeW = len(l)
		}
	}
	maxBannerW := 0
	for _, l := range bannerLines {
		if len(l) > maxBannerW {
			maxBannerW = len(l)
		}
	}
	// 11 rows: tree (11 lines) + banner (9 lines, padded with blank at top and bottom).
	bannerPadded := make([]string, 11)
	bannerPadded[0] = ""
	bannerPadded[10] = ""
	for i := 0; i < 9; i++ {
		bannerPadded[1+i] = bannerLines[i]
	}
	rowWidth := maxTreeW + treeGap + maxBannerW + treeGap + maxTreeW
	leftRowPad := (width - rowWidth) / 2
	if leftRowPad < 0 {
		leftRowPad = 0
	}
	buf.WriteString("\033[32m") // green for the trees
	for i := 0; i < 11; i++ {
		leftLine := treeLines1[i]
		if len(leftLine) < maxTreeW {
			leftLine = leftLine + strings.Repeat(" ", maxTreeW-len(leftLine))
		}
		rightLine := treeLines2[i]
		if len(rightLine) < maxTreeW {
			rightLine = rightLine + strings.Repeat(" ", maxTreeW-len(rightLine))
		}
		bannerLine := bannerPadded[i]
		if len(bannerLine) < maxBannerW {
			bannerLine = bannerLine + strings.Repeat(" ", maxBannerW-len(bannerLine))
		}
		row := leftLine + strings.Repeat(" ", treeGap) + bannerLine + strings.Repeat(" ", treeGap) + rightLine
		if leftRowPad > 0 {
			buf.WriteString(strings.Repeat(" ", leftRowPad))
		}
		buf.WriteString(row + "\n")
	}
	buf.WriteString("\033[0m\n")

	// Column headers.
	fmt.Fprintf(&buf, "%-*s  %-*s  %-*s  %-*s  %s\n",
		idW, "ID", projW, "PROJECT", stateW, "STATE", uptimeW, "UPTIME", "BRANCH")
	fmt.Fprintf(&buf, "\033[2m%s  %s  %s  %s  %s\033[0m\n",
		strings.Repeat("─", idW),
		strings.Repeat("─", projW),
		strings.Repeat("─", stateW),
		strings.Repeat("─", uptimeW),
		strings.Repeat("─", branchW))

	now := time.Now().Unix()
	var running int
	for _, inst := range resp.Instances {
		project := truncate(inst.Project, projW)
		branch := truncate(inst.Branch, branchW)
		uptimeEnd := now
		if inst.EndedAt > 0 {
			uptimeEnd = inst.EndedAt
		}
		uptime := formatUptime(uptimeEnd - inst.CreatedAt)
		stateColored := colorState(inst.State)
		fmt.Fprintf(&buf, "%-*s  %-*s  %s%-*s\033[0m  %-*s  %s\n",
			idW, inst.ID,
			projW, project,
			stateColored, stateW, inst.State,
			uptimeW, uptime,
			branch)
		if inst.State == "RUNNING" || inst.State == "ATTACHED" {
			running++
		}
	}

	if len(resp.Instances) == 0 {
		buf.WriteString("\n  no instances running\n")
	}

	// Status footer.
	fmt.Fprintf(&buf, "\n\033[2m  %d instance(s)  ·  %d running  ·  %s\033[0m\n",
		len(resp.Instances), running, time.Now().Format("15:04:05"))

	fmt.Print(buf.String())
}

func colorState(state string) string {
	switch state {
	case "RUNNING":
		return "\033[32m"
	case "WAITING":
		return "\033[33m"
	case "ATTACHED":
		return "\033[36m"
	case "CHECKING":
		return "\033[36m"
	case "EXITED":
		return "\033[2m"
	case "CRASHED":
		return "\033[31m"
	case "KILLED":
		return "\033[33m"
	case "FINISHED":
		return "\033[2m"
	default:
		return ""
	}
}

func formatUptime(secs int64) string {
	if secs < 0 {
		secs = 0
	}
	if secs < 60 {
		return fmt.Sprintf("%ds", secs)
	}
	if secs < 3600 {
		return fmt.Sprintf("%dm%02ds", secs/60, secs%60)
	}
	return fmt.Sprintf("%dh%02dm", secs/3600, (secs%3600)/60)
}

func cmdStop() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: grove stop <instance-id>")
		os.Exit(1)
	}
	instanceID := os.Args[2]

	mustRequest(proto.Request{
		Type:       proto.ReqStop,
		InstanceID: instanceID,
	})

	fmt.Printf("\n%s✓  Stopped%s %s%s%s\n\n", colorGreen+colorBold, colorReset, colorCyan, instanceID, colorReset)
}

func cmdRestart() {
	rawArgs, detach := stripBoolFlag(os.Args[2:], "d", "detach")
	fs := flag.NewFlagSet("restart", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: grove restart <instance-id> [-d]")
	}
	fs.Parse(rawArgs)
	args := fs.Args()
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: grove restart <instance-id> [-d]")
		os.Exit(1)
	}
	instanceID := args[0]

	// Look up the instance's project so we can check credentials.
	listResp := mustRequest(proto.Request{Type: proto.ReqList})
	var projectName string
	for _, inst := range listResp.Instances {
		if inst.ID == instanceID {
			projectName = inst.Project
			break
		}
	}
	var agentEnv map[string]string
	if projectName != "" {
		agentEnv = ensureAgentCredentials(projectName)
	}

	mustRequest(proto.Request{
		Type:       proto.ReqRestart,
		InstanceID: instanceID,
		AgentEnv:   agentEnv,
	})

	fmt.Printf("\n%s✓  Restarted%s %s%s%s\n\n", colorGreen+colorBold, colorReset, colorCyan, instanceID, colorReset)

	if !detach {
		doAttach(instanceID)
	}
}

func cmdDrop() {
	rawArgs, force := stripBoolFlag(os.Args[2:], "f", "force")
	fs := flag.NewFlagSet("drop", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprintln(os.Stderr, "usage: grove drop <instance-id> [-f]") }
	fs.Parse(rawArgs)
	args := fs.Args()
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: grove drop <instance-id> [-f]")
		os.Exit(1)
	}
	instanceID := args[0]

	// Fetch instance info to display worktree and branch before confirming.
	listResp := mustRequest(proto.Request{Type: proto.ReqList})
	var found *proto.InstanceInfo
	for i := range listResp.Instances {
		if listResp.Instances[i].ID == instanceID {
			found = &listResp.Instances[i]
			break
		}
	}
	if found == nil {
		fmt.Fprintf(os.Stderr, "grove: instance not found: %s\n", instanceID)
		os.Exit(1)
	}

	if !force {
		fmt.Printf("\n%sInstance%s %s%s%s\n\n", colorBold, colorReset, colorCyan, instanceID, colorReset)
		fmt.Printf("  %sProject:%s  %s%s%s\n", colorDim, colorReset, colorCyan, found.Project, colorReset)
		fmt.Printf("  %sWorktree:%s %s%s%s\n", colorDim, colorReset, colorCyan, found.WorktreeDir, colorReset)
		fmt.Printf("  %sBranch:%s   %s%s%s\n\n", colorDim, colorReset, colorCyan, found.Branch, colorReset)
		fmt.Printf("%sDelete instance %q and worktree?%s [y/N] ", colorBold, found.Project, colorReset)

		reader := bufio.NewReader(os.Stdin)
		answer, _ := reader.ReadString('\n')
		answer = strings.TrimSpace(answer)
		if answer != "y" && answer != "Y" {
			fmt.Printf("%saborted%s\n", colorDim, colorReset)
			return
		}
	}

	mustRequest(proto.Request{
		Type:       proto.ReqDrop,
		InstanceID: instanceID,
	})
	fmt.Printf("\n%s✓  Dropped%s %s%s%s\n\n", colorGreen+colorBold, colorReset, colorCyan, instanceID, colorReset)
}

func cmdFinish() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: grove finish <instance-id>")
		os.Exit(1)
	}
	instanceID := os.Args[2]

	socketPath := daemonSocket()
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "grove: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	if err := writeRequest(conn, proto.Request{Type: proto.ReqFinish, InstanceID: instanceID}); err != nil {
		fmt.Fprintf(os.Stderr, "grove: %v\n", err)
		os.Exit(1)
	}

	resp, err := readResponse(conn)
	if err != nil || !resp.OK {
		msg := resp.Error
		if msg == "" && err != nil {
			msg = err.Error()
		}
		fmt.Fprintf(os.Stderr, "grove: %s\n", msg)
		os.Exit(1)
	}

	// Stream complete command output from the daemon.
	io.Copy(os.Stdout, conn)
}

func cmdCheck() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: grove check <instance-id>")
		os.Exit(1)
	}
	instanceID := os.Args[2]

	socketPath := daemonSocket()
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "grove: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	if err := writeRequest(conn, proto.Request{Type: proto.ReqCheck, InstanceID: instanceID}); err != nil {
		fmt.Fprintf(os.Stderr, "grove: %v\n", err)
		os.Exit(1)
	}

	resp, err := readResponse(conn)
	if err != nil || !resp.OK {
		msg := resp.Error
		if msg == "" && err != nil {
			msg = err.Error()
		}
		fmt.Fprintf(os.Stderr, "grove: %s\n", msg)
		os.Exit(1)
	}

	// Stream check command output from the daemon.
	io.Copy(os.Stdout, conn)
}

func cmdProjectDir() {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: grove project dir <project|#>")
		os.Exit(1)
	}
	project := resolveProject(os.Args[3])
	fmt.Println(filepath.Join(rootDir(), "projects", project, "main"))
}

func cmdDir() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: grove dir <instance-id>")
		os.Exit(1)
	}
	id := os.Args[2]

	resp := mustRequest(proto.Request{Type: proto.ReqList})
	for _, inst := range resp.Instances {
		if inst.ID == id {
			fmt.Println(inst.WorktreeDir)
			return
		}
	}
	fmt.Fprintf(os.Stderr, "grove: instance not found: %s\n", id)
	os.Exit(1)
}

func cmdPrune() {
	fs := flag.NewFlagSet("prune", flag.ExitOnError)
	includeFinished := fs.Bool("finished", false, "also drop FINISHED instances")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: grove prune [--finished]")
	}
	fs.Parse(os.Args[2:])

	resp := mustRequest(proto.Request{Type: proto.ReqList})

	var dead []proto.InstanceInfo
	for _, inst := range resp.Instances {
		switch inst.State {
		case proto.StateExited, proto.StateCrashed, proto.StateKilled:
			dead = append(dead, inst)
		case proto.StateFinished:
			if *includeFinished {
				dead = append(dead, inst)
			}
		}
	}

	if len(dead) == 0 {
		fmt.Printf("%snothing to prune%s\n", colorDim, colorReset)
		return
	}

	fmt.Printf("\n%s⚠  Prune%s — the following instance(s) and their worktrees will be removed:\n\n", colorYellow+colorBold, colorReset)
	for _, inst := range dead {
		fmt.Printf("  %s%s%s\n", colorBold, inst.ID, colorReset)
		fmt.Printf("    %sProject:%s   %s%s%s\n", colorDim, colorReset, colorCyan, inst.Project, colorReset)
		fmt.Printf("    %sWorktree:%s  %s%s%s\n", colorDim, colorReset, colorCyan, inst.WorktreeDir, colorReset)
		fmt.Printf("    %sBranch:%s    %s%s%s\n", colorDim, colorReset, colorCyan, inst.Branch, colorReset)
		fmt.Printf("    %sState:%s     %s\n\n", colorDim, colorReset, inst.State)
	}
	fmt.Printf("  This will drop %d instance(s) and their worktrees.\n\n", len(dead))
	fmt.Printf("%sContinue?%s [y/N] ", colorBold, colorReset)

	reader := bufio.NewReader(os.Stdin)
	answer, _ := reader.ReadString('\n')
	answer = strings.TrimSpace(answer)
	if answer != "y" && answer != "Y" {
		fmt.Printf("%saborted%s\n", colorDim, colorReset)
		return
	}

	for _, inst := range dead {
		mustRequest(proto.Request{Type: proto.ReqDrop, InstanceID: inst.ID})
		fmt.Printf("%s✓  Dropped%s %s%s%s\n", colorGreen+colorBold, colorReset, colorCyan, inst.ID, colorReset)
	}
	fmt.Println()
}

// ─── Daemon install/uninstall/status ─────────────────────────────────────────

const launchAgentLabel = "com.grove.daemon"

func launchAgentPlistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist")
}

func cmdDaemon() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: grove daemon <install|uninstall|status|logs>")
		os.Exit(1)
	}
	switch os.Args[2] {
	case "install":
		cmdDaemonInstall()
	case "uninstall":
		cmdDaemonUninstall()
	case "status":
		cmdDaemonStatus()
	case "logs":
		cmdDaemonLogs()
	default:
		fmt.Fprintf(os.Stderr, "grove: unknown daemon subcommand %q\n", os.Args[2])
		os.Exit(1)
	}
}

func cmdDaemonLogs() {
	fs := flag.NewFlagSet("daemon logs", flag.ExitOnError)
	follow := fs.Bool("f", false, "follow log output")
	fs.BoolVar(follow, "follow", false, "follow log output")
	tailLines := fs.Int("n", 0, "print only the last N lines (0 = full file)")
	fs.IntVar(tailLines, "tail", 0, "print only the last N lines (0 = full file)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: grove daemon logs [-f] [-n N]")
	}
	fs.Parse(os.Args[3:])
	if len(fs.Args()) != 0 {
		fmt.Fprintln(os.Stderr, "usage: grove daemon logs [-f] [-n N]")
		os.Exit(1)
	}
	if *tailLines < 0 {
		fmt.Fprintln(os.Stderr, "grove: -n/--tail must be >= 0")
		os.Exit(1)
	}

	logPath := filepath.Join(rootDir(), "daemon.log")
	var err error
	if *tailLines > 0 {
		err = printLastLines(logPath, *tailLines, os.Stdout)
	} else {
		err = copyFileToStdout(logPath)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "grove: %v\n", err)
		os.Exit(1)
	}

	if *follow {
		if err := followFile(logPath); err != nil {
			fmt.Fprintf(os.Stderr, "grove: %v\n", err)
			os.Exit(1)
		}
	}
}

func cmdDaemonInstall() {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "grove: cannot resolve executable path: %v\n", err)
		os.Exit(1)
	}
	daemonBin := filepath.Join(filepath.Dir(exe), "groved")
	if _, err := os.Stat(daemonBin); err != nil {
		fmt.Fprintf(os.Stderr, "grove: groved binary not found at %s\n", daemonBin)
		os.Exit(1)
	}

	root := rootDir()
	logFile := filepath.Join(root, "daemon.log")
	socketPath := filepath.Join(root, "groved.sock")

	plist := buildPlist(daemonBin, root, logFile, os.Getenv("PATH"))

	plistPath := launchAgentPlistPath()
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "grove: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(plistPath, []byte(plist), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "grove: %v\n", err)
		os.Exit(1)
	}

	uid := fmt.Sprintf("%d", os.Getuid())
	// Unload existing instance silently (ignore errors).
	exec.Command("launchctl", "bootout", "gui/"+uid+"/"+launchAgentLabel).Run()

	// Load the new plist.
	out, err := exec.Command("launchctl", "bootstrap", "gui/"+uid, plistPath).CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "grove: launchctl bootstrap failed: %v\n%s", err, out)
		os.Exit(1)
	}

	fmt.Printf("\n%s✓  groved LaunchAgent installed%s\n\n", colorGreen+colorBold, colorReset)
	fmt.Printf("  %sPlist:%s %s%s%s\n", colorDim, colorReset, colorCyan, plistPath, colorReset)
	fmt.Printf("  %sLog:%s   %s%s%s\n\n", colorDim, colorReset, colorCyan, logFile, colorReset)

	// Verify the daemon actually started — the LaunchAgent is registered but
	// the process may have exited immediately (e.g. Docker not running).
	for i := 0; i < 20; i++ {
		time.Sleep(150 * time.Millisecond)
		if pingDaemon(socketPath) {
			fmt.Printf("%s✓  daemon is running%s\n\n", colorGreen+colorBold, colorReset)
			return
		}
	}
	fmt.Fprintf(os.Stderr, "%s✗  daemon did not start%s\n\n", colorRed+colorBold, colorReset)
	warnIfDockerUnavailable()
	fmt.Fprintf(os.Stderr, "  Check the log for details: %s%s%s\n\n", colorCyan, logFile, colorReset)
}

func cmdDaemonUninstall() {
	uid := fmt.Sprintf("%d", os.Getuid())
	exec.Command("launchctl", "bootout", "gui/"+uid+"/"+launchAgentLabel).Run()

	plistPath := launchAgentPlistPath()
	os.Remove(plistPath)

	fmt.Printf("\n%s✓  groved LaunchAgent removed%s\n\n", colorGreen+colorBold, colorReset)
}

func cmdDaemonStatus() {
	plistPath := launchAgentPlistPath()
	if _, err := os.Stat(plistPath); os.IsNotExist(err) {
		fmt.Printf("%snot installed%s\n", colorDim, colorReset)
		return
	}

	root := rootDir()
	sock := filepath.Join(root, "groved.sock")
	if pingDaemon(sock) {
		fmt.Printf("%s✓  running%s\n\n  %splist:%s %s%s%s\n", colorGreen+colorBold, colorReset, colorDim, colorReset, colorCyan, plistPath, colorReset)
	} else {
		fmt.Printf("%s⚠  installed but not running%s\n\n  %splist:%s %s%s%s\n", colorYellow+colorBold, colorReset, colorDim, colorReset, colorCyan, plistPath, colorReset)
	}
}

func copyFileToStdout(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("daemon log not found at %s", path)
		}
		return fmt.Errorf("open daemon log: %w", err)
	}
	defer f.Close()

	if _, err := io.Copy(os.Stdout, f); err != nil {
		return fmt.Errorf("read daemon log: %w", err)
	}
	return nil
}

func printLastLines(path string, n int, w io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("daemon log not found at %s", path)
		}
		return fmt.Errorf("open daemon log: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	ring := make([]string, n)
	count := 0
	for scanner.Scan() {
		ring[count%n] = scanner.Text()
		count++
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read daemon log: %w", err)
	}

	start := 0
	lines := count
	if count > n {
		start = count % n
		lines = n
	}
	for i := 0; i < lines; i++ {
		fmt.Fprintln(w, ring[(start+i)%n])
	}
	return nil
}

func followFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("daemon log not found at %s", path)
		}
		return fmt.Errorf("open daemon log: %w", err)
	}
	defer f.Close()

	offset, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Errorf("seek daemon log: %w", err)
	}

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	for {
		select {
		case <-sigCh:
			return nil
		case <-ticker.C:
			info, err := f.Stat()
			if err != nil {
				return fmt.Errorf("stat daemon log: %w", err)
			}

			size := info.Size()
			if size < offset {
				offset = 0
			}
			if size <= offset {
				continue
			}
			if _, err := f.Seek(offset, io.SeekStart); err != nil {
				return fmt.Errorf("seek daemon log: %w", err)
			}
			if _, err := io.CopyN(os.Stdout, f, size-offset); err != nil && err != io.EOF {
				return fmt.Errorf("read daemon log: %w", err)
			}
			offset = size
		}
	}
}

// buildPlist generates the LaunchAgent plist XML.
// envPath is embedded as EnvironmentVariables.PATH so the daemon inherits the
// user's full shell PATH (launchd provides only a minimal default PATH).
func buildPlist(daemonBin, rootDir, logFile string, envPath string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>--root</string>
		<string>%s</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>%s</string>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`, xmlEscape(launchAgentLabel), xmlEscape(daemonBin), xmlEscape(rootDir),
		xmlEscape(envPath), xmlEscape(logFile), xmlEscape(logFile))
}

func xmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// ─── Daemon connection helpers ────────────────────────────────────────────────

// rootDir returns the groved data directory.
// Precedence: GROVE_ROOT env var > ~/.grove
func rootDir() string {
	if env := os.Getenv("GROVE_ROOT"); env != "" {
		abs, err := filepath.Abs(env)
		if err == nil {
			return abs
		}
		return env
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".grove")
}

// daemonSocket returns the Unix socket path and ensures the daemon is running.
func daemonSocket() string {
	root := rootDir()
	sock := filepath.Join(root, "groved.sock")
	ensureDaemon(root, sock)
	return sock
}

// ensureDaemon starts groved in the background if the socket doesn't exist
// or is not responding to pings.  root is passed via --root so the daemon
// uses the same data directory that grove is targeting.
func ensureDaemon(root, socketPath string) {
	if pingDaemon(socketPath) {
		return
	}

	exe, _ := os.Executable()
	daemonBin := filepath.Join(filepath.Dir(exe), "groved")
	if _, err := os.Stat(daemonBin); err != nil {
		daemonBin = "groved"
	}

	cmd := exec.Command(daemonBin, "--root", root)
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "grove: could not start daemon: %v\n", err)
		os.Exit(1)
	}

	// Wait up to 3 seconds for it to become ready.
	for i := 0; i < 30; i++ {
		time.Sleep(100 * time.Millisecond)
		if pingDaemon(socketPath) {
			return
		}
	}

	fmt.Fprintln(os.Stderr, "grove: daemon did not start in time")
	warnIfDockerUnavailable()
	os.Exit(1)
}

// pingDaemon returns true if the daemon is alive and responding.
func pingDaemon(socketPath string) bool {
	conn, err := net.DialTimeout("unix", socketPath, 500*time.Millisecond)
	if err != nil {
		return false
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(500 * time.Millisecond))
	if err := writeRequest(conn, proto.Request{Type: proto.ReqPing}); err != nil {
		return false
	}
	resp, err := readResponse(conn)
	return err == nil && resp.OK
}

// tryRequest sends a request to the daemon and returns the response.
// Unlike mustRequest it returns an error instead of exiting, so callers
// can tolerate a daemon that isn't running.
func tryRequest(req proto.Request) (proto.Response, error) {
	root := rootDir()
	sock := filepath.Join(root, "groved.sock")
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return proto.Response{}, err
	}
	defer conn.Close()

	if err := writeRequest(conn, req); err != nil {
		return proto.Response{}, err
	}
	resp, err := readResponse(conn)
	if err != nil {
		return proto.Response{}, err
	}
	if !resp.OK {
		return resp, fmt.Errorf("%s", resp.Error)
	}
	return resp, nil
}

// mustRequest sends a request to the daemon and returns the response, exiting
// on any error.
func mustRequest(req proto.Request) proto.Response {
	socketPath := daemonSocket()
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "grove: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	if err := writeRequest(conn, req); err != nil {
		fmt.Fprintf(os.Stderr, "grove: %v\n", err)
		os.Exit(1)
	}

	resp, err := readResponse(conn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "grove: %v\n", err)
		os.Exit(1)
	}
	if !resp.OK {
		fmt.Fprintf(os.Stderr, "grove: %s\n", resp.Error)
		os.Exit(1)
	}
	return resp
}

func writeRequest(conn net.Conn, req proto.Request) error {
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = conn.Write(data)
	return err
}

func readResponse(conn net.Conn) (proto.Response, error) {
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return proto.Response{}, err
		}
		return proto.Response{}, io.EOF
	}
	var resp proto.Response
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		return proto.Response{}, fmt.Errorf("bad response: %w", err)
	}
	return resp, nil
}

// sendFrame writes a single length-prefixed frame to w.
// [1 byte type][4 byte big-endian length][payload]
func sendFrame(w io.Writer, frameType byte, payload []byte) {
	hdr := make([]byte, 5)
	hdr[0] = frameType
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	w.Write(hdr)
	if len(payload) > 0 {
		w.Write(payload)
	}
}

// suppress unused import warning
var _ = log.Printf
