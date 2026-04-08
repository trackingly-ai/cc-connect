package core

import (
	"path/filepath"
	"strings"
)

// SessionWorkDirFromEnv returns a per-session workdir override when a job
// injects CC_WORKTREE_PATH, otherwise it preserves the agent's default workdir.
func SessionWorkDirFromEnv(env []string, defaultDir string) string {
	for i := len(env) - 1; i >= 0; i-- {
		entry := env[i]
		if !strings.HasPrefix(entry, "CC_WORKTREE_PATH=") {
			continue
		}
		workDir := strings.TrimSpace(strings.TrimPrefix(entry, "CC_WORKTREE_PATH="))
		if workDir != "" {
			return workDir
		}
		break
	}
	return defaultDir
}

// SessionExtraDirsFromEnv returns per-session extra work roots requested by the
// bridge. The value is a filepath.ListSeparator-joined list in
// CC_EXTRA_WORK_DIRS.
func SessionExtraDirsFromEnv(env []string) []string {
	for i := len(env) - 1; i >= 0; i-- {
		entry := env[i]
		if !strings.HasPrefix(entry, "CC_EXTRA_WORK_DIRS=") {
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(entry, "CC_EXTRA_WORK_DIRS="))
		if raw == "" {
			return nil
		}
		var dirs []string
		for _, dir := range strings.Split(raw, string(filepath.ListSeparator)) {
			dir = strings.TrimSpace(dir)
			if dir == "" {
				continue
			}
			dirs = append(dirs, dir)
		}
		return dirs
	}
	return nil
}
