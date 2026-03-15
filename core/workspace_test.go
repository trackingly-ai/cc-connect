package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetupWorkspaceCreatesWorktree(t *testing.T) {
	repoPath := initGitRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "worktrees", "task-1")

	if err := SetupWorkspace(repoPath, "main", "echo/task-1", worktreePath); err != nil {
		t.Fatalf("SetupWorkspace: %v", err)
	}

	if _, err := os.Stat(worktreePath); err != nil {
		t.Fatalf("worktree path not created: %v", err)
	}

	branch, err := runGit(worktreePath, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	if branch != "echo/task-1" {
		t.Fatalf("branch = %q, want %q", branch, "echo/task-1")
	}
}

func TestCleanupWorkspaceRemovesWorktree(t *testing.T) {
	repoPath := initGitRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "worktrees", "task-2")

	if err := SetupWorkspace(repoPath, "main", "echo/task-2", worktreePath); err != nil {
		t.Fatalf("SetupWorkspace: %v", err)
	}
	if err := CleanupWorkspace(worktreePath); err != nil {
		t.Fatalf("CleanupWorkspace: %v", err)
	}

	if _, err := os.Stat(worktreePath); !os.IsNotExist(err) {
		t.Fatalf("expected worktree to be removed, stat err = %v", err)
	}

	branches, err := runGit(repoPath, "branch", "--list", "echo/task-2")
	if err != nil {
		t.Fatalf("git branch --list: %v", err)
	}
	if branches != "" {
		t.Fatalf("expected workspace branch to be deleted, got %q", branches)
	}
}

func TestCleanupWorkspaceMissingPathIsNoop(t *testing.T) {
	worktreePath := filepath.Join(t.TempDir(), "missing-worktree")

	if err := CleanupWorkspace(worktreePath); err != nil {
		t.Fatalf("CleanupWorkspace missing path: %v", err)
	}
}

func initGitRepo(t *testing.T) string {
	t.Helper()

	repoPath := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	if _, err := runGit(repoPath, "init", "--initial-branch=main"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	if _, err := runGit(repoPath, "config", "user.email", "fixture@example.com"); err != nil {
		t.Fatalf("git config user.email: %v", err)
	}
	if _, err := runGit(repoPath, "config", "user.name", "Fixture User"); err != nil {
		t.Fatalf("git config user.name: %v", err)
	}
	readmePath := filepath.Join(repoPath, "README.md")
	if err := os.WriteFile(readmePath, []byte("# fixture\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	if _, err := runGit(repoPath, "add", "README.md"); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if _, err := runGit(repoPath, "commit", "-m", "initial commit"); err != nil {
		t.Fatalf("git commit: %v", err)
	}
	list, err := runGit(repoPath, "worktree", "list", "--porcelain")
	if err != nil {
		t.Fatalf("git worktree list: %v", err)
	}
	if !strings.Contains(list, repoPath) {
		t.Fatalf("expected base repo in worktree list, got %q", list)
	}
	return repoPath
}
