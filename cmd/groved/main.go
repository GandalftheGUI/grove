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

// stringSlice is a repeatable string flag (--projects-dir a --projects-dir b).
type stringSlice []string

func (s *stringSlice) String() string { return "" }
func (s *stringSlice) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func main() {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("cannot determine home directory: %v", err)
	}
	defaultRoot := filepath.Join(homeDir, ".grove")
	// GROVE_ROOT env var overrides the default so users can point at a
	// local test directory without touching ~/.grove.
	if env := os.Getenv("GROVE_ROOT"); env != "" {
		defaultRoot = env
	}

	rootDir := flag.String("root", defaultRoot, "groved data directory (env: GROVE_ROOT)")
	var projectsDirs stringSlice
	flag.Var(&projectsDirs, "projects-dir", "project config directory to search (may be repeated; personal before global)")
	flag.Parse()

	d, err := daemon.New(*rootDir, []string(projectsDirs))
	if err != nil {
		log.Fatalf("daemon init: %v", err)
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
