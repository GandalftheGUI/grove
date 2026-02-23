package daemon

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ContainerConfig holds the Docker container or Compose settings for a project.
type ContainerConfig struct {
	Image   string   `yaml:"image"`   // single container image (e.g. "ruby:3.3")
	Compose string   `yaml:"compose"` // path to docker-compose.yml (relative to repo root)
	Service string   `yaml:"service"` // compose service to exec into; default "app"
	Workdir string   `yaml:"workdir"` // working directory inside container; default "/app"
	Mounts  []string `yaml:"mounts"`  // extra host paths to bind-mount; ~/foo maps to /root/foo
}

// Project holds the parsed contents of a project.yaml file.
type Project struct {
	Name string `yaml:"name"`
	Repo string `yaml:"repo"`

	Container ContainerConfig `yaml:"container"`

	Start  []string `yaml:"start"`
	Finish []string `yaml:"finish"`
	Check  []string `yaml:"check"`

	Agent struct {
		Command string   `yaml:"command"`
		Args    []string `yaml:"args"`
	} `yaml:"agent"`

	// DataDir is where all project data lives: registration (project.yaml),
	// canonical clone (main/), and worktrees (worktrees/).
	// Always set to <daemonRoot>/projects/<name>.
	DataDir string `yaml:"-"`
}

// containerWorkdir returns the working directory to use inside the container.
func (p *Project) containerWorkdir() string {
	if p.Container.Workdir != "" {
		return p.Container.Workdir
	}
	return "/app"
}

// containerService returns the compose service name to exec into.
func (p *Project) containerService() string {
	if p.Container.Service != "" {
		return p.Container.Service
	}
	return "app"
}

// MainDir returns the path of the canonical checkout for this project.
func (p *Project) MainDir() string {
	return filepath.Join(p.DataDir, "main")
}

// WorktreesDir returns the base directory that holds all worktrees for this project.
func (p *Project) WorktreesDir() string {
	return filepath.Join(p.DataDir, "worktrees")
}

// WorktreeDir returns the path for a specific instance's worktree.
func (p *Project) WorktreeDir(instanceID string) string {
	return filepath.Join(p.WorktreesDir(), instanceID)
}

// loadProject reads the project registration from <dataRoot>/projects/<name>/project.yaml.
// The registration only carries name and repo — all other config (container, agent,
// start, finish, check) comes exclusively from grove.yaml in the project repo.
func loadProject(dataRoot, name string) (*Project, error) {
	projectDir := filepath.Join(dataRoot, "projects", name)
	yamlPath := filepath.Join(projectDir, "project.yaml")
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("project %q not found (expected %s)", name, yamlPath)
		}
		return nil, fmt.Errorf("read project.yaml: %w", err)
	}

	var reg struct {
		Name string `yaml:"name"`
		Repo string `yaml:"repo"`
	}
	if err := yaml.Unmarshal(data, &reg); err != nil {
		return nil, fmt.Errorf("parse project.yaml: %w", err)
	}

	p := &Project{
		Name:    reg.Name,
		Repo:    reg.Repo,
		DataDir: projectDir,
	}
	if p.Name == "" {
		p.Name = name
	}
	return p, nil
}

// ensureMainCheckout clones the project repo into the main directory if it
// does not already exist.  It is a no-op if the directory already has a git repo.
// All output (git clone progress, etc.) is written to w.
func ensureMainCheckout(p *Project, w io.Writer) error {
	mainDir := p.MainDir()
	gitDir := filepath.Join(mainDir, ".git")

	if _, err := os.Stat(gitDir); err == nil {
		// Already cloned.
		return nil
	}

	if p.Repo == "" {
		return fmt.Errorf("project %q has no repo URL and main checkout does not exist", p.Name)
	}

	if err := os.MkdirAll(filepath.Dir(mainDir), 0o755); err != nil {
		return err
	}

	fmt.Fprintf(w, "Cloning %s into %s …\n", p.Repo, mainDir)
	cmd := exec.Command("git", "clone", p.Repo, mainDir)
	out, err := cmd.CombinedOutput()
	if len(out) > 0 {
		_, _ = w.Write(out)
	}
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail != "" {
			return fmt.Errorf("git clone %q failed: %s", p.Repo, detail)
		}
		return fmt.Errorf("git clone %q failed: %w", p.Repo, err)
	}
	return nil
}

// pullMain runs "git pull" in the main checkout to bring it up-to-date with
// the remote before branching.  Errors are non-fatal — the caller logs and
// continues so that offline use still works.  Output is written to w.
func pullMain(p *Project, w io.Writer) error {
	cmd := exec.Command("git", "-C", p.MainDir(), "pull")
	cmd.Stdout = w
	cmd.Stderr = w
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git pull: %w", err)
	}
	return nil
}

// createWorktree creates a new git worktree at worktreeDir on branch branchName,
// branching off from the current HEAD of the main checkout.
func createWorktree(p *Project, instanceID, branchName string) (string, error) {
	mainDir := p.MainDir()
	worktreeDir := p.WorktreeDir(instanceID)

	if err := os.MkdirAll(p.WorktreesDir(), 0o755); err != nil {
		return "", err
	}

	// Try creating a new branch; if it already exists, check it out directly.
	cmd := exec.Command("git", "-C", mainDir, "worktree", "add", "-b", branchName, worktreeDir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		cmd = exec.Command("git", "-C", mainDir, "worktree", "add", worktreeDir, branchName)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("git worktree add: %w", err)
		}
	}

	return worktreeDir, nil
}

// removeWorktree removes the git worktree for the given instance and deletes
// the associated branch.  Errors are best-effort (logged but not fatal).
func removeWorktree(p *Project, instanceID, branchName string) {
	mainDir := p.MainDir()
	worktreeDir := p.WorktreeDir(instanceID)

	// git worktree remove --force <path>
	exec.Command("git", "-C", mainDir, "worktree", "remove", "--force", worktreeDir).Run()

	// git branch -D <branch>
	exec.Command("git", "-C", mainDir, "branch", "-D", branchName).Run()
}

// loadInRepoConfig reads grove.yaml from the root of the project's main clone
// and overlays its fields onto p.  In-repo config takes precedence over the
// registration so teams can commit authoritative settings alongside their code.
//
// Returns (true, nil) if the file was found and applied, (false, nil) if it
// does not exist, or (false, err) on a parse error.
func loadInRepoConfig(p *Project) (bool, error) {
	inRepoPath := filepath.Join(p.MainDir(), "grove.yaml")
	data, err := os.ReadFile(inRepoPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("read grove.yaml: %w", err)
	}

	var overlay Project
	if err := yaml.Unmarshal(data, &overlay); err != nil {
		return false, fmt.Errorf("parse grove.yaml: %w", err)
	}

	// Overlay container config field by field so a partial in-repo config
	// (e.g. only mounts:) merges with rather than replaces the registration.
	if overlay.Container.Image != "" {
		p.Container.Image = overlay.Container.Image
	}
	if overlay.Container.Compose != "" {
		p.Container.Compose = overlay.Container.Compose
	}
	if overlay.Container.Service != "" {
		p.Container.Service = overlay.Container.Service
	}
	if overlay.Container.Workdir != "" {
		p.Container.Workdir = overlay.Container.Workdir
	}
	if len(overlay.Container.Mounts) > 0 {
		p.Container.Mounts = overlay.Container.Mounts
	}
	if len(overlay.Start) > 0 {
		p.Start = overlay.Start
	}
	if overlay.Agent.Command != "" {
		p.Agent = overlay.Agent
	}
	if len(overlay.Finish) > 0 {
		p.Finish = overlay.Finish
	}
	if len(overlay.Check) > 0 {
		p.Check = overlay.Check
	}

	return true, nil
}

// runStart executes the project start commands sequentially inside the container.
// All output is written to w.
func runStart(p *Project, containerName string, w io.Writer) error {
	for _, cmdStr := range p.Start {
		fmt.Fprintf(w, "Start: %s\n", cmdStr)
		if err := execInContainer(containerName, cmdStr, w); err != nil {
			return fmt.Errorf("start %q: %w", cmdStr, err)
		}
	}
	return nil
}
