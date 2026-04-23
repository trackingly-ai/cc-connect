package core

import (
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"
)

const defaultAgentUpgradeTimeout = 20 * time.Minute
const defaultAgentUpgradeProbeTimeout = 30 * time.Second

type AgentUpgradeTargetConfig struct {
	Enabled        bool
	Strategy       string
	VersionCommand string
	UpdateCommand  string
}

type AgentUpgradeConfig struct {
	Enabled        bool
	RunEnabled     bool
	AllowedUserIDs []string
	Timeout        time.Duration
	Targets        map[string]AgentUpgradeTargetConfig
}

type AgentUpgradeStatus struct {
	Name          string
	Enabled       bool
	Strategy      string
	Actionable    bool
	BlockedReason string
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

type resolvedAgentUpgradeTarget struct {
	cfg           AgentUpgradeTargetConfig
	blockedReason string
}

type AgentUpgradeCommandRunner func(ctx context.Context, cmd string) (string, error)

type AgentUpgradeManager struct {
	mu          sync.Mutex
	cfg         AgentUpgradeConfig
	runCommand  AgentUpgradeCommandRunner
	busyCountFn func(string) int
	running     map[string]bool
}

func NewAgentUpgradeManager(cfg AgentUpgradeConfig) *AgentUpgradeManager {
	m := &AgentUpgradeManager{
		runCommand: defaultAgentUpgradeCommandRunner,
		running:    make(map[string]bool),
	}
	m.ApplyConfig(cfg)
	return m
}

func DefaultAgentUpgradeConfig() AgentUpgradeConfig {
	return AgentUpgradeConfig{
		Enabled:    false,
		RunEnabled: false,
		Timeout:    defaultAgentUpgradeTimeout,
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
	base.RunEnabled = cfg.RunEnabled
	base.AllowedUserIDs = dedupeTrimmedStrings(cfg.AllowedUserIDs)
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
	cfg.AllowedUserIDs = append([]string(nil), cfg.AllowedUserIDs...)
	cfg.Targets = cloneAgentUpgradeTargets(cfg.Targets)
	return cfg
}

func (m *AgentUpgradeManager) AuthorizeRun(userID string) error {
	cfg := m.Snapshot()
	if !cfg.RunEnabled {
		return fmt.Errorf("interactive agent upgrades are disabled; set [agent_updates].run_enabled = true")
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return fmt.Errorf("interactive agent upgrades require a resolvable user id")
	}
	for _, allowed := range cfg.AllowedUserIDs {
		if strings.EqualFold(strings.TrimSpace(allowed), userID) {
			return nil
		}
	}
	return fmt.Errorf("you are not allowed to run agent upgrades")
}

func (m *AgentUpgradeManager) IsTargetRunning(name string) bool {
	name = normalizeAgentUpgradeTarget(name)
	if name == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running[name]
}

func (m *AgentUpgradeManager) Statuses(ctx context.Context) []AgentUpgradeStatus {
	cfg, runner, busyFn := m.snapshotDeps()
	names := sortedAgentUpgradeTargetNames(cfg.Targets)
	statuses := make([]AgentUpgradeStatus, 0, len(names))
	for _, name := range names {
		resolved := m.resolveTarget(ctx, name, cfg.Targets[name], runner)
		target := resolved.cfg
		st := AgentUpgradeStatus{
			Name:          name,
			Enabled:       cfg.Enabled && target.Enabled,
			Strategy:      target.Strategy,
			Actionable:    cfg.Enabled && target.Enabled && resolved.blockedReason == "" && target.Strategy != "observe" && target.Strategy != "disabled" && strings.TrimSpace(target.UpdateCommand) != "",
			BlockedReason: resolved.blockedReason,
			UpdateCommand: target.UpdateCommand,
		}
		if busyFn != nil {
			st.BusySessions = busyFn(name)
		}
		if strings.TrimSpace(target.VersionCommand) != "" {
			timeout := versionProbeTimeout(cfg.Timeout)
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
		resolved := m.resolveTarget(ctx, name, cfg.Targets[name], runner)
		cfgTarget := resolved.cfg
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
		if resolved.blockedReason != "" {
			res.Skipped = true
			res.Reason = resolved.blockedReason
			results = append(results, res)
			continue
		}
		if !m.beginTargetRun(name) {
			res.Skipped = true
			res.Reason = "upgrade already running"
			results = append(results, res)
			continue
		}
		func() {
			defer m.endTargetRun(name)

			if busyFn != nil {
				res.BusySessions = busyFn(name)
				if res.BusySessions > 0 {
					res.Skipped = true
					res.Reason = fmt.Sprintf("%d active session(s)", res.BusySessions)
					results = append(results, res)
					return
				}
			}

			timeout := cfg.Timeout
			if timeout <= 0 {
				timeout = defaultAgentUpgradeTimeout
			}
			if strings.TrimSpace(cfgTarget.VersionCommand) != "" {
				beforeCtx, cancel := context.WithTimeout(ctx, versionProbeTimeout(timeout))
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
				return
			}

			if strings.TrimSpace(cfgTarget.VersionCommand) != "" {
				afterCtx, cancel := context.WithTimeout(ctx, versionProbeTimeout(timeout))
				out, err := runner(afterCtx, cfgTarget.VersionCommand)
				cancel()
				if err == nil {
					res.AfterVersion = normalizeAgentUpgradeOutput(out)
				}
			}
			res.Changed = res.BeforeVersion != "" && res.AfterVersion != "" && res.BeforeVersion != res.AfterVersion
			results = append(results, res)
		}()
	}
	return results, nil
}

func (m *AgentUpgradeManager) resolveTarget(ctx context.Context, name string, target AgentUpgradeTargetConfig, runner AgentUpgradeCommandRunner) resolvedAgentUpgradeTarget {
	resolved := resolvedAgentUpgradeTarget{cfg: target}
	if name != "claudecode" || target.Strategy != "builtin" {
		return resolved
	}

	switch source, updateCmd, blockedReason := detectClaudeCodeUpgradeRoute(ctx, runner); source {
	case "native", "brew", "npm":
		resolved.cfg.UpdateCommand = updateCmd
		if source != "native" {
			resolved.cfg.Strategy = "package_manager"
		}
		return resolved
	case "apt", "dnf", "apk":
		resolved.cfg.UpdateCommand = updateCmd
		resolved.blockedReason = blockedReason
		return resolved
	default:
		return resolved
	}
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

func detectClaudeCodeUpgradeRoute(ctx context.Context, runner AgentUpgradeCommandRunner) (source string, updateCommand string, blockedReason string) {
	if runner == nil {
		runner = defaultAgentUpgradeCommandRunner
	}
	probe := func(cmd string) (string, bool) {
		probeCtx, cancel := context.WithTimeout(ctx, defaultAgentUpgradeProbeTimeout)
		defer cancel()
		out, err := runner(probeCtx, cmd)
		if err != nil {
			return "", false
		}
		return normalizeAgentUpgradeOutput(out), true
	}

	if path, ok := probe("command -v claude"); ok {
		if strings.Contains(path, "/.local/bin/claude") || strings.Contains(path, "/.local/share/claude/") {
			return "native", "claude update", ""
		}
	}
	if _, ok := probe("brew list --cask claude-code@latest >/dev/null && printf ok"); ok {
		return "brew", "brew upgrade claude-code@latest", ""
	}
	if _, ok := probe("brew list --cask claude-code >/dev/null && printf ok"); ok {
		return "brew", "brew upgrade claude-code", ""
	}
	if _, ok := probe("npm -g ls @anthropic-ai/claude-code --depth=0 >/dev/null && printf ok"); ok {
		return "npm", "npm install -g @anthropic-ai/claude-code", ""
	}
	if _, ok := probe("dpkg -s claude-code >/dev/null && printf ok"); ok {
		return "apt", "sudo apt update && sudo apt upgrade claude-code", "requires privileged apt upgrade; run manually"
	}
	if _, ok := probe("rpm -q claude-code >/dev/null && printf ok"); ok {
		return "dnf", "sudo dnf upgrade claude-code", "requires privileged dnf upgrade; run manually"
	}
	if _, ok := probe("apk info -e claude-code >/dev/null && printf ok"); ok {
		return "apk", "apk update && apk upgrade claude-code", "requires privileged apk upgrade; run manually"
	}
	return "", "", ""
}

func normalizeAgentUpgradeStrategy(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case "builtin", "package_manager", "custom", "observe", "disabled":
		return v
	default:
		return "disabled"
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
	c := exec.CommandContext(ctx, "/bin/sh", "-lc", cmd)
	out, err := c.CombinedOutput()
	return string(out), err
}

func (m *AgentUpgradeManager) beginTargetRun(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running[name] {
		return false
	}
	m.running[name] = true
	return true
}

func (m *AgentUpgradeManager) endTargetRun(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.running, name)
}

func versionProbeTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 || timeout > defaultAgentUpgradeProbeTimeout {
		return defaultAgentUpgradeProbeTimeout
	}
	return timeout
}

func dedupeTrimmedStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
