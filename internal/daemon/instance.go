package daemon

// instance.go – per-instance lifecycle: PTY allocation, agent spawn,
// log buffering, and attach/detach handling.
//
// Architecture overview
// ─────────────────────
//
//  ┌──────────────────────────────┐
//  │  Instance                    │
//  │  ┌────────────┐              │
//  │  │ agent proc │◄── PTY slave │
//  │  └────────────┘              │
//  │         ▲  ▼                 │
//  │       PTY master             │
//  │         │                    │
//  │    ptyReader goroutine       │
//  │     ├── appends to logBuf    │
//  │     └── forwards to attachedConn (if any)
//  │                              │
//  │  Attach: client conn ──────► │
//  │    (framed stdin/resize/     │
//  │     detach messages)         │
//  └──────────────────────────────┘

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/ianremillard/grove/internal/proto"
)

const (
	maxLogBytes = 1 << 20 // 1 MiB rolling log per instance

	// waitingIdleThreshold is how long an agent must produce no PTY output
	// before its state is promoted from RUNNING to WAITING.
	waitingIdleThreshold = 2 * time.Second
)

// Instance represents one running (or stopped) agent session.
type Instance struct {
	// Immutable after creation.
	ID          string
	Project     string
	Branch      string
	WorktreeDir string
	CreatedAt   time.Time
	LogFile     string // path to the on-disk log file

	// Mutable; protected by mu.
	mu             sync.Mutex
	state          string
	pid            int
	ptm            *os.File     // PTY master; nil after process exits
	logBuf         []byte       // rolling in-memory copy of recent output
	lastOutputTime time.Time    // last time the PTY produced output
	endedAt        time.Time    // when the process exited; zero if still running
	attachedConn   net.Conn     // non-nil while a client is attached
	attachDone     chan struct{} // closed when the current attach session ends

	// InstancesDir is set so ptyReader can persist state changes on exit.
	InstancesDir string
	// finishRequest, when true, causes ptyReader to transition to FINISHED
	// instead of EXITED/CRASHED when the process stops.
	finishRequest bool
	// killed, when true, means destroy() was called deliberately; ptyReader
	// transitions to KILLED instead of CRASHED on non-zero exit.
	killed bool
	// processDone is closed by ptyReader when the agent process fully exits.
	processDone chan struct{}
}

// Info returns a serialisable snapshot of this instance's metadata.
func (inst *Instance) Info() proto.InstanceInfo {
	inst.mu.Lock()
	defer inst.mu.Unlock()

	state := inst.state
	// Promote RUNNING → WAITING when no PTY output has been seen for 2 seconds.
	// Claude streams output continuously while working; silence means it is
	// waiting for human input.
	if state == proto.StateRunning && !inst.lastOutputTime.IsZero() &&
		time.Since(inst.lastOutputTime) > waitingIdleThreshold {
		state = proto.StateWaiting
	}

	var endedAt int64
	if !inst.endedAt.IsZero() {
		endedAt = inst.endedAt.Unix()
	}
	return proto.InstanceInfo{
		ID:          inst.ID,
		Project:     inst.Project,
		State:       state,
		Branch:      inst.Branch,
		WorktreeDir: inst.WorktreeDir,
		CreatedAt:   inst.CreatedAt.Unix(),
		EndedAt:     endedAt,
		PID:         inst.pid,
	}
}

// persistMeta writes the instance metadata to ~/.grove/instances/<id>.json.
func (inst *Instance) persistMeta(instancesDir string) {
	info := inst.Info()
	data, _ := json.MarshalIndent(info, "", "  ")
	path := filepath.Join(instancesDir, inst.ID+".json")
	_ = os.WriteFile(path, data, 0o644)
}

// startAgent allocates a PTY, starts the agent process inside it, and
// launches the background goroutine that drains PTY output into logBuf.
//
// The agent process is placed in its own process group so that destroy()
// can cleanly kill the whole group.
func (inst *Instance) startAgent(agentCmd string, agentArgs []string) error {
	cmd := exec.Command(agentCmd, agentArgs...)
	cmd.Dir = inst.WorktreeDir
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	// pty.Start sets Setsid:true on the child, which creates a new session and
	// process group (PGID = child PID).  Do NOT also set Setpgid here: calling
	// setpgid() after setsid() on the session leader returns EPERM on macOS,
	// which propagates back as "fork/exec: operation not permitted".
	// The new session group already gives us kill(-pid, SIGKILL) semantics.

	// Start the command attached to a new PTY.
	ptm, err := pty.Start(cmd)
	if err != nil {
		return fmt.Errorf("pty.Start: %w", err)
	}

	inst.mu.Lock()
	inst.ptm = ptm
	inst.pid = cmd.Process.Pid
	inst.state = proto.StateRunning
	inst.processDone = make(chan struct{})
	inst.mu.Unlock()

	// Background goroutine: drain PTY master and buffer/forward output.
	go inst.ptyReader(cmd)

	return nil
}

// ptyReader reads all output from the PTY master in a tight loop.
// It:
//   - appends output to the rolling in-memory log buffer
//   - forwards output to the attached client connection (if any)
//   - writes output to the on-disk log file
//
// It transitions the instance to EXITED or CRASHED when the process ends.
func (inst *Instance) ptyReader(cmd *exec.Cmd) {
	logFd, err := os.OpenFile(inst.LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("instance %s: cannot open log file: %v", inst.ID, err)
	}
	defer func() {
		if logFd != nil {
			logFd.Close()
		}
	}()

	buf := make([]byte, 4096)
	for {
		n, err := inst.ptm.Read(buf)
		if n > 0 {
			chunk := buf[:n]

			// Write to on-disk log.
			if logFd != nil {
				logFd.Write(chunk)
			}

			inst.mu.Lock()
			// Append to rolling in-memory buffer, trimming if too large.
			inst.logBuf = append(inst.logBuf, chunk...)
			if len(inst.logBuf) > maxLogBytes {
				inst.logBuf = inst.logBuf[len(inst.logBuf)-maxLogBytes:]
			}
			inst.lastOutputTime = time.Now()
			conn := inst.attachedConn
			inst.mu.Unlock()

			// Forward to attached client (ignore errors; client may have gone away).
			if conn != nil {
				conn.Write(chunk)
			}
		}
		if err != nil {
			// PTY read error means the slave side closed (process exited).
			break
		}
	}

	// Wait for the process to fully exit and determine the exit code.
	waitErr := cmd.Wait()

	inst.mu.Lock()
	inst.ptm.Close()
	inst.ptm = nil
	inst.endedAt = time.Now()
	if waitErr == nil {
		inst.state = proto.StateExited
	} else if inst.killed {
		inst.state = proto.StateKilled
	} else {
		inst.state = proto.StateCrashed
	}
	conn := inst.attachedConn
	inst.attachedConn = nil
	inst.mu.Unlock()

	// Close the client connection to unblock the Attach goroutine's frame
	// reader.  The Attach goroutine's defer is the sole owner of close(done);
	// closing it here too would double-close the channel and panic the daemon.
	if conn != nil {
		conn.Close()
	}

	log.Printf("instance %s: agent exited (%v)", inst.ID, waitErr)

	// If finish was requested, override state to FINISHED.
	inst.mu.Lock()
	if inst.finishRequest {
		inst.state = proto.StateFinished
	}
	instancesDir := inst.InstancesDir
	processDone := inst.processDone
	inst.mu.Unlock()

	// Persist the final state to disk.
	if instancesDir != "" {
		inst.persistMeta(instancesDir)
	}

	// Signal that the process has fully exited.
	if processDone != nil {
		close(processDone)
	}
}

// Attach connects a client network connection to this instance's PTY.
//
// It:
//  1. Sends the rolling log buffer to the client so they see prior output.
//  2. Registers the connection as the current attached client.
//  3. Starts a goroutine reading framed messages from the client (stdin data,
//     resize events, detach signal).
//  4. Blocks until the session ends (client detaches, client disconnects,
//     or the agent exits).
func (inst *Instance) Attach(conn net.Conn) {
	inst.mu.Lock()
	if inst.state == proto.StateAttached {
		inst.mu.Unlock()
		fmt.Fprintf(conn, `{"ok":false,"error":"already attached"}`+"\n")
		return
	}

	// Grab a copy of the log buffer to replay.
	replay := make([]byte, len(inst.logBuf))
	copy(replay, inst.logBuf)

	done := make(chan struct{})
	inst.attachedConn = conn
	inst.attachDone = done
	inst.state = proto.StateAttached
	ptm := inst.ptm
	inst.mu.Unlock()

	// Replay buffered output so the human sees what the agent has done.
	if len(replay) > 0 {
		conn.Write(replay)
	}

	// If the agent is already gone there's nothing to do.
	if ptm == nil {
		conn.Close()
		return
	}

	// Read framed messages from the client and act on them.
	go func() {
		defer func() {
			// Clean up regardless of how we exit.
			inst.mu.Lock()
			wasAttached := inst.attachedConn == conn
			if wasAttached {
				inst.attachedConn = nil
				if inst.state == proto.StateAttached {
					inst.state = proto.StateRunning
				}
			}
			inst.mu.Unlock()
			conn.Close()
			close(done)
		}()

		for {
			frameType, payload, err := proto.ReadFrame(conn)
			if err != nil {
				if err != io.EOF {
					log.Printf("instance %s: attach read: %v", inst.ID, err)
				}
				return
			}

			switch frameType {
			case proto.AttachFrameData:
				// Write client stdin into the PTY.
				inst.mu.Lock()
				p := inst.ptm
				inst.mu.Unlock()
				if p != nil {
					p.Write(payload)
				}

			case proto.AttachFrameResize:
				// payload: 2-byte cols + 2-byte rows (big-endian uint16)
				if len(payload) == 4 {
					cols := binary.BigEndian.Uint16(payload[0:2])
					rows := binary.BigEndian.Uint16(payload[2:4])
					inst.mu.Lock()
					p := inst.ptm
					inst.mu.Unlock()
					if p != nil {
						pty.Setsize(p, &pty.Winsize{
							Cols: cols,
							Rows: rows,
						})
					}
				}

			case proto.AttachFrameDetach:
				// Client requested a clean detach; just return.
				return
			}
		}
	}()

	// Block the caller (the daemon's request handler) until the attach ends.
	<-done
}

// destroy kills the agent process and its process group, then closes the PTY.
func (inst *Instance) destroy() {
	inst.mu.Lock()
	ptm := inst.ptm
	pid := inst.pid
	conn := inst.attachedConn
	inst.killed = true
	inst.mu.Unlock()

	if pid > 0 {
		// Look up the actual PGID rather than assuming it equals the PID.
		// After pty.Start (which calls setsid), the child is its own session
		// leader and PGID = PID — but using Getpgid makes this explicit and
		// safe against any edge cases.
		pgid, err := syscall.Getpgid(pid)
		if err == nil && pgid > 0 {
			syscall.Kill(-pgid, syscall.SIGKILL)
		} else {
			// Fallback: kill just the process.
			syscall.Kill(pid, syscall.SIGKILL)
		}
	}

	if ptm != nil {
		ptm.Close()
	}

	if conn != nil {
		conn.Close()
	}
}
