// catherd – the CLI client for the catherdd daemon.
//
// Usage:
//
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
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ianremillard/catherdd/internal/proto"
	"golang.org/x/term"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "start":
		cmdStart()
	case "list":
		cmdList()
	case "attach":
		cmdAttach()
	case "logs":
		cmdLogs()
	case "destroy":
		cmdDestroy()
	default:
		fmt.Fprintf(os.Stderr, "catherd: unknown command %q\n", os.Args[1])
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `catherd – supervise AI coding agent instances

Commands:
  start <project> "<task>"   Start a new agent instance
  list                       List all instances
  attach <instance-id>       Attach terminal to an instance (detach: Ctrl-])
  logs <instance-id>         Print buffered output for an instance
  destroy <instance-id>      Stop and destroy an instance`)
}

// ─── Subcommand implementations ───────────────────────────────────────────────

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
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: catherd logs <instance-id>")
		os.Exit(1)
	}
	instanceID := os.Args[2]

	socketPath := daemonSocket()
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "catherd: cannot connect to daemon: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	if err := writeRequest(conn, proto.Request{
		Type:       proto.ReqLogs,
		InstanceID: instanceID,
	}); err != nil {
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

	// After the JSON response line the daemon streams raw log bytes.
	io.Copy(os.Stdout, conn)
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

// ─── Daemon connection helpers ────────────────────────────────────────────────

// rootDir returns the catherdd data directory.
func rootDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".catherdd")
}

// daemonSocket returns the Unix socket path and ensures the daemon is running.
func daemonSocket() string {
	root := rootDir()
	sock := filepath.Join(root, "catherdd.sock")
	ensureDaemon(sock)
	return sock
}

// ensureDaemon starts catherdd in the background if the socket doesn't exist
// or is not responding to pings.
func ensureDaemon(socketPath string) {
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

	cmd := exec.Command(daemonBin)
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
