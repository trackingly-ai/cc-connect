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

	result, err := SetupWorkspace(repoPath, "main", "echo/task-1", worktreePath)
	if err != nil {
		t.Fatalf("SetupWorkspace: %v", err)
	}
	if strings.TrimSpace(result.RequestedBaseCommitSHA) == "" {
		t.Fatalf("RequestedBaseCommitSHA should be populated")
	}
	if strings.TrimSpace(result.InitialHeadCommitSHA) == "" {
		t.Fatalf("InitialHeadCommitSHA should be populated")
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
	if _, err := runGit(repoPath, "checkout", "echo/task-existing-branch"); err != nil {
		t.Fatalf("git checkout existing branch: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoPath, "branch-only.txt"), []byte("existing branch tip\n"), 0o644); err != nil {
		t.Fatalf("write branch-only file: %v", err)
	}
	if _, err := runGit(repoPath, "add", "branch-only.txt"); err != nil {
		t.Fatalf("git add branch-only file: %v", err)
	}
	if _, err := runGit(repoPath, "commit", "-m", "advance existing workspace branch"); err != nil {
		t.Fatalf("git commit branch-only file: %v", err)
	}
	if _, err := runGit(repoPath, "checkout", "main"); err != nil {
		t.Fatalf("git checkout main: %v", err)
	}

	result, err := SetupWorkspace(repoPath, "main", "echo/task-existing-branch", worktreePath)
	if err != nil {
		t.Fatalf("SetupWorkspace with existing branch: %v", err)
	}
	if strings.TrimSpace(result.RequestedBaseCommitSHA) == "" {
		t.Fatalf("RequestedBaseCommitSHA should be populated")
	}
	if strings.TrimSpace(result.InitialHeadCommitSHA) == "" {
		t.Fatalf("InitialHeadCommitSHA should be populated")
	}
	if result.RequestedBaseCommitSHA == result.InitialHeadCommitSHA {
		t.Fatalf(
			"RequestedBaseCommitSHA = InitialHeadCommitSHA = %q, want different SHAs for reused branch",
			result.RequestedBaseCommitSHA,
		)
	}

	branch, err := runGit(worktreePath, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	if branch != "echo/task-existing-branch" {
		t.Fatalf("branch = %q, want %q", branch, "echo/task-existing-branch")
	}
}

func TestSetupWorkspaceUsesFreshRemoteTrackingBaseForNewBranch(t *testing.T) {
	originPath, repoPath := initGitRepoWithOrigin(t)
	worktreePath := filepath.Join(t.TempDir(), "worktrees", "task-fresh-remote-base")

	if _, err := runGit(repoPath, "checkout", "-b", "echo-integration"); err != nil {
		t.Fatalf("git checkout -b echo-integration: %v", err)
	}
	if _, err := runGit(repoPath, "push", "-u", "origin", "echo-integration"); err != nil {
		t.Fatalf("git push -u origin echo-integration: %v", err)
	}
	if _, err := runGit(repoPath, "checkout", "main"); err != nil {
		t.Fatalf("git checkout main: %v", err)
	}

	otherClone := filepath.Join(t.TempDir(), "other-clone")
	if _, err := runGit(
		t.TempDir(),
		"clone",
		"--origin",
		"origin",
		originPath,
		otherClone,
	); err != nil {
		t.Fatalf("git clone other clone: %v", err)
	}
	if _, err := runGit(otherClone, "checkout", "echo-integration"); err != nil {
		t.Fatalf("git checkout echo-integration in other clone: %v", err)
	}
	if err := os.WriteFile(filepath.Join(otherClone, "remote-only.txt"), []byte("fresh remote head\n"), 0o644); err != nil {
		t.Fatalf("write remote-only file: %v", err)
	}
	if _, err := runGit(otherClone, "add", "remote-only.txt"); err != nil {
		t.Fatalf("git add remote-only file: %v", err)
	}
	if _, err := runGit(otherClone, "commit", "-m", "advance remote integration"); err != nil {
		t.Fatalf("git commit remote-only file: %v", err)
	}
	if _, err := runGit(otherClone, "push", "origin", "echo-integration"); err != nil {
		t.Fatalf("git push origin echo-integration from other clone: %v", err)
	}

	localBaseSHA, err := runGit(repoPath, "rev-parse", "echo-integration")
	if err != nil {
		t.Fatalf("git rev-parse local echo-integration: %v", err)
	}
	remoteBaseSHA, err := runGit(otherClone, "rev-parse", "echo-integration")
	if err != nil {
		t.Fatalf("git rev-parse remote echo-integration: %v", err)
	}
	localBaseSHA = strings.TrimSpace(localBaseSHA)
	remoteBaseSHA = strings.TrimSpace(remoteBaseSHA)
	if localBaseSHA == remoteBaseSHA {
		t.Fatalf("expected stale local branch and fresher remote branch, got identical SHA %q", localBaseSHA)
	}

	result, err := SetupWorkspace(repoPath, "echo-integration", "echo/task-fresh-remote-base", worktreePath)
	if err != nil {
		t.Fatalf("SetupWorkspace: %v", err)
	}
	if result.RequestedBaseCommitSHA != remoteBaseSHA {
		t.Fatalf("RequestedBaseCommitSHA = %q, want remote integration SHA %q", result.RequestedBaseCommitSHA, remoteBaseSHA)
	}
	if result.InitialHeadCommitSHA != remoteBaseSHA {
		t.Fatalf("InitialHeadCommitSHA = %q, want remote integration SHA %q", result.InitialHeadCommitSHA, remoteBaseSHA)
	}
}

func TestSetupWorkspaceFallsBackToLocalBaseWhenFetchFails(t *testing.T) {
	repoPath := initGitRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "worktrees", "task-local-fallback")

	if _, err := runGit(repoPath, "remote", "add", "origin", "ssh://git@example.invalid/repo.git"); err != nil {
		t.Fatalf("git remote add origin: %v", err)
	}
	localMainSHA, err := runGit(repoPath, "rev-parse", "main")
	if err != nil {
		t.Fatalf("git rev-parse main: %v", err)
	}
	localMainSHA = strings.TrimSpace(localMainSHA)

	result, err := SetupWorkspace(repoPath, "main", "echo/task-local-fallback", worktreePath)
	if err != nil {
		t.Fatalf("SetupWorkspace fallback after fetch failure: %v", err)
	}
	if result.RequestedBaseCommitSHA != localMainSHA {
		t.Fatalf("RequestedBaseCommitSHA = %q, want local main SHA %q", result.RequestedBaseCommitSHA, localMainSHA)
	}
	if result.InitialHeadCommitSHA != localMainSHA {
		t.Fatalf("InitialHeadCommitSHA = %q, want local main SHA %q", result.InitialHeadCommitSHA, localMainSHA)
	}
}

func TestCleanupWorkspaceRemovesWorktree(t *testing.T) {
	repoPath := initGitRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "worktrees", "task-2")

	if _, err := SetupWorkspace(repoPath, "main", "echo/task-2", worktreePath); err != nil {
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

	if _, err := SetupWorkspace(repoPath, "main", "echo/task-keep", worktreePath); err != nil {
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

	if _, err := SetupWorkspace(repoPath, "main", "echo/task-non-managed", worktreePath); err != nil {
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
	go func() {
		_, err := SetupWorkspace(repoPath, "main", "echo/task-a", worktreeA)
		errCh <- err
	}()
	go func() {
		_, err := SetupWorkspace(repoPath, "main", "echo/task-b", worktreeB)
		errCh <- err
	}()

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

func TestFinalizeSourceCommitSelectivelyCommitsAndPushes(t *testing.T) {
	originPath, repoPath := initGitRepoWithOrigin(t)
	branchName := "echo/task-finalize"
	if _, err := runGit(repoPath, "checkout", "-b", branchName); err != nil {
		t.Fatalf("git checkout -b: %v", err)
	}
	docsDir := filepath.Join(repoPath, "docs")
	if err := os.MkdirAll(docsDir, 0o755); err != nil {
		t.Fatalf("mkdir docs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(docsDir, "RESEARCH.md"), []byte("brief\n"), 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoPath, "scratch.tmp"), []byte("noise\n"), 0o644); err != nil {
		t.Fatalf("write stray file: %v", err)
	}

	result, err := FinalizeSourceCommit(
		repoPath,
		repoPath,
		branchName,
		"echo: finalize research output",
		[]string{"docs/RESEARCH.md"},
	)
	if err != nil {
		t.Fatalf("FinalizeSourceCommit: %v", err)
	}
	if !result.CreatedCommit {
		t.Fatal("expected finalize to create commit")
	}
	if got := result.CommitSHA; got == "" {
		t.Fatal("expected commit sha")
	}
	if got, err := runGit(repoPath, "show", "HEAD:docs/RESEARCH.md"); err != nil || got != "brief" {
		t.Fatalf("artifact not present in HEAD: %q err=%v", got, err)
	}
	if _, err := runGit(originPath, "rev-parse", "refs/heads/"+branchName); err != nil {
		t.Fatalf("expected pushed remote branch: %v", err)
	}
	status, err := runGit(repoPath, "status", "--short")
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if !strings.Contains(status, "?? scratch.tmp") {
		t.Fatalf("expected stray file to remain untracked, got %q", status)
	}
}

func TestFinalizeSourceCommitSupportsDirectoryArtifacts(t *testing.T) {
	originPath, repoPath := initGitRepoWithOrigin(t)
	branchName := "echo/task-directory"
	if _, err := runGit(repoPath, "checkout", "-b", branchName); err != nil {
		t.Fatalf("git checkout -b: %v", err)
	}
	docsDir := filepath.Join(repoPath, "docs", "bundle")
	if err := os.MkdirAll(filepath.Join(docsDir, "nested"), 0o755); err != nil {
		t.Fatalf("mkdir bundle: %v", err)
	}
	if err := os.WriteFile(filepath.Join(docsDir, "nested", "note.md"), []byte("nested\n"), 0o644); err != nil {
		t.Fatalf("write nested file: %v", err)
	}
	result, err := FinalizeSourceCommit(
		repoPath,
		repoPath,
		branchName,
		"echo: finalize directory artifact",
		[]string{"docs/bundle"},
	)
	if err != nil {
		t.Fatalf("FinalizeSourceCommit: %v", err)
	}
	if !result.CreatedCommit {
		t.Fatal("expected directory artifact commit to be created")
	}
	if got, err := runGit(repoPath, "show", "HEAD:docs/bundle/nested/note.md"); err != nil || got != "nested" {
		t.Fatalf("directory artifact not present in HEAD: %q err=%v", got, err)
	}
	if _, err := runGit(originPath, "rev-parse", "refs/heads/"+branchName); err != nil {
		t.Fatalf("expected pushed remote branch: %v", err)
	}
}

func TestInspectWorkspaceAndCheckPathsSmoke(t *testing.T) {
	originPath, repoPath := initGitRepoWithOrigin(t)
	worktreePath := filepath.Join(t.TempDir(), "worktrees", "inspect-smoke")
	branchName := "echo/research/inspect-smoke"

	if _, err := SetupWorkspace(repoPath, "main", branchName, worktreePath); err != nil {
		t.Fatalf("SetupWorkspace: %v", err)
	}
	docsDir := filepath.Join(worktreePath, "docs", "echo", "research")
	if err := os.MkdirAll(docsDir, 0o755); err != nil {
		t.Fatalf("mkdir docs: %v", err)
	}
	researchPath := filepath.Join(docsDir, "brief.md")
	if err := os.WriteFile(researchPath, []byte("brief\n"), 0o644); err != nil {
		t.Fatalf("write brief: %v", err)
	}
	if _, err := runGit(worktreePath, "add", "docs/echo/research/brief.md"); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if _, err := runGit(worktreePath, "commit", "-m", "add brief"); err != nil {
		t.Fatalf("git commit: %v", err)
	}
	if _, err := runGit(worktreePath, "push", "origin", "HEAD:refs/heads/"+branchName); err != nil {
		t.Fatalf("git push: %v", err)
	}

	snapshot, err := CaptureWorkspaceSnapshot(repoPath, worktreePath, branchName)
	if err != nil {
		t.Fatalf("CaptureWorkspaceSnapshot: %v", err)
	}
	if snapshot.HeadSHA == "" {
		t.Fatal("expected snapshot head sha")
	}
	if snapshot.Branch != branchName {
		t.Fatalf("snapshot branch = %q, want %q", snapshot.Branch, branchName)
	}

	inspection, err := InspectWorkspace(
		repoPath,
		worktreePath,
		branchName,
		WorkspaceInspectionRequest{
			ResolveRefs:   []string{branchName},
			RemoteRefs:    []string{"refs/heads/" + branchName},
			WorktreePaths: []string{"docs/echo/research/brief.md"},
			HeadPaths:     []string{"docs/echo/research/brief.md"},
			IncludeHead:   true,
			IncludeBranch: true,
		},
	)
	if err != nil {
		t.Fatalf("InspectWorkspace: %v", err)
	}
	if inspection.Error != "" {
		t.Fatalf("unexpected inspection error: %v", inspection.Error)
	}
	if inspection.HeadSHA == "" {
		t.Fatal("expected inspection head sha")
	}
	if inspection.Branch != branchName {
		t.Fatalf("inspection branch = %q, want %q", inspection.Branch, branchName)
	}
	if inspection.ResolvedRefs[branchName] == "" {
		t.Fatalf("expected resolved ref for %q", branchName)
	}
	if inspection.RemoteRefs["refs/heads/"+branchName] == "" {
		t.Fatalf("expected remote ref for %q", branchName)
	}
	if !inspection.WorktreePaths["docs/echo/research/brief.md"].IsFile {
		t.Fatalf("expected worktree file state, got %#v", inspection.WorktreePaths)
	}
	if !inspection.HeadPaths["docs/echo/research/brief.md"] {
		t.Fatalf("expected HEAD path to exist, got %#v", inspection.HeadPaths)
	}

	dirtyPath := filepath.Join(docsDir, "draft.md")
	if err := os.WriteFile(dirtyPath, []byte("draft\n"), 0o644); err != nil {
		t.Fatalf("write dirty file: %v", err)
	}
	dirtyInspection, err := InspectWorkspace(
		repoPath,
		worktreePath,
		branchName,
		WorkspaceInspectionRequest{
			WorktreePaths: []string{"docs/echo/research/draft.md"},
			HeadPaths:     []string{"docs/echo/research/draft.md"},
		},
	)
	if err != nil {
		t.Fatalf("InspectWorkspace dirty path: %v", err)
	}
	if !dirtyInspection.WorktreePaths["docs/echo/research/draft.md"].IsFile {
		t.Fatalf("expected dirty worktree file state, got %#v", dirtyInspection.WorktreePaths)
	}
	if dirtyInspection.HeadPaths["docs/echo/research/draft.md"] {
		t.Fatalf("expected dirty path to be absent from HEAD, got %#v", dirtyInspection.HeadPaths)
	}

	pathCheck, err := CheckPaths(CheckPathsRequest{
		Paths: []string{
			worktreePath,
			researchPath,
			dirtyPath,
			filepath.Join(originPath, "missing.txt"),
		},
	})
	if err != nil {
		t.Fatalf("CheckPaths: %v", err)
	}
	if !pathCheck.Paths[worktreePath].Exists || !pathCheck.Paths[worktreePath].IsDir {
		t.Fatalf("expected worktree dir state, got %#v", pathCheck.Paths[worktreePath])
	}
	if !pathCheck.Paths[researchPath].Exists || !pathCheck.Paths[researchPath].IsFile {
		t.Fatalf("expected research file state, got %#v", pathCheck.Paths[researchPath])
	}
	if !pathCheck.Paths[dirtyPath].Exists || !pathCheck.Paths[dirtyPath].IsFile {
		t.Fatalf("expected dirty file state, got %#v", pathCheck.Paths[dirtyPath])
	}
	if pathCheck.Paths[filepath.Join(originPath, "missing.txt")].Exists {
		t.Fatalf("expected missing path to be absent, got %#v", pathCheck.Paths)
	}
}

func TestFinalizeSourceCommitRejectsEmptyDirectoryArtifact(t *testing.T) {
	_, repoPath := initGitRepoWithOrigin(t)
	branchName := "echo/task-empty-directory"
	if _, err := runGit(repoPath, "checkout", "-b", branchName); err != nil {
		t.Fatalf("git checkout -b: %v", err)
	}
	emptyDir := filepath.Join(repoPath, "docs", "empty")
	if err := os.MkdirAll(emptyDir, 0o755); err != nil {
		t.Fatalf("mkdir empty dir: %v", err)
	}
	_, err := FinalizeSourceCommit(
		repoPath,
		repoPath,
		branchName,
		"echo: finalize empty directory artifact",
		[]string{"docs/empty"},
	)
	if err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("expected empty directory error, got %v", err)
	}
}

func TestFinalizeSourceCommitReusesAlreadyCommittedArtifact(t *testing.T) {
	originPath, repoPath := initGitRepoWithOrigin(t)
	branchName := "echo/task-already-committed"
	if _, err := runGit(repoPath, "checkout", "-b", branchName); err != nil {
		t.Fatalf("git checkout -b: %v", err)
	}
	docsDir := filepath.Join(repoPath, "docs")
	if err := os.MkdirAll(docsDir, 0o755); err != nil {
		t.Fatalf("mkdir docs: %v", err)
	}
	artifactPath := filepath.Join(docsDir, "DESIGN.md")
	if err := os.WriteFile(artifactPath, []byte("design\n"), 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	if _, err := runGit(repoPath, "add", "docs/DESIGN.md"); err != nil {
		t.Fatalf("git add artifact: %v", err)
	}
	if _, err := runGit(repoPath, "commit", "-m", "manual artifact commit"); err != nil {
		t.Fatalf("git commit artifact: %v", err)
	}
	headSHA, err := runGit(repoPath, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	result, err := FinalizeSourceCommit(
		repoPath,
		repoPath,
		branchName,
		"echo: finalize already committed artifact",
		[]string{"docs/DESIGN.md"},
	)
	if err != nil {
		t.Fatalf("FinalizeSourceCommit: %v", err)
	}
	if result.CreatedCommit {
		t.Fatal("expected no new commit for already committed artifact")
	}
	if result.CommitSHA != headSHA {
		t.Fatalf("commit sha = %q, want %q", result.CommitSHA, headSHA)
	}
	if _, err := runGit(originPath, "rev-parse", "refs/heads/"+branchName); err != nil {
		t.Fatalf("expected pushed remote branch: %v", err)
	}
}

func TestFinalizeSourceCommitPushesWithLeaseWhenRemoteIsAncestor(t *testing.T) {
	originPath, repoPath := initGitRepoWithOrigin(t)
	branchName := "echo/task-lease"
	if _, err := runGit(repoPath, "checkout", "-b", branchName); err != nil {
		t.Fatalf("git checkout -b: %v", err)
	}
	if _, err := runGit(repoPath, "push", "origin", "HEAD:refs/heads/"+branchName); err != nil {
		t.Fatalf("push initial branch: %v", err)
	}
	docsDir := filepath.Join(repoPath, "docs")
	if err := os.MkdirAll(docsDir, 0o755); err != nil {
		t.Fatalf("mkdir docs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(docsDir, "RESEARCH.md"), []byte("lease\n"), 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	result, err := FinalizeSourceCommit(
		repoPath,
		repoPath,
		branchName,
		"echo: finalize lease path",
		[]string{"docs/RESEARCH.md"},
	)
	if err != nil {
		t.Fatalf("FinalizeSourceCommit: %v", err)
	}
	remoteSHA, err := runGit(originPath, "rev-parse", "refs/heads/"+branchName)
	if err != nil {
		t.Fatalf("rev-parse remote branch: %v", err)
	}
	if remoteSHA != result.CommitSHA {
		t.Fatalf("remote sha = %q, want %q", remoteSHA, result.CommitSHA)
	}
}

func TestFinalizeSourceCommitRejectsTrackedDirtyWithoutArtifacts(t *testing.T) {
	_, repoPath := initGitRepoWithOrigin(t)
	branchName := "echo/task-no-artifacts"
	if _, err := runGit(repoPath, "checkout", "-b", branchName); err != nil {
		t.Fatalf("git checkout -b: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoPath, "README.md"), []byte("dirty tracked\n"), 0o644); err != nil {
		t.Fatalf("write tracked change: %v", err)
	}
	_, err := FinalizeSourceCommit(
		repoPath,
		repoPath,
		branchName,
		"echo: finalize design output",
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "tracked uncommitted changes") {
		t.Fatalf("expected tracked uncommitted changes error, got %v", err)
	}
}

func TestFinalizeSourceCommitRejectsDivergedRemoteBranch(t *testing.T) {
	originPath, repoPath := initGitRepoWithOrigin(t)
	branchName := "echo/task-diverged"
	if _, err := runGit(repoPath, "checkout", "-b", branchName); err != nil {
		t.Fatalf("git checkout -b: %v", err)
	}
	if _, err := runGit(repoPath, "push", "origin", "HEAD:refs/heads/"+branchName); err != nil {
		t.Fatalf("push initial branch: %v", err)
	}

	otherClone := filepath.Join(t.TempDir(), "other")
	if _, err := runGit(filepath.Dir(otherClone), "clone", originPath, otherClone); err != nil {
		t.Fatalf("clone other: %v", err)
	}
	if _, err := runGit(otherClone, "config", "user.email", "fixture@example.com"); err != nil {
		t.Fatalf("config user.email: %v", err)
	}
	if _, err := runGit(otherClone, "config", "user.name", "Fixture User"); err != nil {
		t.Fatalf("config user.name: %v", err)
	}
	if _, err := runGit(otherClone, "checkout", branchName); err != nil {
		t.Fatalf("checkout other branch: %v", err)
	}
	if err := os.WriteFile(filepath.Join(otherClone, "README.md"), []byte("remote moved\n"), 0o644); err != nil {
		t.Fatalf("write remote change: %v", err)
	}
	if _, err := runGit(otherClone, "add", "README.md"); err != nil {
		t.Fatalf("git add remote change: %v", err)
	}
	if _, err := runGit(otherClone, "commit", "-m", "remote moved"); err != nil {
		t.Fatalf("git commit remote change: %v", err)
	}
	if _, err := runGit(otherClone, "push", "origin", "HEAD:refs/heads/"+branchName); err != nil {
		t.Fatalf("push remote change: %v", err)
	}

	docsDir := filepath.Join(repoPath, "docs")
	if err := os.MkdirAll(docsDir, 0o755); err != nil {
		t.Fatalf("mkdir docs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(docsDir, "RESEARCH.md"), []byte("local artifact\n"), 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	_, err := FinalizeSourceCommit(
		repoPath,
		repoPath,
		branchName,
		"echo: finalize research output",
		[]string{"docs/RESEARCH.md"},
	)
	if err == nil || !strings.Contains(err.Error(), "has diverged") {
		t.Fatalf("expected diverged branch error, got %v", err)
	}
}

func TestPushRefPushesCommitToRemoteRef(t *testing.T) {
	originPath, repoPath := initGitRepoWithOrigin(t)
	branchName := "echo/task-push-ref"
	if _, err := runGit(repoPath, "checkout", "-b", branchName); err != nil {
		t.Fatalf("git checkout -b: %v", err)
	}
	docsDir := filepath.Join(repoPath, "docs")
	if err := os.MkdirAll(docsDir, 0o755); err != nil {
		t.Fatalf("mkdir docs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(docsDir, "RESEARCH.md"), []byte("brief\n"), 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	if _, err := runGit(repoPath, "add", "docs/RESEARCH.md"); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if _, err := runGit(repoPath, "commit", "-m", "create push ref artifact"); err != nil {
		t.Fatalf("git commit: %v", err)
	}
	headSHA, err := runGit(repoPath, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}

	result, err := PushRef(
		repoPath,
		repoPath,
		headSHA,
		"refs/heads/echo/source/task-push-ref",
	)
	if err != nil {
		t.Fatalf("PushRef: %v", err)
	}
	if result.SourceRef != headSHA {
		t.Fatalf("source ref = %q, want %q", result.SourceRef, headSHA)
	}
	if result.RemoteRef != "refs/heads/echo/source/task-push-ref" {
		t.Fatalf("remote ref = %q", result.RemoteRef)
	}
	remoteSHA, err := runGit(originPath, "rev-parse", "refs/heads/echo/source/task-push-ref")
	if err != nil {
		t.Fatalf("git rev-parse remote ref: %v", err)
	}
	if remoteSHA != headSHA {
		t.Fatalf("remote sha = %q, want %q", remoteSHA, headSHA)
	}
}

func TestDeleteRemoteRefDeletesManagedRemoteRef(t *testing.T) {
	originPath, repoPath := initGitRepoWithOrigin(t)
	branchName := "echo/task-delete-ref"
	if _, err := runGit(repoPath, "checkout", "-b", branchName); err != nil {
		t.Fatalf("git checkout -b: %v", err)
	}
	headSHA, err := runGit(repoPath, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	if _, err := runGit(
		repoPath,
		"push",
		"origin",
		headSHA+":refs/heads/echo/source/task-delete-ref",
	); err != nil {
		t.Fatalf("git push source ref: %v", err)
	}

	result, err := DeleteRemoteRef(
		repoPath,
		repoPath,
		"refs/heads/echo/source/task-delete-ref",
	)
	if err != nil {
		t.Fatalf("DeleteRemoteRef: %v", err)
	}
	if !result.Deleted {
		t.Fatal("expected remote ref to be deleted")
	}
	if result.RemoteRef != "refs/heads/echo/source/task-delete-ref" {
		t.Fatalf("remote ref = %q", result.RemoteRef)
	}
	if _, err := runGit(originPath, "rev-parse", "refs/heads/echo/source/task-delete-ref"); err == nil {
		t.Fatal("expected remote ref to be absent after delete")
	}
}

func TestDeleteRemoteRefTreatsMissingRefAsSuccess(t *testing.T) {
	_, repoPath := initGitRepoWithOrigin(t)

	result, err := DeleteRemoteRef(
		repoPath,
		repoPath,
		"refs/heads/echo/source/missing-ref",
	)
	if err != nil {
		t.Fatalf("DeleteRemoteRef missing ref: %v", err)
	}
	if result.Deleted {
		t.Fatal("expected missing remote ref to report deleted=false")
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

func initGitRepoWithOrigin(t *testing.T) (string, string) {
	t.Helper()

	originPath := filepath.Join(t.TempDir(), "origin.git")
	if err := os.MkdirAll(originPath, 0o755); err != nil {
		t.Fatalf("mkdir origin: %v", err)
	}
	if _, err := runGit(originPath, "init", "--bare"); err != nil {
		t.Fatalf("git init --bare: %v", err)
	}
	repoPath := filepath.Join(t.TempDir(), "repo")
	if _, err := runGit(filepath.Dir(repoPath), "clone", originPath, repoPath); err != nil {
		t.Fatalf("git clone: %v", err)
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
	if _, err := runGit(repoPath, "push", "origin", "HEAD:refs/heads/main"); err != nil {
		t.Fatalf("git push main: %v", err)
	}
	return originPath, repoPath
}
