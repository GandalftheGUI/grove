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
	"path/filepath"
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

	case proto.ReqDestroy:
		d.handleDestroy(conn, req)

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

	instanceID := newInstanceID()
	branchName := "agent/" + instanceID

	// Create the git worktree.
	worktreeDir, err := createWorktree(p, instanceID, branchName)
	if err != nil {
		respond(conn, proto.Response{OK: false, Error: err.Error()})
		return
	}

	// Run bootstrap commands in the new worktree.
	if err := runBootstrap(p, worktreeDir); err != nil {
		removeWorktree(p, instanceID, branchName)
		respond(conn, proto.Response{OK: false, Error: err.Error()})
		return
	}

	logFile := filepath.Join(d.rootDir, "logs", instanceID+".log")

	inst := &Instance{
		ID:          instanceID,
		Project:     req.Project,
		Task:        req.Task,
		Branch:      branchName,
		WorktreeDir: worktreeDir,
		CreatedAt:   time.Now(),
		LogFile:     logFile,
		state:       proto.StateRunning,
	}

	// Start the agent in a PTY.
	agentCmd := p.Agent.Command
	if agentCmd == "" {
		agentCmd = "sh" // fallback for testing
	}
	if err := inst.startAgent(agentCmd, p.Agent.Args); err != nil {
		removeWorktree(p, instanceID, branchName)
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

	respond(conn, proto.Response{OK: true, Instances: infos})
}

func (d *Daemon) handleAttach(conn net.Conn, req proto.Request) {
	inst := d.getInstance(req.InstanceID)
	if inst == nil {
		respond(conn, proto.Response{OK: false, Error: "instance not found: " + req.InstanceID})
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
		if (state == proto.StateExited || state == proto.StateCrashed) && len(newData) == 0 {
			return
		}
	}
}

func (d *Daemon) handleDestroy(conn net.Conn, req proto.Request) {
	inst := d.getInstance(req.InstanceID)
	if inst == nil {
		respond(conn, proto.Response{OK: false, Error: "instance not found: " + req.InstanceID})
		return
	}

	inst.destroy()

	d.mu.Lock()
	delete(d.instances, req.InstanceID)
	d.mu.Unlock()

	// Remove persisted metadata.
	metaPath := filepath.Join(d.rootDir, "instances", req.InstanceID+".json")
	os.Remove(metaPath)

	respond(conn, proto.Response{OK: true})
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func (d *Daemon) getInstance(id string) *Instance {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.instances[id]
}

// newInstanceID returns an 8-character random hex string.
func newInstanceID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// loadPersistedInstances reads instance JSON files written by previous daemon
// runs and re-registers instances that were RUNNING or ATTACHED (they will
// appear as EXITED on reload since the processes are gone).
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
		// Register as EXITED; the process is gone after a daemon restart.
		inst := &Instance{
			ID:          info.ID,
			Project:     info.Project,
			Task:        info.Task,
			Branch:      info.Branch,
			WorktreeDir: info.WorktreeDir,
			CreatedAt:   time.Unix(info.CreatedAt, 0),
			LogFile:     filepath.Join(d.rootDir, "logs", info.ID+".log"),
			state:       proto.StateExited,
		}
		d.instances[info.ID] = inst
	}

	return nil
}
