package daemon

import (
	"fmt"
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

	Agent struct {
		Command string   `yaml:"command"`
		Args    []string `yaml:"args"`
	} `yaml:"agent"`

	Dev struct {
		Start []string `yaml:"start"`
	} `yaml:"dev"`

	// Dir is the project root (~/.catherdd/projects/<name>), set after loading.
	Dir string `yaml:"-"`
}

// MainDir returns the path of the canonical checkout for this project.
func (p *Project) MainDir() string {
	return filepath.Join(p.Dir, "main")
}

// WorktreesDir returns the base directory that holds all worktrees for this project.
func (p *Project) WorktreesDir() string {
	return filepath.Join(p.Dir, "worktrees")
}

// WorktreeDir returns the path for a specific instance's worktree.
func (p *Project) WorktreeDir(instanceID string) string {
	return filepath.Join(p.WorktreesDir(), instanceID)
}

// loadProject reads and parses the project.yaml for the named project.
// rootDir is the ~/.catherdd/projects directory.
func loadProject(rootDir, name string) (*Project, error) {
	yamlPath := filepath.Join(rootDir, name, "project.yaml")
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		return nil, fmt.Errorf("read project.yaml: %w", err)
	}

	var p Project
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse project.yaml: %w", err)
	}
	if p.Name == "" {
		p.Name = name
	}
	p.Dir = filepath.Join(rootDir, name)
	return &p, nil
}

// ensureMainCheckout clones the project repo into the main directory if it
// does not already exist.  It is a no-op if the directory already has a git repo.
func ensureMainCheckout(p *Project) error {
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

	fmt.Printf("Cloning %s into %s …\n", p.Repo, mainDir)
	cmd := exec.Command("git", "clone", p.Repo, mainDir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// createWorktree creates a new git worktree at worktreeDir on branch branchName,
// branching off from the current HEAD of the main checkout.
func createWorktree(p *Project, instanceID, branchName string) (string, error) {
	mainDir := p.MainDir()
	worktreeDir := p.WorktreeDir(instanceID)

	if err := os.MkdirAll(p.WorktreesDir(), 0o755); err != nil {
		return "", err
	}

	// git worktree add <path> -b <branch>
	cmd := exec.Command("git", "-C", mainDir, "worktree", "add", worktreeDir, "-b", branchName)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git worktree add: %w", err)
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
func runBootstrap(p *Project, dir string) error {
	for _, cmdStr := range p.Bootstrap {
		fmt.Printf("Bootstrap: %s\n", cmdStr)
		cmd := exec.Command("sh", "-c", cmdStr)
		cmd.Dir = dir
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("bootstrap %q: %w", cmdStr, err)
		}
	}
	return nil
}
