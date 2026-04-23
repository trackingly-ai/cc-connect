package core

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func singleTargetUpgradeConfig(name, strategy, versionCmd, updateCmd string) AgentUpgradeConfig {
	cfg := DefaultAgentUpgradeConfig()
	cfg.Enabled = true
	cfg.Policy = "idle_only"
	cfg.Interval = 10 * time.Millisecond
	for targetName, target := range cfg.Targets {
		target.Enabled = false
		cfg.Targets[targetName] = target
	}
	cfg.Targets[name] = AgentUpgradeTargetConfig{
		Enabled:        true,
		Strategy:       strategy,
		VersionCommand: versionCmd,
		UpdateCommand:  updateCmd,
	}
	return cfg
}

func waitForLoopExit(t *testing.T, mgr *AgentUpgradeManager) {
	t.Helper()
	select {
	case <-mgr.loopDone:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("timed out waiting for agent upgrade loop to stop")
	}
}

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

func TestAgentUpgradeManagerAuthorizeRun(t *testing.T) {
	mgr := NewAgentUpgradeManager(AgentUpgradeConfig{
		Enabled:        true,
		RunEnabled:     true,
		AllowedUserIDs: []string{"ou_admin"},
	})
	if err := mgr.AuthorizeRun("ou_admin"); err != nil {
		t.Fatalf("AuthorizeRun(allowed): %v", err)
	}
	if err := mgr.AuthorizeRun("ou_other"); err == nil {
		t.Fatal("expected unauthorized user to be rejected")
	}
}

func TestAgentUpgradeManagerRunCustomStrategy(t *testing.T) {
	mgr := NewAgentUpgradeManager(AgentUpgradeConfig{
		Enabled: true,
		Targets: map[string]AgentUpgradeTargetConfig{
			"codex": {
				Enabled:        true,
				Strategy:       "custom",
				VersionCommand: "codex --version",
				UpdateCommand:  "custom-codex-upgrade",
			},
		},
	})
	versions := map[string]string{"codex --version": "codex 1.0.0"}
	mgr.SetCommandRunner(func(_ context.Context, cmd string) (string, error) {
		switch cmd {
		case "codex --version":
			return versions[cmd], nil
		case "custom-codex-upgrade":
			versions["codex --version"] = "codex 1.1.0"
			return "custom updated", nil
		default:
			return "", fmt.Errorf("unexpected command %q", cmd)
		}
	})

	results, err := mgr.Run(context.Background(), "codex")
	if err != nil {
		t.Fatalf("Run(codex): %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if results[0].Strategy != "custom" || !results[0].Changed {
		t.Fatalf("result = %#v, want custom changed run", results[0])
	}
}

func TestAgentUpgradeManagerApplyConfigUnknownStrategyDisablesTarget(t *testing.T) {
	mgr := NewAgentUpgradeManager(AgentUpgradeConfig{Enabled: true})
	mgr.ApplyConfig(AgentUpgradeConfig{
		Enabled: true,
		Targets: map[string]AgentUpgradeTargetConfig{
			"codex": {Enabled: true, Strategy: "yolo"},
		},
	})

	cfg := mgr.Snapshot()
	if got := cfg.Targets["codex"].Strategy; got != "disabled" {
		t.Fatalf("codex strategy = %q, want disabled", got)
	}
}

func TestAgentUpgradeManagerAutoCheckIdleOnlyRunsUpgrades(t *testing.T) {
	mgr := NewAgentUpgradeManager(AgentUpgradeConfig{
		Enabled:  true,
		Policy:   "idle_only",
		Interval: time.Hour,
	})
	versions := map[string]string{
		"claude --version":   "claude 1.0.0",
		"codex --version":    "codex 0.1.0",
		"gemini --version":   "gemini 0.2.0",
		"qodercli --version": "qoder 0.3.0",
	}
	mgr.SetBusyCountFunc(func(string) int { return 0 })
	mgr.SetCommandRunner(func(_ context.Context, cmd string) (string, error) {
		switch cmd {
		case "claude --version", "codex --version", "gemini --version", "qodercli --version":
			return versions[cmd], nil
		case "claude update":
			versions["claude --version"] = "claude 1.1.0"
			return "updated", nil
		case "qodercli update":
			return "already up to date", nil
		default:
			return "", fmt.Errorf("unexpected command %q", cmd)
		}
	})

	results, err := mgr.AutoCheck(context.Background())
	if err != nil {
		t.Fatalf("AutoCheck: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected idle_only auto-check to return run results")
	}
}

func TestAgentUpgradeManagerAutoCheckOffReturnsNil(t *testing.T) {
	mgr := NewAgentUpgradeManager(AgentUpgradeConfig{
		Enabled: true,
		Policy:  "off",
	})
	results, err := mgr.AutoCheck(context.Background())
	if err != nil {
		t.Fatalf("AutoCheck(off): %v", err)
	}
	if results != nil {
		t.Fatalf("results = %#v, want nil", results)
	}
}

func TestAgentUpgradeManagerLoopWakeStartsChecksFromOff(t *testing.T) {
	cfg := singleTargetUpgradeConfig("codex", "custom", "codex --version", "custom-codex-upgrade")
	cfg.Policy = "off"

	mgr := NewAgentUpgradeManager(cfg)
	triggered := make(chan struct{}, 1)
	mgr.SetCommandRunner(func(_ context.Context, cmd string) (string, error) {
		switch cmd {
		case "codex --version":
			return "codex 1.0.0", nil
		case "custom-codex-upgrade":
			select {
			case triggered <- struct{}{}:
			default:
			}
			return "updated", nil
		default:
			return "", fmt.Errorf("unexpected command %q", cmd)
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	mgr.Start(ctx)
	defer func() {
		cancel()
		waitForLoopExit(t, mgr)
	}()

	select {
	case <-triggered:
		t.Fatal("unexpected auto-upgrade while policy=off")
	case <-time.After(40 * time.Millisecond):
	}

	cfg.Policy = "idle_only"
	cfg.Interval = 10 * time.Millisecond
	mgr.ApplyConfig(cfg)

	select {
	case <-triggered:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("expected wake-triggered auto-check after enabling idle_only")
	}
}

func TestAgentUpgradeManagerLoopRecalculatesNextRunAfterReload(t *testing.T) {
	cfg := singleTargetUpgradeConfig("codex", "custom", "codex --version", "custom-codex-upgrade")
	mgr := NewAgentUpgradeManager(cfg)

	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var mu sync.Mutex
	runCount := 0
	mgr.SetCommandRunner(func(_ context.Context, cmd string) (string, error) {
		switch cmd {
		case "codex --version":
			return "codex 1.0.0", nil
		case "custom-codex-upgrade":
			mu.Lock()
			runCount++
			current := runCount
			mu.Unlock()
			if current == 1 {
				select {
				case started <- struct{}{}:
				default:
				}
				<-release
			}
			return "updated", nil
		default:
			return "", fmt.Errorf("unexpected command %q", cmd)
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	mgr.Start(ctx)
	defer func() {
		cancel()
		waitForLoopExit(t, mgr)
	}()

	select {
	case <-started:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("expected first auto-check to start")
	}

	cfg.Interval = time.Hour
	mgr.ApplyConfig(cfg)
	close(release)

	deadline := time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(deadline) {
		lastRun, nextRun := mgr.ScheduleState()
		if !lastRun.IsZero() && !nextRun.IsZero() {
			if got := nextRun.Sub(lastRun); got < 59*time.Minute {
				t.Fatalf("next run delta = %s, want reload-adjusted interval close to 1h", got)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	lastRun, nextRun := mgr.ScheduleState()
	t.Fatalf("schedule state not updated after reload, last=%v next=%v", lastRun, nextRun)
}

func TestDetectClaudeCodeUpgradeRoutePrefersNativeInstall(t *testing.T) {
	runner := func(_ context.Context, cmd string) (string, error) {
		switch cmd {
		case "command -v claude":
			return "/Users/edward/.local/bin/claude\n", nil
		default:
			return "", fmt.Errorf("unexpected command %q", cmd)
		}
	}

	source, updateCmd, blockedReason := detectClaudeCodeUpgradeRoute(context.Background(), runner)
	if source != "native" || updateCmd != "claude update" || blockedReason != "" {
		t.Fatalf("route = (%q, %q, %q), want native/claude update/no block", source, updateCmd, blockedReason)
	}
}

func TestDetectClaudeCodeUpgradeRouteFallsBackToHomebrew(t *testing.T) {
	runner := func(_ context.Context, cmd string) (string, error) {
		switch cmd {
		case "command -v claude":
			return "/opt/homebrew/bin/claude\n", nil
		case "brew list --cask claude-code@latest >/dev/null && printf ok":
			return "ok", nil
		case "brew list --cask claude-code >/dev/null && printf ok":
			return "", fmt.Errorf("not installed")
		default:
			return "", fmt.Errorf("unexpected command %q", cmd)
		}
	}

	source, updateCmd, blockedReason := detectClaudeCodeUpgradeRoute(context.Background(), runner)
	if source != "brew" || updateCmd != "brew upgrade claude-code@latest" || blockedReason != "" {
		t.Fatalf("route = (%q, %q, %q), want brew/claude-code@latest/no block", source, updateCmd, blockedReason)
	}
}

func TestDetectClaudeCodeUpgradeRouteMarksLinuxPackageManagersManualOnly(t *testing.T) {
	runner := func(_ context.Context, cmd string) (string, error) {
		switch cmd {
		case "command -v claude":
			return "/usr/bin/claude\n", nil
		case "brew list --cask claude-code@latest >/dev/null && printf ok",
			"brew list --cask claude-code >/dev/null && printf ok",
			"npm -g ls @anthropic-ai/claude-code --depth=0 >/dev/null && printf ok":
			return "", fmt.Errorf("not installed")
		case "dpkg -s claude-code >/dev/null && printf ok":
			return "ok", nil
		default:
			return "", fmt.Errorf("unexpected command %q", cmd)
		}
	}

	source, updateCmd, blockedReason := detectClaudeCodeUpgradeRoute(context.Background(), runner)
	if source != "apt" || updateCmd == "" || blockedReason == "" {
		t.Fatalf("route = (%q, %q, %q), want manual apt fallback", source, updateCmd, blockedReason)
	}
}
