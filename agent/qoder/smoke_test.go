package qoder

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestAgentSessionSmoke_UsesManagedWorkspaceAndExtraDirs(t *testing.T) {
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

	argsFile := filepath.Join(baseDir, "qoder-args.txt")
	cwdFile := filepath.Join(baseDir, "qoder-cwd.txt")
	script := "#!/bin/sh\n" +
		"pwd > \"$QODER_CWD_FILE\"\n" +
		"printf '%s\\n' \"$@\" > \"$QODER_ARGS_FILE\"\n" +
		"printf '%s\\n' '{\"type\":\"result\",\"session_id\":\"qoder-smoke\",\"done\":true,\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"ok\"}]}}'\n"
	scriptPath := filepath.Join(binDir, "qodercli")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake qodercli: %v", err)
	}

	t.Setenv("QODER_ARGS_FILE", argsFile)
	t.Setenv("QODER_CWD_FILE", cwdFile)
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

	if err := sess.Send("hello", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}

	waitForQoderResult(t, sess.Events())
	args := readQoderArgs(t, argsFile)
	var workspaces []string
	for i := 0; i < len(args); i++ {
		if args[i] == "-w" && i+1 < len(args) {
			workspaces = append(workspaces, args[i+1])
		}
	}
	if len(workspaces) != 2 || workspaces[0] != repoDir || workspaces[1] != managedDir {
		t.Fatalf("workspace args = %#v, want [%q, %q]", workspaces, repoDir, managedDir)
	}
	if got := strings.TrimSpace(readQoderText(t, cwdFile)); canonicalQoderPath(got) != canonicalQoderPath(managedDir) {
		t.Fatalf("cwd = %q, want %q", got, managedDir)
	}
}

func waitForQoderResult(t *testing.T, events <-chan core.Event) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case evt := <-events:
			if evt.Type == core.EventError {
				t.Fatalf("unexpected error event: %v", evt.Error)
			}
			if evt.Type == core.EventResult && evt.Done {
				return
			}
		case <-timeout:
			t.Fatal("timed out waiting for result")
		}
	}
}

func readQoderArgs(t *testing.T, path string) []string {
	t.Helper()
	waitForQoderArtifact(t, path)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read args file: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func readQoderText(t *testing.T, path string) string {
	t.Helper()
	waitForQoderArtifact(t, path)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read text file: %v", err)
	}
	return string(data)
}

func waitForQoderArtifact(t *testing.T, path string) {
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

func canonicalQoderPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}
