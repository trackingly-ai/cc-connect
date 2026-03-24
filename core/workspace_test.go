package core

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

func TestSetupWorkspaceReusesExistingBranch(t *testing.T) {
	repoPath := initGitRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "worktrees", "task-existing-branch")

	if _, err := runGit(repoPath, "branch", "echo/task-existing-branch", "main"); err != nil {
		t.Fatalf("git branch: %v", err)
	}

	if err := SetupWorkspace(repoPath, "main", "echo/task-existing-branch", worktreePath); err != nil {
		t.Fatalf("SetupWorkspace with existing branch: %v", err)
	}

	branch, err := runGit(worktreePath, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	if branch != "echo/task-existing-branch" {
		t.Fatalf("branch = %q, want %q", branch, "echo/task-existing-branch")
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

func TestCleanupWorkspaceCanKeepBranch(t *testing.T) {
	repoPath := initGitRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "worktrees", "task-keep")

	if err := SetupWorkspace(repoPath, "main", "echo/task-keep", worktreePath); err != nil {
		t.Fatalf("SetupWorkspace: %v", err)
	}
	if err := CleanupWorkspaceWithOptions(worktreePath, CleanupWorkspaceOptions{
		KeepBranch: true,
	}); err != nil {
		t.Fatalf("CleanupWorkspaceWithOptions: %v", err)
	}

	branches, err := runGit(repoPath, "branch", "--list", "echo/task-keep")
	if err != nil {
		t.Fatalf("git branch --list: %v", err)
	}
	if !strings.Contains(branches, "echo/task-keep") {
		t.Fatalf("expected workspace branch to remain, got %q", branches)
	}
}

func TestCleanupWorkspaceKeepsNonManagedBranch(t *testing.T) {
	repoPath := initGitRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "worktrees", "task-non-managed")

	if err := SetupWorkspace(repoPath, "main", "echo/task-non-managed", worktreePath); err != nil {
		t.Fatalf("SetupWorkspace: %v", err)
	}
	if _, err := runGit(worktreePath, "checkout", "-b", "manual/debug"); err != nil {
		t.Fatalf("git checkout -b manual/debug: %v", err)
	}

	if err := CleanupWorkspace(worktreePath); err != nil {
		t.Fatalf("CleanupWorkspace: %v", err)
	}
	if _, err := os.Stat(worktreePath); !os.IsNotExist(err) {
		t.Fatalf("expected worktree to be removed, stat err = %v", err)
	}

	branches, err := runGit(repoPath, "branch", "--list", "manual/debug")
	if err != nil {
		t.Fatalf("git branch --list: %v", err)
	}
	if !strings.Contains(branches, "manual/debug") {
		t.Fatalf("expected non-managed branch to remain, got %q", branches)
	}
}

func TestSetupWorkspaceSerializesByRepo(t *testing.T) {
	repoPath := initGitRepo(t)
	originalRunGit := runGit
	defer func() { runGit = originalRunGit }()

	var active int32
	var maxActive int32
	var mu sync.Mutex
	runGit = func(dir string, args ...string) (string, error) {
		if dir == repoPath && len(args) >= 2 && args[0] == "worktree" && args[1] == "add" {
			current := atomic.AddInt32(&active, 1)
			mu.Lock()
			if current > maxActive {
				maxActive = current
			}
			mu.Unlock()
			time.Sleep(50 * time.Millisecond)
			defer atomic.AddInt32(&active, -1)
		}
		return originalRunGit(dir, args...)
	}

	worktreeA := filepath.Join(t.TempDir(), "worktrees", "task-a")
	worktreeB := filepath.Join(t.TempDir(), "worktrees", "task-b")

	errCh := make(chan error, 2)
	go func() { errCh <- SetupWorkspace(repoPath, "main", "echo/task-a", worktreeA) }()
	go func() { errCh <- SetupWorkspace(repoPath, "main", "echo/task-b", worktreeB) }()

	for range 2 {
		if err := <-errCh; err != nil {
			t.Fatalf("SetupWorkspace: %v", err)
		}
	}
	if maxActive != 1 {
		t.Fatalf("max concurrent git worktree add = %d, want 1", maxActive)
	}
}

func TestEnsureRepoCheckoutClonesMissingRepo(t *testing.T) {
	sourceRepo := initGitRepo(t)
	checkoutPath := filepath.Join(t.TempDir(), "clones", "frontend")

	if err := EnsureRepoCheckout(sourceRepo, checkoutPath, "main"); err != nil {
		t.Fatalf("EnsureRepoCheckout: %v", err)
	}

	originURL, err := runGit(checkoutPath, "remote", "get-url", "origin")
	if err != nil {
		t.Fatalf("git remote get-url origin: %v", err)
	}
	if originURL != sourceRepo {
		t.Fatalf("origin = %q, want %q", originURL, sourceRepo)
	}
	if _, err := os.Stat(filepath.Join(checkoutPath, "README.md")); err != nil {
		t.Fatalf("expected cloned checkout README: %v", err)
	}
}

func TestEnsureRepoCheckoutRejectsMismatchedOrigin(t *testing.T) {
	sourceRepo := initGitRepo(t)
	otherRepo := initGitRepo(t)
	checkoutPath := filepath.Join(t.TempDir(), "clones", "frontend")

	if err := EnsureRepoCheckout(sourceRepo, checkoutPath, "main"); err != nil {
		t.Fatalf("EnsureRepoCheckout initial clone: %v", err)
	}
	err := EnsureRepoCheckout(otherRepo, checkoutPath, "main")
	if err == nil {
		t.Fatal("expected origin mismatch error")
	}
	if !strings.Contains(err.Error(), "does not match repo_url") {
		t.Fatalf("unexpected error: %v", err)
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
