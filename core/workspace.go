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

func SetupWorkspace(
	repoPath string,
	baseBranch string,
	branchName string,
	worktreePath string,
) error {
	repoPath = strings.TrimSpace(repoPath)
	baseBranch = strings.TrimSpace(baseBranch)
	branchName = strings.TrimSpace(branchName)
	worktreePath = strings.TrimSpace(worktreePath)

	if repoPath == "" {
		return fmt.Errorf("repo_path is required")
	}
	if baseBranch == "" {
		return fmt.Errorf("base_branch is required")
	}
	if branchName == "" {
		return fmt.Errorf("branch_name is required")
	}
	if worktreePath == "" {
		return fmt.Errorf("worktree_path is required")
	}
	return withWorkspacePathLock(worktreePath, func() error {
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
				args = append(args, worktreePath, branchName)
			} else {
				args = append(args, "-b", branchName, worktreePath, baseBranch)
			}
			if _, err := runGit(repoPath, args...); err != nil {
				return fmt.Errorf("git worktree add: %w", err)
			}
			return nil
		})
	})
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

	for _, artifactPath := range artifactPaths {
		artifactPath = strings.TrimSpace(artifactPath)
		if artifactPath == "" {
			continue
		}
		if info, err := os.Stat(filepath.Join(worktreePath, artifactPath)); err != nil {
			return nil, fmt.Errorf("stat artifact path %q: %w", artifactPath, err)
		} else if info.IsDir() {
			continue
		}
	}

	createdCommit := false
	if len(statusEntries) > 0 {
		args := append([]string{"add", "--"}, artifactPaths...)
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

	for _, artifactPath := range artifactPaths {
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
