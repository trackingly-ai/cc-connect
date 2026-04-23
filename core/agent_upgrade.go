package core

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"
)

const defaultAgentUpgradeTimeout = 20 * time.Minute
const defaultAgentUpgradeProbeTimeout = 30 * time.Second
const defaultAgentUpgradeInterval = 24 * time.Hour

type AgentUpgradeTargetConfig struct {
	Enabled        bool
	Strategy       string
	Channel        string
	VersionCommand string
	UpdateCommand  string
	MinimumVersion string
	PinVersion     string
	FailureBackoff time.Duration
	MaxFailures    int
}

type AgentUpgradeConfig struct {
	Enabled        bool
	RunEnabled     bool
	AllowedUserIDs []string
	Policy         string
	Interval       time.Duration
	Timeout        time.Duration
	Targets        map[string]AgentUpgradeTargetConfig
}

type AgentUpgradeStatus struct {
	Name                string
	Enabled             bool
	Strategy            string
	Channel             string
	MinimumVersion      string
	PinVersion          string
	Actionable          bool
	BlockedReason       string
	BusySessions        int
	Version             string
	VersionErr          string
	UpdateCommand       string
	ConsecutiveFailures int
	LastFailureReason   string
	BackoffUntil        time.Time
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

type agentUpgradeFailureState struct {
	ConsecutiveFailures int
	LastFailureReason   string
	LastFailureAt       time.Time
	BackoffUntil        time.Time
}

type AgentUpgradeCommandRunner func(ctx context.Context, cmd string) (string, error)

type AgentUpgradeManager struct {
	mu          sync.Mutex
	cfg         AgentUpgradeConfig
	runCommand  AgentUpgradeCommandRunner
	busyCountFn func(string) int
	running     map[string]bool
	failures    map[string]agentUpgradeFailureState
	lastAutoRun time.Time
	nextAutoRun time.Time
	startOnce   sync.Once
	wakeCh      chan struct{}
	loopDone    chan struct{}
}

func NewAgentUpgradeManager(cfg AgentUpgradeConfig) *AgentUpgradeManager {
	m := &AgentUpgradeManager{
		runCommand: defaultAgentUpgradeCommandRunner,
		running:    make(map[string]bool),
		failures:   make(map[string]agentUpgradeFailureState),
		wakeCh:     make(chan struct{}, 1),
		loopDone:   make(chan struct{}),
	}
	m.ApplyConfig(cfg)
	return m
}

func DefaultAgentUpgradeConfig() AgentUpgradeConfig {
	return AgentUpgradeConfig{
		Enabled:    false,
		RunEnabled: false,
		Policy:     "off",
		Interval:   defaultAgentUpgradeInterval,
		Timeout:    defaultAgentUpgradeTimeout,
		Targets: map[string]AgentUpgradeTargetConfig{
			"claudecode": {
				Enabled:        true,
				Strategy:       "builtin",
				Channel:        "stable",
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
	if cfg.Policy != "" {
		base.Policy = normalizeAgentUpgradePolicy(cfg.Policy)
	}
	if cfg.Interval > 0 {
		base.Interval = cfg.Interval
	}
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
			if override.Channel != "" {
				target.Channel = normalizeAgentUpgradeChannel(override.Channel)
			}
			if strings.TrimSpace(override.VersionCommand) != "" {
				target.VersionCommand = strings.TrimSpace(override.VersionCommand)
			}
			if strings.TrimSpace(override.UpdateCommand) != "" {
				target.UpdateCommand = strings.TrimSpace(override.UpdateCommand)
			}
			if minVersion := strings.TrimSpace(override.MinimumVersion); minVersion != "" {
				target.MinimumVersion = minVersion
			}
			if pinVersion := strings.TrimSpace(override.PinVersion); pinVersion != "" {
				target.PinVersion = pinVersion
			}
			if override.FailureBackoff > 0 {
				target.FailureBackoff = override.FailureBackoff
			}
			if override.MaxFailures > 0 {
				target.MaxFailures = override.MaxFailures
			}
			base.Targets[name] = target
		}
	}

	m.cfg = base
	select {
	case m.wakeCh <- struct{}{}:
	default:
	}
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

func (m *AgentUpgradeManager) Start(ctx context.Context) {
	m.startOnce.Do(func() {
		go func() {
			defer close(m.loopDone)
			m.loop(ctx)
		}()
	})
}

func (m *AgentUpgradeManager) ScheduleState() (time.Time, time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastAutoRun, m.nextAutoRun
}

func (m *AgentUpgradeManager) AutoCheck(ctx context.Context) ([]AgentUpgradeRunResult, error) {
	cfg := m.Snapshot()
	if !cfg.Enabled {
		return nil, nil
	}
	switch cfg.Policy {
	case "check_only":
		statuses := m.Statuses(ctx)
		for _, st := range statuses {
			slog.Info("agent upgrade check",
				"agent", st.Name,
				"strategy", st.Strategy,
				"actionable", st.Actionable,
				"busy_sessions", st.BusySessions,
				"version", st.Version,
				"version_error", st.VersionErr,
				"blocked_reason", st.BlockedReason,
			)
		}
		return nil, nil
	case "idle_only":
		return m.runWithOptions(ctx, "all", true)
	default:
		return nil, nil
	}
}

func (m *AgentUpgradeManager) loop(ctx context.Context) {
	nextRun := time.Time{}
	for {
		cfg := m.Snapshot()
		if !cfg.Enabled || cfg.Policy == "off" {
			nextRun = time.Time{}
			m.updateScheduleState(time.Time{}, nextRun)
			select {
			case <-ctx.Done():
				return
			case <-m.wakeCh:
				continue
			}
		}

		interval := cfg.Interval
		if interval <= 0 {
			interval = defaultAgentUpgradeInterval
		}
		if nextRun.IsZero() && cfg.Enabled && cfg.Policy != "off" {
			nextRun = time.Now().Add(interval)
		}
		m.updateScheduleState(time.Time{}, nextRun)

		wait := time.Until(nextRun)
		if wait < 0 {
			wait = 0
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-m.wakeCh:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			nextRun = time.Time{}
			continue
		case <-timer.C:
		}

		cfg = m.Snapshot()
		if !cfg.Enabled || cfg.Policy == "off" {
			nextRun = time.Time{}
			m.updateScheduleState(time.Time{}, nextRun)
			continue
		}
		results, err := m.AutoCheck(ctx)
		completedAt := time.Now()
		if err != nil {
			slog.Warn("agent upgrade auto-check failed", "error", err)
		} else {
			for _, res := range results {
				slog.Info("agent upgrade auto-run",
					"agent", res.Name,
					"strategy", res.Strategy,
					"busy_sessions", res.BusySessions,
					"skipped", res.Skipped,
					"reason", res.Reason,
					"before", res.BeforeVersion,
					"after", res.AfterVersion,
					"changed", res.Changed,
				)
			}
		}
		nextRun = time.Time{}
		m.updateScheduleState(completedAt, nextRun)
	}
}

func (m *AgentUpgradeManager) Statuses(ctx context.Context) []AgentUpgradeStatus {
	cfg, runner, busyFn := m.snapshotDeps()
	names := sortedAgentUpgradeTargetNames(cfg.Targets)
	statuses := make([]AgentUpgradeStatus, 0, len(names))
	for _, name := range names {
		resolved := m.resolveTarget(ctx, name, cfg.Targets[name], runner)
		target := resolved.cfg
		failureState := m.failureState(name)
		blockedReason := resolved.blockedReason
		if backoffReason := targetBackoffReason(failureState); backoffReason != "" {
			blockedReason = joinBlockedReasons(blockedReason, backoffReason)
		}
		st := AgentUpgradeStatus{
			Name:                name,
			Enabled:             cfg.Enabled && target.Enabled,
			Strategy:            target.Strategy,
			Channel:             target.Channel,
			MinimumVersion:      target.MinimumVersion,
			PinVersion:          target.PinVersion,
			Actionable:          cfg.Enabled && target.Enabled && blockedReason == "" && target.Strategy != "observe" && target.Strategy != "disabled" && strings.TrimSpace(target.UpdateCommand) != "",
			BlockedReason:       blockedReason,
			UpdateCommand:       target.UpdateCommand,
			ConsecutiveFailures: failureState.ConsecutiveFailures,
			LastFailureReason:   failureState.LastFailureReason,
			BackoffUntil:        failureState.BackoffUntil,
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
				if policyReason := evaluateTargetVersionPolicy(target, st.Version); policyReason != "" {
					st.BlockedReason = joinBlockedReasons(st.BlockedReason, policyReason)
				}
			}
		}
		st.Actionable = cfg.Enabled && target.Enabled && st.BlockedReason == "" && target.Strategy != "observe" && target.Strategy != "disabled" && strings.TrimSpace(target.UpdateCommand) != ""
		statuses = append(statuses, st)
	}
	return statuses
}

func (m *AgentUpgradeManager) Run(ctx context.Context, target string) ([]AgentUpgradeRunResult, error) {
	return m.runWithOptions(ctx, target, false)
}

func (m *AgentUpgradeManager) runWithOptions(ctx context.Context, target string, automatic bool) ([]AgentUpgradeRunResult, error) {
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
		if automatic {
			if backoffReason := targetBackoffReason(m.failureState(name)); backoffReason != "" {
				res.Skipped = true
				res.Reason = backoffReason
				results = append(results, res)
				continue
			}
		}
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
			if policyReason := evaluateTargetVersionPolicy(cfgTarget, res.BeforeVersion); policyReason != "" {
				res.Skipped = true
				res.Reason = policyReason
				m.clearFailureState(name)
				results = append(results, res)
				return
			}

			runCtx, cancel := context.WithTimeout(ctx, timeout)
			out, err := runner(runCtx, cfgTarget.UpdateCommand)
			cancel()
			res.Output = normalizeAgentUpgradeOutput(out)
			if err != nil {
				res.Err = err
				m.recordFailure(name, cfgTarget, err.Error())
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
			m.clearFailureState(name)
			results = append(results, res)
		}()
	}
	return results, nil
}

func (m *AgentUpgradeManager) resolveTarget(ctx context.Context, name string, target AgentUpgradeTargetConfig, runner AgentUpgradeCommandRunner) resolvedAgentUpgradeTarget {
	resolved := resolvedAgentUpgradeTarget{cfg: target}
	switch name {
	case "claudecode":
		return resolveClaudeCodeUpgradeTarget(ctx, runner, target)
	case "codex":
		if target.Channel != "" {
			resolved.blockedReason = fmt.Sprintf("channel %q is not supported for codex", target.Channel)
			return resolved
		}
		if target.PinVersion != "" && target.Strategy == "package_manager" {
			resolved.cfg.UpdateCommand = fmt.Sprintf("npm install -g @openai/codex@%s", shellEscapeSingleQuotes(target.PinVersion))
		}
		return resolved
	case "gemini":
		if target.Channel != "" {
			resolved.blockedReason = fmt.Sprintf("channel %q is not supported for gemini", target.Channel)
			return resolved
		}
		if target.PinVersion != "" && target.Strategy == "package_manager" {
			resolved.cfg.UpdateCommand = fmt.Sprintf("npm install -g @google/gemini-cli@%s", shellEscapeSingleQuotes(target.PinVersion))
		}
		return resolved
	case "qoder":
		if target.Channel != "" {
			resolved.blockedReason = fmt.Sprintf("channel %q is not supported for qoder", target.Channel)
		}
		if target.PinVersion != "" {
			resolved.blockedReason = joinBlockedReasons(resolved.blockedReason, "pin_version is not supported for qoder")
		}
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

func resolveClaudeCodeUpgradeTarget(ctx context.Context, runner AgentUpgradeCommandRunner, target AgentUpgradeTargetConfig) resolvedAgentUpgradeTarget {
	resolved := resolvedAgentUpgradeTarget{cfg: target}
	source, updateCommand, blockedReason := detectClaudeCodeUpgradeRoute(ctx, runner, target.Channel)
	switch source {
	case "native", "brew", "npm":
		resolved.cfg.UpdateCommand = updateCommand
		if source != "native" {
			resolved.cfg.Strategy = "package_manager"
		}
	case "apt", "dnf", "apk":
		resolved.cfg.UpdateCommand = updateCommand
	}
	resolved.blockedReason = blockedReason
	if target.PinVersion != "" {
		switch source {
		case "npm":
			resolved.cfg.UpdateCommand = fmt.Sprintf("npm install -g @anthropic-ai/claude-code@%s", shellEscapeSingleQuotes(target.PinVersion))
		case "native", "brew", "apt", "dnf", "apk":
			resolved.blockedReason = joinBlockedReasons(resolved.blockedReason, fmt.Sprintf("pin_version %q is not supported for the detected claude install route", target.PinVersion))
		}
	}
	return resolved
}

func detectClaudeCodeUpgradeRoute(ctx context.Context, runner AgentUpgradeCommandRunner, channel string) (source string, updateCommand string, blockedReason string) {
	if runner == nil {
		runner = defaultAgentUpgradeCommandRunner
	}
	channel = normalizeAgentUpgradeChannel(channel)
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
			if channel == "latest" {
				return "native", "claude update", "channel \"latest\" is not supported for native claude installs"
			}
			return "native", "claude update", ""
		}
	}
	switch channel {
	case "stable":
		if _, ok := probe("brew list --cask claude-code >/dev/null && printf ok"); ok {
			return "brew", "brew upgrade claude-code", ""
		}
	case "latest":
		if _, ok := probe("brew list --cask claude-code@latest >/dev/null && printf ok"); ok {
			return "brew", "brew upgrade claude-code@latest", ""
		}
	}
	if _, ok := probe("brew list --cask claude-code@latest >/dev/null && printf ok"); ok {
		if channel == "stable" {
			return "brew", "brew upgrade claude-code", "requested claude stable channel but Homebrew stable cask is not installed"
		}
		return "brew", "brew upgrade claude-code@latest", ""
	}
	if _, ok := probe("brew list --cask claude-code >/dev/null && printf ok"); ok {
		if channel == "latest" {
			return "brew", "brew upgrade claude-code@latest", "requested claude latest channel but Homebrew latest cask is not installed"
		}
		return "brew", "brew upgrade claude-code", ""
	}
	if _, ok := probe("npm -g ls @anthropic-ai/claude-code --depth=0 >/dev/null && printf ok"); ok {
		if channel == "latest" || channel == "stable" {
			return "npm", "npm install -g @anthropic-ai/claude-code", fmt.Sprintf("channel %q is not supported for npm-based claude installs", channel)
		}
		return "npm", "npm install -g @anthropic-ai/claude-code", ""
	}
	if _, ok := probe("dpkg -s claude-code >/dev/null && printf ok"); ok {
		return "apt", "sudo apt update && sudo apt upgrade claude-code", joinBlockedReasons("requires privileged apt upgrade; run manually", unsupportedClaudeChannelReason(channel))
	}
	if _, ok := probe("rpm -q claude-code >/dev/null && printf ok"); ok {
		return "dnf", "sudo dnf upgrade claude-code", joinBlockedReasons("requires privileged dnf upgrade; run manually", unsupportedClaudeChannelReason(channel))
	}
	if _, ok := probe("apk info -e claude-code >/dev/null && printf ok"); ok {
		return "apk", "apk update && apk upgrade claude-code", joinBlockedReasons("requires privileged apk upgrade; run manually", unsupportedClaudeChannelReason(channel))
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

func normalizeAgentUpgradeChannel(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case "", "stable", "latest":
		return v
	default:
		return ""
	}
}

func normalizeAgentUpgradePolicy(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case "off", "check_only", "idle_only":
		return v
	default:
		return "off"
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

func shellEscapeSingleQuotes(v string) string {
	return strings.ReplaceAll(strings.TrimSpace(v), `'`, `'\''`)
}

func evaluateTargetVersionPolicy(target AgentUpgradeTargetConfig, currentVersion string) string {
	if target.MinimumVersion == "" && target.PinVersion == "" {
		return ""
	}
	current := extractAgentVersion(currentVersion)
	if current == "" {
		return "version constraints require a parseable version from version_command"
	}
	if target.PinVersion != "" && sameVersion(current, target.PinVersion) {
		return fmt.Sprintf("already pinned at %s", target.PinVersion)
	}
	if target.MinimumVersion != "" {
		if ok, cmp := compareVersionStrings(current, target.MinimumVersion); ok && cmp >= 0 {
			return fmt.Sprintf("already meets minimum_version %s", target.MinimumVersion)
		}
	}
	return ""
}

func extractAgentVersion(v string) string {
	v = strings.TrimSpace(v)
	start := -1
	for i, r := range v {
		if r >= '0' && r <= '9' {
			start = i
			break
		}
	}
	if start < 0 {
		return ""
	}
	end := start
	for end < len(v) {
		c := v[end]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '.' || c == '-' || c == '+' {
			end++
			continue
		}
		break
	}
	return strings.Trim(v[start:end], ".,;:()[]{}")
}

func compareVersionStrings(a, b string) (bool, int) {
	aParts, aOk := parseVersionParts(extractAgentVersion(a))
	bParts, bOk := parseVersionParts(extractAgentVersion(b))
	if !aOk || !bOk {
		return false, 0
	}
	n := len(aParts)
	if len(bParts) > n {
		n = len(bParts)
	}
	for i := 0; i < n; i++ {
		var av, bv int
		if i < len(aParts) {
			av = aParts[i]
		}
		if i < len(bParts) {
			bv = bParts[i]
		}
		switch {
		case av < bv:
			return true, -1
		case av > bv:
			return true, 1
		}
	}
	return true, 0
}

func parseVersionParts(v string) ([]int, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, false
	}
	parts := strings.Split(v, ".")
	out := make([]int, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, false
		}
		value := 0
		digits := 0
		for i := 0; i < len(part); i++ {
			c := part[i]
			if c < '0' || c > '9' {
				break
			}
			value = value*10 + int(c-'0')
			digits++
		}
		if digits == 0 {
			return nil, false
		}
		out = append(out, value)
	}
	return out, true
}

func sameVersion(a, b string) bool {
	ok, cmp := compareVersionStrings(a, b)
	return ok && cmp == 0
}

func unsupportedClaudeChannelReason(channel string) string {
	if channel == "" {
		return ""
	}
	return fmt.Sprintf("channel %q is not supported for the detected claude install route", channel)
}

func joinBlockedReasons(parts ...string) string {
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if _, ok := seen[part]; ok {
			continue
		}
		seen[part] = struct{}{}
		out = append(out, part)
	}
	return strings.Join(out, "; ")
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

func (m *AgentUpgradeManager) updateScheduleState(last, next time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !last.IsZero() {
		m.lastAutoRun = last
	}
	m.nextAutoRun = next
}

func (m *AgentUpgradeManager) failureState(name string) agentUpgradeFailureState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.failures[name]
}

func (m *AgentUpgradeManager) recordFailure(name string, target AgentUpgradeTargetConfig, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.failures[name]
	state.ConsecutiveFailures++
	state.LastFailureReason = strings.TrimSpace(reason)
	state.LastFailureAt = time.Now()
	if target.MaxFailures > 0 && target.FailureBackoff > 0 && state.ConsecutiveFailures >= target.MaxFailures {
		state.BackoffUntil = state.LastFailureAt.Add(target.FailureBackoff)
	}
	m.failures[name] = state
}

func (m *AgentUpgradeManager) clearFailureState(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.failures, name)
}

func targetBackoffReason(state agentUpgradeFailureState) string {
	if state.BackoffUntil.IsZero() || time.Now().After(state.BackoffUntil) {
		return ""
	}
	if state.LastFailureReason != "" {
		return fmt.Sprintf("backing off until %s after failures: %s", state.BackoffUntil.Format(time.RFC3339), state.LastFailureReason)
	}
	return fmt.Sprintf("backing off until %s", state.BackoffUntil.Format(time.RFC3339))
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
