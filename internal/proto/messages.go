// Package proto defines the IPC message types and attach-stream framing
// used between catherd (client) and catherdd (daemon) over a Unix domain socket.
//
// Normal commands use newline-delimited JSON: client sends one Request,
// daemon sends one Response, then the connection closes.
//
// The attach command is special: after the JSON handshake the connection
// enters a streaming mode where the server sends raw PTY output and the
// client sends framed control messages (data, resize, detach).
package proto

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Request type constants.
const (
	ReqPing    = "ping"
	ReqStart   = "start"
	ReqList    = "list"
	ReqAttach  = "attach"
	ReqLogs       = "logs"
	ReqLogsFollow = "logs_follow"
	ReqStop = "stop"
	ReqDrop       = "drop"
	ReqFinish     = "finish"
	ReqRestart    = "restart"
)

// Instance state constants.
const (
	StateRunning  = "RUNNING"
	StateWaiting  = "WAITING"
	StateAttached = "ATTACHED"
	StateExited   = "EXITED"
	StateCrashed  = "CRASHED"
	StateKilled   = "KILLED"
	StateFinished = "FINISHED"
)

// Request is the JSON payload sent from catherd to catherdd.
type Request struct {
	Type       string `json:"type"`
	Project    string `json:"project,omitempty"`
	Branch     string `json:"branch,omitempty"`
	InstanceID string `json:"instance_id,omitempty"`
}

// InstanceInfo is a point-in-time snapshot of an instance's metadata.
type InstanceInfo struct {
	ID          string `json:"id"`
	Project     string `json:"project"`
	State       string `json:"state"`
	Branch      string `json:"branch"`
	WorktreeDir string `json:"worktree_dir"`
	CreatedAt   int64  `json:"created_at"`
	EndedAt     int64  `json:"ended_at,omitempty"` // unix timestamp; 0 if still running
	PID         int    `json:"pid"`
}

// Response is the JSON payload returned by the daemon for all non-attach commands.
type Response struct {
	OK         bool           `json:"ok"`
	Error      string         `json:"error,omitempty"`
	InstanceID string         `json:"instance_id,omitempty"`
	Instances  []InstanceInfo `json:"instances,omitempty"`

	// Fields used by ReqFinish response.
	WorktreeDir      string   `json:"worktree_dir,omitempty"`
	CompleteCommands []string `json:"complete_commands,omitempty"`
	Branch           string   `json:"branch,omitempty"`
}

// ─── Attach stream framing ────────────────────────────────────────────────────
//
// After the JSON handshake the attach connection becomes asymmetric:
//
//   Server → Client : raw PTY output bytes (no framing; terminal handles escapes)
//   Client → Server : length-prefixed frames:
//
//     [1 byte type][4 bytes big-endian length][payload]
//
//     0x00  data    – stdin bytes to write into the PTY
//     0x01  resize  – payload: 2-byte cols + 2-byte rows (big-endian uint16)
//     0x02  detach  – no payload; client wants to detach cleanly

const (
	AttachFrameData   byte = 0x00
	AttachFrameResize byte = 0x01
	AttachFrameDetach byte = 0x02
)

// WriteFrame writes a single framed message to w.
func WriteFrame(w io.Writer, frameType byte, payload []byte) error {
	hdr := make([]byte, 5)
	hdr[0] = frameType
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	if len(payload) > 0 {
		_, err := w.Write(payload)
		return err
	}
	return nil
}

// ReadFrame reads a single framed message from r.
// Returns (frameType, payload, error).
func ReadFrame(r io.Reader) (byte, []byte, error) {
	hdr := make([]byte, 5)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return 0, nil, err
	}
	frameType := hdr[0]
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > 1<<20 { // sanity cap: 1 MiB
		return 0, nil, fmt.Errorf("attach frame too large: %d bytes", n)
	}
	if n == 0 {
		return frameType, nil, nil
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return frameType, payload, nil
}
