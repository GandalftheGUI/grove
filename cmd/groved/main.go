// groved – the background daemon that supervises AI coding agent instances.
//
// Usage:
//
//	groved [--root <dir>]
//
// The daemon listens on a Unix domain socket at <root>/groved.sock and
// handles commands from the grove CLI.  It is normally started automatically
// by grove; you do not need to run it by hand.
package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/ianremillard/grove/internal/daemon"
)

func main() {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("cannot determine home directory: %v", err)
	}
	defaultRoot := filepath.Join(homeDir, ".grove")
	if env := os.Getenv("GROVE_ROOT"); env != "" {
		defaultRoot = env
	}

	rootDir := flag.String("root", defaultRoot, "groved data directory (env: GROVE_ROOT)")
	flag.Parse()

	d, err := daemon.New(*rootDir)
	if err != nil {
		log.Printf("daemon init: %v", err)
		// Exit 0 so launchd / systemd does not restart the daemon in a tight
		// loop.  A configuration error (e.g. Docker not available) will not
		// resolve itself on its own; the user must fix the problem and then
		// re-run `grove daemon install` or restart the service manually.
		os.Exit(0)
	}

	socketPath := filepath.Join(*rootDir, "groved.sock")

	// Graceful shutdown on SIGINT / SIGTERM.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("received %v, shutting down", sig)
		os.Remove(socketPath)
		os.Exit(0)
	}()

	if err := d.Run(socketPath); err != nil {
		log.Fatalf("daemon run: %v", err)
	}
}
