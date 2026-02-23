package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProjectDirHelpers(t *testing.T) {
	p := &Project{DataDir: "/data/my-app"}

	assert.Equal(t, "/data/my-app/main", p.MainDir())
	assert.Equal(t, "/data/my-app/worktrees", p.WorktreesDir())
	assert.Equal(t, "/data/my-app/worktrees/abc", p.WorktreeDir("abc"))
}

func TestLoadProject(t *testing.T) {
	dataRoot := t.TempDir()

	projectDir := filepath.Join(dataRoot, "projects", "my-app")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	// Registration only contains name + repo; any extra fields are ignored.
	yaml := "name: my-app\nrepo: git@github.com:org/my-app.git\nagent:\n  command: claude\n  args: []\n"
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "project.yaml"), []byte(yaml), 0o644))

	p, err := loadProject(dataRoot, "my-app")
	require.NoError(t, err)
	assert.Equal(t, "my-app", p.Name)
	assert.Equal(t, "git@github.com:org/my-app.git", p.Repo)
	assert.Empty(t, p.Agent.Command, "registration must not populate agent fields")
	assert.Equal(t, projectDir, p.DataDir)
}

func TestLoadProjectFallsBackToDirectoryName(t *testing.T) {
	dataRoot := t.TempDir()

	projectDir := filepath.Join(dataRoot, "projects", "my-app")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	// YAML has no name field — should fall back to directory name.
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "project.yaml"), []byte("repo: git@github.com:org/repo.git\n"), 0o644))

	p, err := loadProject(dataRoot, "my-app")
	require.NoError(t, err)
	assert.Equal(t, "my-app", p.Name)
}

func TestLoadProjectNotFound(t *testing.T) {
	_, err := loadProject(t.TempDir(), "nonexistent")
	assert.Error(t, err)
}

func TestLoadInRepoConfig(t *testing.T) {
	dataDir := t.TempDir()
	mainDir := filepath.Join(dataDir, "main")
	require.NoError(t, os.MkdirAll(mainDir, 0o755))

	yaml := "start:\n  - npm install\nagent:\n  command: aider\n  args: []\nfinish:\n  - git push\n"
	require.NoError(t, os.WriteFile(filepath.Join(mainDir, "grove.yaml"), []byte(yaml), 0o644))

	p := &Project{DataDir: dataDir}
	p.Agent.Command = "claude" // original value — should be overridden

	found, err := loadInRepoConfig(p)
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "aider", p.Agent.Command)
	assert.Equal(t, []string{"npm install"}, p.Start)
	assert.Equal(t, []string{"git push"}, p.Finish)
}

func TestLoadInRepoConfigMissing(t *testing.T) {
	p := &Project{DataDir: t.TempDir()}
	found, err := loadInRepoConfig(p)
	assert.NoError(t, err)
	assert.False(t, found)
}

func TestLoadInRepoConfigPartialDoesNotWipeOtherFields(t *testing.T) {
	dataDir := t.TempDir()
	mainDir := filepath.Join(dataDir, "main")
	require.NoError(t, os.MkdirAll(mainDir, 0o755))

	// In-repo config only sets start; agent and finish are absent.
	yaml := "start:\n  - make setup\n"
	require.NoError(t, os.WriteFile(filepath.Join(mainDir, "grove.yaml"), []byte(yaml), 0o644))

	// Registration always starts with zero-value agent/finish — only
	// in-repo config fills those in.
	p := &Project{DataDir: dataDir}

	_, err := loadInRepoConfig(p)
	require.NoError(t, err)
	assert.Equal(t, []string{"make setup"}, p.Start)
	assert.Empty(t, p.Agent.Command, "agent should remain empty when absent from in-repo config")
	assert.Empty(t, p.Finish, "finish should remain empty when absent from in-repo config")
}
