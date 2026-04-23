package core

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestAgentUpgradeManagerStatuses(t *testing.T) {
	mgr := NewAgentUpgradeManager(AgentUpgradeConfig{Enabled: true})
	mgr.SetBusyCountFunc(func(name string) int {
		if name == "codex" {
			return 2
		}
		return 0
	})
	mgr.SetCommandRunner(func(_ context.Context, cmd string) (string, error) {
		return "ok:" + cmd, nil
	})

	statuses := mgr.Statuses(context.Background())
	if len(statuses) != 4 {
		t.Fatalf("len(statuses) = %d, want 4", len(statuses))
	}

	var codex *AgentUpgradeStatus
	var gemini *AgentUpgradeStatus
	for i := range statuses {
		switch statuses[i].Name {
		case "codex":
			codex = &statuses[i]
		case "gemini":
			gemini = &statuses[i]
		}
	}
	if codex == nil || gemini == nil {
		t.Fatalf("statuses missing codex/gemini: %#v", statuses)
	}
	if codex.BusySessions != 2 {
		t.Fatalf("codex busy = %d, want 2", codex.BusySessions)
	}
	if !strings.Contains(codex.Version, "codex --version") {
		t.Fatalf("codex version = %q", codex.Version)
	}
	if gemini.Actionable {
		t.Fatalf("gemini actionable = true, want false for observe strategy")
	}
}

func TestAgentUpgradeManagerRunAll(t *testing.T) {
	mgr := NewAgentUpgradeManager(AgentUpgradeConfig{Enabled: true, Timeout: time.Minute})
	mgr.SetBusyCountFunc(func(name string) int {
		if name == "codex" {
			return 1
		}
		return 0
	})

	versions := map[string]string{
		"claude --version":   "claude 1.0.0",
		"codex --version":    "codex 0.1.0",
		"gemini --version":   "gemini 0.2.0",
		"qodercli --version": "qoder 0.3.0",
	}
	updateRuns := make(map[string]int)
	mgr.SetCommandRunner(func(_ context.Context, cmd string) (string, error) {
		switch cmd {
		case "claude --version", "codex --version", "gemini --version", "qodercli --version":
			return versions[cmd], nil
		case "claude update":
			updateRuns[cmd]++
			versions["claude --version"] = "claude 1.1.0"
			return "updated claude", nil
		case "qodercli update":
			updateRuns[cmd]++
			return "already up to date", nil
		default:
			return "", fmt.Errorf("unexpected command %q", cmd)
		}
	})

	results, err := mgr.Run(context.Background(), "all")
	if err != nil {
		t.Fatalf("Run(all): %v", err)
	}
	if len(results) != 4 {
		t.Fatalf("len(results) = %d, want 4", len(results))
	}

	var claude, codex, gemini, qoder *AgentUpgradeRunResult
	for i := range results {
		switch results[i].Name {
		case "claudecode":
			claude = &results[i]
		case "codex":
			codex = &results[i]
		case "gemini":
			gemini = &results[i]
		case "qoder":
			qoder = &results[i]
		}
	}
	if claude == nil || codex == nil || gemini == nil || qoder == nil {
		t.Fatalf("unexpected results: %#v", results)
	}
	if !claude.Changed || claude.BeforeVersion != "claude 1.0.0" || claude.AfterVersion != "claude 1.1.0" {
		t.Fatalf("claude result = %#v", claude)
	}
	if !codex.Skipped || !strings.Contains(codex.Reason, "active session") {
		t.Fatalf("codex result = %#v, want busy skip", codex)
	}
	if !gemini.Skipped || gemini.Reason != "observe-only" {
		t.Fatalf("gemini result = %#v, want observe-only skip", gemini)
	}
	if qoder.Skipped || qoder.Err != nil || qoder.Changed {
		t.Fatalf("qoder result = %#v, want successful no-change update", qoder)
	}
	if updateRuns["claude update"] != 1 || updateRuns["qodercli update"] != 1 {
		t.Fatalf("updateRuns = %#v", updateRuns)
	}
}

func TestAgentUpgradeManagerRunDisabled(t *testing.T) {
	mgr := NewAgentUpgradeManager(AgentUpgradeConfig{Enabled: false})
	if _, err := mgr.Run(context.Background(), "all"); err == nil {
		t.Fatal("expected disabled error")
	}
}
