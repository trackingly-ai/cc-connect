package core

import (
	"bytes"
	"context"
	"fmt"
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
			if strings.TrimSpace(existingBranch) != "" {
				return fmt.Errorf("workspace branch already exists: %s", branchName)
			}
			if err := os.MkdirAll(filepath.Dir(worktreePath), 0o755); err != nil {
				return fmt.Errorf("create worktree parent dir: %w", err)
			}

			if _, err := runGit(
				repoPath,
				"worktree",
				"add",
				"--checkout",
				"-b",
				branchName,
				worktreePath,
				baseBranch,
			); err != nil {
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
