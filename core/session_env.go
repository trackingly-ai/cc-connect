package core

import "strings"

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
