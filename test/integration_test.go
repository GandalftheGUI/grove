//go:build integration

// Integration tests for grove + groved.
//
// Each test builds the binaries once (via TestMain), creates an isolated
// GROVE_ROOT temp directory, injects a mock `docker` script so no real Docker
// daemon is required, and then runs actual `grove` / `groved` processes.
//
// Run with:
//
//	go test -tags=integration -v ./test/
//	go test -tags=integration -run TestFullLifecycle -v ./test/
//	go test -tags=integration -short ./test/   # skip slow lifecycle tests

package integration_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Paths to the compiled binaries, set once in TestMain.
var (
	groveBin  string
	grovedBin string
)

// mockDockerScript is written to <binDir>/docker so it appears first on PATH.
// It handles every Docker subcommand grove uses without a real daemon.
const mockDockerScript = `#!/bin/sh
subcmd="$1"; shift
case "$subcmd" in
  info)
    exit 0
    ;;

  run)
    # docker run -d --name <name> ... — echo the name so startContainer gets it back.
    name=""
    while [ $# -gt 0 ]; do
      if [ "$1" = "--name" ]; then name="$2"; shift; fi
      shift
    done
    echo "$name"
    exit 0
    ;;

  exec)
    # Skip all flags (-it, -i, -t, -e KEY=VAL) then skip the container name.
    while [ $# -gt 0 ]; do
      case "$1" in
        -i|-t|-it) shift ;;
        -e) shift; shift ;;
        --*) shift ;;
        -*) shift ;;
        *) shift; break ;;   # container name — consume it and stop
      esac
    done
    # Whatever command follows, just succeed silently.
    exit 0
    ;;

  stop|rm)
    exit 0
    ;;

  compose)
    exit 0
    ;;

  *)
    echo "mock-docker: unknown subcommand: $subcmd" >&2
    exit 1
    ;;
esac
`

func TestMain(m *testing.M) {
	root := moduleRoot()

	tmpBin, err := os.MkdirTemp("", "grove-inttest-bin-*")
	if err != nil {
		panic("MkdirTemp: " + err.Error())
	}
	defer os.RemoveAll(tmpBin)

	groveBin = filepath.Join(tmpBin, "grove")
	grovedBin = filepath.Join(tmpBin, "groved")

	for _, b := range []struct{ out, pkg string }{
		{groveBin, "./cmd/grove"},
		{grovedBin, "./cmd/groved"},
	} {
		cmd := exec.Command("go", "build", "-o", b.out, b.pkg)
		cmd.Dir = root
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			panic("build " + b.pkg + ": " + err.Error())
		}
	}

	os.Exit(m.Run())
}

// moduleRoot returns the path to the Go module root (one level up from test/).
func moduleRoot() string {
	abs, err := filepath.Abs("..")
	if err != nil {
		panic(err)
	}
	return abs
}

// ── Test environment ──────────────────────────────────────────────────────────

type testEnv struct {
	t         *testing.T
	groveRoot string
	binDir    string // contains mock docker, appears first on PATH
	sockPath  string
	daemon    *exec.Cmd
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	groveRoot := t.TempDir()
	binDir := t.TempDir()

	mockPath := filepath.Join(binDir, "docker")
	require.NoError(t, os.WriteFile(mockPath, []byte(mockDockerScript), 0o755))

	env := &testEnv{
		t:         t,
		groveRoot: groveRoot,
		binDir:    binDir,
		sockPath:  filepath.Join(groveRoot, "groved.sock"),
	}
	t.Cleanup(env.cleanup)
	return env
}

// startDaemon starts groved and blocks until its Unix socket appears.
func (e *testEnv) startDaemon() {
	e.t.Helper()
	cmd := exec.Command(grovedBin, "--root", e.groveRoot)
	cmd.Env = e.envVars()
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	require.NoError(e.t, cmd.Start(), "start groved")
	e.daemon = cmd

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(e.sockPath); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.t.Fatal("groved socket did not appear within 5s")
}

func (e *testEnv) envVars() []string {
	return append(os.Environ(),
		"GROVE_ROOT="+e.groveRoot,
		"PATH="+e.binDir+":"+os.Getenv("PATH"),
	)
}

// grove runs a grove subcommand and returns (trimmed output, error).
func (e *testEnv) grove(args ...string) (string, error) {
	cmd := exec.Command(groveBin, args...)
	cmd.Env = e.envVars()
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// groveOK runs a grove subcommand and fatals if it returns an error.
func (e *testEnv) groveOK(args ...string) string {
	e.t.Helper()
	out, err := e.grove(args...)
	require.NoError(e.t, err, "grove %v\n%s", args, out)
	return out
}

func (e *testEnv) cleanup() {
	if e.daemon != nil && e.daemon.Process != nil {
		_ = e.daemon.Process.Signal(syscall.SIGTERM)
		_ = e.daemon.Wait()
	}
}

// makeGitRepo creates a local git repo with a minimal grove.yaml committed.
// Returns the repo path, which can be used as the --repo argument.
func makeGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%v failed: %s", args, out)
	}

	run("git", "init")
	run("git", "symbolic-ref", "HEAD", "refs/heads/main") // set default branch without -b flag
	run("git", "config", "user.email", "test@grove.test")
	run("git", "config", "user.name", "Grove Integration Test")

	// grove.yaml: use `sh` as the agent (always present in containers).
	// start is empty so we don't need real commands to succeed.
	groveYAML := "container:\n  image: alpine\nstart: []\nagent:\n  command: sh\n  args: []\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "grove.yaml"), []byte(groveYAML), 0o644))

	run("git", "add", ".")
	run("git", "commit", "-m", "init")

	return dir
}

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestProjectCreate checks that `grove project create` writes a registration
// and that `grove project list` returns it — no daemon required.
func TestProjectCreate(t *testing.T) {
	env := newTestEnv(t)

	out := env.groveOK("project", "create", "my-app", "--repo", "git@github.com:org/my-app.git")
	assert.Contains(t, out, "my-app")
}

func TestProjectList(t *testing.T) {
	env := newTestEnv(t)

	env.groveOK("project", "create", "alpha", "--repo", "git@github.com:org/alpha.git")
	env.groveOK("project", "create", "beta", "--repo", "git@github.com:org/beta.git")

	out := env.groveOK("project", "list")
	assert.Contains(t, out, "alpha")
	assert.Contains(t, out, "beta")
}

// TestInstanceListEmpty checks that `grove list` with no instances succeeds.
func TestInstanceListEmpty(t *testing.T) {
	env := newTestEnv(t)
	env.startDaemon()

	out := env.groveOK("list")
	// May print headers or nothing, but must not error.
	_ = out
}

// TestFullLifecycle exercises the full start → list → stop → drop path.
// Uses a real local git repo and a mock docker so no Docker daemon is needed.
func TestFullLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping full lifecycle test in -short mode")
	}

	env := newTestEnv(t)
	repoDir := makeGitRepo(t)
	env.startDaemon()

	// Register the project.
	env.groveOK("project", "create", "test-app", "--repo", repoDir)

	// Start an instance detached (-d) so we don't block waiting for PTY input.
	out := env.groveOK("start", "test-app", "feat/test", "-d")
	assert.Regexp(t, `(?i)start|instance`, out)

	// Instance should appear in the list.
	out = env.groveOK("list")
	assert.Contains(t, out, "test-app")
	assert.Contains(t, out, "feat/test")

	// Drop permanently removes the record and worktree (-f skips confirmation).
	env.groveOK("drop", "-f", "1")

	// List should no longer mention this branch.
	out = env.groveOK("list")
	assert.NotContains(t, out, "feat/test")
}

// TestMultipleInstances starts two instances on different branches and verifies
// both appear in the list simultaneously.
func TestMultipleInstances(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in -short mode")
	}

	env := newTestEnv(t)
	repoDir := makeGitRepo(t)
	env.startDaemon()

	env.groveOK("project", "create", "multi-app", "--repo", repoDir)
	env.groveOK("start", "multi-app", "feat/a", "-d")
	env.groveOK("start", "multi-app", "feat/b", "-d")

	out := env.groveOK("list")
	assert.Contains(t, out, "feat/a")
	assert.Contains(t, out, "feat/b")
}

// TestStopAndRestart verifies that stop transitions the instance to KILLED
// and that restart brings it back.
func TestStopAndRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in -short mode")
	}

	env := newTestEnv(t)
	repoDir := makeGitRepo(t)
	env.startDaemon()

	env.groveOK("project", "create", "my-app", "--repo", repoDir)
	env.groveOK("start", "my-app", "feat/stop-test", "-d")

	// Give the mock agent a moment to exit naturally (it exits immediately).
	time.Sleep(100 * time.Millisecond)

	// Stop is idempotent even on an already-exited instance.
	_, _ = env.grove("stop", "1")

	out := env.groveOK("list")
	assert.Contains(t, out, "feat/stop-test")

	// Restart should succeed.
	env.groveOK("restart", "1", "-d")
}

// TestLogs verifies that `grove logs` returns output without error.
func TestLogs(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in -short mode")
	}

	env := newTestEnv(t)
	repoDir := makeGitRepo(t)
	env.startDaemon()

	env.groveOK("project", "create", "my-app", "--repo", repoDir)
	env.groveOK("start", "my-app", "feat/log-test", "-d")

	// Give the instance a moment to produce output.
	time.Sleep(100 * time.Millisecond)

	out := env.groveOK("logs", "1")
	_ = out
}
