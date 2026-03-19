package core

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func WriteRepoFile(repoPath string, relativePath string, content string) (string, error) {
	repoPath = strings.TrimSpace(repoPath)
	relativePath = strings.TrimSpace(relativePath)
	if repoPath == "" {
		return "", fmt.Errorf("repo_path is required")
	}
	if relativePath == "" {
		return "", fmt.Errorf("relative_path is required")
	}

	repoInfo, err := os.Stat(repoPath)
	if err != nil {
		return "", fmt.Errorf("stat repo_path: %w", err)
	}
	if !repoInfo.IsDir() {
		return "", fmt.Errorf("repo_path is not a directory: %s", repoPath)
	}

	gitPath := filepath.Join(repoPath, ".git")
	if _, err := os.Stat(gitPath); err != nil {
		return "", fmt.Errorf("repo_path does not point to a git repository: %w", err)
	}

	if filepath.IsAbs(relativePath) {
		return "", fmt.Errorf("relative_path must be relative to repo_path")
	}
	cleanRelativePath := filepath.Clean(relativePath)
	if cleanRelativePath == "." {
		return "", fmt.Errorf("relative_path must not point to repo root")
	}
	if cleanRelativePath == ".." || strings.HasPrefix(cleanRelativePath, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("relative_path must stay within repo_path")
	}

	targetPath := filepath.Join(repoPath, cleanRelativePath)
	rel, err := filepath.Rel(repoPath, targetPath)
	if err != nil {
		return "", fmt.Errorf("resolve repo-relative path: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("relative_path must stay within repo_path")
	}

	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return "", fmt.Errorf("create parent directories: %w", err)
	}
	if err := os.WriteFile(targetPath, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("write repo file: %w", err)
	}

	return targetPath, nil
}
