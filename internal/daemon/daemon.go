// Package daemon implements the groved background daemon.
//
// The daemon listens on a Unix domain socket and handles requests from grove
// clients.  Each request is a single newline-terminated JSON object; the daemon
// writes a single newline-terminated JSON response and then closes the
// connection — except for attach requests, which enter a bidirectional
// streaming mode (see instance.go and proto/messages.go for the wire format).
package daemon

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ianremillard/grove/internal/proto"
)

// Daemon is the central supervisor.  It owns a map of live instances and
// handles all IPC requests from grove.
type Daemon struct {
	rootDir string // ~/.grove  (data root: projects, instances, logs)

	mu        sync.Mutex
	instances map[string]*Instance // keyed by instance ID
}

// New creates a Daemon that uses rootDir (~/.grove) as its data directory.
// Project registrations are read from rootDir/projects/<name>/project.yaml.
// Returns an error if Docker is not available.
func New(rootDir string) (*Daemon, error) {
	if err := validateDocker(); err != nil {
		return nil, err
	}

	for _, sub := range []string{
		"projects",
		"instances",
		"logs",
	} {
		if err := os.MkdirAll(filepath.Join(rootDir, sub), 0o755); err != nil {
			return nil, err
		}
	}

	d := &Daemon{
		rootDir:   rootDir,
		instances: make(map[string]*Instance),
	}

	if err := d.loadPersistedInstances(); err != nil {
		log.Printf("warning: could not reload persisted instances: %v", err)
	}

	return d, nil
}

// Run starts the Unix socket listener and blocks until it is closed.
func (d *Daemon) Run(socketPath string) error {
	// Remove stale socket.
	os.Remove(socketPath)

	l, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", socketPath, err)
	}
	defer l.Close()

	log.Printf("groved listening on %s", socketPath)

	for {
		conn, err := l.Accept()
		if err != nil {
			// Listener was closed (shutdown).
			return nil
		}
		go d.handleConn(conn)
	}
}

// ─── Connection handling ──────────────────────────────────────────────────────

func (d *Daemon) handleConn(conn net.Conn) {
	// Non-attach requests are handled quickly; attach blocks for its duration.
	defer func() {
		// conn may already be closed by Attach(); that's fine.
		conn.Close()
	}()

	var req proto.Request
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		return
	}
	if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
		respond(conn, proto.Response{OK: false, Error: "bad request: " + err.Error()})
		return
	}

	switch req.Type {
	case proto.ReqPing:
		respond(conn, proto.Response{OK: true})

	case proto.ReqStart:
		d.handleStart(conn, req)

	case proto.ReqList:
		d.handleList(conn)

	case proto.ReqAttach:
		d.handleAttach(conn, req)

	case proto.ReqLogs:
		d.handleLogs(conn, req)

	case proto.ReqLogsFollow:
		d.handleLogsFollow(conn, req)

	case proto.ReqStop:
		d.handleStop(conn, req)

	case proto.ReqDrop:
		d.handleDrop(conn, req)

	case proto.ReqFinish:
		d.handleFinish(conn, req)

	case proto.ReqCheck:
		d.handleCheck(conn, req)

	case proto.ReqRestart:
		d.handleRestart(conn, req)

	default:
		respond(conn, proto.Response{OK: false, Error: "unknown request type: " + req.Type})
	}
}

func respond(conn net.Conn, r proto.Response) {
	data, _ := json.Marshal(r)
	data = append(data, '\n')
	conn.Write(data)
}

// ─── Request handlers ─────────────────────────────────────────────────────────

func (d *Daemon) handleStart(conn net.Conn, req proto.Request) {
	if req.Project == "" {
		respond(conn, proto.Response{OK: false, Error: "project name required"})
		return
	}
	if req.Branch == "" {
		respond(conn, proto.Response{OK: false, Error: "branch name required"})
		return
	}

	p, err := loadProject(d.rootDir, req.Project)
	if err != nil {
		respond(conn, proto.Response{OK: false, Error: err.Error()})
		return
	}

	// Allocate instance ID early so the log file can be named after it.
	d.mu.Lock()
	instanceID := d.nextInstanceID()
	d.mu.Unlock()
	startedAt := time.Now()

	logFile := filepath.Join(d.rootDir, "logs", instanceID+".log")
	logFd, _ := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if logFd != nil {
		defer logFd.Close()
	}

	// setupW captures all clone/pull/bootstrap output in memory and also
	// writes it to the log file so it's preserved after the connection closes.
	var outputBuf bytes.Buffer
	var setupW io.Writer = &outputBuf
	if logFd != nil {
		setupW = io.MultiWriter(&outputBuf, logFd)
	}
	log.Printf("start requested: project=%s branch=%s instance=%s repo=%q main_dir=%s", req.Project, req.Branch, instanceID, p.Repo, p.MainDir())

	// Ensure the canonical checkout exists (clone if needed).
	if err := ensureMainCheckout(p, setupW); err != nil {
		log.Printf("start failed: stage=clone project=%s branch=%s instance=%s repo=%q elapsed=%s err=%v%s",
			req.Project, req.Branch, instanceID, p.Repo, time.Since(startedAt).Round(time.Millisecond), err, repoURLHintSuffix(p.Repo))
		respond(conn, proto.Response{OK: false, Error: err.Error()})
		return
	}

	// Pull latest changes so the new worktree branches from current remote HEAD.
	// Non-fatal: log the warning and continue so offline use still works.
	if err := pullMain(p, setupW); err != nil {
		log.Printf("warning: git pull failed for %s: %v", req.Project, err)
	}

	// Overlay grove.yaml from the repo root if it exists.
	// This is the canonical config location — teams commit it alongside their
	// code so any grove user gets the right container/agent/start/finish setup.
	inRepoFound, err := loadInRepoConfig(p)
	if err != nil {
		log.Printf("warning: could not read grove.yaml for %s: %v", req.Project, err)
	}

	// If there is no grove.yaml the project is not configured enough to start.
	// Tell the client so it can prompt the user to create one.
	if !inRepoFound {
		respond(conn, proto.Response{
			OK:       false,
			Error:    "no grove.yaml found in " + req.Project,
			InitPath: p.MainDir(),
		})
		return
	}

	// Create the git worktree on the user-specified branch.
	worktreeDir, err := createWorktree(p, instanceID, req.Branch)
	if err != nil {
		log.Printf("start failed: stage=worktree project=%s branch=%s instance=%s main_dir=%s elapsed=%s err=%v",
			req.Project, req.Branch, instanceID, p.MainDir(), time.Since(startedAt).Round(time.Millisecond), err)
		respond(conn, proto.Response{OK: false, Error: err.Error()})
		return
	}

	// Start the container with the worktree bind-mounted inside it.
	containerName, err := startContainer(p, instanceID, worktreeDir, setupW)
	if err != nil {
		removeWorktree(p, instanceID, req.Branch)
		log.Printf("start failed: stage=container project=%s branch=%s instance=%s worktree=%s elapsed=%s err=%v",
			req.Project, req.Branch, instanceID, worktreeDir, time.Since(startedAt).Round(time.Millisecond), err)
		respond(conn, proto.Response{OK: false, Error: err.Error()})
		return
	}
	composeProject := ""
	if p.Container.Compose != "" {
		composeProject = "grove-" + instanceID
	}

	// Run start commands inside the container.
	if err := runStart(p, containerName, setupW); err != nil {
		stopContainer(containerName, composeProject)
		removeWorktree(p, instanceID, req.Branch)
		log.Printf("start failed: stage=start project=%s branch=%s instance=%s worktree=%s elapsed=%s err=%v",
			req.Project, req.Branch, instanceID, worktreeDir, time.Since(startedAt).Round(time.Millisecond), err)
		respond(conn, proto.Response{OK: false, Error: err.Error()})
		return
	}

	// Ensure the agent binary is available inside the container.
	// For known agents (claude, aider) this auto-installs if missing.
	agentCmd := p.Agent.Command
	if agentCmd == "" {
		agentCmd = "sh"
	}
	if err := ensureAgentInstalled(agentCmd, containerName, setupW); err != nil {
		stopContainer(containerName, composeProject)
		removeWorktree(p, instanceID, req.Branch)
		log.Printf("start failed: stage=agent-install project=%s branch=%s instance=%s worktree=%s elapsed=%s err=%v",
			req.Project, req.Branch, instanceID, worktreeDir, time.Since(startedAt).Round(time.Millisecond), err)
		respond(conn, proto.Response{OK: false, Error: err.Error()})
		return
	}

	inst := &Instance{
		ID:             instanceID,
		Project:        req.Project,
		Branch:         req.Branch,
		WorktreeDir:    worktreeDir,
		CreatedAt:      time.Now(),
		LogFile:        logFile,
		state:          proto.StateRunning,
		InstancesDir:   filepath.Join(d.rootDir, "instances"),
		ContainerID:    containerName,
		ComposeProject: composeProject,
	}

	// Build the agent environment: env file is the base, request-level
	// values (from the CLI prompt or host env) override.
	agentEnv := loadEnvFile(d.rootDir)
	for k, v := range req.AgentEnv {
		agentEnv[k] = v
	}

	if err := inst.startAgent(agentCmd, p.Agent.Args, agentEnv); err != nil {
		stopContainer(containerName, composeProject)
		removeWorktree(p, instanceID, req.Branch)
		log.Printf("start failed: stage=agent-launch project=%s branch=%s instance=%s worktree=%s elapsed=%s err=%v",
			req.Project, req.Branch, instanceID, worktreeDir, time.Since(startedAt).Round(time.Millisecond), err)
		respond(conn, proto.Response{OK: false, Error: err.Error()})
		return
	}

	d.mu.Lock()
	d.instances[instanceID] = inst
	d.mu.Unlock()

	inst.persistMeta(filepath.Join(d.rootDir, "instances"))

	// Send the JSON ACK first, then stream any captured setup output.
	// The client reads the JSON line, io.Copy's the rest to stdout, then attaches.
	respond(conn, proto.Response{OK: true, InstanceID: instanceID})
	if outputBuf.Len() > 0 {
		conn.Write(outputBuf.Bytes())
	}
	log.Printf("start succeeded: project=%s branch=%s instance=%s worktree=%s elapsed=%s", req.Project, req.Branch, instanceID, worktreeDir, time.Since(startedAt).Round(time.Millisecond))
}

func repoURLHintSuffix(repo string) string {
	if strings.HasPrefix(repo, "github.com/") || strings.HasPrefix(repo, "gitlab.com/") || strings.HasPrefix(repo, "bitbucket.org/") {
		return " hint=\"repo URL may be missing scheme; try https://host/org/repo.git or git@host:org/repo.git\""
	}
	return ""
}

func (d *Daemon) handleList(conn net.Conn) {
	d.mu.Lock()
	infos := make([]proto.InstanceInfo, 0, len(d.instances))
	for _, inst := range d.instances {
		infos = append(infos, inst.Info())
	}
	d.mu.Unlock()

	sort.Slice(infos, func(i, j int) bool {
		return infos[i].CreatedAt < infos[j].CreatedAt
	})

	respond(conn, proto.Response{OK: true, Instances: infos})
}

func (d *Daemon) handleAttach(conn net.Conn, req proto.Request) {
	inst := d.getInstance(req.InstanceID)
	if inst == nil {
		respond(conn, proto.Response{OK: false, Error: "instance not found: " + req.InstanceID})
		return
	}

	inst.mu.Lock()
	state := inst.state
	inst.mu.Unlock()

	if state == proto.StateExited || state == proto.StateCrashed || state == proto.StateKilled || state == proto.StateFinished {
		respond(conn, proto.Response{OK: false, Error: "instance has " + strings.ToLower(state)})
		return
	}

	// Send the handshake ACK before entering streaming mode.
	respond(conn, proto.Response{OK: true})

	// Attach blocks until the client detaches or the agent exits.
	inst.Attach(conn)
}

func (d *Daemon) handleLogs(conn net.Conn, req proto.Request) {
	inst := d.getInstance(req.InstanceID)
	if inst == nil {
		respond(conn, proto.Response{OK: false, Error: "instance not found: " + req.InstanceID})
		return
	}

	inst.mu.Lock()
	logs := make([]byte, len(inst.logBuf))
	copy(logs, inst.logBuf)
	inst.mu.Unlock()

	// Send as a JSON string.
	respond(conn, proto.Response{OK: true, InstanceID: req.InstanceID})
	conn.Write(logs)
}

func (d *Daemon) handleLogsFollow(conn net.Conn, req proto.Request) {
	inst := d.getInstance(req.InstanceID)
	if inst == nil {
		respond(conn, proto.Response{OK: false, Error: "instance not found: " + req.InstanceID})
		return
	}
	respond(conn, proto.Response{OK: true})

	// Snapshot current logBuf; track how many bytes we've sent.
	inst.mu.Lock()
	initial := make([]byte, len(inst.logBuf))
	copy(initial, inst.logBuf)
	offset := len(inst.logBuf)
	inst.mu.Unlock()

	if len(initial) > 0 {
		if _, err := conn.Write(initial); err != nil {
			return
		}
	}

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		inst.mu.Lock()
		state := inst.state
		// Clamp offset if logBuf was trimmed (rolled over 1 MiB cap).
		if offset > len(inst.logBuf) {
			offset = 0
		}
		newData := make([]byte, len(inst.logBuf)-offset)
		copy(newData, inst.logBuf[offset:])
		offset += len(newData)
		inst.mu.Unlock()

		if len(newData) > 0 {
			if _, err := conn.Write(newData); err != nil {
				return // client disconnected
			}
		}

		// Exit when instance is done AND no more new bytes remain.
		if (state == proto.StateExited || state == proto.StateCrashed || state == proto.StateKilled || state == proto.StateFinished) && len(newData) == 0 {
			return
		}
	}
}

func (d *Daemon) handleStop(conn net.Conn, req proto.Request) {
	inst := d.getInstance(req.InstanceID)
	if inst == nil {
		respond(conn, proto.Response{OK: false, Error: "instance not found: " + req.InstanceID})
		return
	}

	// Kill the agent process if it is running; ptyReader will transition
	// the state to CRASHED and persist it.  For already-dead instances
	// (EXITED/CRASHED/FINISHED) this is a no-op.
	inst.destroy()

	respond(conn, proto.Response{OK: true})
}

func (d *Daemon) handleDrop(conn net.Conn, req proto.Request) {
	inst := d.getInstance(req.InstanceID)
	if inst == nil {
		respond(conn, proto.Response{OK: false, Error: "instance not found: " + req.InstanceID})
		return
	}

	worktreeDir := inst.WorktreeDir
	branch := inst.Branch
	containerID := inst.ContainerID
	composeProject := inst.ComposeProject

	// Kill the docker exec session (container keeps running until stopContainer).
	inst.destroy()

	// Stop and remove the container (or compose stack).
	stopContainer(containerID, composeProject)

	// Derive mainDir: worktreeDir is <dataDir>/worktrees/<id>, so main is <dataDir>/main.
	mainDir := filepath.Join(filepath.Dir(filepath.Dir(worktreeDir)), "main")

	exec.Command("git", "-C", mainDir, "worktree", "remove", "--force", worktreeDir).Run()
	exec.Command("git", "-C", mainDir, "branch", "-D", branch).Run()

	d.mu.Lock()
	delete(d.instances, req.InstanceID)
	d.mu.Unlock()

	os.Remove(filepath.Join(d.rootDir, "instances", req.InstanceID+".json"))

	respond(conn, proto.Response{OK: true})
}

func (d *Daemon) handleFinish(conn net.Conn, req proto.Request) {
	inst := d.getInstance(req.InstanceID)
	if inst == nil {
		respond(conn, proto.Response{OK: false, Error: "instance not found: " + req.InstanceID})
		return
	}

	worktreeDir := inst.WorktreeDir
	branch := inst.Branch
	projectName := inst.Project

	inst.mu.Lock()
	state := inst.state
	switch state {
	case proto.StateExited, proto.StateCrashed, proto.StateKilled:
		// Process already dead; transition to FINISHED directly.
		inst.state = proto.StateFinished
		inst.mu.Unlock()
	case proto.StateFinished:
		// Already finished; nothing to do.
		inst.mu.Unlock()
	default:
		// Agent is alive; request finish and wait for ptyReader to exit.
		inst.finishRequest = true
		processDone := inst.processDone
		inst.mu.Unlock()
		inst.destroy()
		if processDone != nil {
			<-processDone
		}
	}

	// Persist FINISHED state. (ptyReader may have already done this if it ran,
	// but an extra write is harmless.)
	inst.persistMeta(filepath.Join(d.rootDir, "instances"))

	// Send ACK — instance is now FINISHED regardless of what complete commands do.
	respond(conn, proto.Response{OK: true, WorktreeDir: worktreeDir, Branch: branch})

	p, err := loadProject(d.rootDir, projectName)
	if err != nil {
		fmt.Fprintf(conn, "warning: could not load project to run finish commands: %v\n", err)
		stopContainer(inst.ContainerID, inst.ComposeProject)
		return
	}
	if _, err := loadInRepoConfig(p); err != nil {
		log.Printf("warning: could not read grove.yaml for %s: %v", projectName, err)
	}
	if len(p.Finish) == 0 {
		stopContainer(inst.ContainerID, inst.ComposeProject)
		return
	}

	// Open the instance log file for appending so finish command output is
	// preserved even if the client disconnects mid-way.
	logFd, _ := os.OpenFile(inst.LogFile, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if logFd != nil {
		defer logFd.Close()
	}

	// w writes to both the connection and the log file.  If the client
	// disconnects, writes to conn are silently dropped but the log keeps
	// receiving output and commands run to completion.
	w := newResilientWriter(conn, logFd)

	containerID := inst.ContainerID
	composeProject := inst.ComposeProject

	for _, cmdStr := range p.Finish {
		expanded := strings.ReplaceAll(cmdStr, "{{branch}}", branch)
		fmt.Fprintf(w, "$ %s\n", expanded)
		if err := execInContainer(containerID, expanded, w); err != nil {
			fmt.Fprintf(w, "error: command failed: %v\n", err)
			log.Printf("instance %s: finish command failed: %v", inst.ID, err)
			stopContainer(containerID, composeProject)
			return
		}
	}

	stopContainer(containerID, composeProject)
}

func (d *Daemon) handleCheck(conn net.Conn, req proto.Request) {
	inst := d.getInstance(req.InstanceID)
	if inst == nil {
		respond(conn, proto.Response{OK: false, Error: "instance not found: " + req.InstanceID})
		return
	}

	projectName := inst.Project

	inst.mu.Lock()
	state := inst.state
	switch state {
	case proto.StateFinished, proto.StateExited, proto.StateCrashed, proto.StateKilled, proto.StateChecking:
		inst.mu.Unlock()
		respond(conn, proto.Response{OK: false, Error: "cannot check: instance is " + state})
		return
	default:
		inst.state = proto.StateChecking
		inst.mu.Unlock()
	}

	defer func() {
		inst.mu.Lock()
		if inst.state == proto.StateChecking {
			inst.state = proto.StateWaiting
		}
		inst.mu.Unlock()
	}()

	p, err := loadProject(d.rootDir, projectName)
	if err != nil {
		respond(conn, proto.Response{OK: false, Error: err.Error()})
		return
	}
	if _, err := loadInRepoConfig(p); err != nil {
		log.Printf("warning: could not read grove.yaml for %s: %v", projectName, err)
	}
	if len(p.Check) == 0 {
		respond(conn, proto.Response{OK: false, Error: "no check commands defined in grove.yaml"})
		return
	}

	respond(conn, proto.Response{OK: true})

	logFd, _ := os.OpenFile(inst.LogFile, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if logFd != nil {
		defer logFd.Close()
	}

	w := newResilientWriter(conn, logFd)

	containerID := inst.ContainerID

	var wg sync.WaitGroup
	for _, cmdStr := range p.Check {
		wg.Add(1)
		go func(cmd string) {
			defer wg.Done()
			fmt.Fprintf(w, "$ %s\n", cmd)
			if err := execInContainer(containerID, cmd, w); err != nil {
				fmt.Fprintf(w, "error: check command failed: %v\n", err)
				log.Printf("instance %s: check command %q failed: %v", inst.ID, cmd, err)
			}
		}(cmdStr)
	}
	wg.Wait()
}

func (d *Daemon) handleRestart(conn net.Conn, req proto.Request) {
	inst := d.getInstance(req.InstanceID)
	if inst == nil {
		respond(conn, proto.Response{OK: false, Error: "instance not found: " + req.InstanceID})
		return
	}

	inst.mu.Lock()
	state := inst.state
	inst.mu.Unlock()

	if state != proto.StateExited && state != proto.StateCrashed && state != proto.StateKilled && state != proto.StateFinished {
		respond(conn, proto.Response{OK: false, Error: "cannot restart: instance is " + state})
		return
	}

	p, err := loadProject(d.rootDir, inst.Project)
	if err != nil {
		respond(conn, proto.Response{OK: false, Error: err.Error()})
		return
	}

	// Non-fatal pull; output goes to daemon log only.
	if err := pullMain(p, log.Writer()); err != nil {
		log.Printf("warning: git pull failed for %s: %v", inst.Project, err)
	}

	agentCmd := p.Agent.Command
	if agentCmd == "" {
		agentCmd = "sh"
	}

	// Reset mutable state before restarting.
	inst.mu.Lock()
	inst.endedAt = time.Time{}
	inst.finishRequest = false
	inst.killed = false
	inst.mu.Unlock()

	agentEnv := loadEnvFile(d.rootDir)
	for k, v := range req.AgentEnv {
		agentEnv[k] = v
	}

	if err := inst.startAgent(agentCmd, p.Agent.Args, agentEnv); err != nil {
		respond(conn, proto.Response{OK: false, Error: err.Error()})
		return
	}

	inst.persistMeta(filepath.Join(d.rootDir, "instances"))

	respond(conn, proto.Response{OK: true})
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func (d *Daemon) getInstance(id string) *Instance {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.instances[id]
}

// idAlphabet is the ordered set of characters used to build instance IDs.
// Single-character IDs are assigned first (digits 1-9, then a-z), giving 35
// slots before falling back to two-character combinations.
var idAlphabet = []string{
	"1", "2", "3", "4", "5", "6", "7", "8", "9",
	"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l", "m",
	"n", "o", "p", "q", "r", "s", "t", "u", "v", "w", "x", "y", "z",
}

// nextInstanceID returns the lowest unused instance ID.
// Must be called with d.mu held.
func (d *Daemon) nextInstanceID() string {
	for _, id := range idAlphabet {
		if _, taken := d.instances[id]; !taken {
			return id
		}
	}
	for _, a := range idAlphabet {
		for _, b := range idAlphabet {
			id := a + b
			if _, taken := d.instances[id]; !taken {
				return id
			}
		}
	}
	// Extremely unlikely: fall back to random hex.
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// loadPersistedInstances reads instance JSON files written by previous daemon
// runs and re-registers them with the correct state.  Instances that were
// RUNNING/WAITING/ATTACHED when the daemon was killed are marked as CRASHED.
// EXITED, CRASHED, and FINISHED states are preserved as-is.
func (d *Daemon) loadPersistedInstances() error {
	instancesDir := filepath.Join(d.rootDir, "instances")
	entries, err := os.ReadDir(instancesDir)
	if err != nil {
		return err
	}

	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(instancesDir, e.Name()))
		if err != nil {
			continue
		}
		var info proto.InstanceInfo
		if err := json.Unmarshal(data, &info); err != nil {
			continue
		}

		// Determine the correct state on reload.
		state := info.State
		endedAt := time.Time{}
		if info.EndedAt > 0 {
			endedAt = time.Unix(info.EndedAt, 0)
		}

		// If the daemon was killed mid-run, the process is gone → CRASHED.
		if state == proto.StateRunning || state == proto.StateWaiting || state == proto.StateAttached {
			state = proto.StateCrashed
			endedAt = time.Now()
		}

		inst := &Instance{
			ID:             info.ID,
			Project:        info.Project,
			Branch:         info.Branch,
			WorktreeDir:    info.WorktreeDir,
			CreatedAt:      time.Unix(info.CreatedAt, 0),
			LogFile:        filepath.Join(d.rootDir, "logs", info.ID+".log"),
			state:          state,
			endedAt:        endedAt,
			InstancesDir:   instancesDir,
			ContainerID:    info.ContainerID,
			ComposeProject: info.ComposeProject,
		}
		d.instances[info.ID] = inst

		// Persist the corrected state if it changed (e.g., RUNNING → CRASHED).
		if state != info.State {
			inst.persistMeta(instancesDir)
		}
	}

	return nil
}

// ─── resilientWriter ──────────────────────────────────────────────────────────

// resilientWriter fans output to a log file (always) and a network connection
// (best-effort).  If the connection breaks, writes continue to the log and the
// caller (exec.Command) never sees an error, so the child process keeps running
// even if the client disconnects.
type resilientWriter struct {
	mu     sync.Mutex
	conn   net.Conn
	log    *os.File
	connOK bool
}

func newResilientWriter(conn net.Conn, log *os.File) *resilientWriter {
	return &resilientWriter{conn: conn, log: log, connOK: true}
}

func (rw *resilientWriter) Write(p []byte) (int, error) {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	if rw.connOK {
		if _, err := rw.conn.Write(p); err != nil {
			rw.connOK = false
		}
	}
	if rw.log != nil {
		rw.log.Write(p) // best-effort; ignore log errors
	}
	return len(p), nil // always succeed so child processes never get SIGPIPE
}
