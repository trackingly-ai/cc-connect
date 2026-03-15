package core

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const workspaceCommandTimeout = 30 * time.Second

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
	if _, err := os.Stat(worktreePath); err == nil {
		return fmt.Errorf("worktree path already exists: %s", worktreePath)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat worktree path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(worktreePath), 0o755); err != nil {
		return fmt.Errorf("create worktree parent dir: %w", err)
	}

	if _, err := runGit(
		repoPath,
		"worktree",
		"add",
		"--checkout",
		"-B",
		branchName,
		worktreePath,
		baseBranch,
	); err != nil {
		return fmt.Errorf("git worktree add: %w", err)
	}
	return nil
}

func CleanupWorkspace(worktreePath string) error {
	worktreePath = strings.TrimSpace(worktreePath)
	if worktreePath == "" {
		return fmt.Errorf("worktree_path is required")
	}
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
	if _, err := runGit(repoPath, "worktree", "remove", "--force", worktreePath); err != nil {
		return fmt.Errorf("git worktree remove: %w", err)
	}
	branchName = strings.TrimSpace(branchName)
	if branchName != "" && branchName != "HEAD" {
		if _, err := runGit(repoPath, "branch", "-D", branchName); err != nil {
			return fmt.Errorf("delete workspace branch: %w", err)
		}
	}
	return nil
}

func runGit(dir string, args ...string) (string, error) {
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
