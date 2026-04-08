package claudecode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAgentStartSession_UsesManagedWorkspaceAndExtraDirs(t *testing.T) {
	baseDir := t.TempDir()
	repoDir := filepath.Join(baseDir, "repo")
	managedDir := filepath.Join(baseDir, "managed")
	binDir := filepath.Join(baseDir, "bin")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	if err := os.MkdirAll(managedDir, 0o755); err != nil {
		t.Fatalf("mkdir managed: %v", err)
	}
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}

	argsFile := filepath.Join(baseDir, "claude-args.txt")
	cwdFile := filepath.Join(baseDir, "claude-cwd.txt")
	script := "#!/bin/sh\n" +
		"pwd > \"$CLAUDE_CWD_FILE\"\n" +
		"printf '%s\\n' \"$@\" > \"$CLAUDE_ARGS_FILE\"\n" +
		"printf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"claude-smoke\"}'\n" +
		"sleep 5\n"
	scriptPath := filepath.Join(binDir, "claude")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}

	t.Setenv("CLAUDE_ARGS_FILE", argsFile)
	t.Setenv("CLAUDE_CWD_FILE", cwdFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	agentRaw, err := New(map[string]any{"work_dir": repoDir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	agent := agentRaw.(*Agent)
	agent.SetSessionEnv([]string{
		"CC_WORKTREE_PATH=" + managedDir,
		"CC_EXTRA_WORK_DIRS=" + repoDir,
	})

	sess, err := agent.StartSession(context.Background(), "")
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	defer func() { _ = sess.Close() }()

	waitForFile(t, argsFile)
	waitForFile(t, cwdFile)

	argsBytes, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(argsBytes)), "\n")
	if !containsArgPair(args, "--add-dir", repoDir) {
		t.Fatalf("args missing --add-dir %q: %#v", repoDir, args)
	}

	cwdBytes, err := os.ReadFile(cwdFile)
	if err != nil {
		t.Fatalf("read cwd: %v", err)
	}
	if got := strings.TrimSpace(string(cwdBytes)); canonicalPath(got) != canonicalPath(managedDir) {
		t.Fatalf("cwd = %q, want %q", got, managedDir)
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func containsArgPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func canonicalPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}
