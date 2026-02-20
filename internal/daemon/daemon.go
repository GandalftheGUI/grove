// Package daemon implements the catherdd background daemon.
//
// The daemon listens on a Unix domain socket and handles requests from catherd
// clients.  Each request is a single newline-terminated JSON object; the daemon
// writes a single newline-terminated JSON response and then closes the
// connection — except for attach requests, which enter a bidirectional
// streaming mode (see instance.go and proto/messages.go for the wire format).
package daemon

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ianremillard/catherdd/internal/proto"
)

// Daemon is the central supervisor.  It owns a map of live instances and
// handles all IPC requests from catherd.
type Daemon struct {
	rootDir    string   // ~/.catherdd  (runtime data root)
	configDirs []string // ordered list of directories to search for project YAMLs

	mu        sync.Mutex
	instances map[string]*Instance // keyed by instance ID
}

// New creates a Daemon that uses rootDir (~/.catherdd) as its data directory.
// configDirs is the ordered list of directories to search for project.yaml
// files (personal first, then global, then fallback).  If empty, the daemon
// falls back to rootDir/projects/ for backward compatibility.
func New(rootDir string, configDirs []string) (*Daemon, error) {
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
		rootDir:    rootDir,
		configDirs: configDirs,
		instances:  make(map[string]*Instance),
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

	log.Printf("catherdd listening on %s", socketPath)

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

	p, err := loadProject(d.configDirs, d.rootDir, req.Project)
	if err != nil {
		respond(conn, proto.Response{OK: false, Error: err.Error()})
		return
	}

	// Ensure the canonical checkout exists (clone if needed).
	if err := ensureMainCheckout(p); err != nil {
		respond(conn, proto.Response{OK: false, Error: err.Error()})
		return
	}

	// Pull latest changes so the new worktree branches from current remote HEAD.
	// Non-fatal: log the warning and continue so offline use still works.
	if err := pullMain(p); err != nil {
		log.Printf("warning: git pull failed for %s: %v", req.Project, err)
	}

	d.mu.Lock()
	instanceID := d.nextInstanceID()
	d.mu.Unlock()

	// Create the git worktree on the user-specified branch.
	worktreeDir, err := createWorktree(p, instanceID, req.Branch)
	if err != nil {
		respond(conn, proto.Response{OK: false, Error: err.Error()})
		return
	}

	// Run bootstrap commands in the new worktree.
	if err := runBootstrap(p, worktreeDir); err != nil {
		removeWorktree(p, instanceID, req.Branch)
		respond(conn, proto.Response{OK: false, Error: err.Error()})
		return
	}

	logFile := filepath.Join(d.rootDir, "logs", instanceID+".log")

	inst := &Instance{
		ID:           instanceID,
		Project:      req.Project,
		Branch:       req.Branch,
		WorktreeDir:  worktreeDir,
		CreatedAt:    time.Now(),
		LogFile:      logFile,
		state:        proto.StateRunning,
		InstancesDir: filepath.Join(d.rootDir, "instances"),
	}

	// Start the agent in a PTY.
	agentCmd := p.Agent.Command
	if agentCmd == "" {
		agentCmd = "sh" // fallback for testing
	}
	if err := inst.startAgent(agentCmd, p.Agent.Args); err != nil {
		removeWorktree(p, instanceID, req.Branch)
		respond(conn, proto.Response{OK: false, Error: err.Error()})
		return
	}

	d.mu.Lock()
	d.instances[instanceID] = inst
	d.mu.Unlock()

	inst.persistMeta(filepath.Join(d.rootDir, "instances"))

	respond(conn, proto.Response{OK: true, InstanceID: instanceID})
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

	inst.destroy()

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

	p, err := loadProject(d.configDirs, d.rootDir, projectName)
	var completeCommands []string
	if err == nil {
		completeCommands = p.Complete
	}

	respond(conn, proto.Response{
		OK:               true,
		WorktreeDir:      worktreeDir,
		CompleteCommands: completeCommands,
		Branch:           branch,
	})
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

	p, err := loadProject(d.configDirs, d.rootDir, inst.Project)
	if err != nil {
		respond(conn, proto.Response{OK: false, Error: err.Error()})
		return
	}

	// Non-fatal pull.
	if err := pullMain(p); err != nil {
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

	if err := inst.startAgent(agentCmd, p.Agent.Args); err != nil {
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
			ID:           info.ID,
			Project:      info.Project,
			Branch:       info.Branch,
			WorktreeDir:  info.WorktreeDir,
			CreatedAt:    time.Unix(info.CreatedAt, 0),
			LogFile:      filepath.Join(d.rootDir, "logs", info.ID+".log"),
			state:        state,
			endedAt:      endedAt,
			InstancesDir: instancesDir,
		}
		d.instances[info.ID] = inst

		// Persist the corrected state if it changed (e.g., RUNNING → CRASHED).
		if state != info.State {
			inst.persistMeta(instancesDir)
		}
	}

	return nil
}
