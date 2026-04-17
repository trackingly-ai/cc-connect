package qoder

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestQoderSession(t *testing.T) {
	if os.Getenv("QODER_INTEGRATION") == "" {
		t.Skip("set QODER_INTEGRATION=1 to run")
	}

	agent, err := New(map[string]any{
		"work_dir": "/tmp",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	sess, err := agent.StartSession(context.Background(), "")
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	defer sess.Close()

	if err := sess.Send("say hello in one word", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}

	timeout := time.After(30 * time.Second)
	var gotResult bool
	for !gotResult {
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				t.Fatal("events channel closed prematurely")
			}
			switch ev.Type {
			case core.EventText:
				fmt.Printf("[TEXT] %s\n", ev.Content)
			case core.EventToolUse:
				fmt.Printf("[TOOL] %s: %s\n", ev.ToolName, ev.ToolInput)
			case core.EventResult:
				fmt.Printf("[RESULT] sid=%s content=%s\n", ev.SessionID, ev.Content)
				gotResult = true
			case core.EventError:
				t.Fatalf("[ERROR] %v", ev.Error)
			default:
				fmt.Printf("[%s] %s\n", ev.Type, ev.Content)
			}
		case <-timeout:
			t.Fatal("timeout waiting for result")
		}
	}

	sid := sess.CurrentSessionID()
	if sid == "" {
		t.Error("expected a session ID from init event")
	}
	fmt.Printf("Session ID: %s\n", sid)
}

func TestSkillDirsUseQoderPaths(t *testing.T) {
	agent, err := New(map[string]any{"work_dir": "/tmp/demo"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	q, ok := agent.(*Agent)
	if !ok {
		t.Fatalf("unexpected agent type %T", agent)
	}
	dirs := q.SkillDirs()
	if len(dirs) < 2 {
		t.Fatalf("expected qoder + legacy claude skill dirs, got %#v", dirs)
	}
	if got := dirs[0]; got != "/tmp/demo/.qoder/skills" {
		t.Fatalf("first skill dir = %q, want %q", got, "/tmp/demo/.qoder/skills")
	}
	if got := dirs[1]; got != "/tmp/demo/.claude/skills" {
		t.Fatalf("second skill dir = %q, want %q", got, "/tmp/demo/.claude/skills")
	}
}

func TestNew_WarnsWhenReasoningOptionsAreIgnored(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	if _, err := New(map[string]any{
		"work_dir":        "/tmp/demo",
		"reasoning_level": "high",
		"thinking_budget": int64(1024),
	}); err != nil {
		t.Fatalf("New: %v", err)
	}

	logs := buf.String()
	if !strings.Contains(logs, "reasoning_level is ignored") {
		t.Fatalf("logs = %q, want reasoning_level warning", logs)
	}
	if !strings.Contains(logs, "thinking_budget is ignored") {
		t.Fatalf("logs = %q, want thinking_budget warning", logs)
	}
}
