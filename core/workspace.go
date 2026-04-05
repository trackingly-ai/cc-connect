package core

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const workspaceCommandTimeout = 30 * time.Second
const workspaceBranchPrefix = "echo/"

var workspaceRepoLocks sync.Map
var workspacePathLocks sync.Map
var runGit = runGitCommand

type CleanupWorkspaceOptions struct {
	KeepBranch bool
}

type SourceCommitFinalizationResult struct {
	CommitSHA     string `json:"commit_sha"`
	BranchName    string `json:"branch_name"`
	CreatedCommit bool   `json:"created_commit"`
}

type WorkspaceSnapshot struct {
	RepoPath     string   `json:"repo_path,omitempty"`
	WorktreePath string   `json:"worktree_path,omitempty"`
	Branch       string   `json:"branch,omitempty"`
	HeadSHA      string   `json:"head_sha,omitempty"`
	StatusShort  []string `json:"status_short,omitempty"`
	Error        string   `json:"error,omitempty"`
}

type WorkspaceSetupResult struct {
	RepoPath               string `json:"repo_path,omitempty"`
	BaseBranch             string `json:"base_branch,omitempty"`
	BranchName             string `json:"branch_name,omitempty"`
	WorktreePath           string `json:"worktree_path,omitempty"`
	RequestedBaseCommitSHA string `json:"requested_base_commit_sha,omitempty"`
	InitialHeadCommitSHA   string `json:"initial_head_commit_sha,omitempty"`
}

type WorkspacePathState struct {
	Exists bool   `json:"exists"`
	IsFile bool   `json:"is_file"`
	IsDir  bool   `json:"is_dir"`
	Error  string `json:"error,omitempty"`
}

type WorkspaceInspectionRequest struct {
	ResolveRefs   []string `json:"resolve_refs,omitempty"`
	RemoteRefs    []string `json:"remote_refs,omitempty"`
	WorktreePaths []string `json:"worktree_paths,omitempty"`
	HeadPaths     []string `json:"head_paths,omitempty"`
	IncludeHead   bool     `json:"include_head,omitempty"`
	IncludeBranch bool     `json:"include_branch,omitempty"`
}

type WorkspaceInspectionResult struct {
	RepoPath      string                        `json:"repo_path,omitempty"`
	WorktreePath  string                        `json:"worktree_path,omitempty"`
	Branch        string                        `json:"branch,omitempty"`
	HeadSHA       string                        `json:"head_sha,omitempty"`
	ResolvedRefs  map[string]string             `json:"resolved_refs,omitempty"`
	RemoteRefs    map[string]string             `json:"remote_refs,omitempty"`
	WorktreePaths map[string]WorkspacePathState `json:"worktree_paths,omitempty"`
	HeadPaths     map[string]bool               `json:"head_paths,omitempty"`
	Error         string                        `json:"error,omitempty"`
}

type PushRefResult struct {
	SourceRef string `json:"source_ref,omitempty"`
	RemoteRef string `json:"remote_ref,omitempty"`
}

type DeleteRemoteRefResult struct {
	RemoteRef string `json:"remote_ref,omitempty"`
	Deleted   bool   `json:"deleted"`
}

type CheckPathsRequest struct {
	Paths []string `json:"paths"`
}

type CheckPathsResult struct {
	Paths map[string]WorkspacePathState `json:"paths"`
}

func (s *WorkspaceSnapshot) Summary() string {
	if s == nil {
		return "workspace snapshot unavailable"
	}
	if s.Error != "" {
		return "Workspace snapshot failed\nError: " + s.Error
	}
	lines := []string{
		"Workspace snapshot",
		"Repo: " + emptyFallback(s.RepoPath, "unknown"),
		"Worktree: " + emptyFallback(s.WorktreePath, "unknown"),
		"Branch: " + emptyFallback(s.Branch, "unknown"),
		"HEAD: " + emptyFallback(s.HeadSHA, "unknown"),
	}
	if len(s.StatusShort) == 0 {
		lines = append(lines, "Status: clean")
		return strings.Join(lines, "\n")
	}
	lines = append(lines, "Status:")
	lines = append(lines, s.StatusShort...)
	return strings.Join(lines, "\n")
}

func (s *WorkspaceSnapshot) Metadata() map[string]any {
	if s == nil {
		return map[string]any{}
	}
	metadata := map[string]any{
		"repo_path":      s.RepoPath,
		"worktree_path":  s.WorktreePath,
		"branch":         s.Branch,
		"head_sha":       s.HeadSHA,
		"status_short":   append([]string(nil), s.StatusShort...),
		"snapshot_error": s.Error,
	}
	return metadata
}

func SetupWorkspace(
	repoPath string,
	baseBranch string,
	branchName string,
	worktreePath string,
) (*WorkspaceSetupResult, error) {
	repoPath = strings.TrimSpace(repoPath)
	baseBranch = strings.TrimSpace(baseBranch)
	branchName = strings.TrimSpace(branchName)
	worktreePath = strings.TrimSpace(worktreePath)

	if repoPath == "" {
		return nil, fmt.Errorf("repo_path is required")
	}
	if baseBranch == "" {
		return nil, fmt.Errorf("base_branch is required")
	}
	if branchName == "" {
		return nil, fmt.Errorf("branch_name is required")
	}
	if worktreePath == "" {
		return nil, fmt.Errorf("worktree_path is required")
	}
	result := &WorkspaceSetupResult{
		RepoPath:     repoPath,
		BaseBranch:   baseBranch,
		BranchName:   branchName,
		WorktreePath: worktreePath,
	}
	baseRefOrCommit, baseCommit, err := resolveAuthoritativeBaseRef(repoPath, baseBranch)
	if err != nil {
		return nil, fmt.Errorf("resolve base branch commit: %w", err)
	}
	result.RequestedBaseCommitSHA = strings.TrimSpace(baseCommit)
	if err := withWorkspacePathLock(worktreePath, func() error {
		return withWorkspaceRepoLock(repoPath, func() error {
			if _, err := os.Stat(worktreePath); err == nil {
				return fmt.Errorf("worktree path already exists: %s", worktreePath)
			} else if !os.IsNotExist(err) {
				return fmt.Errorf("stat worktree path: %w", err)
			}
			existingBranch, err := runGit(
				repoPath,
				"branch",
				"--list",
				"--format=%(refname:short)",
				"--",
				branchName,
			)
			if err != nil {
				return fmt.Errorf("check workspace branch: %w", err)
			}
			branchExists := strings.TrimSpace(existingBranch) != ""
			if err := os.MkdirAll(filepath.Dir(worktreePath), 0o755); err != nil {
				return fmt.Errorf("create worktree parent dir: %w", err)
			}

			args := []string{"worktree", "add", "--checkout"}
			if branchExists {
				// Reusing an existing branch keeps that branch tip as the worktree's
				// initial HEAD, even though RequestedBaseCommitSHA still records the
				// requested base branch that setup was asked to use.
				args = append(args, worktreePath, branchName)
			} else {
				args = append(args, "-b", branchName, worktreePath, baseRefOrCommit)
			}
			if _, err := runGit(repoPath, args...); err != nil {
				return fmt.Errorf("git worktree add: %w", err)
			}
			headCommit, err := runGit(worktreePath, "rev-parse", "HEAD")
			if err != nil {
				return fmt.Errorf("resolve initial workspace head: %w", err)
			}
			result.InitialHeadCommitSHA = strings.TrimSpace(headCommit)
			return nil
		})
	}); err != nil {
		return nil, err
	}
	return result, nil
}

func resolveAuthoritativeBaseRef(repoPath string, baseBranch string) (string, string, error) {
	baseBranch = strings.TrimSpace(baseBranch)
	if baseBranch == "" {
		return "", "", fmt.Errorf("base_branch is required")
	}
	if _, err := runGit(repoPath, "remote", "get-url", "origin"); err == nil {
		if _, err := runGit(repoPath, "fetch", "origin", "--prune"); err != nil {
			slog.Warn(
				"workspace setup could not refresh origin; falling back to local base ref",
				"repo_path",
				repoPath,
				"base_branch",
				baseBranch,
				"error",
				err,
			)
		} else {
			remoteTrackingRef := remoteTrackingRefForBranch(baseBranch)
			if remoteTrackingRef != "" {
				remoteCommit, err := runGit(repoPath, "rev-parse", remoteTrackingRef)
				if err == nil {
					return strings.TrimSpace(remoteCommit), strings.TrimSpace(remoteCommit), nil
				}
			}
		}
	}

	baseCommit, err := runGit(repoPath, "rev-parse", baseBranch)
	if err != nil {
		return "", "", err
	}
	return baseBranch, strings.TrimSpace(baseCommit), nil
}

func remoteTrackingRefForBranch(branch string) string {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return ""
	}
	if strings.HasPrefix(branch, "refs/remotes/") {
		return branch
	}
	if strings.HasPrefix(branch, "origin/") {
		return "refs/remotes/" + branch
	}
	if strings.HasPrefix(branch, "refs/heads/") {
		return "refs/remotes/origin/" + strings.TrimPrefix(branch, "refs/heads/")
	}
	if strings.HasPrefix(branch, "refs/") {
		// Only branch refs have a well-defined origin/<branch> remote-tracking
		// counterpart. Other refs (for example tags) intentionally fall back to
		// direct local resolution.
		return ""
	}
	return "refs/remotes/origin/" + branch
}

func EnsureRepoCheckout(repoURL string, repoPath string, defaultBranch string) error {
	repoURL = strings.TrimSpace(repoURL)
	repoPath = strings.TrimSpace(repoPath)
	defaultBranch = strings.TrimSpace(defaultBranch)
	if repoURL == "" {
		return fmt.Errorf("repo_url is required")
	}
	if repoPath == "" {
		return fmt.Errorf("repo_path is required")
	}
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	return withWorkspaceRepoLock(repoPath, func() error {
		if info, err := os.Stat(repoPath); err == nil {
			if !info.IsDir() {
				return fmt.Errorf("repo_path is not a directory: %s", repoPath)
			}
			if _, err := runGit(repoPath, "rev-parse", "--is-inside-work-tree"); err != nil {
				return fmt.Errorf("repo_path does not point to a git repository")
			}
			originURL, err := runGit(repoPath, "remote", "get-url", "origin")
			if err != nil {
				return fmt.Errorf("resolve origin remote: %w", err)
			}
			if strings.TrimSpace(originURL) != repoURL {
				return fmt.Errorf(
					"repo_path origin %q does not match repo_url %q",
					strings.TrimSpace(originURL),
					repoURL,
				)
			}
			if _, err := runGit(repoPath, "fetch", "origin", "--prune"); err != nil {
				return fmt.Errorf("fetch repo checkout: %w", err)
			}
			if _, err := runGit(repoPath, "checkout", defaultBranch); err != nil {
				return fmt.Errorf("checkout default branch: %w", err)
			}
			return nil
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("stat repo_path: %w", err)
		}

		if err := os.MkdirAll(filepath.Dir(repoPath), 0o755); err != nil {
			return fmt.Errorf("create repo parent dir: %w", err)
		}
		if _, err := runGit(
			filepath.Dir(repoPath),
			"clone",
			"--origin",
			"origin",
			"--branch",
			defaultBranch,
			"--single-branch",
			repoURL,
			repoPath,
		); err != nil {
			return fmt.Errorf("clone repo checkout: %w", err)
		}
		return nil
	})
}

func CleanupWorkspace(worktreePath string) error {
	return CleanupWorkspaceWithOptions(worktreePath, CleanupWorkspaceOptions{})
}

func CaptureWorkspaceSnapshot(
	repoPath string,
	worktreePath string,
	branchName string,
) (*WorkspaceSnapshot, error) {
	repoPath = strings.TrimSpace(repoPath)
	worktreePath = strings.TrimSpace(worktreePath)
	branchName = strings.TrimSpace(branchName)
	if worktreePath == "" {
		return &WorkspaceSnapshot{
			RepoPath:     repoPath,
			WorktreePath: worktreePath,
			Branch:       branchName,
			Error:        "missing worktree path",
		}, nil
	}

	snapshot := &WorkspaceSnapshot{
		RepoPath:     repoPath,
		WorktreePath: worktreePath,
		Branch:       branchName,
	}

	if _, err := os.Stat(worktreePath); err != nil {
		snapshot.Error = fmt.Sprintf("stat worktree path: %v", err)
		return snapshot, nil
	}

	err := withWorkspacePathLock(worktreePath, func() error {
		headSHA, err := runGit(worktreePath, "rev-parse", "HEAD")
		if err != nil {
			snapshot.Error = fmt.Sprintf("resolve HEAD: %v", err)
			return nil
		}
		snapshot.HeadSHA = strings.TrimSpace(headSHA)

		statusOutput, err := runGit(
			worktreePath,
			"status",
			"--short",
			"--branch",
			"--untracked-files=all",
		)
		if err != nil {
			snapshot.Error = fmt.Sprintf("git status: %v", err)
			return nil
		}
		snapshot.StatusShort = nonEmptyLines(statusOutput)

		if snapshot.Branch == "" {
			currentBranch, err := runGit(worktreePath, "rev-parse", "--abbrev-ref", "HEAD")
			if err == nil {
				snapshot.Branch = strings.TrimSpace(currentBranch)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return snapshot, nil
}

func InspectWorkspace(
	repoPath string,
	worktreePath string,
	branchName string,
	req WorkspaceInspectionRequest,
) (*WorkspaceInspectionResult, error) {
	repoPath = strings.TrimSpace(repoPath)
	worktreePath = strings.TrimSpace(worktreePath)
	branchName = strings.TrimSpace(branchName)
	if repoPath == "" {
		return nil, fmt.Errorf("repo_path is required")
	}
	if worktreePath == "" {
		return nil, fmt.Errorf("worktree_path is required")
	}

	result := &WorkspaceInspectionResult{
		RepoPath:      repoPath,
		WorktreePath:  worktreePath,
		Branch:        branchName,
		ResolvedRefs:  map[string]string{},
		RemoteRefs:    map[string]string{},
		WorktreePaths: map[string]WorkspacePathState{},
		HeadPaths:     map[string]bool{},
	}
	if _, err := os.Stat(worktreePath); err != nil {
		result.Error = fmt.Sprintf("stat worktree path: %v", err)
		return result, nil
	}

	err := withWorkspacePathLock(worktreePath, func() error {
		if req.IncludeHead {
			headSHA, err := runGit(worktreePath, "rev-parse", "HEAD")
			if err != nil {
				result.Error = fmt.Sprintf("resolve HEAD: %v", err)
				return nil
			}
			result.HeadSHA = strings.TrimSpace(headSHA)
		}
		if req.IncludeBranch && result.Branch == "" {
			currentBranch, err := runGit(worktreePath, "rev-parse", "--abbrev-ref", "HEAD")
			if err == nil {
				result.Branch = strings.TrimSpace(currentBranch)
			}
		}
		for _, ref := range req.ResolveRefs {
			ref = strings.TrimSpace(ref)
			if ref == "" {
				continue
			}
			sha, err := runGit(worktreePath, "rev-parse", ref)
			if err == nil {
				result.ResolvedRefs[ref] = strings.TrimSpace(sha)
			}
		}
		for _, ref := range req.RemoteRefs {
			ref = strings.TrimSpace(ref)
			if ref == "" {
				continue
			}
			sha, err := resolveRemoteBranchHeadSHA(worktreePath, ref)
			if err == nil && sha != "" {
				result.RemoteRefs[ref] = strings.TrimSpace(sha)
			}
		}
		for _, relPath := range req.WorktreePaths {
			relPath = strings.TrimSpace(relPath)
			if relPath == "" {
				continue
			}
			state := WorkspacePathState{}
			info, err := os.Stat(filepath.Join(worktreePath, relPath))
			if err != nil {
				if !os.IsNotExist(err) {
					state.Error = err.Error()
				}
				result.WorktreePaths[relPath] = state
				continue
			}
			state.Exists = true
			state.IsDir = info.IsDir()
			state.IsFile = info.Mode().IsRegular()
			result.WorktreePaths[relPath] = state
		}
		for _, relPath := range req.HeadPaths {
			relPath = strings.TrimSpace(relPath)
			if relPath == "" {
				continue
			}
			result.HeadPaths[relPath] = headPathExists(worktreePath, relPath)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func FinalizeSourceCommit(
	repoPath string,
	worktreePath string,
	branchName string,
	commitMessage string,
	artifactPaths []string,
) (*SourceCommitFinalizationResult, error) {
	repoPath = strings.TrimSpace(repoPath)
	worktreePath = strings.TrimSpace(worktreePath)
	branchName = strings.TrimSpace(branchName)
	commitMessage = strings.TrimSpace(commitMessage)
	if repoPath == "" {
		return nil, fmt.Errorf("repo_path is required")
	}
	if worktreePath == "" {
		return nil, fmt.Errorf("worktree_path is required")
	}
	if branchName == "" {
		return nil, fmt.Errorf("branch_name is required")
	}
	if commitMessage == "" {
		return nil, fmt.Errorf("commit_message is required")
	}
	if _, err := os.Stat(worktreePath); err != nil {
		return nil, fmt.Errorf("stat worktree path: %w", err)
	}

	var finalResult *SourceCommitFinalizationResult
	err := withWorkspaceRepoLock(repoPath, func() error {
		result, err := finalizeSourceCommitLocked(
			worktreePath,
			branchName,
			commitMessage,
			artifactPaths,
		)
		if err != nil {
			return err
		}
		finalResult = result
		return nil
	})
	if err != nil {
		return nil, err
	}
	return finalResult, nil
}

func CleanupWorkspaceWithOptions(
	worktreePath string,
	opts CleanupWorkspaceOptions,
) error {
	worktreePath = strings.TrimSpace(worktreePath)
	if worktreePath == "" {
		return fmt.Errorf("worktree_path is required")
	}
	return withWorkspacePathLock(worktreePath, func() error {
		if _, err := os.Stat(worktreePath); os.IsNotExist(err) {
			return nil
		} else if err != nil {
			return fmt.Errorf("stat worktree path: %w", err)
		}

		branchName, err := runGit(worktreePath, "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil {
			return fmt.Errorf("resolve workspace branch: %w", err)
		}
		commonDir, err := runGit(worktreePath, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err != nil {
			return fmt.Errorf("resolve git common dir: %w", err)
		}
		repoPath := filepath.Dir(strings.TrimSpace(commonDir))
		return withWorkspaceRepoLock(repoPath, func() error {
			if _, err := runGit(repoPath, "worktree", "remove", "--force", worktreePath); err != nil {
				if os.IsNotExist(err) || strings.Contains(err.Error(), "is not a working tree") {
					return nil
				}
				return fmt.Errorf("git worktree remove: %w", err)
			}
			branchName = strings.TrimSpace(branchName)
			if opts.KeepBranch || branchName == "" || branchName == "HEAD" {
				return nil
			}
			if !strings.HasPrefix(branchName, workspaceBranchPrefix) {
				return nil
			}
			if _, err := runGit(repoPath, "branch", "-D", branchName); err != nil {
				return fmt.Errorf("delete workspace branch: %w", err)
			}
			return nil
		})
	})
}

func finalizeSourceCommitLocked(
	worktreePath string,
	branchName string,
	commitMessage string,
	artifactPaths []string,
) (*SourceCommitFinalizationResult, error) {
	statusOutput, err := runGit(
		worktreePath,
		"status",
		"--porcelain",
		"--untracked-files=all",
	)
	if err != nil {
		return nil, fmt.Errorf("git status: %w", err)
	}
	statusEntries := nonEmptyLines(statusOutput)
	trackedChanges, untrackedChanges := splitGitStatusEntries(statusEntries)

	if len(artifactPaths) == 0 {
		if len(trackedChanges) > 0 {
			return nil, fmt.Errorf(
				"tracked uncommitted changes present without repo_artifacts",
			)
		}
		commitSHA, err := runGit(worktreePath, "rev-parse", "HEAD")
		if err != nil {
			return nil, fmt.Errorf("resolve HEAD commit: %w", err)
		}
		if err := pushSourceBranch(worktreePath, branchName); err != nil {
			return nil, err
		}
		if len(untrackedChanges) > 0 {
			slog.Warn(
				"echo finalize_source_commit ignored untracked non-artifact files",
				"branch_name", branchName,
				"ignored_paths", untrackedChanges,
			)
		}
		return &SourceCommitFinalizationResult{
			CommitSHA:     strings.TrimSpace(commitSHA),
			BranchName:    branchName,
			CreatedCommit: false,
		}, nil
	}

	cleanArtifactPaths := make([]string, 0, len(artifactPaths))
	for _, artifactPath := range artifactPaths {
		artifactPath = strings.TrimSpace(artifactPath)
		if artifactPath == "" {
			continue
		}
		cleanArtifactPaths = append(cleanArtifactPaths, artifactPath)
		info, err := os.Stat(filepath.Join(worktreePath, artifactPath))
		if err != nil {
			return nil, fmt.Errorf("stat artifact path %q: %w", artifactPath, err)
		}
		if info.IsDir() {
			entries, err := os.ReadDir(filepath.Join(worktreePath, artifactPath))
			if err != nil {
				return nil, fmt.Errorf("read artifact directory %q: %w", artifactPath, err)
			}
			if len(entries) == 0 {
				return nil, fmt.Errorf("artifact directory %q is empty", artifactPath)
			}
		}
	}
	if len(cleanArtifactPaths) == 0 {
		return nil, fmt.Errorf("artifact_paths must contain at least one non-empty path")
	}

	createdCommit := false
	if len(statusEntries) > 0 {
		args := append([]string{"add", "--"}, cleanArtifactPaths...)
		if _, err := runGit(worktreePath, args...); err != nil {
			return nil, fmt.Errorf("git add artifacts: %w", err)
		}
		stagedOutput, err := runGit(worktreePath, "diff", "--cached", "--name-only")
		if err != nil {
			return nil, fmt.Errorf("git diff --cached: %w", err)
		}
		if len(nonEmptyLines(stagedOutput)) > 0 {
			if _, err := runGit(worktreePath, "commit", "-m", commitMessage); err != nil {
				return nil, fmt.Errorf("git commit: %w", err)
			}
			createdCommit = true
		} else if len(trackedChanges) > 0 || len(untrackedChanges) > 0 {
			slog.Warn(
				"echo finalize_source_commit found dirty workspace outside declared artifacts",
				"branch_name", branchName,
				"tracked_changes", trackedChanges,
				"untracked_changes", untrackedChanges,
			)
		}
	}

	for _, artifactPath := range cleanArtifactPaths {
		if err := verifyArtifactInHEAD(worktreePath, artifactPath); err != nil {
			return nil, err
		}
	}

	commitSHA, err := runGit(worktreePath, "rev-parse", branchName)
	if err != nil {
		return nil, fmt.Errorf("resolve branch commit: %w", err)
	}
	if err := pushSourceBranch(worktreePath, branchName); err != nil {
		return nil, err
	}

	return &SourceCommitFinalizationResult{
		CommitSHA:     strings.TrimSpace(commitSHA),
		BranchName:    branchName,
		CreatedCommit: createdCommit,
	}, nil
}

func splitGitStatusEntries(entries []string) ([]string, []string) {
	tracked := make([]string, 0, len(entries))
	untracked := make([]string, 0, len(entries))
	for _, entry := range entries {
		if len(entry) >= 2 && entry[:2] == "??" {
			untracked = append(untracked, entry)
			continue
		}
		tracked = append(tracked, entry)
	}
	return tracked, untracked
}

func nonEmptyLines(output string) []string {
	lines := strings.Split(output, "\n")
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		result = append(result, line)
	}
	return result
}

func PushRef(
	repoPath string,
	worktreePath string,
	sourceRef string,
	remoteRef string,
) (*PushRefResult, error) {
	repoPath = strings.TrimSpace(repoPath)
	worktreePath = strings.TrimSpace(worktreePath)
	sourceRef = strings.TrimSpace(sourceRef)
	remoteRef = strings.TrimSpace(remoteRef)
	if repoPath == "" {
		return nil, fmt.Errorf("repo_path is required")
	}
	if worktreePath == "" {
		return nil, fmt.Errorf("worktree_path is required")
	}
	if sourceRef == "" {
		return nil, fmt.Errorf("source_ref is required")
	}
	if remoteRef == "" {
		return nil, fmt.Errorf("remote_ref is required")
	}
	if _, err := os.Stat(worktreePath); err != nil {
		return nil, fmt.Errorf("stat worktree path: %w", err)
	}
	var finalResult *PushRefResult
	err := withWorkspacePathLock(worktreePath, func() error {
		if _, err := runGit(worktreePath, "cat-file", "-e", sourceRef+"^{commit}"); err != nil {
			return fmt.Errorf("source ref %s does not resolve to a commit: %w", sourceRef, err)
		}
		if _, err := runGit(worktreePath, "push", "origin", sourceRef+":"+remoteRef); err != nil {
			return fmt.Errorf("push %s to %s: %w", sourceRef, remoteRef, err)
		}
		finalResult = &PushRefResult{
			SourceRef: sourceRef,
			RemoteRef: remoteRef,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return finalResult, nil
}

func DeleteRemoteRef(
	repoPath string,
	worktreePath string,
	remoteRef string,
) (*DeleteRemoteRefResult, error) {
	repoPath = strings.TrimSpace(repoPath)
	worktreePath = strings.TrimSpace(worktreePath)
	remoteRef = strings.TrimSpace(remoteRef)
	if repoPath == "" {
		return nil, fmt.Errorf("repo_path is required")
	}
	if worktreePath == "" {
		return nil, fmt.Errorf("worktree_path is required")
	}
	if remoteRef == "" {
		return nil, fmt.Errorf("remote_ref is required")
	}
	if _, err := os.Stat(worktreePath); err != nil {
		return nil, fmt.Errorf("stat worktree path: %w", err)
	}

	// Remote ref deletion is best-effort and does not mutate local workspace state,
	// so avoid holding the worktree lock across potentially slow network calls.
	remoteListing, err := runGit(worktreePath, "ls-remote", "--refs", "origin", remoteRef)
	if err != nil {
		return nil, fmt.Errorf("resolve remote ref %s: %w", remoteRef, err)
	}
	if strings.TrimSpace(remoteListing) == "" {
		return &DeleteRemoteRefResult{
			RemoteRef: remoteRef,
			Deleted:   false,
		}, nil
	}
	if _, err := runGit(worktreePath, "push", "origin", ":"+remoteRef); err != nil {
		return nil, fmt.Errorf("delete remote ref %s: %w", remoteRef, err)
	}
	return &DeleteRemoteRefResult{
		RemoteRef: remoteRef,
		Deleted:   true,
	}, nil
}

func verifyArtifactInHEAD(worktreePath string, artifactPath string) error {
	artifactPath = strings.TrimSpace(artifactPath)
	info, err := os.Stat(filepath.Join(worktreePath, artifactPath))
	if err != nil {
		return fmt.Errorf("stat artifact path %q: %w", artifactPath, err)
	}
	if info.IsDir() {
		treeOutput, err := runGit(
			worktreePath,
			"ls-tree",
			"-r",
			"--name-only",
			"HEAD",
			"--",
			artifactPath,
		)
		if err != nil {
			return fmt.Errorf("verify artifact directory %q in HEAD: %w", artifactPath, err)
		}
		if strings.TrimSpace(treeOutput) == "" {
			return fmt.Errorf("artifact directory %q is missing from HEAD", artifactPath)
		}
		return nil
	}
	if _, err := runGit(worktreePath, "cat-file", "-e", "HEAD:"+artifactPath); err != nil {
		return fmt.Errorf("verify artifact %q in HEAD: %w", artifactPath, err)
	}
	return nil
}

func headPathExists(worktreePath string, artifactPath string) bool {
	artifactPath = strings.TrimSpace(artifactPath)
	info, err := os.Stat(filepath.Join(worktreePath, artifactPath))
	if err == nil && info.IsDir() {
		treeOutput, treeErr := runGit(
			worktreePath,
			"ls-tree",
			"-r",
			"--name-only",
			"HEAD",
			"--",
			artifactPath,
		)
		return treeErr == nil && strings.TrimSpace(treeOutput) != ""
	}
	_, err = runGit(worktreePath, "cat-file", "-e", "HEAD:"+artifactPath)
	return err == nil
}

func pushSourceBranch(worktreePath string, branchName string) error {
	remoteRef := "refs/heads/" + strings.TrimSpace(branchName)
	remoteCommitSHA, err := resolveRemoteBranchHeadSHA(worktreePath, remoteRef)
	if err != nil {
		return err
	}
	if remoteCommitSHA != "" {
		if _, err := runGit(
			worktreePath,
			"merge-base",
			"--is-ancestor",
			remoteCommitSHA,
			"HEAD",
		); err != nil {
			return fmt.Errorf("remote branch %s has diverged", branchName)
		}
		if _, err := runGit(
			worktreePath,
			"push",
			"--force-with-lease="+remoteRef+":"+remoteCommitSHA,
			"origin",
			"HEAD:"+remoteRef,
		); err != nil {
			return fmt.Errorf("push source branch with lease: %w", err)
		}
		return nil
	}
	if _, err := runGit(worktreePath, "push", "origin", "HEAD:"+remoteRef); err != nil {
		return fmt.Errorf("push source branch: %w", err)
	}
	return nil
}

func resolveRemoteBranchHeadSHA(worktreePath string, remoteRef string) (string, error) {
	output, err := runGit(worktreePath, "ls-remote", "--heads", "origin", remoteRef)
	if err != nil {
		return "", fmt.Errorf("ls-remote %s: %w", remoteRef, err)
	}
	lines := nonEmptyLines(output)
	if len(lines) == 0 {
		return "", nil
	}
	fields := strings.Fields(lines[0])
	if len(fields) == 0 {
		return "", nil
	}
	return strings.TrimSpace(fields[0]), nil
}

func withWorkspaceRepoLock(repoPath string, fn func() error) error {
	lockKey, err := workspaceLockKey(repoPath)
	if err != nil {
		return err
	}
	raw, _ := workspaceRepoLocks.LoadOrStore(lockKey, &sync.Mutex{})
	mu := raw.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	return fn()
}

func withWorkspacePathLock(worktreePath string, fn func() error) error {
	lockKey, err := workspaceLockKey(worktreePath)
	if err != nil {
		return err
	}
	raw, _ := workspacePathLocks.LoadOrStore(lockKey, &sync.Mutex{})
	mu := raw.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	return fn()
}

func workspaceLockKey(repoPath string) (string, error) {
	repoPath = strings.TrimSpace(repoPath)
	if repoPath == "" {
		return "", fmt.Errorf("repo_path is required")
	}
	absPath, err := filepath.Abs(repoPath)
	if err != nil {
		return "", fmt.Errorf("abs repo path: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(absPath); err == nil {
		return resolved, nil
	}
	parent := filepath.Dir(absPath)
	if resolvedParent, err := filepath.EvalSymlinks(parent); err == nil {
		return filepath.Join(resolvedParent, filepath.Base(absPath)), nil
	}
	return filepath.Clean(absPath), nil
}

func runGitCommand(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), workspaceCommandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), ctx.Err())
		}
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = strings.TrimSpace(stdout.String())
		}
		if message == "" {
			message = err.Error()
		}
		return "", fmt.Errorf("%s", message)
	}
	return strings.TrimSpace(stdout.String()), nil
}

func emptyFallback(value string, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func CheckPaths(req CheckPathsRequest) (*CheckPathsResult, error) {
	result := &CheckPathsResult{
		Paths: make(map[string]WorkspacePathState),
	}
	for _, path := range req.Paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		state := WorkspacePathState{}
		info, err := os.Stat(path)
		if err != nil {
			if !os.IsNotExist(err) {
				state.Error = err.Error()
			}
			result.Paths[path] = state
			continue
		}
		state.Exists = true
		state.IsDir = info.IsDir()
		state.IsFile = info.Mode().IsRegular()
		result.Paths[path] = state
	}
	return result, nil
}
