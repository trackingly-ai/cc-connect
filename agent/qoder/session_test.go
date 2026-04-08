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

func TestSend_PassesAttachmentsToQoderCLI(t *testing.T) {
	workDir := t.TempDir()
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}

	argsFile := filepath.Join(workDir, "args.txt")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$QODER_ARGS_FILE"
echo '{"type":"result","session_id":"qoder-session-1","done":true,"message":{"content":[{"type":"text","text":"ok"}]}}'
`
	scriptPath := filepath.Join(binDir, "qodercli")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake qodercli: %v", err)
	}

	t.Setenv("QODER_ARGS_FILE", argsFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	qs, err := newQoderSession(context.Background(), workDir, nil, "", "", "", nil)
	if err != nil {
		t.Fatalf("newQoderSession: %v", err)
	}
	defer func() { _ = qs.Close() }()

	err = qs.Send("", []core.ImageAttachment{{
		MimeType: "image/png",
		Data:     []byte("pngdata"),
		FileName: "screen.png",
	}}, []core.FileAttachment{{
		MimeType: "video/mp4",
		Data:     []byte("mp4data"),
		FileName: "clip.mp4",
	}})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	timeout := time.After(5 * time.Second)
	for {
		select {
		case evt := <-qs.Events():
			if evt.Type == core.EventError {
				t.Fatalf("unexpected error event: %v", evt.Error)
			}
			if evt.Type == core.EventResult && evt.Done {
				goto done
			}
		case <-timeout:
			t.Fatal("timed out waiting for result")
		}
	}

done:
	argsBytes, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("ReadFile args: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(argsBytes)), "\n")
	var attachments []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--attachment" && i+1 < len(args) {
			attachments = append(attachments, args[i+1])
		}
	}
	if len(attachments) != 2 {
		t.Fatalf("attachments = %#v, want 2 attachment args", attachments)
	}
	for _, attachment := range attachments {
		if !strings.HasPrefix(attachment, filepath.Join(workDir, ".cc-connect")+string(os.PathSeparator)) {
			t.Fatalf("attachment path = %q, want saved path in work dir", attachment)
		}
	}
	if got := args[1]; got != "Please analyze the attached file(s)." {
		t.Fatalf("prompt = %q, want fallback attachment prompt", got)
	}
}

func TestSend_PlacesExtraWorkspacesBeforeManagedWorkspace(t *testing.T) {
	workDir := t.TempDir()
	primaryDir := filepath.Join(workDir, "repo")
	if err := os.MkdirAll(primaryDir, 0o755); err != nil {
		t.Fatalf("mkdir primary: %v", err)
	}
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	argsFile := filepath.Join(workDir, "args.txt")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$QODER_ARGS_FILE"
echo '{"type":"result","session_id":"qoder-session-2","done":true,"message":{"content":[{"type":"text","text":"ok"}]}}'
`
	scriptPath := filepath.Join(binDir, "qodercli")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake qodercli: %v", err)
	}

	t.Setenv("QODER_ARGS_FILE", argsFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	qs, err := newQoderSession(context.Background(), workDir, []string{primaryDir}, "", "", "", nil)
	if err != nil {
		t.Fatalf("newQoderSession: %v", err)
	}
	defer func() { _ = qs.Close() }()

	if err := qs.Send("hello", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}

	timeout := time.After(5 * time.Second)
	for {
		select {
		case evt := <-qs.Events():
			if evt.Type == core.EventError {
				t.Fatalf("unexpected error event: %v", evt.Error)
			}
			if evt.Type == core.EventResult && evt.Done {
				goto done
			}
		case <-timeout:
			t.Fatal("timed out waiting for result")
		}
	}

done:
	argsBytes, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("ReadFile args: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(argsBytes)), "\n")
	var workspaces []string
	for i := 0; i < len(args); i++ {
		if args[i] == "-w" && i+1 < len(args) {
			workspaces = append(workspaces, args[i+1])
		}
	}
	if len(workspaces) != 2 {
		t.Fatalf("workspace args = %#v, want 2", workspaces)
	}
	if workspaces[0] != primaryDir || workspaces[1] != workDir {
		t.Fatalf("workspace args = %#v, want [%q, %q]", workspaces, primaryDir, workDir)
	}
}

func TestSend_DeduplicatesNormalizedWorkspacePaths(t *testing.T) {
	workDir := filepath.Join(t.TempDir(), "managed")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir managed: %v", err)
	}
	binDir := filepath.Join(filepath.Dir(workDir), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	argsFile := filepath.Join(filepath.Dir(workDir), "args.txt")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$QODER_ARGS_FILE"
echo '{"type":"result","session_id":"qoder-session-3","done":true,"message":{"content":[{"type":"text","text":"ok"}]}}'
`
	scriptPath := filepath.Join(binDir, "qodercli")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake qodercli: %v", err)
	}

	t.Setenv("QODER_ARGS_FILE", argsFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	qs, err := newQoderSession(
		context.Background(),
		workDir,
		[]string{workDir + string(os.PathSeparator), workDir},
		"",
		"",
		"",
		nil,
	)
	if err != nil {
		t.Fatalf("newQoderSession: %v", err)
	}
	defer func() { _ = qs.Close() }()

	if err := qs.Send("hello", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}

	timeout := time.After(5 * time.Second)
	for {
		select {
		case evt := <-qs.Events():
			if evt.Type == core.EventError {
				t.Fatalf("unexpected error event: %v", evt.Error)
			}
			if evt.Type == core.EventResult && evt.Done {
				goto done
			}
		case <-timeout:
			t.Fatal("timed out waiting for result")
		}
	}

done:
	argsBytes, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("ReadFile args: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(argsBytes)), "\n")
	var workspaces []string
	for i := 0; i < len(args); i++ {
		if args[i] == "-w" && i+1 < len(args) {
			workspaces = append(workspaces, args[i+1])
		}
	}
	if len(workspaces) != 1 || workspaces[0] != workDir {
		t.Fatalf("workspace args = %#v, want [%q]", workspaces, workDir)
	}
}
