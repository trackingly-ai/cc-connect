package gemini

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

	argsFile := filepath.Join(baseDir, "gemini-args.txt")
	cwdFile := filepath.Join(baseDir, "gemini-cwd.txt")
	script := "#!/bin/sh\n" +
		"pwd > \"$GEMINI_CWD_FILE\"\n" +
		"printf '%s\\n' \"$@\" > \"$GEMINI_ARGS_FILE\"\n" +
		"printf '%s\\n' '{\"type\":\"init\",\"session_id\":\"gemini-smoke\",\"model\":\"gemini-test\"}'\n" +
		"printf '%s\\n' '{\"type\":\"result\",\"status\":\"success\"}'\n"
	scriptPath := filepath.Join(binDir, "gemini")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gemini: %v", err)
	}

	t.Setenv("GEMINI_ARGS_FILE", argsFile)
	t.Setenv("GEMINI_CWD_FILE", cwdFile)
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

	waitForGeminiResult(t, sess.Events())
	args := readGeminiArgs(t, argsFile)
	if !containsGeminiArgPair(args, "--include-directories", repoDir) {
		t.Fatalf("args missing --include-directories %q: %#v", repoDir, args)
	}
	if got := strings.TrimSpace(readGeminiText(t, cwdFile)); canonicalGeminiPath(got) != canonicalGeminiPath(managedDir) {
		t.Fatalf("cwd = %q, want %q", got, managedDir)
	}
}

func TestSkillDirsPreferAgentsAlias(t *testing.T) {
	a := &Agent{workDir: "/tmp/demo"}
	dirs := a.SkillDirs()
	if len(dirs) < 2 {
		t.Fatalf("expected multiple skill dirs, got %#v", dirs)
	}
	if got := dirs[0]; got != "/tmp/demo/.agents/skills" {
		t.Fatalf("first skill dir = %q, want %q", got, "/tmp/demo/.agents/skills")
	}
	if got := dirs[1]; got != "/tmp/demo/.gemini/skills" {
		t.Fatalf("second skill dir = %q, want %q", got, "/tmp/demo/.gemini/skills")
	}
}

func waitForGeminiResult(t *testing.T, events <-chan core.Event) {
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

func readGeminiArgs(t *testing.T, path string) []string {
	t.Helper()
	waitForGeminiArtifact(t, path)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read args file: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func readGeminiText(t *testing.T, path string) string {
	t.Helper()
	waitForGeminiArtifact(t, path)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read text file: %v", err)
	}
	return string(data)
}

func waitForGeminiArtifact(t *testing.T, path string) {
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

func containsGeminiArgPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func canonicalGeminiPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}
