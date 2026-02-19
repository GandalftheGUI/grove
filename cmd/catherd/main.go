// catherd – the CLI client for the catherdd daemon.
//
// Usage:
//
//	catherd project create <name>      – define a new project
//	catherd project list               – list defined projects
//	catherd start <project> "<task>"   – create and start a new agent instance
//	catherd list                       – list all instances
//	catherd attach <instance-id>       – attach your terminal to an instance PTY
//	catherd logs <instance-id>         – print buffered logs for an instance
//	catherd destroy <instance-id>      – stop and remove an instance
//
// catherd will start the daemon automatically if it is not already running.
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
	"strings"
	"syscall"
	"time"

	"github.com/ianremillard/catherdd/internal/proto"
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
	case "destroy":
		cmdDestroy()
	case "daemon":
		cmdDaemon()
	default:
		fmt.Fprintf(os.Stderr, "catherd: unknown command %q\n", os.Args[1])
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `catherd – supervise AI coding agent instances

Project commands:
  project create <name> [--global] [--repo <url>] [--agent <cmd>]
                             Define a new project (personal by default, --global for shared)
  project list               List defined projects

Instance commands:
  start <project> "<task>"   Start a new agent instance
  list                       List all instances
  watch                      Live dashboard (refreshes every second, Ctrl-C to exit)
  attach <instance-id>       Attach terminal to an instance (detach: Ctrl-])
  logs <instance-id> [-f]    Print buffered output for an instance
  destroy <instance-id>      Stop and destroy an instance

Daemon commands:
  daemon install             Register catherdd as a login LaunchAgent
  daemon uninstall           Remove the LaunchAgent
  daemon status              Show whether the LaunchAgent is installed and running`)
}

// ─── Subcommand implementations ───────────────────────────────────────────────

func cmdProject() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: catherd project <create|list>")
		os.Exit(1)
	}
	switch os.Args[2] {
	case "create":
		cmdProjectCreate()
	case "list":
		cmdProjectList()
	default:
		fmt.Fprintf(os.Stderr, "catherd: unknown project subcommand %q\n", os.Args[2])
		os.Exit(1)
	}
}

// cmdProjectCreate handles: catherd project create <name> [--global] [--repo <url>] [--agent <cmd>]
//
// By default the project.yaml is written to projects.local/<name>/ (personal,
// git-ignored).  Pass --global to write to projects/<name>/ instead (tracked).
// This is a pure filesystem operation — no daemon required.
func cmdProjectCreate() {
	if len(os.Args) < 4 || os.Args[3] == "" || os.Args[3][0] == '-' {
		fmt.Fprintln(os.Stderr, "usage: catherd project create <name> [--global] [--repo <url>] [--agent <cmd>]")
		os.Exit(1)
	}
	name := os.Args[3]

	fs := flag.NewFlagSet("project create", flag.ExitOnError)
	global := fs.Bool("global", false, "write to the repo's global projects/ directory (tracked by git)")
	repo := fs.String("repo", "", "git remote URL (can be added later)")
	agent := fs.String("agent", "claude", "agent command to run inside the worktree")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: catherd project create <name> [--global] [--repo <url>] [--agent <cmd>]")
		fs.PrintDefaults()
	}
	fs.Parse(os.Args[4:])

	// Determine which config directory to write into.
	var targetDir string
	root := repoRoot()
	if *global {
		if root != "" {
			targetDir = filepath.Join(root, "projects")
		}
	} else {
		// Personal: projects.local/ next to the binary's repo root.
		if root != "" {
			targetDir = filepath.Join(root, "projects.local")
		}
	}
	// Fall back to ~/.catherdd/projects/ if we can't find the repo root
	// (e.g., catherd installed system-wide).
	if targetDir == "" {
		targetDir = filepath.Join(rootDir(), "projects")
	}

	projectDir := filepath.Join(targetDir, name)
	if _, err := os.Stat(projectDir); err == nil {
		fmt.Fprintf(os.Stderr, "catherd: project %q already exists at %s\n", name, projectDir)
		os.Exit(1)
	}
	// MkdirAll creates projects.local/ automatically if it doesn't exist yet.
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "catherd: %v\n", err)
		os.Exit(1)
	}

	yamlPath := filepath.Join(projectDir, "project.yaml")
	content := fmt.Sprintf("name: %s\nrepo: %s\n\nbootstrap: []\n\nagent:\n  command: %s\n  args: []\n\ndev:\n  start: []\n",
		name, *repo, *agent)
	if err := os.WriteFile(yamlPath, []byte(content), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "catherd: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("created project %q\n", name)
	fmt.Printf("config: %s\n", yamlPath)
	fmt.Println("edit the file to set your repo URL and bootstrap steps, then run:")
	fmt.Printf("  catherd start %s \"your task\"\n", name)
}

// cmdProjectList handles: catherd project list
//
// Scans all config directories (personal → global → home) and prints a summary
// table.  Projects with the same name in multiple dirs are deduplicated (the
// highest-priority dir wins).  This is a pure filesystem operation — no daemon
// required.
func cmdProjectList() {
	type row struct{ name, source, repo, agent string }
	var rows []row
	seen := make(map[string]bool)

	for _, entry := range allConfigDirEntries() {
		dirEntries, err := os.ReadDir(entry.path)
		if err != nil {
			continue // dir may not exist yet
		}
		for _, e := range dirEntries {
			if !e.IsDir() || seen[e.Name()] {
				continue
			}
			data, err := os.ReadFile(filepath.Join(entry.path, e.Name(), "project.yaml"))
			if err != nil {
				continue
			}
			var p struct {
				Name  string `yaml:"name"`
				Repo  string `yaml:"repo"`
				Agent struct {
					Command string `yaml:"command"`
				} `yaml:"agent"`
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
			agent := p.Agent.Command
			if agent == "" {
				agent = "(none)"
			}
			source := entry.label
			if source == "" {
				source = "home"
			}
			seen[e.Name()] = true
			rows = append(rows, row{name, source, repo, agent})
		}
	}

	if len(rows) == 0 {
		fmt.Println("no projects defined")
		return
	}

	fmt.Printf("%-20s  %-10s  %-40s  %s\n", "NAME", "SOURCE", "REPO", "AGENT")
	fmt.Printf("%-20s  %-10s  %-40s  %s\n", "--------------------", "----------", "----------------------------------------", "-----")
	for _, r := range rows {
		fmt.Printf("%-20s  %-10s  %-40s  %s\n", r.name, r.source, r.repo, r.agent)
	}
}

func cmdStart() {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: catherd start <project> \"<task>\"")
		os.Exit(1)
	}
	project := os.Args[2]
	task := os.Args[3]

	resp := mustRequest(proto.Request{
		Type:    proto.ReqStart,
		Project: project,
		Task:    task,
	})

	fmt.Printf("started instance %s\n", resp.InstanceID)
	fmt.Printf("run: catherd attach %s\n", resp.InstanceID)
}

func cmdList() {
	resp := mustRequest(proto.Request{Type: proto.ReqList})

	if len(resp.Instances) == 0 {
		fmt.Println("no instances")
		return
	}

	fmt.Printf("%-10s  %-12s  %-10s  %s\n", "ID", "PROJECT", "STATE", "TASK")
	fmt.Printf("%-10s  %-12s  %-10s  %s\n", "----------", "------------", "----------", "----")
	for _, inst := range resp.Instances {
		task := inst.Task
		if len(task) > 50 {
			task = task[:47] + "..."
		}
		fmt.Printf("%-10s  %-12s  %-10s  %s\n", inst.ID, inst.Project, inst.State, task)
	}
}

func cmdAttach() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: catherd attach <instance-id>")
		os.Exit(1)
	}
	instanceID := os.Args[2]

	socketPath := daemonSocket()
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "catherd: cannot connect to daemon: %v\n", err)
		os.Exit(1)
	}
	// Note: conn is NOT deferred-closed here; the attach loop owns its lifetime.

	// Send attach request and read the JSON handshake response.
	if err := writeRequest(conn, proto.Request{
		Type:       proto.ReqAttach,
		InstanceID: instanceID,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "catherd: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "catherd: %s\n", msg)
		conn.Close()
		os.Exit(1)
	}

	// ── Attach session ────────────────────────────────────────────────────────
	//
	// Terminal is put into raw mode so all keystrokes go directly to the agent.
	// Ctrl-] (0x1D) is intercepted as the detach escape.
	//
	// Wire format (client → server) after handshake:
	//   [1 byte type][4 byte big-endian length][payload]
	//
	// Server → client: raw PTY bytes (written directly to stdout).

	fd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "catherd: cannot set raw mode: %v\n", err)
		conn.Close()
		os.Exit(1)
	}

	restore := func() {
		term.Restore(fd, oldState)
	}
	defer restore()

	fmt.Fprintf(os.Stdout, "\r\n[catherd] attached to %s  (detach: Ctrl-])\r\n", instanceID)

	// Channel signalling that either the server closed the connection or the
	// user sent a detach frame.
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
				// Check for detach byte (Ctrl-] = 0x1D).
				for i := 0; i < n; i++ {
					if buf[i] == 0x1D {
						// Send detach frame, then signal done.
						sendFrame(conn, proto.AttachFrameDetach, nil)
						select {
						case done <- struct{}{}:
						default:
						}
						return
					}
				}
				// Send data frame.
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

	// Handle terminal resize: forward SIGWINCH to the daemon as a resize frame.
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

	// restore() is called by defer; print a newline after the raw session.
	restore()
	defer func() { /* suppress the second restore() from defer */ }()
	fmt.Fprintf(os.Stdout, "\n[catherd] detached from %s\n", instanceID)
}

func cmdLogs() {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	follow := fs.Bool("f", false, "follow log output")
	fs.BoolVar(follow, "follow", false, "follow log output")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: catherd logs <instance-id> [-f]")
	}
	fs.Parse(os.Args[2:])
	remaining := fs.Args()
	if len(remaining) < 1 {
		fmt.Fprintln(os.Stderr, "usage: catherd logs <instance-id> [-f]")
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
		fmt.Fprintf(os.Stderr, "catherd: cannot connect to daemon: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	if err := writeRequest(conn, proto.Request{Type: reqType, InstanceID: instanceID}); err != nil {
		fmt.Fprintf(os.Stderr, "catherd: %v\n", err)
		os.Exit(1)
	}
	resp, err := readResponse(conn)
	if err != nil || !resp.OK {
		msg := "logs failed"
		if resp.Error != "" {
			msg = resp.Error
		}
		fmt.Fprintf(os.Stderr, "catherd: %s\n", msg)
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

	// Column widths: ID(10), PROJECT(12), STATE(10), UPTIME(10), TASK(dynamic).
	const idW, projW, stateW, uptimeW = 10, 12, 10, 10
	fixed := idW + projW + stateW + uptimeW + 4*2 // 4 separators of 2 spaces
	taskW := width - fixed
	if taskW < 10 {
		taskW = 10
	}

	fmt.Print("\033[H\033[2J")
	fmt.Printf("%-*s  %-*s  %-*s  %-*s  %s\n",
		idW, "ID", projW, "PROJECT", stateW, "STATE", uptimeW, "UPTIME", "TASK")
	fmt.Printf("%-*s  %-*s  %-*s  %-*s  %s\n",
		idW, "----------", projW, "------------", stateW, "----------", uptimeW, "----------", "----")

	now := time.Now().Unix()
	for _, inst := range resp.Instances {
		task := inst.Task
		if len(task) > taskW {
			task = task[:taskW-3] + "..."
		}
		uptime := formatUptime(now - inst.CreatedAt)
		stateColored := colorState(inst.State)
		fmt.Printf("%-*s  %-*s  %s%-*s\033[0m  %-*s  %s\n",
			idW, inst.ID,
			projW, inst.Project,
			stateColored, stateW, inst.State,
			uptimeW, uptime,
			task)
	}

	if len(resp.Instances) == 0 {
		fmt.Println("no instances")
	}
}

func colorState(state string) string {
	switch state {
	case "RUNNING":
		return "\033[32m"
	case "WAITING":
		return "\033[33m"
	case "ATTACHED":
		return "\033[36m"
	case "EXITED":
		return "\033[2m"
	case "CRASHED":
		return "\033[31m"
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

func cmdDestroy() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: catherd destroy <instance-id>")
		os.Exit(1)
	}
	instanceID := os.Args[2]

	resp := mustRequest(proto.Request{
		Type:       proto.ReqDestroy,
		InstanceID: instanceID,
	})

	_ = resp
	fmt.Printf("destroyed %s\n", instanceID)
}

// ─── Daemon install/uninstall/status ─────────────────────────────────────────

const launchAgentLabel = "com.catherd.daemon"

func launchAgentPlistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist")
}

func cmdDaemon() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: catherd daemon <install|uninstall|status>")
		os.Exit(1)
	}
	switch os.Args[2] {
	case "install":
		cmdDaemonInstall()
	case "uninstall":
		cmdDaemonUninstall()
	case "status":
		cmdDaemonStatus()
	default:
		fmt.Fprintf(os.Stderr, "catherd: unknown daemon subcommand %q\n", os.Args[2])
		os.Exit(1)
	}
}

func cmdDaemonInstall() {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "catherd: cannot resolve executable path: %v\n", err)
		os.Exit(1)
	}
	daemonBin := filepath.Join(filepath.Dir(exe), "catherdd")
	if _, err := os.Stat(daemonBin); err != nil {
		fmt.Fprintf(os.Stderr, "catherd: catherdd binary not found at %s\n", daemonBin)
		os.Exit(1)
	}

	root := rootDir()
	logFile := filepath.Join(root, "daemon.log")
	configDirs := installConfigDirPaths()

	plist := buildPlist(daemonBin, root, logFile, configDirs)

	plistPath := launchAgentPlistPath()
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "catherd: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(plistPath, []byte(plist), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "catherd: %v\n", err)
		os.Exit(1)
	}

	uid := fmt.Sprintf("%d", os.Getuid())
	// Unload existing instance silently (ignore errors).
	exec.Command("launchctl", "bootout", "gui/"+uid+"/"+launchAgentLabel).Run()

	// Load the new plist.
	out, err := exec.Command("launchctl", "bootstrap", "gui/"+uid, plistPath).CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "catherd: launchctl bootstrap failed: %v\n%s", err, out)
		os.Exit(1)
	}

	fmt.Printf("catherdd LaunchAgent installed\nplist:  %s\nlog:    %s\n", plistPath, logFile)
}

func cmdDaemonUninstall() {
	uid := fmt.Sprintf("%d", os.Getuid())
	exec.Command("launchctl", "bootout", "gui/"+uid+"/"+launchAgentLabel).Run()

	plistPath := launchAgentPlistPath()
	os.Remove(plistPath)

	fmt.Println("catherdd LaunchAgent removed")
}

func cmdDaemonStatus() {
	plistPath := launchAgentPlistPath()
	if _, err := os.Stat(plistPath); os.IsNotExist(err) {
		fmt.Println("not installed")
		return
	}

	root := rootDir()
	sock := filepath.Join(root, "catherdd.sock")
	if pingDaemon(sock) {
		fmt.Printf("running\nplist: %s\n", plistPath)
	} else {
		fmt.Printf("installed but not running\nplist: %s\n", plistPath)
	}
}

// installConfigDirPaths returns the project config dirs to pass to the daemon
// plist. Always includes projects.local/ and projects/ relative to the repo
// root, even if they don't exist yet (the daemon handles missing dirs).
func installConfigDirPaths() []string {
	root := repoRoot()
	if root == "" {
		return nil
	}
	return []string{
		filepath.Join(root, "projects.local"),
		filepath.Join(root, "projects"),
	}
}

// buildPlist generates the LaunchAgent plist XML.
func buildPlist(daemonBin, rootDir, logFile string, configDirs []string) string {
	var args strings.Builder
	args.WriteString(fmt.Sprintf("\t\t<string>%s</string>\n", xmlEscape(daemonBin)))
	args.WriteString(fmt.Sprintf("\t\t<string>--root</string>\n\t\t<string>%s</string>\n", xmlEscape(rootDir)))
	for _, dir := range configDirs {
		args.WriteString(fmt.Sprintf("\t\t<string>--projects-dir</string>\n\t\t<string>%s</string>\n", xmlEscape(dir)))
	}

	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
%s	</array>
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
`, xmlEscape(launchAgentLabel), args.String(), xmlEscape(logFile), xmlEscape(logFile))
}

func xmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// ─── Project directory helpers ────────────────────────────────────────────────

// repoRoot returns the directory one level above the catherd binary, which is
// the repo root when running from a local checkout (bin/catherd → repo/).
// Returns an empty string if the executable path cannot be determined.
func repoRoot() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Dir(filepath.Dir(exe))
}

// repoProjDirs returns the personal (projects.local/) and global (projects/)
// config directories relative to the repo root, if they exist on disk.
// Either value may be empty.
func repoProjDirs() (personal, global string) {
	root := repoRoot()
	if root == "" {
		return "", ""
	}
	g := filepath.Join(root, "projects")
	if fi, err := os.Stat(g); err == nil && fi.IsDir() {
		global = g
	}
	p := filepath.Join(root, "projects.local")
	if fi, err := os.Stat(p); err == nil && fi.IsDir() {
		personal = p
	}
	return
}

// configDirEntry pairs a filesystem path with a human-readable label.
type configDirEntry struct {
	path  string
	label string // "personal", "global", or "" for the ~/.catherdd fallback
}

// allConfigDirEntries returns every project config directory in priority order:
// personal repo dir → global repo dir → ~/.catherdd/projects fallback.
func allConfigDirEntries() []configDirEntry {
	personal, global := repoProjDirs()
	var entries []configDirEntry
	if personal != "" {
		entries = append(entries, configDirEntry{personal, "personal"})
	}
	if global != "" {
		entries = append(entries, configDirEntry{global, "global"})
	}
	return entries
}

// configDirPaths returns just the paths from allConfigDirEntries.
func configDirPaths() []string {
	entries := allConfigDirEntries()
	paths := make([]string, len(entries))
	for i, e := range entries {
		paths[i] = e.path
	}
	return paths
}

// ─── Daemon connection helpers ────────────────────────────────────────────────

// rootDir returns the catherdd data directory.
// Precedence: CATHERDD_ROOT env var > ~/.catherdd
func rootDir() string {
	if env := os.Getenv("CATHERDD_ROOT"); env != "" {
		abs, err := filepath.Abs(env)
		if err == nil {
			return abs
		}
		return env
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".catherdd")
}

// daemonSocket returns the Unix socket path and ensures the daemon is running.
func daemonSocket() string {
	root := rootDir()
	sock := filepath.Join(root, "catherdd.sock")
	ensureDaemon(root, sock)
	return sock
}

// ensureDaemon starts catherdd in the background if the socket doesn't exist
// or is not responding to pings.  root is passed via --root so the daemon
// uses the same data directory that catherd is targeting.
func ensureDaemon(root, socketPath string) {
	if pingDaemon(socketPath) {
		return
	}

	// Find the catherdd binary next to the current executable.
	exe, _ := os.Executable()
	daemonBin := filepath.Join(filepath.Dir(exe), "catherdd")
	if _, err := os.Stat(daemonBin); err != nil {
		// Fall back to PATH.
		daemonBin = "catherdd"
	}

	// Build argument list: --root <dir> [--projects-dir <dir> ...]
	args := []string{"--root", root}
	for _, dir := range configDirPaths() {
		args = append(args, "--projects-dir", dir)
	}

	cmd := exec.Command(daemonBin, args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "catherd: could not start daemon: %v\n", err)
		os.Exit(1)
	}

	// Wait up to 3 seconds for it to become ready.
	for i := 0; i < 30; i++ {
		time.Sleep(100 * time.Millisecond)
		if pingDaemon(socketPath) {
			return
		}
	}

	fmt.Fprintln(os.Stderr, "catherd: daemon did not start in time")
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

// mustRequest sends a request to the daemon and returns the response, exiting
// on any error.
func mustRequest(req proto.Request) proto.Response {
	socketPath := daemonSocket()
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "catherd: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	if err := writeRequest(conn, req); err != nil {
		fmt.Fprintf(os.Stderr, "catherd: %v\n", err)
		os.Exit(1)
	}

	resp, err := readResponse(conn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "catherd: %v\n", err)
		os.Exit(1)
	}
	if !resp.OK {
		fmt.Fprintf(os.Stderr, "catherd: %s\n", resp.Error)
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
