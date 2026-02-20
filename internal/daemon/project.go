package daemon

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Project holds the parsed contents of a project.yaml file.
type Project struct {
	Name string `yaml:"name"`
	Repo string `yaml:"repo"`

	Bootstrap []string `yaml:"bootstrap"`
	Complete  []string `yaml:"complete"`

	Agent struct {
		Command string   `yaml:"command"`
		Args    []string `yaml:"args"`
	} `yaml:"agent"`

	Dev struct {
		Start []string `yaml:"start"`
	} `yaml:"dev"`

	// ConfigDir is the directory that contains the project.yaml (may be inside
	// the repo's projects/ or projects.local/ tree, or in ~/.catherdd/projects/).
	ConfigDir string `yaml:"-"`

	// DataDir is where runtime data lives (canonical clone, worktrees).
	// Always set to <daemonRoot>/projects/<name>, independent of where the
	// project.yaml was found.
	DataDir string `yaml:"-"`
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

// loadProject searches configDirs in order for a project named name, returning
// the first match.  Runtime data (clones, worktrees) is placed under
// dataRoot/projects/<name> regardless of which config dir the YAML came from.
func loadProject(configDirs []string, dataRoot, name string) (*Project, error) {
	for _, dir := range configDirs {
		yamlPath := filepath.Join(dir, name, "project.yaml")
		data, err := os.ReadFile(yamlPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read project.yaml: %w", err)
		}

		var p Project
		if err := yaml.Unmarshal(data, &p); err != nil {
			return nil, fmt.Errorf("parse project.yaml: %w", err)
		}
		if p.Name == "" {
			p.Name = name
		}
		p.ConfigDir = filepath.Join(dir, name)
		p.DataDir = filepath.Join(dataRoot, "projects", name)
		return &p, nil
	}
	return nil, fmt.Errorf("project %q not found in any projects directory", name)
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
	cmd.Stdout = w
	cmd.Stderr = w
	return cmd.Run()
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

// runBootstrap executes the project bootstrap commands sequentially in dir.
// All output is written to w.
func runBootstrap(p *Project, dir string, w io.Writer) error {
	for _, cmdStr := range p.Bootstrap {
		fmt.Fprintf(w, "Bootstrap: %s\n", cmdStr)
		cmd := exec.Command("sh", "-c", cmdStr)
		cmd.Dir = dir
		cmd.Stdout = w
		cmd.Stderr = w
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("bootstrap %q: %w", cmdStr, err)
		}
	}
	return nil
}
