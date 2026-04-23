package core

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"
)

const defaultAgentUpgradeTimeout = 20 * time.Minute

type AgentUpgradeTargetConfig struct {
	Enabled        bool
	Strategy       string
	VersionCommand string
	UpdateCommand  string
}

type AgentUpgradeConfig struct {
	Enabled bool
	Timeout time.Duration
	Targets map[string]AgentUpgradeTargetConfig
}

type AgentUpgradeStatus struct {
	Name          string
	Enabled       bool
	Strategy      string
	Actionable    bool
	BusySessions  int
	Version       string
	VersionErr    string
	UpdateCommand string
}

type AgentUpgradeRunResult struct {
	Name          string
	Strategy      string
	BusySessions  int
	Skipped       bool
	Reason        string
	BeforeVersion string
	AfterVersion  string
	Changed       bool
	Output        string
	Err           error
}

type AgentUpgradeCommandRunner func(ctx context.Context, cmd string) (string, error)

type AgentUpgradeManager struct {
	mu          sync.Mutex
	cfg         AgentUpgradeConfig
	runCommand  AgentUpgradeCommandRunner
	busyCountFn func(string) int
}

func NewAgentUpgradeManager(cfg AgentUpgradeConfig) *AgentUpgradeManager {
	m := &AgentUpgradeManager{
		runCommand: defaultAgentUpgradeCommandRunner,
	}
	m.ApplyConfig(cfg)
	return m
}

func DefaultAgentUpgradeConfig() AgentUpgradeConfig {
	return AgentUpgradeConfig{
		Enabled: false,
		Timeout: defaultAgentUpgradeTimeout,
		Targets: map[string]AgentUpgradeTargetConfig{
			"claudecode": {
				Enabled:        true,
				Strategy:       "builtin",
				VersionCommand: "claude --version",
				UpdateCommand:  "claude update",
			},
			"codex": {
				Enabled:        true,
				Strategy:       "package_manager",
				VersionCommand: "codex --version",
				UpdateCommand:  "npm install -g @openai/codex@latest",
			},
			"gemini": {
				Enabled:        true,
				Strategy:       "observe",
				VersionCommand: "gemini --version",
				UpdateCommand:  "npm install -g @google/gemini-cli@latest",
			},
			"qoder": {
				Enabled:        true,
				Strategy:       "builtin",
				VersionCommand: "qodercli --version",
				UpdateCommand:  "qodercli update",
			},
		},
	}
}

func (m *AgentUpgradeManager) SetBusyCountFunc(fn func(string) int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.busyCountFn = fn
}

func (m *AgentUpgradeManager) SetCommandRunner(fn AgentUpgradeCommandRunner) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if fn == nil {
		m.runCommand = defaultAgentUpgradeCommandRunner
		return
	}
	m.runCommand = fn
}

func (m *AgentUpgradeManager) ApplyConfig(cfg AgentUpgradeConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()

	base := DefaultAgentUpgradeConfig()
	base.Enabled = cfg.Enabled
	if cfg.Timeout > 0 {
		base.Timeout = cfg.Timeout
	}
	if len(cfg.Targets) > 0 {
		for name, override := range cfg.Targets {
			name = normalizeAgentUpgradeTarget(name)
			if name == "" {
				continue
			}
			target := base.Targets[name]
			target.Enabled = override.Enabled
			if override.Strategy != "" {
				target.Strategy = normalizeAgentUpgradeStrategy(override.Strategy)
			}
			if strings.TrimSpace(override.VersionCommand) != "" {
				target.VersionCommand = strings.TrimSpace(override.VersionCommand)
			}
			if strings.TrimSpace(override.UpdateCommand) != "" {
				target.UpdateCommand = strings.TrimSpace(override.UpdateCommand)
			}
			base.Targets[name] = target
		}
	}

	m.cfg = base
}

func (m *AgentUpgradeManager) Snapshot() AgentUpgradeConfig {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg := m.cfg
	cfg.Targets = cloneAgentUpgradeTargets(cfg.Targets)
	return cfg
}

func (m *AgentUpgradeManager) Statuses(ctx context.Context) []AgentUpgradeStatus {
	cfg, runner, busyFn := m.snapshotDeps()
	names := sortedAgentUpgradeTargetNames(cfg.Targets)
	statuses := make([]AgentUpgradeStatus, 0, len(names))
	for _, name := range names {
		target := cfg.Targets[name]
		st := AgentUpgradeStatus{
			Name:          name,
			Enabled:       cfg.Enabled && target.Enabled,
			Strategy:      target.Strategy,
			Actionable:    cfg.Enabled && target.Enabled && target.Strategy != "observe" && target.Strategy != "disabled" && strings.TrimSpace(target.UpdateCommand) != "",
			UpdateCommand: target.UpdateCommand,
		}
		if busyFn != nil {
			st.BusySessions = busyFn(name)
		}
		if strings.TrimSpace(target.VersionCommand) != "" {
			timeout := cfg.Timeout
			if timeout <= 0 {
				timeout = defaultAgentUpgradeTimeout
			}
			runCtx, cancel := context.WithTimeout(ctx, timeout)
			out, err := runner(runCtx, target.VersionCommand)
			cancel()
			if err != nil {
				st.VersionErr = err.Error()
			} else {
				st.Version = normalizeAgentUpgradeOutput(out)
			}
		}
		statuses = append(statuses, st)
	}
	return statuses
}

func (m *AgentUpgradeManager) Run(ctx context.Context, target string) ([]AgentUpgradeRunResult, error) {
	cfg, runner, busyFn := m.snapshotDeps()
	if !cfg.Enabled {
		return nil, fmt.Errorf("agent upgrades are disabled in config")
	}

	names, err := resolveAgentUpgradeRunTargets(target, cfg.Targets)
	if err != nil {
		return nil, err
	}
	results := make([]AgentUpgradeRunResult, 0, len(names))
	for _, name := range names {
		cfgTarget := cfg.Targets[name]
		res := AgentUpgradeRunResult{Name: name, Strategy: cfgTarget.Strategy}
		if !cfgTarget.Enabled || cfgTarget.Strategy == "disabled" {
			res.Skipped = true
			res.Reason = "disabled"
			results = append(results, res)
			continue
		}
		if cfgTarget.Strategy == "observe" {
			res.Skipped = true
			res.Reason = "observe-only"
			results = append(results, res)
			continue
		}
		if strings.TrimSpace(cfgTarget.UpdateCommand) == "" {
			res.Skipped = true
			res.Reason = "no update command configured"
			results = append(results, res)
			continue
		}
		if busyFn != nil {
			res.BusySessions = busyFn(name)
			if res.BusySessions > 0 {
				res.Skipped = true
				res.Reason = fmt.Sprintf("%d active session(s)", res.BusySessions)
				results = append(results, res)
				continue
			}
		}

		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = defaultAgentUpgradeTimeout
		}
		if strings.TrimSpace(cfgTarget.VersionCommand) != "" {
			beforeCtx, cancel := context.WithTimeout(ctx, timeout)
			out, err := runner(beforeCtx, cfgTarget.VersionCommand)
			cancel()
			if err == nil {
				res.BeforeVersion = normalizeAgentUpgradeOutput(out)
			}
		}

		runCtx, cancel := context.WithTimeout(ctx, timeout)
		out, err := runner(runCtx, cfgTarget.UpdateCommand)
		cancel()
		res.Output = normalizeAgentUpgradeOutput(out)
		if err != nil {
			res.Err = err
			results = append(results, res)
			continue
		}

		if strings.TrimSpace(cfgTarget.VersionCommand) != "" {
			afterCtx, cancel := context.WithTimeout(ctx, timeout)
			out, err := runner(afterCtx, cfgTarget.VersionCommand)
			cancel()
			if err == nil {
				res.AfterVersion = normalizeAgentUpgradeOutput(out)
			}
		}
		res.Changed = res.BeforeVersion != "" && res.AfterVersion != "" && res.BeforeVersion != res.AfterVersion
		results = append(results, res)
	}
	return results, nil
}

func (m *AgentUpgradeManager) snapshotDeps() (AgentUpgradeConfig, AgentUpgradeCommandRunner, func(string) int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg := m.cfg
	cfg.Targets = cloneAgentUpgradeTargets(cfg.Targets)
	runner := m.runCommand
	if runner == nil {
		runner = defaultAgentUpgradeCommandRunner
	}
	return cfg, runner, m.busyCountFn
}

func resolveAgentUpgradeRunTargets(target string, targets map[string]AgentUpgradeTargetConfig) ([]string, error) {
	target = strings.TrimSpace(target)
	if target == "" || strings.EqualFold(target, "all") {
		return sortedAgentUpgradeTargetNames(targets), nil
	}
	name := normalizeAgentUpgradeTarget(target)
	if name == "" {
		return nil, fmt.Errorf("unknown agent target %q", target)
	}
	if _, ok := targets[name]; !ok {
		return nil, fmt.Errorf("unknown agent target %q", target)
	}
	return []string{name}, nil
}

func sortedAgentUpgradeTargetNames(targets map[string]AgentUpgradeTargetConfig) []string {
	names := make([]string, 0, len(targets))
	for name := range targets {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func cloneAgentUpgradeTargets(in map[string]AgentUpgradeTargetConfig) map[string]AgentUpgradeTargetConfig {
	out := make(map[string]AgentUpgradeTargetConfig, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func normalizeAgentUpgradeStrategy(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case "builtin", "package_manager", "custom", "observe", "disabled":
		return v
	default:
		return v
	}
}

func normalizeAgentUpgradeTarget(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case "claude", "claudecode", "claude-code":
		return "claudecode"
	case "codex":
		return "codex"
	case "gemini":
		return "gemini"
	case "qoder":
		return "qoder"
	default:
		return ""
	}
}

func normalizeAgentUpgradeOutput(v string) string {
	return strings.TrimSpace(strings.ReplaceAll(v, "\r\n", "\n"))
}

func defaultAgentUpgradeCommandRunner(ctx context.Context, cmd string) (string, error) {
	shell := strings.TrimSpace(os.Getenv("SHELL"))
	if shell == "" {
		shell = "/bin/sh"
	}
	c := exec.CommandContext(ctx, shell, "-lc", cmd)
	out, err := c.CombinedOutput()
	return string(out), err
}
