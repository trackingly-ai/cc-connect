package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestSend_HandlesLargeJSONLines(t *testing.T) {
	workDir := t.TempDir()
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}

	largeText := strings.Repeat("x", 11*1024*1024)
	encodedText, err := json.Marshal(largeText)
	if err != nil {
		t.Fatalf("marshal large text: %v", err)
	}

	payload := strings.Join([]string{
		`{"type":"thread.started","thread_id":"thread-large"}`,
		`{"type":"item.completed","item":{"type":"agent_message","content":[{"type":"output_text","text":` + string(encodedText) + `}]}}`,
		`{"type":"turn.completed"}`,
	}, "\n") + "\n"

	payloadFile := filepath.Join(workDir, "payload.jsonl")
	if err := os.WriteFile(payloadFile, []byte(payload), 0o644); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	script := "#!/bin/sh\ncat \"$CODEX_PAYLOAD_FILE\"\n"
	scriptPath := filepath.Join(binDir, "codex")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}

	t.Setenv("CODEX_PAYLOAD_FILE", payloadFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cs, err := newCodexSession(context.Background(), workDir, "", "", "", nil)
	if err != nil {
		t.Fatalf("newCodexSession: %v", err)
	}
	defer func() {
		if err := cs.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}()

	if err := cs.Send("hello", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}

	var gotTextLen int
	var gotResult bool
	timeout := time.After(5 * time.Second)

	for !gotResult {
		select {
		case evt := <-cs.Events():
			if evt.Type == core.EventError {
				t.Fatalf("unexpected error event: %v", evt.Error)
			}
			if evt.Type == core.EventText {
				gotTextLen = len(evt.Content)
			}
			if evt.Type == core.EventResult && evt.Done {
				gotResult = true
			}
		case <-timeout:
			t.Fatal("timed out waiting for large JSON line events")
		}
	}

	if gotTextLen != len(largeText) {
		t.Fatalf("text len = %d, want %d", gotTextLen, len(largeText))
	}
	if got := cs.CurrentSessionID(); got != "thread-large" {
		t.Fatalf("CurrentSessionID() = %q, want thread-large", got)
	}
}

func TestClose_KillsProcessGroupAfterTimeout(t *testing.T) {
	workDir := t.TempDir()
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}

	script := `#!/bin/sh
echo '{"type":"thread.started","thread_id":"thread-stuck"}'
sh -c 'trap "" TERM INT; while :; do sleep 1; done' &
child=$!
wait $child
`
	scriptPath := filepath.Join(binDir, "codex")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cs, err := newCodexSession(context.Background(), workDir, "", "", "", nil)
	if err != nil {
		t.Fatalf("newCodexSession: %v", err)
	}
	cs.closeTimeout = 200 * time.Millisecond

	if err := cs.Send("hello", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pid, _ := cs.currentProcessInfo()
		if pid != 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid, _ := cs.currentProcessInfo(); pid == 0 {
		t.Fatal("expected codex process to start")
	}

	start := time.Now()
	if err := cs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Close took too long: %v", elapsed)
	}

	select {
	case _, ok := <-cs.Events():
		if ok {
			t.Fatal("expected events channel to be closed after forced kill")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for events channel to close")
	}
}

func TestSend_PassesImagesToCodexCLI(t *testing.T) {
	workDir := t.TempDir()
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}

	argsFile := filepath.Join(workDir, "args.txt")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$CODEX_ARGS_FILE"
echo '{"type":"thread.started","thread_id":"thread-images"}'
echo '{"type":"turn.completed"}'
`
	scriptPath := filepath.Join(binDir, "codex")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}

	t.Setenv("CODEX_ARGS_FILE", argsFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cs, err := newCodexSession(context.Background(), workDir, "", "", "", nil)
	if err != nil {
		t.Fatalf("newCodexSession: %v", err)
	}
	defer func() {
		if err := cs.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}()

	if err := cs.Send("", []core.ImageAttachment{{
		MimeType: "image/png",
		Data:     []byte("pngdata"),
		FileName: "screen shot.png",
	}}, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}

	timeout := time.After(5 * time.Second)
	for {
		select {
		case evt := <-cs.Events():
			if evt.Type == core.EventError {
				t.Fatalf("unexpected error event: %v", evt.Error)
			}
			if evt.Type == core.EventResult && evt.Done {
				goto done
			}
		case <-timeout:
			t.Fatal("timed out waiting for turn completion")
		}
	}

done:
	argsBytes, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("ReadFile args: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(argsBytes)), "\n")
	imageIdx := -1
	for i, arg := range args {
		if arg == "--image" {
			imageIdx = i
			break
		}
	}
	if imageIdx < 0 || imageIdx+1 >= len(args) {
		t.Fatalf("args = %q, want --image <path>", string(argsBytes))
	}
	if !strings.HasPrefix(args[imageIdx+1], filepath.Join(workDir, ".cc-connect", "images")+string(os.PathSeparator)) {
		t.Fatalf("image arg = %q, want saved image path in work dir", args[imageIdx+1])
	}
	if got := args[len(args)-1]; got != "-" {
		t.Fatalf("last arg = %q, want stdin marker '-'", got)
	}
}

func TestSend_UsesStdinForMultilinePrompt(t *testing.T) {
	workDir := t.TempDir()
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}

	argsFile := filepath.Join(workDir, "args.txt")
	stdinFile := filepath.Join(workDir, "stdin.txt")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > \"$CODEX_ARGS_FILE\"\n" +
		"cat > \"$CODEX_STDIN_FILE\"\n" +
		"printf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"thread-stdin\"}'\n" +
		"printf '%s\\n' '{\"type\":\"turn.completed\"}'\n"
	scriptPath := filepath.Join(binDir, "codex")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}

	t.Setenv("CODEX_ARGS_FILE", argsFile)
	t.Setenv("CODEX_STDIN_FILE", stdinFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cs, err := newCodexSession(context.Background(), workDir, "", "", "", nil)
	if err != nil {
		t.Fatalf("newCodexSession: %v", err)
	}
	defer func() {
		if err := cs.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}()

	prompt := "line1\nline2"
	if err := cs.Send(prompt, nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}

	argsBytes, err := waitForFileContent(argsFile)
	if err != nil {
		t.Fatalf("wait for args: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(argsBytes)), "\n")
	if len(args) == 0 || args[len(args)-1] != "-" {
		t.Fatalf("args last element = %q, want stdin marker '-'; args=%v", args[len(args)-1], args)
	}
	foundJSON := false
	for _, arg := range args {
		if arg == "--json" {
			foundJSON = true
			break
		}
	}
	if !foundJSON {
		t.Fatalf("args missing --json: %v", args)
	}

	data, err := waitForFileContent(stdinFile)
	if err != nil {
		t.Fatalf("wait for stdin: %v", err)
	}
	if !strings.Contains(string(data), prompt) {
		t.Fatalf("stdin content = %q, want to contain %q", string(data), prompt)
	}
}

func waitForFileContent(path string) ([]byte, error) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && len(data) > 0 {
			return data, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil, os.ErrNotExist
}
