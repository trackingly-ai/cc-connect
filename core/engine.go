package core

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const maxPlatformMessageLen = 4000
const maxParallelTTSRequests = 3

const (
	defaultThinkingMaxLen = 300
	defaultToolMaxLen     = 500
)

// Slow-operation thresholds. Operations exceeding these durations produce a
// slog.Warn so operators can quickly pinpoint bottlenecks.
const (
	slowPlatformSend    = 2 * time.Second  // platform Reply / Send
	slowAgentStart      = 5 * time.Second  // agent.StartSession
	slowAgentClose      = 3 * time.Second  // agentSession.Close
	slowAgentSend       = 2 * time.Second  // agentSession.Send
	slowAgentFirstEvent = 15 * time.Second // time from send to first agent event
)

// VersionInfo is set by main at startup so that /version works.
var VersionInfo string

// CurrentVersion is the semver tag (e.g. "v1.2.0-beta.1"), set by main.
var CurrentVersion string

// RestartRequest carries info needed to send a post-restart notification.
type RestartRequest struct {
	SessionKey string `json:"session_key"`
	Platform   string `json:"platform"`
}

// SaveRestartNotify persists restart info so the new process can send
// a "restart successful" message after startup.
func SaveRestartNotify(dataDir string, req RestartRequest) error {
	dir := filepath.Join(dataDir, "run")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create run dir: %w", err)
	}
	data, _ := json.Marshal(req)
	return os.WriteFile(filepath.Join(dir, "restart_notify"), data, 0o644)
}

// ConsumeRestartNotify reads and deletes the restart notification file.
// Returns nil if no notification is pending.
func ConsumeRestartNotify(dataDir string) *RestartRequest {
	p := filepath.Join(dataDir, "run", "restart_notify")
	data, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	os.Remove(p)
	var req RestartRequest
	if json.Unmarshal(data, &req) != nil {
		return nil
	}
	return &req
}

// SendRestartNotification sends a "restart successful" message to the
// platform/session that initiated the restart.
func (e *Engine) SendRestartNotification(platformName, sessionKey string) {
	for _, p := range e.platforms {
		if p.Name() != platformName {
			continue
		}
		rc, ok := p.(ReplyContextReconstructor)
		if !ok {
			slog.Debug("restart notify: platform does not support ReconstructReplyCtx", "platform", platformName)
			return
		}
		rctx, err := rc.ReconstructReplyCtx(sessionKey)
		if err != nil {
			slog.Debug("restart notify: reconstruct failed", "error", err)
			return
		}
		text := e.i18n.T(MsgRestartSuccess)
		if CurrentVersion != "" {
			text += fmt.Sprintf(" (%s)", CurrentVersion)
		}
		if err := p.Send(e.ctx, rctx, text); err != nil {
			slog.Debug("restart notify: send failed", "error", err)
		}
		return
	}
}

// RestartCh is signaled when /restart is invoked. main listens on it
// to perform a graceful shutdown followed by syscall.Exec.
var RestartCh = make(chan RestartRequest, 1)

// DisplayCfg controls truncation of intermediate messages.
// A value of -1 means "use default", 0 means "no truncation".
type DisplayCfg struct {
	ThinkingMaxLen int // max runes for thinking preview; 0 = no truncation
	ToolMaxLen     int // max runes for tool use preview; 0 = no truncation
}

// RateLimitCfg controls per-session message rate limiting.
type RateLimitCfg struct {
	MaxMessages int           // max messages per window; 0 = disabled
	Window      time.Duration // sliding window size
}

// Engine routes messages between platforms and the agent for a single project.
type Engine struct {
	name         string
	agent        Agent
	platforms    []Platform
	sessions     *SessionManager
	ctx          context.Context
	cancel       context.CancelFunc
	i18n         *I18n
	speech       SpeechCfg
	tts          *TTSCfg
	display      DisplayCfg
	defaultQuiet bool
	startedAt    time.Time

	providerSaveFunc       func(providerName string) error
	providerAddSaveFunc    func(p ProviderConfig) error
	providerRemoveSaveFunc func(name string) error

	ttsSaveFunc func(mode string) error

	commandSaveAddFunc func(name, description, prompt, exec, workDir string) error
	commandSaveDelFunc func(name string) error

	displaySaveFunc  func(thinkingMaxLen, toolMaxLen *int) error
	configReloadFunc func() (*ConfigReloadResult, error)

	cronScheduler *CronScheduler

	commands *CommandRegistry
	skills   *SkillRegistry
	aliases  map[string]string // trigger → command (e.g. "帮助" → "/help")
	aliasMu  sync.RWMutex

	aliasSaveAddFunc func(name, command string) error
	aliasSaveDelFunc func(name string) error

	bannedWords []string
	bannedMu    sync.RWMutex

	disabledCmds map[string]bool

	rateLimiter       *RateLimiter
	streamPreview     StreamPreviewCfg
	relayManager      *RelayManager
	eventIdleTimeout  time.Duration
	firstEventTimeout time.Duration
	turnTimeout       time.Duration

	// Interactive agent session management
	interactiveMu     sync.Mutex
	interactiveStates map[string]*interactiveState // key = sessionKey
	promptMu          sync.Mutex
	prompts           map[string]*pendingInteractionPrompt
	voiceConfirmMu    sync.Mutex
	voiceConfirms     map[string]*pendingVoiceConfirmation
	pendingAttachMu   sync.Mutex
	pendingAttach     map[string]*pendingAttachments
	cronEditMu        sync.Mutex
	pendingCronEdits  map[string]*pendingCronPromptEdit
	reviewMu          sync.Mutex
	reviewFlows       map[string]*reviewFlow

	quietMu sync.RWMutex
	quiet   bool // when true, suppress thinking and tool progress messages globally

	agentStartMu sync.Mutex
}

// interactiveState tracks a running interactive agent session and its permission state.
type interactiveState struct {
	agentSession AgentSession
	platform     Platform
	replyCtx     any
	mu           sync.Mutex
	pending      *pendingPermission
	approveAll   bool // when true, auto-approve all permission requests for this session
	quiet        bool // when true, suppress thinking and tool progress for this session
	fromVoice    bool // true if current turn originated from voice transcription
	stopped      chan struct{}
	stopOnce     sync.Once
}

func newInteractiveState(agentSession AgentSession, p Platform, replyCtx any, quiet bool) *interactiveState {
	return &interactiveState{
		agentSession: agentSession,
		platform:     p,
		replyCtx:     replyCtx,
		quiet:        quiet,
		stopped:      make(chan struct{}),
	}
}

func (s *interactiveState) stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		close(s.stopped)
	})
}

// pendingPermission represents a permission request waiting for user response.
type pendingPermission struct {
	RequestID    string
	ToolName     string
	ToolInput    map[string]any
	InputPreview string
	Resolved     chan struct{} // closed when user responds
	resolveOnce  sync.Once
}

type pendingVoiceConfirmation struct {
	Text         string
	AwaitingEdit bool
}

type pendingAttachments struct {
	Images []ImageAttachment
	Files  []FileAttachment
}

type pendingCronPromptEdit struct {
	JobID string
}

const maxCronPromptLen = 10000
const maxReviewPromptLen = 10000

type reviewFlow struct {
	ReviewerProject   string
	LastOriginSummary string
	LastReviewSummary string
	Running           bool
}

type managedTurnOpts struct {
	Prefix            string
	AutoApprove       bool
	OfferFollowUpCard bool
	Silent            bool
}

// resolve safely closes the Resolved channel exactly once.
func (pp *pendingPermission) resolve() {
	pp.resolveOnce.Do(func() { close(pp.Resolved) })
}

func NewEngine(name string, ag Agent, platforms []Platform, sessionStorePath string, lang Language) *Engine {
	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{
		name:              name,
		agent:             ag,
		platforms:         platforms,
		sessions:          NewSessionManager(sessionStorePath),
		ctx:               ctx,
		cancel:            cancel,
		i18n:              NewI18n(lang),
		display:           DisplayCfg{ThinkingMaxLen: defaultThinkingMaxLen, ToolMaxLen: defaultToolMaxLen},
		commands:          NewCommandRegistry(),
		skills:            NewSkillRegistry(),
		aliases:           make(map[string]string),
		interactiveStates: make(map[string]*interactiveState),
		prompts:           make(map[string]*pendingInteractionPrompt),
		voiceConfirms:     make(map[string]*pendingVoiceConfirmation),
		pendingAttach:     make(map[string]*pendingAttachments),
		pendingCronEdits:  make(map[string]*pendingCronPromptEdit),
		reviewFlows:       make(map[string]*reviewFlow),
		startedAt:         time.Now(),
		streamPreview:     DefaultStreamPreviewCfg(),
		eventIdleTimeout:  defaultEventIdleTimeout,
		firstEventTimeout: defaultFirstEventTimeout,
		turnTimeout:       defaultTurnTimeout,
	}

	if cp, ok := ag.(CommandProvider); ok {
		e.commands.SetAgentDirs(cp.CommandDirs())
	}
	if sp, ok := ag.(SkillProvider); ok {
		e.skills.SetDirs(sp.SkillDirs())
	}
	for _, p := range platforms {
		if nav, ok := p.(CardNavigable); ok {
			nav.SetCardNavigationHandler(e.handleCardNav)
		}
	}

	return e
}

// SetSpeechConfig configures the speech-to-text subsystem.
func (e *Engine) SetSpeechConfig(cfg SpeechCfg) {
	e.speech = cfg
}

// SetTTSConfig configures the text-to-speech subsystem.
func (e *Engine) SetTTSConfig(cfg *TTSCfg) {
	e.tts = cfg
}

// SetTTSSaveFunc registers a callback that persists TTS mode changes.
func (e *Engine) SetTTSSaveFunc(fn func(mode string) error) {
	e.ttsSaveFunc = fn
}

// SetDisplayConfig overrides the default truncation settings.
func (e *Engine) SetDisplayConfig(cfg DisplayCfg) {
	e.display = cfg
}

// SetDefaultQuiet sets whether new sessions start in quiet mode.
func (e *Engine) SetDefaultQuiet(q bool) {
	e.defaultQuiet = q
}

func (e *Engine) SetLanguageSaveFunc(fn func(Language) error) {
	e.i18n.SetSaveFunc(fn)
}

func (e *Engine) SetProviderSaveFunc(fn func(providerName string) error) {
	e.providerSaveFunc = fn
}

func (e *Engine) SetProviderAddSaveFunc(fn func(ProviderConfig) error) {
	e.providerAddSaveFunc = fn
}

func (e *Engine) SetProviderRemoveSaveFunc(fn func(string) error) {
	e.providerRemoveSaveFunc = fn
}

func (e *Engine) SetCronScheduler(cs *CronScheduler) {
	e.cronScheduler = cs
}

// ActiveSessionKeys returns session keys that currently have interactive state.
func (e *Engine) ActiveSessionKeys() []string {
	e.interactiveMu.Lock()
	defer e.interactiveMu.Unlock()

	keys := make([]string, 0, len(e.interactiveStates))
	for key := range e.interactiveStates {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (e *Engine) SetCommandSaveAddFunc(fn func(name, description, prompt, exec, workDir string) error) {
	e.commandSaveAddFunc = fn
}

func (e *Engine) SetCommandSaveDelFunc(fn func(name string) error) {
	e.commandSaveDelFunc = fn
}

func (e *Engine) SetDisplaySaveFunc(fn func(thinkingMaxLen, toolMaxLen *int) error) {
	e.displaySaveFunc = fn
}

// ConfigReloadResult describes what was updated by a config reload.
type ConfigReloadResult struct {
	DisplayUpdated   bool
	ProvidersUpdated int
	CommandsUpdated  int
}

func (e *Engine) SetConfigReloadFunc(fn func() (*ConfigReloadResult, error)) {
	e.configReloadFunc = fn
}

// GetAgent returns the engine's agent (for type assertions like ProviderSwitcher).
func (e *Engine) GetAgent() Agent {
	return e.agent
}

// AddCommand registers a custom slash command.
func (e *Engine) AddCommand(name, description, prompt, exec, workDir, source string) {
	e.commands.Add(name, description, prompt, exec, workDir, source)
}

// ClearCommands removes all commands from the given source.
func (e *Engine) ClearCommands(source string) {
	e.commands.ClearSource(source)
}

// AddAlias registers a command alias.
func (e *Engine) AddAlias(name, command string) {
	e.aliasMu.Lock()
	defer e.aliasMu.Unlock()
	e.aliases[name] = command
}

func (e *Engine) SetAliasSaveAddFunc(fn func(name, command string) error) {
	e.aliasSaveAddFunc = fn
}

func (e *Engine) SetAliasSaveDelFunc(fn func(name string) error) {
	e.aliasSaveDelFunc = fn
}

// ClearAliases removes all aliases (for config reload).
func (e *Engine) ClearAliases() {
	e.aliasMu.Lock()
	defer e.aliasMu.Unlock()
	e.aliases = make(map[string]string)
}

// SetDisabledCommands sets the list of command IDs that are disabled for this project.
func (e *Engine) SetDisabledCommands(cmds []string) {
	m := make(map[string]bool, len(cmds))
	for _, c := range cmds {
		c = strings.ToLower(strings.TrimPrefix(c, "/"))
		// Resolve alias names to canonical IDs
		id := matchPrefix(c, builtinCommands)
		if id != "" {
			m[id] = true
		} else {
			m[c] = true
		}
	}
	e.disabledCmds = m
}

// SetBannedWords replaces the banned words list.
func (e *Engine) SetBannedWords(words []string) {
	e.bannedMu.Lock()
	defer e.bannedMu.Unlock()
	lower := make([]string, len(words))
	for i, w := range words {
		lower[i] = strings.ToLower(w)
	}
	e.bannedWords = lower
}

// SetRateLimitCfg configures per-session message rate limiting.
func (e *Engine) SetRateLimitCfg(cfg RateLimitCfg) {
	e.rateLimiter = NewRateLimiter(cfg.MaxMessages, cfg.Window)
}

// SetStreamPreviewCfg configures the streaming preview behavior.
func (e *Engine) SetStreamPreviewCfg(cfg StreamPreviewCfg) {
	e.streamPreview = cfg
}

// SetEventIdleTimeout sets the maximum time to wait between consecutive agent events.
// 0 disables the timeout entirely.
func (e *Engine) SetEventIdleTimeout(d time.Duration) {
	e.eventIdleTimeout = d
}

// SetFirstEventTimeout sets the maximum time to wait for the first agent event in a turn.
// 0 disables the timeout entirely.
func (e *Engine) SetFirstEventTimeout(d time.Duration) {
	e.firstEventTimeout = d
}

// SetTurnTimeout sets the maximum total time allowed for a single agent turn.
// 0 disables the timeout entirely.
func (e *Engine) SetTurnTimeout(d time.Duration) {
	e.turnTimeout = d
}

func (e *Engine) SetRelayManager(rm *RelayManager) {
	e.relayManager = rm
}

func (e *Engine) RelayManager() *RelayManager {
	return e.relayManager
}

// RemoveCommand removes a custom command by name. Returns false if not found.
func (e *Engine) RemoveCommand(name string) bool {
	return e.commands.Remove(name)
}

func (e *Engine) ProjectName() string {
	return e.name
}

// ExecuteCronJob runs a cron job by injecting a synthetic message into the engine.
// It finds the platform that owns the session key, reconstructs a reply context,
// and processes the message as if the user sent it.
func (e *Engine) ExecuteCronJob(ctx context.Context, job *CronJob) error {
	if ctx == nil {
		ctx = context.Background()
	}
	sessionKey := job.SessionKey
	platformName := ""
	if idx := strings.Index(sessionKey, ":"); idx > 0 {
		platformName = sessionKey[:idx]
	}

	var targetPlatform Platform
	for _, p := range e.platforms {
		if p.Name() == platformName {
			targetPlatform = p
			break
		}
	}
	if targetPlatform == nil {
		return fmt.Errorf("platform %q not found for session %q", platformName, sessionKey)
	}

	rc, ok := targetPlatform.(ReplyContextReconstructor)
	if !ok {
		return fmt.Errorf("platform %q does not support proactive messaging (cron)", platformName)
	}

	replyCtx, err := rc.ReconstructReplyCtx(sessionKey)
	if err != nil {
		return fmt.Errorf("reconstruct reply context: %w", err)
	}

	// Notify user that a cron job is executing (unless silent)
	silent := false
	if e.cronScheduler != nil {
		silent = e.cronScheduler.IsSilent(job)
	}
	if !silent {
		desc := job.Description
		if desc == "" {
			if job.IsShellJob() {
				desc = truncateStr(job.Exec, 40)
			} else {
				desc = truncateStr(job.Prompt, 40)
			}
		}
		e.send(targetPlatform, replyCtx, fmt.Sprintf("⏰ %s", desc))
	}

	if job.IsShellJob() {
		return e.executeCronShell(targetPlatform, replyCtx, job)
	}

	msg := &Message{
		SessionKey: sessionKey,
		Platform:   platformName,
		UserID:     "cron",
		UserName:   "cron",
		Content:    job.Prompt,
		ReplyCtx:   replyCtx,
	}

	cancelDone := make(chan struct{})
	defer close(cancelDone)
	go func() {
		select {
		case <-ctx.Done():
			slog.Warn("cron: cancelling interactive job", "project", e.name, "session", sessionKey, "error", ctx.Err())
			e.cleanupInteractiveState(sessionKey)
		case <-cancelDone:
		}
	}()

	if job.UsesNewSessionPerRun() {
		e.cleanupInteractiveState(sessionKey)
		session := e.sessions.NewSession(sessionKey, "cron-"+job.ID)
		if !session.TryLock() {
			return fmt.Errorf("session %q is busy", sessionKey)
		}
		// Intentionally synchronous: the scheduler waits for this turn to
		// finish, so timeout/cancellation is enforced via ctx and cleanup.
		e.processInteractiveMessage(targetPlatform, msg, session)
		return ctx.Err()
	}

	session := e.sessions.GetOrCreateActive(sessionKey)
	if !session.TryLock() {
		return fmt.Errorf("session %q is busy", sessionKey)
	}

	e.processInteractiveMessage(targetPlatform, msg, session)
	return ctx.Err()
}

func (e *Engine) executeCronShell(p Platform, replyCtx any, job *CronJob) error {
	workDir := job.WorkDir
	if workDir == "" {
		if wd, ok := e.agent.(interface{ GetWorkDir() string }); ok {
			workDir = wd.GetWorkDir()
		}
	}
	if workDir == "" {
		workDir, _ = os.Getwd()
	}

	var (
		ctx    context.Context
		cancel context.CancelFunc
	)
	if timeout := job.ExecutionTimeout(); timeout > 0 {
		ctx, cancel = context.WithTimeout(e.ctx, timeout)
	} else {
		ctx, cancel = context.WithCancel(e.ctx)
	}
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", job.Exec)
	cmd.Dir = workDir
	output, err := cmd.CombinedOutput()

	if ctx.Err() == context.DeadlineExceeded {
		e.send(p, replyCtx, fmt.Sprintf("⏰ ⚠️ timeout: `%s`", truncateStr(job.Exec, 60)))
		return fmt.Errorf("shell command timed out")
	}

	result := strings.TrimSpace(string(output))
	if err != nil {
		if result != "" {
			e.send(p, replyCtx, fmt.Sprintf("⏰ ❌ `%s`\n\n%s\n\nerror: %v", truncateStr(job.Exec, 60), truncateStr(result, 3000), err))
		} else {
			e.send(p, replyCtx, fmt.Sprintf("⏰ ❌ `%s`\nerror: %v", truncateStr(job.Exec, 60), err))
		}
		return fmt.Errorf("shell: %w", err)
	}

	if result == "" {
		result = "(no output)"
	}
	e.send(p, replyCtx, fmt.Sprintf("⏰ ✅ `%s`\n\n%s", truncateStr(job.Exec, 60), truncateStr(result, 3000)))
	return nil
}

func (e *Engine) Start() error {
	var startErrs []error
	for _, p := range e.platforms {
		if err := p.Start(e.handleMessage); err != nil {
			slog.Warn("platform start failed", "project", e.name, "platform", p.Name(), "error", err)
			startErrs = append(startErrs, fmt.Errorf("[%s] start platform %s: %w", e.name, p.Name(), err))
			continue
		}
		slog.Info("platform started", "project", e.name, "platform", p.Name())

		// Register commands on platforms that support it (e.g. Telegram setMyCommands)
		if registrar, ok := p.(CommandRegistrar); ok {
			commands := e.GetAllCommands()
			if err := registrar.RegisterCommands(commands); err != nil {
				slog.Error("platform command registration failed", "project", e.name, "platform", p.Name(), "error", err)
			} else {
				slog.Debug("platform commands registered", "project", e.name, "platform", p.Name(), "count", len(commands))
			}
		}
	}

	// Log summary
	startedCount := len(e.platforms) - len(startErrs)
	if len(startErrs) > 0 {
		slog.Warn("engine started with some failures", "project", e.name, "agent", e.agent.Name(), "started", startedCount, "failed", len(startErrs))
	} else {
		slog.Info("engine started", "project", e.name, "agent", e.agent.Name(), "platforms", len(e.platforms))
	}

	// Only return error if ALL platforms failed
	if len(startErrs) == len(e.platforms) && len(e.platforms) > 0 {
		return startErrs[0] // Return first error
	}
	return nil
}

func (e *Engine) Stop() error {
	// Stop platforms first to prevent new incoming messages
	var errs []error
	for _, p := range e.platforms {
		if err := p.Stop(); err != nil {
			errs = append(errs, fmt.Errorf("stop platform %s: %w", p.Name(), err))
		}
	}

	// Now cancel context and clean up sessions
	e.cancel()

	e.interactiveMu.Lock()
	states := make(map[string]*interactiveState, len(e.interactiveStates))
	for k, v := range e.interactiveStates {
		states[k] = v
		delete(e.interactiveStates, k)
	}
	e.interactiveMu.Unlock()

	for key, state := range states {
		if state.agentSession != nil {
			slog.Debug("engine.Stop: closing agent session", "session", key)
			state.agentSession.Close()
		}
	}

	if err := e.agent.Stop(); err != nil {
		errs = append(errs, fmt.Errorf("stop agent %s: %w", e.agent.Name(), err))
	}
	if len(errs) > 0 {
		return fmt.Errorf("engine stop errors: %v", errs)
	}
	return nil
}

// matchBannedWord returns the first banned word found in content, or "".
func (e *Engine) matchBannedWord(content string) string {
	e.bannedMu.RLock()
	defer e.bannedMu.RUnlock()
	if len(e.bannedWords) == 0 {
		return ""
	}
	lower := strings.ToLower(content)
	for _, w := range e.bannedWords {
		if strings.Contains(lower, w) {
			return w
		}
	}
	return ""
}

// resolveAlias checks if the content (or its first word) matches an alias and replaces it.
func (e *Engine) resolveAlias(content string) string {
	e.aliasMu.RLock()
	defer e.aliasMu.RUnlock()

	if len(e.aliases) == 0 {
		return content
	}

	// Exact match on full content
	if cmd, ok := e.aliases[content]; ok {
		return cmd
	}

	// Match first word, append remaining args
	parts := strings.SplitN(content, " ", 2)
	if cmd, ok := e.aliases[parts[0]]; ok {
		if len(parts) > 1 {
			return cmd + " " + parts[1]
		}
		return cmd
	}
	return content
}

func (e *Engine) handleMessage(p Platform, msg *Message) {
	slog.Info("message received",
		"platform", msg.Platform, "msg_id", msg.MessageID,
		"session", msg.SessionKey, "user", msg.UserName,
		"content_len", len(msg.Content),
		"has_quote", msg.HasQuotedContent(),
		"has_images", len(msg.Images) > 0, "has_files", len(msg.Files) > 0, "has_audio", msg.Audio != nil,
	)

	// Voice message: transcribe to text first
	if msg.Audio != nil {
		e.handleVoiceMessage(p, msg)
		return
	}

	content := strings.TrimSpace(msg.Content)
	if content == "" && len(msg.Images) == 0 && len(msg.Files) == 0 {
		return
	}
	if content == "" && (len(msg.Images) > 0 || len(msg.Files) > 0) {
		e.storePendingAttachments(msg.SessionKey, msg.Images, msg.Files)
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgAttachmentsBuffered, len(msg.Images)+len(msg.Files)))
		return
	}

	content, consumed := e.resolvePendingInteraction(msg.SessionKey, content)
	if consumed {
		return
	}
	msg.Content = content

	if e.handlePendingVoiceConfirmation(p, msg, content) {
		return
	}

	if e.handleReadAloudRequest(p, msg, content) {
		return
	}

	if e.handlePendingCronPromptEdit(p, msg, content) {
		return
	}

	// Resolve aliases: check if the first word (or whole content) matches an alias
	content = e.resolveAlias(content)
	msg.Content = content

	// Rate limit check
	if e.rateLimiter != nil && !e.rateLimiter.Allow(msg.SessionKey) {
		slog.Info("message rate limited", "session", msg.SessionKey, "user", msg.UserName)
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgRateLimited))
		return
	}

	// Banned words check (skip for slash commands)
	if !strings.HasPrefix(content, "/") {
		if word := e.matchBannedWord(content); word != "" {
			slog.Info("message blocked by banned word", "word", word, "user", msg.UserName)
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgBannedWordBlocked))
			return
		}
	}

	if len(msg.Images) == 0 && len(msg.Files) == 0 && strings.HasPrefix(content, "/") {
		if e.handleCommand(p, msg, content) {
			return
		}
		// Unrecognized slash command — fall through to agent as normal message
	}

	// Permission responses bypass the session lock
	if e.handlePendingPermission(p, msg, content) {
		return
	}

	pendingAttach := e.getPendingAttachments(msg.SessionKey)
	if content != "" && pendingAttach != nil {
		msg.Images = append(append([]ImageAttachment{}, pendingAttach.Images...), msg.Images...)
		msg.Files = append(append([]FileAttachment{}, pendingAttach.Files...), msg.Files...)
	}

	session := e.sessions.GetOrCreateActive(msg.SessionKey)
	if !session.TryLock() {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgPreviousProcessing))
		return
	}
	if content != "" && pendingAttach != nil {
		e.clearPendingAttachments(msg.SessionKey)
	}

	slog.Info("processing message",
		"platform", msg.Platform,
		"user", msg.UserName,
		"session", session.ID,
	)

	go e.processInteractiveMessage(p, msg, session)
}

func (e *Engine) storePendingAttachments(sessionKey string, images []ImageAttachment, files []FileAttachment) {
	if len(images) == 0 && len(files) == 0 {
		return
	}
	e.pendingAttachMu.Lock()
	defer e.pendingAttachMu.Unlock()
	if e.pendingAttach[sessionKey] == nil {
		e.pendingAttach[sessionKey] = &pendingAttachments{}
	}
	e.pendingAttach[sessionKey].Images = append(e.pendingAttach[sessionKey].Images, cloneImages(images)...)
	e.pendingAttach[sessionKey].Files = append(e.pendingAttach[sessionKey].Files, cloneFiles(files)...)
}

func (e *Engine) getPendingAttachments(sessionKey string) *pendingAttachments {
	e.pendingAttachMu.Lock()
	defer e.pendingAttachMu.Unlock()
	pending := e.pendingAttach[sessionKey]
	if pending == nil {
		return nil
	}
	return &pendingAttachments{
		Images: cloneImages(pending.Images),
		Files:  cloneFiles(pending.Files),
	}
}

func (e *Engine) clearPendingAttachments(sessionKey string) {
	e.pendingAttachMu.Lock()
	defer e.pendingAttachMu.Unlock()
	delete(e.pendingAttach, sessionKey)
}

func (e *Engine) setPendingCronPromptEdit(sessionKey, jobID string) {
	e.cronEditMu.Lock()
	defer e.cronEditMu.Unlock()
	e.pendingCronEdits[sessionKey] = &pendingCronPromptEdit{JobID: jobID}
}

func (e *Engine) getPendingCronPromptEdit(sessionKey string) *pendingCronPromptEdit {
	e.cronEditMu.Lock()
	defer e.cronEditMu.Unlock()
	edit := e.pendingCronEdits[sessionKey]
	if edit == nil {
		return nil
	}
	cp := *edit
	return &cp
}

func (e *Engine) clearPendingCronPromptEdit(sessionKey string) {
	e.cronEditMu.Lock()
	defer e.cronEditMu.Unlock()
	delete(e.pendingCronEdits, sessionKey)
}

func (e *Engine) clearPendingCronPromptEditIfMatch(sessionKey, jobID string) {
	e.cronEditMu.Lock()
	defer e.cronEditMu.Unlock()
	edit := e.pendingCronEdits[sessionKey]
	if edit != nil && edit.JobID == jobID {
		delete(e.pendingCronEdits, sessionKey)
	}
}

func reviewFlowMapKey(project, sessionKey string) string {
	return project + "|" + sessionKey
}

func reviewerSessionKey(originProject, reviewerProject, originSessionKey string) string {
	return "review:" + originProject + ":" + reviewerProject + ":" + originSessionKey
}

func (e *Engine) getReviewFlow(sessionKey string) *reviewFlow {
	e.reviewMu.Lock()
	defer e.reviewMu.Unlock()
	flow := e.reviewFlows[reviewFlowMapKey(e.name, sessionKey)]
	if flow == nil {
		return nil
	}
	cp := *flow
	return &cp
}

func (e *Engine) reviewerProjects() []string {
	if e.relayManager == nil {
		return nil
	}
	names := e.relayManager.ListEngineNames()
	out := make([]string, 0, len(names))
	for _, name := range names {
		if name == "" || name == e.name {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (e *Engine) reconstructPlatformReply(sessionKey string) (Platform, any, error) {
	platformName, _, err := parseSessionKeyParts(sessionKey)
	if err != nil {
		return nil, nil, err
	}
	for _, p := range e.platforms {
		if p.Name() != platformName {
			continue
		}
		rc, ok := p.(ReplyContextReconstructor)
		if !ok {
			return nil, nil, fmt.Errorf("platform %q does not support reply context reconstruction", platformName)
		}
		replyCtx, err := rc.ReconstructReplyCtx(sessionKey)
		if err != nil {
			return nil, nil, err
		}
		return p, replyCtx, nil
	}
	return nil, nil, fmt.Errorf("platform %q not found", platformName)
}

func (e *Engine) sendCardToSession(sessionKey string, card *Card) error {
	p, replyCtx, err := e.reconstructPlatformReply(sessionKey)
	if err != nil {
		return err
	}
	if cs, ok := p.(CardSender); ok {
		return cs.SendCard(e.ctx, replyCtx, card)
	}
	return p.Send(e.ctx, replyCtx, card.RenderText())
}

func (e *Engine) sendTextToSession(sessionKey, content string) error {
	p, replyCtx, err := e.reconstructPlatformReply(sessionKey)
	if err != nil {
		return err
	}
	return p.Send(e.ctx, replyCtx, content)
}

func (e *Engine) sendChunksWithPrefix(p Platform, replyCtx any, prefix, content string) error {
	text := strings.TrimSpace(content)
	if text == "" {
		return nil
	}
	if prefix != "" {
		text = prefix + text
	}
	for _, chunk := range splitMessage(text, maxPlatformMessageLen) {
		if err := p.Send(e.ctx, replyCtx, chunk); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) runManagedTurn(state *interactiveState, session *Session, agentSessionKey string, prompt string, opts managedTurnOpts) (string, error) {
	if state == nil || state.agentSession == nil {
		return "", fmt.Errorf("failed to start agent session")
	}

	state.mu.Lock()
	p := state.platform
	replyCtx := state.replyCtx
	state.approveAll = opts.AutoApprove
	sessionQuiet := state.quiet
	state.mu.Unlock()

	e.quietMu.RLock()
	globalQuiet := e.quiet
	e.quietMu.RUnlock()
	quiet := globalQuiet || sessionQuiet

	session.AddHistory("user", prompt)
	e.sessions.Save()

	drainEvents(state.agentSession.Events())
	if err := state.agentSession.Send(prompt, nil, nil); err != nil {
		return "", err
	}

	var textParts []string
	toolCount := 0
	waitStart := time.Now()
	firstEventLogged := false

	var idleTimer *time.Timer
	var idleCh <-chan time.Time
	if e.eventIdleTimeout > 0 {
		idleTimer = time.NewTimer(e.eventIdleTimeout)
		defer idleTimer.Stop()
		idleCh = idleTimer.C
	}

	var firstEventTimer *time.Timer
	var firstEventCh <-chan time.Time
	if e.firstEventTimeout > 0 {
		firstEventTimer = time.NewTimer(e.firstEventTimeout)
		defer firstEventTimer.Stop()
		firstEventCh = firstEventTimer.C
	}

	var turnTimer *time.Timer
	var turnCh <-chan time.Time
	if e.turnTimeout > 0 {
		turnTimer = time.NewTimer(e.turnTimeout)
		defer turnTimer.Stop()
		turnCh = turnTimer.C
	}

	events := state.agentSession.Events()
	for {
		var event Event
		var ok bool

		select {
		case event, ok = <-events:
			if !ok {
				if len(textParts) > 0 {
					final := strings.Join(textParts, "")
					session.AddHistory("assistant", final)
					e.sessions.Save()
					return final, nil
				}
				return "", fmt.Errorf("agent process exited without response")
			}
		case <-state.stopped:
			return "", fmt.Errorf("agent session stopped")
		case <-firstEventCh:
			e.cleanupInteractiveState(agentSessionKey)
			return "", fmt.Errorf("agent session timed out waiting for first response")
		case <-turnCh:
			e.cleanupInteractiveState(agentSessionKey)
			return "", fmt.Errorf("agent session timed out before completing the turn")
		case <-idleCh:
			e.cleanupInteractiveState(agentSessionKey)
			return "", fmt.Errorf("agent session timed out (no response)")
		case <-e.ctx.Done():
			return "", e.ctx.Err()
		}

		if idleTimer != nil {
			if !idleTimer.Stop() {
				select {
				case <-idleTimer.C:
				default:
				}
			}
			idleTimer.Reset(e.eventIdleTimeout)
		}

		if !firstEventLogged {
			firstEventLogged = true
			if firstEventTimer != nil {
				if !firstEventTimer.Stop() {
					select {
					case <-firstEventTimer.C:
					default:
					}
				}
				firstEventCh = nil
			}
			if elapsed := time.Since(waitStart); elapsed >= slowAgentFirstEvent {
				slog.Warn("slow managed turn first event", "elapsed", elapsed, "session", agentSessionKey, "event_type", event.Type)
			}
		}

		e.bindAgentSessionID(session, event.SessionID)

		switch event.Type {
		case EventThinking:
			if !opts.Silent && !quiet && event.Content != "" {
				preview := truncateIf(event.Content, e.display.ThinkingMaxLen)
				msg := fmt.Sprintf(e.i18n.T(MsgThinking), preview)
				if err := e.sendChunksWithPrefix(p, replyCtx, opts.Prefix, msg); err != nil {
					return "", err
				}
			}
		case EventToolUse:
			toolCount++
			if !opts.Silent && !quiet {
				inputPreview := truncateIf(event.ToolInput, e.display.ToolMaxLen)
				lineCount := strings.Count(inputPreview, "\n") + 1
				var formattedInput string
				if lineCount > 5 || utf8.RuneCountInString(inputPreview) > 200 {
					formattedInput = fmt.Sprintf("```\n%s\n```", inputPreview)
				} else {
					formattedInput = fmt.Sprintf("`%s`", inputPreview)
				}
				msg := fmt.Sprintf(e.i18n.T(MsgTool), toolCount, event.ToolName, formattedInput)
				if err := e.sendChunksWithPrefix(p, replyCtx, opts.Prefix, msg); err != nil {
					return "", err
				}
			}
		case EventToolResult:
			if opts.Silent || quiet {
				continue
			}
			result := strings.TrimSpace(event.ToolResult)
			if result == "" {
				result = strings.TrimSpace(event.Content)
			}
			if result != "" {
				if err := e.sendChunksWithPrefix(p, replyCtx, opts.Prefix, result); err != nil {
					return "", err
				}
			}
		case EventText:
			if event.Content != "" {
				textParts = append(textParts, event.Content)
			}
		case EventPermissionRequest:
			if !opts.AutoApprove {
				return "", fmt.Errorf("permission request is not supported in automated review flow")
			}
			if err := state.agentSession.RespondPermission(event.RequestID, PermissionResult{
				Behavior:     "allow",
				UpdatedInput: event.ToolInputRaw,
			}); err != nil {
				return "", err
			}
		case EventResult:
			fullResponse := event.Content
			if fullResponse == "" && len(textParts) > 0 {
				fullResponse = strings.Join(textParts, "")
			}
			prompt := detectTextInteractionPrompt(fullResponse)
			displayResponse := fullResponse
			if prompt != nil && strings.Contains(strings.ToLower(fullResponse), "<options>") {
				displayResponse = strings.TrimSpace(prompt.Prompt)
			}
			if displayResponse == "" && prompt == nil {
				displayResponse = e.i18n.T(MsgEmptyResponse)
			}
			if displayResponse != "" {
				session.AddHistory("assistant", displayResponse)
				e.sessions.Save()
				if !opts.Silent {
					if err := e.sendChunksWithPrefix(p, replyCtx, opts.Prefix, displayResponse); err != nil {
						return "", err
					}
				}
				if !opts.Silent && prompt != nil && opts.Prefix == "" {
					followUp := prompt.Description
					if strings.TrimSpace(followUp) == "" {
						followUp = "Choose a reply below, or type your own response."
					}
					e.replyWithInteraction(agentSessionKey, p, replyCtx, followUp, prompt.Choices, false)
				}
				if !opts.Silent && opts.OfferFollowUpCard {
					e.offerResultActionCard(p, replyCtx, displayResponse)
				}
			}
			if e.shouldResetAgentSessionOnResult(displayResponse) {
				slog.Warn("resetting agent session after fatal result", "agent", e.agent.Name(), "session", session.ID, "agent_session", session.AgentSessionID)
				session.mu.Lock()
				session.AgentSessionID = ""
				session.NeedsReplay = true
				session.mu.Unlock()
				e.sessions.Save()
				e.cleanupInteractiveState(agentSessionKey)
			}
			return displayResponse, nil
		case EventError:
			if event.Error != nil {
				_ = e.sendChunksWithPrefix(p, replyCtx, opts.Prefix, fmt.Sprintf(e.i18n.T(MsgError), event.Error))
				return "", event.Error
			}
			return "", fmt.Errorf("agent error")
		}
	}
}

func (e *Engine) handlePendingCronPromptEdit(p Platform, msg *Message, content string) bool {
	edit := e.getPendingCronPromptEdit(msg.SessionKey)
	if edit == nil {
		return false
	}
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return true
	}
	if trimmed == "/cancel" {
		e.clearPendingCronPromptEdit(msg.SessionKey)
		e.reply(p, msg.ReplyCtx, "Cancelled cron prompt editing.")
		e.replyCronCardIfSupported(p, msg, "Cancelled cron prompt editing.")
		return true
	}
	if strings.HasPrefix(trimmed, "/") {
		return false
	}
	if len(trimmed) > maxCronPromptLen {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf("❌ prompt too long (max %d characters)", maxCronPromptLen))
		return true
	}
	if e.cronScheduler == nil {
		e.clearPendingCronPromptEdit(msg.SessionKey)
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCronNotAvailable))
		return true
	}
	job := e.cronScheduler.Store().Get(edit.JobID)
	if job == nil || job.SessionKey != msg.SessionKey {
		e.clearPendingCronPromptEditIfMatch(msg.SessionKey, edit.JobID)
		e.reply(p, msg.ReplyCtx, "❌ cron job not found for this session")
		return true
	}
	if err := e.cronScheduler.UpdateJob(edit.JobID, "prompt", trimmed); err != nil {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf("❌ update cron prompt failed: %v", err))
		return true
	}
	e.clearPendingCronPromptEditIfMatch(msg.SessionKey, edit.JobID)
	e.reply(p, msg.ReplyCtx, fmt.Sprintf("✅ Updated cron `%s` prompt.", edit.JobID))
	e.replyCronCardIfSupported(p, msg, fmt.Sprintf("Updated `%s`.", edit.JobID))
	return true
}

func cloneImages(images []ImageAttachment) []ImageAttachment {
	if len(images) == 0 {
		return nil
	}
	out := make([]ImageAttachment, 0, len(images))
	for _, img := range images {
		cp := img
		if len(img.Data) > 0 {
			cp.Data = append([]byte(nil), img.Data...)
		}
		out = append(out, cp)
	}
	return out
}

func cloneFiles(files []FileAttachment) []FileAttachment {
	if len(files) == 0 {
		return nil
	}
	out := make([]FileAttachment, 0, len(files))
	for _, f := range files {
		cp := f
		if len(f.Data) > 0 {
			cp.Data = append([]byte(nil), f.Data...)
		}
		out = append(out, cp)
	}
	return out
}

func (e *Engine) buildAgentPrompt(session *Session, msg *Message) string {
	base := strings.TrimSpace(msg.Content)
	quoted := strings.TrimSpace(msg.QuotedContent)
	if quoted != "" {
		header := "Reply context"
		if name := strings.TrimSpace(msg.QuotedUserName); name != "" {
			header = fmt.Sprintf("Reply context from %s", name)
		}

		if base == "" {
			base = fmt.Sprintf("%s:\n%s", header, quoted)
		} else {
			base = fmt.Sprintf("%s:\n%s\n\nUser message:\n%s", header, quoted, base)
		}
	}

	if session == nil || !session.NeedsReplay {
		return base
	}

	replay := e.formatSessionReplay(session, 8)
	if replay == "" {
		return base
	}

	if base == "" {
		return fmt.Sprintf("Previous conversation context:\n%s", replay)
	}

	return fmt.Sprintf("Previous conversation context:\n%s\n\nContinue from that context.\n\n%s", replay, base)
}

// ──────────────────────────────────────────────────────────────
// Voice message handling
// ──────────────────────────────────────────────────────────────

func (e *Engine) handleVoiceMessage(p Platform, msg *Message) {
	if !e.speech.Enabled || e.speech.STT == nil {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgVoiceNotEnabled))
		return
	}

	audio := msg.Audio
	if NeedsConversion(audio.Format) && !HasFFmpeg() {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgVoiceNoFFmpeg))
		return
	}

	slog.Info("transcribing voice message",
		"platform", msg.Platform, "user", msg.UserName,
		"format", audio.Format, "size", len(audio.Data),
	)
	e.send(p, msg.ReplyCtx, e.i18n.T(MsgVoiceTranscribing))

	hint := e.buildSpeechHint(msg)
	text, err := TranscribeAudioWithHint(e.ctx, e.speech.STT, audio, e.speech.Language, hint)
	if err != nil {
		slog.Error("speech transcription failed", "error", err)
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgVoiceTranscribeFailed), err))
		return
	}

	text = strings.TrimSpace(text)
	if text == "" {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgVoiceEmpty))
		return
	}

	slog.Info("voice transcribed", "text_len", len(text))

	if e.speech.ConfirmBeforeSend {
		e.storeVoiceConfirmation(msg.SessionKey, text)
		e.replyVoiceConfirmationPrompt(msg.SessionKey, p, msg.ReplyCtx, text)
		return
	}

	e.send(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgVoiceTranscribed), text))

	// Replace audio with transcribed text and re-dispatch
	msg.Audio = nil
	msg.Content = text
	msg.FromVoice = true
	e.handleMessage(p, msg)
}

func (e *Engine) buildSpeechHint(msg *Message) SpeechTranscriptionHint {
	session := e.sessions.GetOrCreateActive(msg.SessionKey)
	history := session.GetHistory(6)

	return SpeechTranscriptionHint{
		ProjectName:    e.speech.ProjectName,
		AgentName:      e.speech.AgentName,
		WorkDir:        e.speech.WorkDir,
		Platform:       msg.Platform,
		SessionKey:     msg.SessionKey,
		RecentHistory:  history,
		TechnicalTerms: collectTechnicalTerms(e.speech.ProjectName, e.speech.AgentName, e.speech.WorkDir, msg.SessionKey, history),
	}
}

func collectTechnicalTerms(projectName, agentName, workDir, sessionKey string, history []HistoryEntry) []string {
	candidates := []string{
		projectName,
		agentName,
		workDir,
		filepath.Base(workDir),
		sessionKey,
		"cc-connect",
		"tmux",
		"git",
		"github",
		"ffmpeg",
		"config.toml",
		"Claude Code",
		"claudecode",
		"Codex",
		"Gemini",
		"Vertex AI",
		"Feishu",
		"Telegram",
		"Slack",
		"Discord",
		"LINE",
	}
	for _, entry := range history {
		candidates = append(candidates, extractTechnicalTerms(entry.Content)...)
	}

	seen := make(map[string]bool)
	terms := make([]string, 0, 24)
	for _, candidate := range candidates {
		term := strings.TrimSpace(candidate)
		if term == "" {
			continue
		}
		if filepath.IsAbs(term) {
			term = filepath.Base(term)
		}
		if seen[term] {
			continue
		}
		seen[term] = true
		terms = append(terms, term)
		if len(terms) >= 24 {
			break
		}
	}
	return terms
}

func extractTechnicalTerms(text string) []string {
	fields := strings.FieldsFunc(text, func(r rune) bool {
		switch {
		case r >= 'a' && r <= 'z':
			return false
		case r >= 'A' && r <= 'Z':
			return false
		case r >= '0' && r <= '9':
			return false
		case r == '-', r == '_', r == '.', r == '/', r == ':':
			return false
		default:
			return true
		}
	})

	var out []string
	for _, field := range fields {
		if len(field) < 3 {
			continue
		}
		if strings.ContainsAny(field, "-_./:") || strings.IndexFunc(field, func(r rune) bool { return r >= 'A' && r <= 'Z' }) >= 0 || strings.IndexFunc(field, func(r rune) bool { return r >= '0' && r <= '9' }) >= 0 {
			out = append(out, field)
		}
	}
	return out
}

func (e *Engine) handlePendingVoiceConfirmation(p Platform, msg *Message, content string) bool {
	pending, ok := e.getVoiceConfirmation(msg.SessionKey)
	if !ok {
		return false
	}

	lower := strings.ToLower(strings.TrimSpace(content))
	switch {
	case isVoiceConfirmResponse(lower):
		e.clearInteractionPrompt(msg.SessionKey)
		e.clearVoiceConfirmation(msg.SessionKey)
		msg.Content = pending.Text
		msg.FromVoice = true
		e.handleMessage(p, msg)
		return true
	case isVoiceModifyResponse(lower):
		e.clearInteractionPrompt(msg.SessionKey)
		pending.AwaitingEdit = true
		e.storeVoiceConfirmation(msg.SessionKey, pending.Text, pending)
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgVoiceEditPrompt))
		return true
	case isVoiceCancelResponse(lower):
		e.clearInteractionPrompt(msg.SessionKey)
		e.clearVoiceConfirmation(msg.SessionKey)
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgVoiceCanceled))
		return true
	}

	if strings.HasPrefix(content, "/") {
		if pending.AwaitingEdit {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgVoiceEditPrompt))
		} else {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgVoiceConfirmHint))
		}
		return true
	}

	pending.Text = content
	pending.AwaitingEdit = false
	e.storeVoiceConfirmation(msg.SessionKey, pending.Text, pending)
	e.replyVoiceConfirmationPrompt(msg.SessionKey, p, msg.ReplyCtx, pending.Text)
	return true
}

func (e *Engine) replyVoiceConfirmationPrompt(sessionKey string, p Platform, replyCtx any, text string) {
	content := fmt.Sprintf(e.i18n.T(MsgVoiceConfirmPrompt), text)
	choices := [][]interactionChoice{
		{
			{ID: "confirm", Label: e.i18n.T(MsgVoiceBtnConfirm), SendText: "voice confirm", MatchTexts: []string{"voice confirm", "confirm"}},
			{ID: "modify", Label: e.i18n.T(MsgVoiceBtnModify), SendText: "voice modify", MatchTexts: []string{"voice modify", "modify"}},
		},
	}
	e.replyWithInteraction(sessionKey, p, replyCtx, content, choices, true)
}

func (e *Engine) storeVoiceConfirmation(sessionKey, text string, existing ...*pendingVoiceConfirmation) {
	e.voiceConfirmMu.Lock()
	defer e.voiceConfirmMu.Unlock()

	if len(existing) > 0 && existing[0] != nil {
		existing[0].Text = text
		e.voiceConfirms[sessionKey] = existing[0]
		return
	}

	e.voiceConfirms[sessionKey] = &pendingVoiceConfirmation{Text: text}
}

func (e *Engine) getVoiceConfirmation(sessionKey string) (*pendingVoiceConfirmation, bool) {
	e.voiceConfirmMu.Lock()
	defer e.voiceConfirmMu.Unlock()

	pending, ok := e.voiceConfirms[sessionKey]
	return pending, ok
}

func (e *Engine) clearVoiceConfirmation(sessionKey string) {
	e.voiceConfirmMu.Lock()
	defer e.voiceConfirmMu.Unlock()
	delete(e.voiceConfirms, sessionKey)
}

func (e *Engine) replyWithInteraction(sessionKey string, p Platform, replyCtx any, content string, rows [][]interactionChoice, strict bool) {
	prompt := &pendingInteractionPrompt{
		Token:   newInteractionToken(),
		Choices: make(map[string]interactionChoice),
		Strict:  strict,
	}

	renderContent := content
	useIndexedButtons := p.Name() == "feishu" && interactionNeedsExpandedList(rows)
	if useIndexedButtons {
		renderContent = appendInteractionOptionList(content, rows)
	}

	buttonRows := make([][]ButtonOption, 0, len(rows))
	fallbackChoices := make([]string, 0, 4)
	buttonIndex := 1
	for _, row := range rows {
		if len(row) == 0 {
			continue
		}
		buttonRow := make([]ButtonOption, 0, len(row))
		for _, choice := range row {
			prompt.Choices[choice.ID] = choice
			label := choice.Label
			if useIndexedButtons {
				label = fmt.Sprintf("%d", buttonIndex)
				buttonIndex++
			}
			buttonRow = append(buttonRow, ButtonOption{
				Text: label,
				Data: buildInteractionCallbackData(prompt.Token, choice.ID),
			})
			fallbackChoices = append(fallbackChoices, choice.SendText)
		}
		buttonRows = append(buttonRows, buttonRow)
	}

	e.promptMu.Lock()
	e.prompts[sessionKey] = prompt
	e.promptMu.Unlock()

	if supportsCards(p) {
		e.replyWithCard(p, replyCtx, interactionCard(renderContent, rows, useIndexedButtons, prompt.Token))
		return
	}

	if bs, ok := p.(InlineButtonSender); ok {
		if err := bs.SendWithButtons(e.ctx, replyCtx, renderContent, buttonRows); err == nil {
			return
		}
	}

	if len(fallbackChoices) > 0 {
		renderContent += "\n\nReply with: `" + strings.Join(fallbackChoices, "` / `") + "`"
	}
	e.reply(p, replyCtx, renderContent)
}

func interactionCard(content string, rows [][]interactionChoice, useIndexedButtons bool, token string) *Card {
	cb := NewCard().Markdown(content)
	buttonIndex := 1
	for _, row := range rows {
		if len(row) == 0 {
			continue
		}
		buttons := make([]CardButton, 0, len(row))
		for _, choice := range row {
			label := choice.Label
			if useIndexedButtons {
				label = fmt.Sprintf("%d", buttonIndex)
				buttonIndex++
			}
			buttons = append(buttons, DefaultBtn(label, buildInteractionCallbackData(token, choice.ID)))
		}
		cb.Buttons(buttons...)
	}
	return cb.Build()
}

func interactionNeedsExpandedList(rows [][]interactionChoice) bool {
	for _, row := range rows {
		for _, choice := range row {
			if strings.TrimSpace(choice.SendText) != strings.TrimSpace(choice.Label) {
				return true
			}
		}
	}
	return false
}

func appendInteractionOptionList(content string, rows [][]interactionChoice) string {
	var options []string
	index := 1
	for _, row := range rows {
		for _, choice := range row {
			label := strings.TrimSpace(choice.SendText)
			if label == "" {
				label = strings.TrimSpace(choice.Label)
			}
			if label == "" {
				continue
			}
			options = append(options, fmt.Sprintf("%d. %s", index, label))
			index++
		}
	}
	if len(options) == 0 {
		return content
	}
	if strings.TrimSpace(content) == "" {
		return "Options:\n" + strings.Join(options, "\n")
	}
	return content + "\n\nOptions:\n" + strings.Join(options, "\n")
}

func (e *Engine) resolvePendingInteraction(sessionKey, content string) (string, bool) {
	e.promptMu.Lock()
	prompt := e.prompts[sessionKey]
	e.promptMu.Unlock()
	if prompt == nil {
		return content, false
	}

	if token, choiceID, ok := parseInteractionCallbackData(content); ok {
		if token != prompt.Token {
			return "", true
		}
		if choice, ok := prompt.Choices[choiceID]; ok {
			e.clearInteractionPrompt(sessionKey)
			return choice.SendText, false
		}
		return "", true
	}

	normalized := strings.ToLower(strings.TrimSpace(content))
	if normalized == "" {
		return content, false
	}
	for _, choice := range prompt.Choices {
		for _, match := range choice.MatchTexts {
			if normalized == strings.ToLower(strings.TrimSpace(match)) {
				e.clearInteractionPrompt(sessionKey)
				return choice.SendText, false
			}
		}
	}

	if !prompt.Strict && !strings.HasPrefix(content, "/") {
		e.clearInteractionPrompt(sessionKey)
	}
	return content, false
}

func (e *Engine) clearInteractionPrompt(sessionKey string) {
	e.promptMu.Lock()
	defer e.promptMu.Unlock()
	delete(e.prompts, sessionKey)
}

func isVoiceConfirmResponse(s string) bool {
	for _, w := range []string{
		"voice confirm", "confirm", "send", "yes", "ok", "好的", "确认", "發送", "发送", "是",
	} {
		if s == w {
			return true
		}
	}
	return false
}

func isVoiceModifyResponse(s string) bool {
	for _, w := range []string{
		"voice modify", "modify", "edit", "change", "修改", "编辑", "編輯",
	} {
		if s == w {
			return true
		}
	}
	return false
}

func isVoiceCancelResponse(s string) bool {
	for _, w := range []string{
		"voice cancel", "cancel", "/cancel", "取消",
	} {
		if s == w {
			return true
		}
	}
	return false
}

// ──────────────────────────────────────────────────────────────
// Permission handling
// ──────────────────────────────────────────────────────────────

func (e *Engine) handlePendingPermission(p Platform, msg *Message, content string) bool {
	e.interactiveMu.Lock()
	state, ok := e.interactiveStates[msg.SessionKey]
	e.interactiveMu.Unlock()
	if !ok || state == nil {
		return false
	}

	state.mu.Lock()
	pending := state.pending
	state.mu.Unlock()
	if pending == nil {
		return false
	}

	lower := strings.ToLower(strings.TrimSpace(content))

	if isApproveAllResponse(lower) {
		state.mu.Lock()
		state.approveAll = true
		state.mu.Unlock()

		if err := state.agentSession.RespondPermission(pending.RequestID, PermissionResult{
			Behavior:     "allow",
			UpdatedInput: pending.ToolInput,
		}); err != nil {
			slog.Error("failed to send permission response", "error", err)
			e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgError), err))
		} else {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgPermissionApproveAll))
		}
	} else if isAllowResponse(lower) {
		if err := state.agentSession.RespondPermission(pending.RequestID, PermissionResult{
			Behavior:     "allow",
			UpdatedInput: pending.ToolInput,
		}); err != nil {
			slog.Error("failed to send permission response", "error", err)
			e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgError), err))
		} else {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgPermissionAllowed))
		}
	} else if isDenyResponse(lower) {
		if err := state.agentSession.RespondPermission(pending.RequestID, PermissionResult{
			Behavior: "deny",
			Message:  "User denied this tool use.",
		}); err != nil {
			slog.Error("failed to send deny response", "error", err)
		}
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgPermissionDenied))
	} else {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgPermissionHint))
		return true
	}

	state.mu.Lock()
	state.pending = nil
	state.mu.Unlock()
	e.clearInteractionPrompt(msg.SessionKey)
	pending.resolve()

	return true
}

func isApproveAllResponse(s string) bool {
	for _, w := range []string{
		"allow all", "allowall", "approve all", "yes all",
		"允许所有", "允许全部", "全部允许", "所有允许", "都允许", "全部同意",
	} {
		if s == w {
			return true
		}
	}
	return false
}

func isAllowResponse(s string) bool {
	for _, w := range []string{"allow", "yes", "y", "ok", "允许", "同意", "可以", "好", "好的", "是", "确认", "approve"} {
		if s == w {
			return true
		}
	}
	return false
}

func isDenyResponse(s string) bool {
	for _, w := range []string{"deny", "no", "n", "reject", "拒绝", "不允许", "不行", "不", "否", "取消", "cancel"} {
		if s == w {
			return true
		}
	}
	return false
}

// ──────────────────────────────────────────────────────────────
// Interactive agent processing
// ──────────────────────────────────────────────────────────────

func (e *Engine) processInteractiveMessage(p Platform, msg *Message, session *Session) {
	defer session.Unlock()

	if e.ctx.Err() != nil {
		return
	}

	turnStart := time.Now()
	prompt := e.buildAgentPrompt(session, msg)

	e.i18n.DetectAndSet(msg.Content)
	session.AddHistory("user", prompt)

	state := e.getOrCreateInteractiveState(msg.SessionKey, p, msg.ReplyCtx, session)

	// Update reply context for this turn
	state.mu.Lock()
	state.platform = p
	state.replyCtx = msg.ReplyCtx
	state.mu.Unlock()

	if state.agentSession == nil {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgError), "failed to start agent session"))
		return
	}

	// Start typing indicator if platform supports it
	var stopTyping func()
	if ti, ok := p.(TypingIndicator); ok {
		stopTyping = ti.StartTyping(e.ctx, msg.ReplyCtx)
	}
	defer func() {
		if stopTyping != nil {
			stopTyping()
		}
	}()

	// Drain any stale events left in the channel from a previous turn.
	// This prevents the next processInteractiveEvents from reading an old
	// EventResult that was pushed after the previous turn already returned.
	drainEvents(state.agentSession.Events())

	sendStart := time.Now()
	state.mu.Lock()
	state.fromVoice = msg.FromVoice
	state.mu.Unlock()
	if err := state.agentSession.Send(prompt, msg.Images, msg.Files); err != nil {
		slog.Error("failed to send prompt", "error", err)

		if !state.agentSession.Alive() {
			e.cleanupInteractiveState(msg.SessionKey)
			e.send(p, msg.ReplyCtx, e.i18n.T(MsgSessionRestarting))

			state = e.getOrCreateInteractiveState(msg.SessionKey, p, msg.ReplyCtx, session)
			if state.agentSession == nil {
				e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgError), "failed to restart agent session"))
				return
			}
			sendStart = time.Now()
			if err := state.agentSession.Send(prompt, msg.Images, msg.Files); err != nil {
				e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgError), err))
				return
			}
		} else {
			e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgError), err))
			return
		}
	}
	if session.NeedsReplay {
		session.mu.Lock()
		session.NeedsReplay = false
		session.mu.Unlock()
		e.sessions.Save()
	}
	if elapsed := time.Since(sendStart); elapsed >= slowAgentSend {
		slog.Warn("slow agent send", "elapsed", elapsed, "session", msg.SessionKey, "content_len", len(msg.Content))
	}

	e.processInteractiveEvents(state, session, msg.SessionKey, msg.MessageID, turnStart)
}

func (e *Engine) getOrCreateInteractiveState(sessionKey string, p Platform, replyCtx any, session *Session) *interactiveState {
	e.interactiveMu.Lock()
	defer e.interactiveMu.Unlock()

	state, ok := e.interactiveStates[sessionKey]
	if ok && state.agentSession != nil && state.agentSession.Alive() {
		return state
	}

	// Preserve quiet setting from existing state (e.g. set via /quiet before session started)
	quietMode := e.defaultQuiet
	if ok && state != nil {
		state.mu.Lock()
		quietMode = state.quiet
		state.mu.Unlock()
	}

	envVars := e.sessionEnv(sessionKey)

	// Check if context is already canceled (e.g. during shutdown/restart)
	if e.ctx.Err() != nil {
		slog.Debug("skipping session start: context canceled", "session_key", sessionKey)
		state = newInteractiveState(nil, p, replyCtx, quietMode)
		e.interactiveStates[sessionKey] = state
		return state
	}

	startAt := time.Now()
	agentSession, err := e.startAgentSession(
		e.ctx,
		session.AgentSessionID,
		envVars,
	)
	startElapsed := time.Since(startAt)
	if err != nil {
		slog.Error("failed to start interactive session", "error", err, "elapsed", startElapsed)
		state = newInteractiveState(nil, p, replyCtx, quietMode)
		e.interactiveStates[sessionKey] = state
		return state
	}
	if startElapsed >= slowAgentStart {
		slog.Warn("slow agent session start", "elapsed", startElapsed, "agent", e.agent.Name(), "session_id", session.AgentSessionID)
	}

	state = newInteractiveState(agentSession, p, replyCtx, quietMode)
	e.interactiveStates[sessionKey] = state

	slog.Info("interactive session started", "session_key", sessionKey, "agent_session", session.AgentSessionID, "elapsed", startElapsed)
	return state
}

func (e *Engine) cleanupInteractiveState(sessionKey string) {
	e.clearInteractionPrompt(sessionKey)
	e.interactiveMu.Lock()
	state, ok := e.interactiveStates[sessionKey]
	delete(e.interactiveStates, sessionKey)
	e.interactiveMu.Unlock()

	if ok && state != nil && state.agentSession != nil {
		state.stop()
		slog.Debug("cleanupInteractiveState: closing agent session", "session", sessionKey)
		closeStart := time.Now()

		done := make(chan struct{})
		go func() {
			state.agentSession.Close()
			close(done)
		}()

		select {
		case <-done:
			if elapsed := time.Since(closeStart); elapsed >= slowAgentClose {
				slog.Warn("slow agent session close", "elapsed", elapsed, "session", sessionKey)
			}
		case <-time.After(10 * time.Second):
			slog.Error("agent session close timed out (10s), abandoning", "session", sessionKey)
		}
	}
}

const defaultEventIdleTimeout = 2 * time.Hour
const defaultFirstEventTimeout = 5 * time.Minute
const defaultTurnTimeout = 45 * time.Minute

func (e *Engine) processInteractiveEvents(state *interactiveState, session *Session, sessionKey string, msgID string, turnStart time.Time) {
	var textParts []string
	toolCount := 0
	waitStart := time.Now()
	firstEventLogged := false
	resetAgentSession := false

	state.mu.Lock()
	sp := newStreamPreview(e.streamPreview, state.platform, state.replyCtx, e.ctx)
	cp := newCompactProgressWriter(e.ctx, state.platform, state.replyCtx)
	state.mu.Unlock()

	// Idle timeout: 0 = disabled
	var idleTimer *time.Timer
	var idleCh <-chan time.Time
	if e.eventIdleTimeout > 0 {
		idleTimer = time.NewTimer(e.eventIdleTimeout)
		defer idleTimer.Stop()
		idleCh = idleTimer.C
	}

	var firstEventTimer *time.Timer
	var firstEventCh <-chan time.Time
	if e.firstEventTimeout > 0 {
		firstEventTimer = time.NewTimer(e.firstEventTimeout)
		defer firstEventTimer.Stop()
		firstEventCh = firstEventTimer.C
	}

	var turnTimer *time.Timer
	var turnCh <-chan time.Time
	if e.turnTimeout > 0 {
		turnTimer = time.NewTimer(e.turnTimeout)
		defer turnTimer.Stop()
		turnCh = turnTimer.C
	}

	events := state.agentSession.Events()
	for {
		var event Event
		var ok bool

		select {
		case event, ok = <-events:
			if !ok {
				goto channelClosed
			}
		case <-state.stopped:
			return
		case <-firstEventCh:
			slog.Error("agent session first event timeout: no initial event received",
				"session_key", sessionKey, "timeout", e.firstEventTimeout, "elapsed", time.Since(turnStart))
			sp.finish("")
			state.mu.Lock()
			p := state.platform
			replyCtx := state.replyCtx
			state.mu.Unlock()
			e.send(p, replyCtx, fmt.Sprintf(e.i18n.T(MsgError), "agent session timed out waiting for first response"))
			e.cleanupInteractiveState(sessionKey)
			return
		case <-turnCh:
			slog.Error("agent session turn timeout: turn exceeded maximum duration",
				"session_key", sessionKey, "timeout", e.turnTimeout, "elapsed", time.Since(turnStart))
			sp.finish("")
			state.mu.Lock()
			p := state.platform
			replyCtx := state.replyCtx
			state.mu.Unlock()
			e.send(p, replyCtx, fmt.Sprintf(e.i18n.T(MsgError), "agent session timed out before completing the turn"))
			e.cleanupInteractiveState(sessionKey)
			return
		case <-idleCh:
			slog.Error("agent session idle timeout: no events for too long, killing session",
				"session_key", sessionKey, "timeout", e.eventIdleTimeout, "elapsed", time.Since(turnStart))
			sp.finish("")
			state.mu.Lock()
			p := state.platform
			replyCtx := state.replyCtx
			state.mu.Unlock()
			e.send(p, replyCtx, fmt.Sprintf(e.i18n.T(MsgError), "agent session timed out (no response)"))
			e.cleanupInteractiveState(sessionKey)
			return
		case <-e.ctx.Done():
			return
		}

		// Reset idle timer after receiving an event
		if idleTimer != nil {
			if !idleTimer.Stop() {
				select {
				case <-idleTimer.C:
				default:
				}
			}
			idleTimer.Reset(e.eventIdleTimeout)
		}

		if !firstEventLogged {
			firstEventLogged = true
			if firstEventTimer != nil {
				if !firstEventTimer.Stop() {
					select {
					case <-firstEventTimer.C:
					default:
					}
				}
				firstEventCh = nil
			}
			if elapsed := time.Since(waitStart); elapsed >= slowAgentFirstEvent {
				slog.Warn("slow agent first event", "elapsed", elapsed, "session", sessionKey, "event_type", event.Type)
			}
		}

		e.bindAgentSessionID(session, event.SessionID)

		state.mu.Lock()
		p := state.platform
		replyCtx := state.replyCtx
		sessionQuiet := state.quiet
		state.mu.Unlock()

		e.quietMu.RLock()
		globalQuiet := e.quiet
		e.quietMu.RUnlock()

		quiet := globalQuiet || sessionQuiet

		switch event.Type {
		case EventThinking:
			if !quiet && event.Content != "" {
				sp.freeze()
				preview := truncateIf(event.Content, e.display.ThinkingMaxLen)
				msg := fmt.Sprintf(e.i18n.T(MsgThinking), preview)
				if !cp.Append(msg) {
					e.send(p, replyCtx, msg)
				}
			}

		case EventToolUse:
			toolCount++
			if !quiet {
				sp.freeze()
				inputPreview := truncateIf(event.ToolInput, e.display.ToolMaxLen)
				// Use code block if content is long (>5 lines or >200 chars), otherwise inline code
				lineCount := strings.Count(inputPreview, "\n") + 1
				var formattedInput string
				if lineCount > 5 || utf8.RuneCountInString(inputPreview) > 200 {
					formattedInput = fmt.Sprintf("```\n%s\n```", inputPreview)
				} else {
					formattedInput = fmt.Sprintf("`%s`", inputPreview)
				}
				msg := fmt.Sprintf(e.i18n.T(MsgTool), toolCount, event.ToolName, formattedInput)
				if !cp.Append(msg) {
					e.send(p, replyCtx, msg)
				}
			}

		case EventText:
			if event.Content != "" {
				textParts = append(textParts, event.Content)
				if sp.canPreview() {
					sp.appendText(event.Content)
				}
			}

		case EventPermissionRequest:
			state.mu.Lock()
			autoApprove := state.approveAll
			state.mu.Unlock()

			if autoApprove {
				slog.Debug("auto-approving (approve-all)", "request_id", event.RequestID, "tool", event.ToolName)
				_ = state.agentSession.RespondPermission(event.RequestID, PermissionResult{
					Behavior:     "allow",
					UpdatedInput: event.ToolInputRaw,
				})
				continue
			}

			// Stop streaming preview before sending permission prompt
			sp.freeze()

			slog.Info("permission request",
				"request_id", event.RequestID,
				"tool", event.ToolName,
			)

			permLimit := e.display.ToolMaxLen
			if permLimit > 0 {
				permLimit = permLimit * 8 / 5 // permission prompts get ~1.6x more room
			}
			prompt := fmt.Sprintf(e.i18n.T(MsgPermissionPrompt), event.ToolName, truncateIf(event.ToolInput, permLimit))
			e.sendPermissionPrompt(sessionKey, p, replyCtx, prompt)

			pending := &pendingPermission{
				RequestID:    event.RequestID,
				ToolName:     event.ToolName,
				ToolInput:    event.ToolInputRaw,
				InputPreview: event.ToolInput,
				Resolved:     make(chan struct{}),
			}
			state.mu.Lock()
			state.pending = pending
			state.mu.Unlock()

			// Stop idle timer while waiting for user permission response;
			// the user may take a long time to decide, and we don't want
			// the idle timeout to kill the session during that wait.
			if idleTimer != nil {
				idleTimer.Stop()
			}

			select {
			case <-pending.Resolved:
				slog.Info("permission resolved", "request_id", event.RequestID)
			case <-state.stopped:
				return
			case <-e.ctx.Done():
				return
			}

			// Restart idle timer after permission is resolved
			if idleTimer != nil {
				idleTimer.Reset(e.eventIdleTimeout)
			}

		case EventResult:
			cp.Finalize("done")
			if event.SessionID != "" {
				session.mu.Lock()
				session.AgentSessionID = event.SessionID
				session.mu.Unlock()
			}

			fullResponse := event.Content
			if fullResponse == "" && len(textParts) > 0 {
				fullResponse = strings.Join(textParts, "")
			}
			prompt := detectTextInteractionPrompt(fullResponse)
			displayResponse := fullResponse
			if prompt != nil && strings.Contains(strings.ToLower(fullResponse), "<options>") {
				displayResponse = strings.TrimSpace(prompt.Prompt)
			}
			if displayResponse == "" && prompt == nil {
				displayResponse = e.i18n.T(MsgEmptyResponse)
			}

			if displayResponse != "" {
				session.AddHistory("assistant", displayResponse)
				e.sessions.Save()
			}

			if e.shouldResetAgentSessionOnResult(displayResponse) {
				slog.Warn("resetting agent session after fatal result", "agent", e.agent.Name(), "session", session.ID, "agent_session", session.AgentSessionID)
				session.mu.Lock()
				session.AgentSessionID = ""
				session.NeedsReplay = true
				session.mu.Unlock()
				e.sessions.Save()
				resetAgentSession = true
			}

			turnDuration := time.Since(turnStart)
			slog.Info("turn complete",
				"session", session.ID,
				"agent_session", session.AgentSessionID,
				"msg_id", msgID,
				"tools", toolCount,
				"response_len", len(displayResponse),
				"turn_duration", turnDuration,
			)

			replyStart := time.Now()

			// If streaming preview was active, try to finalize in-place
			if displayResponse != "" {
				if sp.finish(displayResponse) {
					slog.Debug("EventResult: finalized via stream preview", "response_len", len(displayResponse))
				} else {
					slog.Debug("EventResult: sending via p.Send (preview inactive or failed)", "response_len", len(displayResponse), "chunks", len(splitMessage(displayResponse, maxPlatformMessageLen)))
					for _, chunk := range splitMessage(displayResponse, maxPlatformMessageLen) {
						if err := p.Send(e.ctx, replyCtx, chunk); err != nil {
							slog.Error("failed to send reply", "error", err, "msg_id", msgID)
							return
						}
					}
				}
			} else {
				sp.finish("")
			}

			if elapsed := time.Since(replyStart); elapsed >= slowPlatformSend {
				slog.Warn("slow final reply send", "platform", p.Name(), "elapsed", elapsed, "response_len", len(displayResponse))
			}

			if prompt != nil {
				followUp := prompt.Description
				if strings.TrimSpace(followUp) == "" {
					followUp = "Choose a reply below, or type your own response."
				}
				e.replyWithInteraction(sessionKey, p, replyCtx, followUp, prompt.Choices, false)
			}

			if displayResponse != "" {
				e.offerResultActionCard(p, replyCtx, displayResponse)
			}

			// TTS: async voice reply if enabled
			if displayResponse != "" && e.tts != nil && e.tts.Enabled && e.tts.TTS != nil {
				state.mu.Lock()
				fromVoice := state.fromVoice
				state.mu.Unlock()
				mode := e.tts.GetTTSMode()
				if mode == "always" || (mode == "voice_only" && fromVoice) {
					go e.sendTTSReply(p, replyCtx, displayResponse)
				}
			}

			if resetAgentSession {
				slog.Info("cleaning up interactive state after fatal result", "session_key", sessionKey, "session", session.ID)
				e.cleanupInteractiveState(sessionKey)
			}

			return

		case EventError:
			cp.Finalize("error")
			sp.finish("") // clean up preview on error
			if event.Error != nil {
				slog.Error("agent error", "error", event.Error)
				e.send(p, replyCtx, fmt.Sprintf(e.i18n.T(MsgError), event.Error))
			}
			return
		}
	}

channelClosed:
	// Channel closed - process exited unexpectedly
	slog.Warn("agent process exited", "session_key", sessionKey)
	cp.Finalize("stopped")
	e.cleanupInteractiveState(sessionKey)

	if len(textParts) > 0 {
		state.mu.Lock()
		p := state.platform
		replyCtx := state.replyCtx
		state.mu.Unlock()

		fullResponse := strings.Join(textParts, "")
		session.AddHistory("assistant", fullResponse)

		if sp.finish(fullResponse) {
			slog.Debug("stream preview: finalized in-place (process exited)")
		} else {
			for _, chunk := range splitMessage(fullResponse, maxPlatformMessageLen) {
				e.send(p, replyCtx, chunk)
			}
		}
	}
}

func (e *Engine) shouldResetAgentSessionOnResult(content string) bool {
	if strings.TrimSpace(content) == "" {
		return false
	}
	if e.agent == nil || e.agent.Name() != "claudecode" {
		return false
	}
	lower := strings.ToLower(content)
	return strings.Contains(lower, "could not process image") ||
		(strings.Contains(lower, "api error: 400") && strings.Contains(lower, "image"))
}

func (e *Engine) formatSessionReplay(session *Session, limit int) string {
	if session == nil {
		return ""
	}
	history := session.GetHistory(limit)
	if len(history) == 0 {
		return ""
	}

	var lines []string
	for _, entry := range history {
		content := strings.TrimSpace(entry.Content)
		if content == "" {
			continue
		}
		if e.shouldResetAgentSessionOnResult(content) {
			continue
		}
		role := "User"
		if entry.Role == "assistant" {
			role = "Assistant"
		}
		lines = append(lines, fmt.Sprintf("%s: %s", role, truncateIf(content, 1200)))
	}
	return strings.Join(lines, "\n")
}

func (e *Engine) bindAgentSessionID(session *Session, agentSessionID string) {
	if session == nil || agentSessionID == "" {
		return
	}

	session.mu.Lock()
	if session.AgentSessionID != "" {
		session.mu.Unlock()
		return
	}
	session.AgentSessionID = agentSessionID
	pendingName := session.Name
	session.mu.Unlock()

	if pendingName != "" && pendingName != "session" && pendingName != "default" {
		e.sessions.SetSessionName(agentSessionID, pendingName)
	}
	e.sessions.Save()
}

// ──────────────────────────────────────────────────────────────
// Command handling
// ──────────────────────────────────────────────────────────────

// builtinCommands maps canonical command names to their aliases/full names.
// The first entry is the canonical name used for prefix matching.
var builtinCommands = []struct {
	names []string
	id    string
}{
	{[]string{"new"}, "new"},
	{[]string{"list", "sessions"}, "list"},
	{[]string{"switch"}, "switch"},
	{[]string{"name", "rename"}, "name"},
	{[]string{"current"}, "current"},
	{[]string{"status"}, "status"},
	{[]string{"history"}, "history"},
	{[]string{"allow"}, "allow"},
	{[]string{"model"}, "model"},
	{[]string{"mode"}, "mode"},
	{[]string{"lang"}, "lang"},
	{[]string{"quiet"}, "quiet"},
	{[]string{"provider"}, "provider"},
	{[]string{"memory"}, "memory"},
	{[]string{"cron"}, "cron"},
	{[]string{"compress", "compact"}, "compress"},
	{[]string{"stop"}, "stop"},
	{[]string{"help"}, "help"},
	{[]string{"version"}, "version"},
	{[]string{"commands", "command", "cmd"}, "commands"},
	{[]string{"skills", "skill"}, "skills"},
	{[]string{"config"}, "config"},
	{[]string{"doctor"}, "doctor"},
	{[]string{"upgrade", "update"}, "upgrade"},
	{[]string{"restart"}, "restart"},
	{[]string{"alias"}, "alias"},
	{[]string{"delete", "del", "rm"}, "delete"},
	{[]string{"bind"}, "bind"},
	{[]string{"search", "find"}, "search"},
	{[]string{"shell", "sh", "exec", "run"}, "shell"},
	{[]string{"dir", "cd", "chdir", "workdir"}, "dir"},
	{[]string{"tts"}, "tts"},
}

// matchPrefix finds a unique command matching the given prefix.
// Returns the command id or "" if no match / ambiguous.
func matchPrefix(prefix string, candidates []struct {
	names []string
	id    string
}) string {
	// Exact match first
	for _, c := range candidates {
		for _, n := range c.names {
			if prefix == n {
				return c.id
			}
		}
	}
	// Prefix match
	var matched string
	for _, c := range candidates {
		for _, n := range c.names {
			if strings.HasPrefix(n, prefix) {
				if matched != "" && matched != c.id {
					return "" // ambiguous
				}
				matched = c.id
				break
			}
		}
	}
	return matched
}

// matchSubCommand does prefix matching against a flat list of subcommand names.
func matchSubCommand(input string, candidates []string) string {
	for _, c := range candidates {
		if input == c {
			return c
		}
	}
	var matched string
	for _, c := range candidates {
		if strings.HasPrefix(c, input) {
			if matched != "" {
				return input // ambiguous → return raw input (will hit default)
			}
			matched = c
		}
	}
	if matched != "" {
		return matched
	}
	return input
}

func (e *Engine) handleCommand(p Platform, msg *Message, raw string) bool {
	parts := strings.Fields(raw)
	cmd := strings.ToLower(strings.TrimPrefix(parts[0], "/"))
	args := parts[1:]

	cmdID := matchPrefix(cmd, builtinCommands)

	if cmdID != "" && e.disabledCmds[cmdID] {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgCommandDisabled), "/"+cmdID))
		return true
	}

	switch cmdID {
	case "new":
		e.cmdNew(p, msg, args)
	case "list":
		e.cmdList(p, msg, args)
	case "switch":
		e.cmdSwitch(p, msg, args)
	case "name":
		e.cmdName(p, msg, args)
	case "current":
		e.cmdCurrent(p, msg)
	case "status":
		e.cmdStatus(p, msg)
	case "history":
		e.cmdHistory(p, msg, args)
	case "allow":
		e.cmdAllow(p, msg, args)
	case "model":
		e.cmdModel(p, msg, args)
	case "mode":
		e.cmdMode(p, msg, args)
	case "lang":
		e.cmdLang(p, msg, args)
	case "quiet":
		e.cmdQuiet(p, msg, args)
	case "provider":
		e.cmdProvider(p, msg, args)
	case "memory":
		e.cmdMemory(p, msg, args)
	case "cron":
		e.cmdCron(p, msg, args)
	case "compress":
		e.cmdCompress(p, msg)
	case "stop":
		e.cmdStop(p, msg)
	case "help":
		e.cmdHelp(p, msg)
	case "version":
		e.reply(p, msg.ReplyCtx, VersionInfo)
	case "commands":
		e.cmdCommands(p, msg, args)
	case "skills":
		e.cmdSkills(p, msg)
	case "config":
		e.cmdConfig(p, msg, args)
	case "doctor":
		e.cmdDoctor(p, msg)
	case "upgrade":
		e.cmdUpgrade(p, msg, args)
	case "restart":
		e.cmdRestart(p, msg)
	case "alias":
		e.cmdAlias(p, msg, args)
	case "delete":
		e.cmdDelete(p, msg, args)
	case "bind":
		e.cmdBind(p, msg, args)
	case "search":
		e.cmdSearch(p, msg, args)
	case "shell":
		e.cmdShell(p, msg, raw)
	case "dir":
		e.cmdDir(p, msg, args)
	case "tts":
		e.cmdTTS(p, msg, args)
	default:
		if custom, ok := e.commands.Resolve(cmd); ok {
			e.executeCustomCommand(p, msg, custom, args)
			return true
		}
		if skill := e.skills.Resolve(cmd); skill != nil {
			e.executeSkill(p, msg, skill, args)
			return true
		}
		// Not a cc-connect command — notify user, then fall through to agent
		e.send(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgUnknownCommand), "/"+cmd))
		return false
	}
	return true
}

func (e *Engine) cmdNew(p Platform, msg *Message, args []string) {
	slog.Info("cmdNew: cleaning up old session", "session_key", msg.SessionKey)
	e.cleanupInteractiveState(msg.SessionKey)
	slog.Info("cmdNew: cleanup done, creating new session", "session_key", msg.SessionKey)
	name := ""
	if len(args) > 0 {
		name = strings.Join(args, " ")
	}
	s := e.sessions.NewSession(msg.SessionKey, name)
	if name != "" {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgNewSessionCreatedName), name))
	} else {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgNewSessionCreated))
	}
	_ = s
}

const listPageSize = 20

func (e *Engine) cmdList(p Platform, msg *Message, args []string) {
	agentSessions, err := e.agent.ListSessions(e.ctx)
	if err != nil {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgListError), err))
		return
	}
	if len(agentSessions) == 0 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgListEmpty))
		return
	}

	total := len(agentSessions)
	totalPages := (total + listPageSize - 1) / listPageSize

	page := 1
	if len(args) > 0 {
		if n, err := strconv.Atoi(args[0]); err == nil && n > 0 {
			page = n
		}
	}
	if page > totalPages {
		page = totalPages
	}

	if supportsCards(p) {
		e.replyWithCard(p, msg.ReplyCtx, e.renderSessionCard(msg.SessionKey, page, ""))
		return
	}

	start := (page - 1) * listPageSize
	end := start + listPageSize
	if end > total {
		end = total
	}

	agentName := e.agent.Name()
	activeSession := e.sessions.GetOrCreateActive(msg.SessionKey)
	activeAgentID := activeSession.AgentSessionID

	var sb strings.Builder
	if totalPages > 1 {
		sb.WriteString(fmt.Sprintf(e.i18n.T(MsgListTitlePaged), agentName, total, page, totalPages))
	} else {
		sb.WriteString(fmt.Sprintf(e.i18n.T(MsgListTitle), agentName, total))
	}
	for i := start; i < end; i++ {
		s := agentSessions[i]
		marker := "◻"
		if s.ID == activeAgentID {
			marker = "▶"
		}
		displayName := e.sessions.GetSessionName(s.ID)
		if displayName != "" {
			displayName = "📌 " + displayName
		} else {
			displayName = strings.ReplaceAll(s.Summary, "\n", " ")
			displayName = strings.Join(strings.Fields(displayName), " ")
			if displayName == "" {
				displayName = "(empty)"
			}
			if len([]rune(displayName)) > 40 {
				displayName = string([]rune(displayName)[:40]) + "…"
			}
		}
		sb.WriteString(fmt.Sprintf("%s **%d.** %s · **%d** msgs · %s\n",
			marker, i+1, displayName, s.MessageCount, s.ModifiedAt.Format("01-02 15:04")))
	}
	if totalPages > 1 {
		sb.WriteString(fmt.Sprintf(e.i18n.T(MsgListPageHint), page, totalPages))
	}
	sb.WriteString(e.i18n.T(MsgListSwitchHint))
	e.reply(p, msg.ReplyCtx, sb.String())
}

func sessionDisplayName(sm *SessionManager, s AgentSessionInfo) string {
	displayName := sm.GetSessionName(s.ID)
	if displayName != "" {
		return "📌 " + displayName
	}
	displayName = strings.ReplaceAll(s.Summary, "\n", " ")
	displayName = strings.Join(strings.Fields(displayName), " ")
	if displayName == "" {
		displayName = "(empty)"
	}
	if len([]rune(displayName)) > 40 {
		displayName = string([]rune(displayName)[:40]) + "…"
	}
	return displayName
}

func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return strings.TrimSpace(s)
}

func parseSessionCardPage(args string) int {
	fields := strings.Fields(strings.TrimSpace(args))
	if len(fields) == 0 {
		return 1
	}
	if len(fields) == 1 {
		if n, err := strconv.Atoi(fields[0]); err == nil && n > 0 {
			return n
		}
		return 1
	}
	if len(fields) >= 3 && (fields[0] == "switch" || fields[0] == "delete") {
		if n, err := strconv.Atoi(fields[2]); err == nil && n > 0 {
			return n
		}
	}
	return 1
}

func (e *Engine) renderSessionCard(sessionKey string, page int, notice string) *Card {
	agentSessions, err := e.agent.ListSessions(e.ctx)
	if err != nil {
		return e.simpleCard("Sessions", "orange", fmt.Sprintf(e.i18n.T(MsgListError), err))
	}
	if len(agentSessions) == 0 {
		return e.simpleCard("Sessions", "blue", e.i18n.T(MsgListEmpty))
	}

	total := len(agentSessions)
	totalPages := (total + listPageSize - 1) / listPageSize
	if page < 1 {
		page = 1
	}
	if page > totalPages {
		page = totalPages
	}

	start := (page - 1) * listPageSize
	end := start + listPageSize
	if end > total {
		end = total
	}

	activeSession := e.sessions.GetOrCreateActive(sessionKey)
	activeAgentID := activeSession.AgentSessionID

	title := fmt.Sprintf("%s Sessions", e.agent.Name())
	if totalPages > 1 {
		title = fmt.Sprintf("%s Sessions (%d/%d)", e.agent.Name(), page, totalPages)
	}

	cb := NewCard().Title(title, "blue")
	if notice != "" {
		cb.Note(notice)
	}

	for i := start; i < end; i++ {
		s := agentSessions[i]
		marker := "◻"
		if s.ID == activeAgentID {
			marker = "▶"
		}
		cb.Markdown(fmt.Sprintf("%s **%d.** %s · **%d** msgs · %s",
			marker, i+1, sessionDisplayName(e.sessions, s), s.MessageCount, s.ModifiedAt.Format("01-02 15:04")))

		var btns []CardButton
		if s.ID == activeAgentID {
			btns = append(btns, PrimaryBtn("Current", fmt.Sprintf("nav:/sessions %d", page)))
		} else {
			btns = append(btns, DefaultBtn("Switch", fmt.Sprintf("act:/sessions switch %s %d", s.ID, page)))
		}
		btns = append(btns, DangerBtn("Delete", fmt.Sprintf("act:/sessions delete %s %d", s.ID, page)))
		cb.ButtonsEqual(btns...)
	}

	if totalPages > 1 {
		var nav []CardButton
		if page > 1 {
			nav = append(nav, DefaultBtn("Prev", fmt.Sprintf("nav:/sessions %d", page-1)))
		}
		if page < totalPages {
			nav = append(nav, DefaultBtn("Next", fmt.Sprintf("nav:/sessions %d", page+1)))
		}
		if len(nav) > 0 {
			cb.ButtonsEqual(nav...)
		}
	}
	backBtn := e.cardBackButton()
	if page > 1 {
		backBtn = DefaultBtn("Back", "nav:/sessions 1")
	}
	cb.Buttons(backBtn)
	return cb.Build()
}

func (e *Engine) cmdSwitch(p Platform, msg *Message, args []string) {
	if len(args) == 0 {
		e.reply(p, msg.ReplyCtx, "Usage: /switch <number | id_prefix | name>")
		return
	}
	query := strings.TrimSpace(strings.Join(args, " "))

	slog.Info("cmdSwitch: listing agent sessions", "session_key", msg.SessionKey)
	agentSessions, err := e.agent.ListSessions(e.ctx)
	if err != nil {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf("❌ %v", err))
		return
	}

	matched := e.matchSession(agentSessions, query)
	if matched == nil {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgSwitchNoMatch), query))
		return
	}

	slog.Info("cmdSwitch: cleaning up old session", "session_key", msg.SessionKey)
	e.cleanupInteractiveState(msg.SessionKey)
	slog.Info("cmdSwitch: cleanup done", "session_key", msg.SessionKey)

	session := e.sessions.GetOrCreateActive(msg.SessionKey)
	session.AgentSessionID = matched.ID
	session.Name = matched.Summary
	session.ClearHistory()
	e.sessions.Save()

	shortID := matched.ID
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}
	displayName := e.sessions.GetSessionName(matched.ID)
	if displayName == "" {
		displayName = matched.Summary
	}
	e.reply(p, msg.ReplyCtx,
		e.i18n.Tf(MsgSwitchSuccess, displayName, shortID, matched.MessageCount))
}

func (e *Engine) switchSessionByID(sessionKey, id string) string {
	agentSessions, err := e.agent.ListSessions(e.ctx)
	if err != nil {
		return fmt.Sprintf("❌ %v", err)
	}

	var matched *AgentSessionInfo
	for i := range agentSessions {
		if agentSessions[i].ID == id {
			matched = &agentSessions[i]
			break
		}
	}
	if matched == nil {
		return fmt.Sprintf(e.i18n.T(MsgSwitchNoMatch), id)
	}

	e.cleanupInteractiveState(sessionKey)

	session := e.sessions.GetOrCreateActive(sessionKey)
	session.AgentSessionID = matched.ID
	session.Name = matched.Summary
	session.ClearHistory()
	e.sessions.Save()

	shortID := matched.ID
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}
	displayName := e.sessions.GetSessionName(matched.ID)
	if displayName == "" {
		displayName = matched.Summary
	}
	return e.i18n.Tf(MsgSwitchSuccess, displayName, shortID, matched.MessageCount)
}

// matchSession resolves a user query to an agent session. Priority:
//  1. Numeric index (1-based, matching /list output)
//  2. Exact custom name match (case-insensitive)
//  3. Session ID prefix match
//  4. Custom name prefix match (case-insensitive)
//  5. Summary substring match (case-insensitive)
func (e *Engine) matchSession(sessions []AgentSessionInfo, query string) *AgentSessionInfo {
	if len(sessions) == 0 {
		return nil
	}

	// 1. Numeric index
	if idx, err := strconv.Atoi(query); err == nil && idx >= 1 && idx <= len(sessions) {
		return &sessions[idx-1]
	}

	queryLower := strings.ToLower(query)

	// 2. Exact custom name match
	for i := range sessions {
		name := e.sessions.GetSessionName(sessions[i].ID)
		if name != "" && strings.ToLower(name) == queryLower {
			return &sessions[i]
		}
	}

	// 3. Session ID prefix match
	for i := range sessions {
		if strings.HasPrefix(sessions[i].ID, query) {
			return &sessions[i]
		}
	}

	// 4. Custom name prefix match
	for i := range sessions {
		name := e.sessions.GetSessionName(sessions[i].ID)
		if name != "" && strings.HasPrefix(strings.ToLower(name), queryLower) {
			return &sessions[i]
		}
	}

	// 5. Summary substring match
	for i := range sessions {
		if sessions[i].Summary != "" && strings.Contains(strings.ToLower(sessions[i].Summary), queryLower) {
			return &sessions[i]
		}
	}

	return nil
}

func (e *Engine) cmdShell(p Platform, msg *Message, raw string) {
	// Strip the command prefix ("/shell ", "/sh ", "/exec ", "/run ")
	shellCmd := raw
	for _, prefix := range []string{"/shell ", "/sh ", "/exec ", "/run "} {
		if strings.HasPrefix(strings.ToLower(raw), prefix) {
			shellCmd = raw[len(prefix):]
			break
		}
	}
	shellCmd = strings.TrimSpace(shellCmd)

	if shellCmd == "" {
		e.reply(p, msg.ReplyCtx, "Usage: /shell <command>\nExample: /shell ls -la")
		return
	}

	go func() {
		workDir := ""
		if wd, ok := e.agent.(interface{ GetWorkDir() string }); ok {
			workDir = wd.GetWorkDir()
		}
		if workDir == "" {
			workDir, _ = os.Getwd()
		}

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		cmd := exec.CommandContext(ctx, "sh", "-c", shellCmd)
		cmd.Dir = workDir
		output, err := cmd.CombinedOutput()

		if ctx.Err() == context.DeadlineExceeded {
			e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgCommandTimeout), shellCmd))
			return
		}

		result := strings.TrimSpace(string(output))
		if err != nil && result == "" {
			result = err.Error()
		}
		if result == "" {
			result = "(no output)"
		}
		if len(result) > 4000 {
			result = result[:3997] + "..."
		}

		e.reply(p, msg.ReplyCtx, fmt.Sprintf("$ %s\n```\n%s\n```", shellCmd, result))
	}()
}

func (e *Engine) cmdDir(p Platform, msg *Message, args []string) {
	switcher, ok := e.agent.(WorkDirSwitcher)
	if !ok {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgDirNotSupported))
		return
	}

	if len(args) == 0 {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgDirCurrent, switcher.GetWorkDir()))
		return
	}
	if len(args) == 1 {
		switch strings.ToLower(strings.TrimSpace(args[0])) {
		case "help", "-h", "--help":
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgDirUsage))
			return
		}
	}

	newDir := filepath.Clean(strings.Join(args, " "))
	if !filepath.IsAbs(newDir) {
		baseDir := switcher.GetWorkDir()
		if baseDir == "" {
			baseDir, _ = os.Getwd()
		}
		newDir = filepath.Join(baseDir, newDir)
	}

	info, err := os.Stat(newDir)
	if err != nil || !info.IsDir() {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgDirInvalidPath, newDir))
		return
	}

	switcher.SetWorkDir(newDir)
	e.cleanupInteractiveState(msg.SessionKey)

	s := e.sessions.GetOrCreateActive(msg.SessionKey)
	s.AgentSessionID = ""
	s.ClearHistory()
	e.sessions.Save()

	e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgDirChanged, newDir))
}

// cmdSearch searches sessions by name or message content.
// Usage: /search <keyword>
func (e *Engine) cmdSearch(p Platform, msg *Message, args []string) {
	if len(args) == 0 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgSearchUsage))
		return
	}

	keyword := strings.ToLower(strings.Join(args, " "))

	// Get all agent sessions
	agentSessions, err := e.agent.ListSessions(e.ctx)
	if err != nil {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgSearchError), err))
		return
	}

	type searchResult struct {
		id           string
		name         string
		summary      string
		matchType    string // "name" or "message"
		messageCount int
	}

	var results []searchResult

	for _, s := range agentSessions {
		// Check session name (custom name or summary)
		customName := e.sessions.GetSessionName(s.ID)
		displayName := customName
		if displayName == "" {
			displayName = s.Summary
		}

		// Match by name/summary
		if strings.Contains(strings.ToLower(displayName), keyword) {
			results = append(results, searchResult{
				id:           s.ID,
				name:         displayName,
				summary:      s.Summary,
				matchType:    "name",
				messageCount: s.MessageCount,
			})
			continue
		}

		// Match by session ID prefix
		if strings.HasPrefix(strings.ToLower(s.ID), keyword) {
			results = append(results, searchResult{
				id:           s.ID,
				name:         displayName,
				summary:      s.Summary,
				matchType:    "id",
				messageCount: s.MessageCount,
			})
			continue
		}
	}

	if len(results) == 0 {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgSearchNoResult), keyword))
		return
	}

	// Build result message
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(e.i18n.T(MsgSearchResult), len(results), keyword))

	for i, r := range results {
		shortID := r.id
		if len(shortID) > 12 {
			shortID = shortID[:12]
		}
		sb.WriteString(fmt.Sprintf("\n%d. [%s] %s", i+1, shortID, r.name))
	}

	sb.WriteString("\n\n" + e.i18n.T(MsgSearchHint))

	e.reply(p, msg.ReplyCtx, sb.String())
}

func (e *Engine) cmdName(p Platform, msg *Message, args []string) {
	if len(args) == 0 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgNameUsage))
		return
	}

	current := e.sessions.GetOrCreateActive(msg.SessionKey)

	// Check if first arg is a number → naming a specific session by list index
	var targetID string
	var name string

	if idx, err := strconv.Atoi(args[0]); err == nil && idx >= 1 {
		// /name <number> <name...>
		if len(args) < 2 {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgNameUsage))
			return
		}
		agentSessions, err := e.agent.ListSessions(e.ctx)
		if err != nil {
			e.reply(p, msg.ReplyCtx, fmt.Sprintf("❌ %v", err))
			return
		}
		if idx > len(agentSessions) {
			e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgSwitchNoSession), idx))
			return
		}
		targetID = agentSessions[idx-1].ID
		name = strings.Join(args[1:], " ")
	} else {
		// /name <name...> → current local session (and linked agent session if present)
		targetID = current.AgentSessionID
		name = strings.Join(args, " ")
	}

	name = strings.TrimSpace(name)
	if name == "" {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgNameUsage))
		return
	}

	if idx, err := strconv.Atoi(args[0]); err == nil && idx >= 1 {
		e.sessions.RenameByAgentSessionID(targetID, name)
		if targetID != "" {
			e.sessions.SetSessionName(targetID, name)
		}
	} else {
		e.sessions.RenameActiveSession(msg.SessionKey, name)
		if targetID != "" {
			e.sessions.SetSessionName(targetID, name)
		}
	}

	shortID := targetID
	if shortID == "" {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgNameSet), name, "local"))
		return
	}
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}
	e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgNameSet), name, shortID))
}

func (e *Engine) cmdCurrent(p Platform, msg *Message) {
	s := e.sessions.GetOrCreateActive(msg.SessionKey)
	agentID := s.AgentSessionID
	if agentID == "" {
		agentID = e.i18n.T(MsgSessionNotStarted)
	}
	e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgCurrentSession), s.Name, agentID, len(s.History)))
}

func (e *Engine) cmdStatus(p Platform, msg *Message) {
	// Platforms
	platNames := make([]string, len(e.platforms))
	for i, pl := range e.platforms {
		platNames[i] = pl.Name()
	}
	platformStr := strings.Join(platNames, ", ")
	if len(platNames) == 0 {
		platformStr = "-"
	}

	// Uptime
	uptimeStr := formatDurationI18n(time.Since(e.startedAt), e.i18n.CurrentLang())

	// Language
	cur := e.i18n.CurrentLang()
	langStr := fmt.Sprintf("%s (%s)", string(cur), langDisplayName(cur))

	// Mode (optional)
	var modeStr string
	if ms, ok := e.agent.(ModeSwitcher); ok {
		mode := ms.GetMode()
		if mode != "" {
			modeStr = e.i18n.Tf(MsgStatusMode, mode)
		}
	}

	// Quiet mode
	e.quietMu.RLock()
	globalQuiet := e.quiet
	e.quietMu.RUnlock()

	e.interactiveMu.Lock()
	state, hasState := e.interactiveStates[msg.SessionKey]
	e.interactiveMu.Unlock()

	sessionQuiet := false
	if hasState && state != nil {
		state.mu.Lock()
		sessionQuiet = state.quiet
		state.mu.Unlock()
	}

	quietStr := e.i18n.T(MsgQuietOffShort)
	if globalQuiet || sessionQuiet {
		quietStr = e.i18n.T(MsgQuietOnShort)
	}
	modeStr += e.i18n.Tf(MsgStatusQuiet, quietStr)

	// Session info
	s := e.sessions.GetOrCreateActive(msg.SessionKey)
	sessionDisplayName := e.sessions.GetSessionName(s.AgentSessionID)
	if sessionDisplayName == "" {
		sessionDisplayName = s.Name
	}
	sessionStr := e.i18n.Tf(MsgStatusSession, sessionDisplayName, len(s.History))

	// Cron jobs
	var cronStr string
	if e.cronScheduler != nil {
		jobs := e.cronScheduler.Store().ListBySessionKey(msg.SessionKey)
		if len(jobs) > 0 {
			enabledCount := 0
			for _, j := range jobs {
				if j.Enabled {
					enabledCount++
				}
			}
			cronStr = e.i18n.Tf(MsgStatusCron, len(jobs), enabledCount)
		}
	}

	e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgStatusTitle,
		e.name,
		e.agent.Name(),
		platformStr,
		uptimeStr,
		langStr,
		modeStr,
		sessionStr,
		cronStr,
	))
}

func cronTimeFormat(t, now time.Time) string {
	if t.Year() != now.Year() {
		return "2006-01-02 15:04"
	}
	return "01-02 15:04"
}

func formatDurationI18n(d time.Duration, lang Language) string {
	d = d.Round(time.Second)
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	minutes := int(d.Minutes()) % 60

	switch lang {
	case LangChinese, LangTraditionalChinese:
		if days > 0 {
			return fmt.Sprintf("%d天 %d小时 %d分钟", days, hours, minutes)
		}
		if hours > 0 {
			return fmt.Sprintf("%d小时 %d分钟", hours, minutes)
		}
		return fmt.Sprintf("%d分钟", minutes)
	case LangJapanese:
		if days > 0 {
			return fmt.Sprintf("%d日 %d時間 %d分", days, hours, minutes)
		}
		if hours > 0 {
			return fmt.Sprintf("%d時間 %d分", hours, minutes)
		}
		return fmt.Sprintf("%d分", minutes)
	case LangSpanish:
		if days > 0 {
			return fmt.Sprintf("%d días %dh %dm", days, hours, minutes)
		}
		if hours > 0 {
			return fmt.Sprintf("%dh %dm", hours, minutes)
		}
		return fmt.Sprintf("%dm", minutes)
	default:
		if days > 0 {
			return fmt.Sprintf("%dd %dh %dm", days, hours, minutes)
		}
		if hours > 0 {
			return fmt.Sprintf("%dh %dm", hours, minutes)
		}
		return fmt.Sprintf("%dm", minutes)
	}
}

func (e *Engine) cmdHistory(p Platform, msg *Message, args []string) {
	s := e.sessions.GetOrCreateActive(msg.SessionKey)
	n := 10
	if len(args) > 0 {
		if v, err := strconv.Atoi(args[0]); err == nil && v > 0 {
			n = v
		}
	}

	entries := s.GetHistory(n)

	// Fallback: load from agent backend if in-memory history is empty
	if len(entries) == 0 && s.AgentSessionID != "" {
		if hp, ok := e.agent.(HistoryProvider); ok {
			if agentEntries, err := hp.GetSessionHistory(e.ctx, s.AgentSessionID, n); err == nil {
				entries = agentEntries
			}
		}
	}

	if len(entries) == 0 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgHistoryEmpty))
		return
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📜 History (last %d):\n\n", len(entries)))
	for _, h := range entries {
		icon := "👤"
		if h.Role == "assistant" {
			icon = "🤖"
		}
		content := h.Content
		if len([]rune(content)) > 200 {
			content = string([]rune(content)[:200]) + "..."
		}
		sb.WriteString(fmt.Sprintf("%s [%s]\n%s\n\n", icon, h.Timestamp.Format("15:04:05"), content))
	}
	e.reply(p, msg.ReplyCtx, sb.String())
}

func (e *Engine) cmdLang(p Platform, msg *Message, args []string) {
	if len(args) == 0 {
		cur := e.i18n.CurrentLang()
		name := langDisplayName(cur)
		text := e.i18n.Tf(MsgLangCurrent, name)
		buttons := [][]ButtonOption{
			{
				{Text: "English", Data: "cmd:/lang en"},
				{Text: "中文", Data: "cmd:/lang zh"},
				{Text: "繁體中文", Data: "cmd:/lang zh-TW"},
			},
			{
				{Text: "日本語", Data: "cmd:/lang ja"},
				{Text: "Español", Data: "cmd:/lang es"},
				{Text: "Auto", Data: "cmd:/lang auto"},
			},
		}
		e.replyWithButtons(p, msg.ReplyCtx, text, buttons)
		return
	}

	target := strings.ToLower(strings.TrimSpace(args[0]))
	var lang Language
	switch target {
	case "en", "english":
		lang = LangEnglish
	case "zh", "cn", "chinese", "中文":
		lang = LangChinese
	case "zh-tw", "zh_tw", "zhtw", "繁體", "繁体":
		lang = LangTraditionalChinese
	case "ja", "jp", "japanese", "日本語":
		lang = LangJapanese
	case "es", "spanish", "español":
		lang = LangSpanish
	case "auto":
		lang = LangAuto
	default:
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgLangInvalid))
		return
	}

	e.i18n.SetLang(lang)
	name := langDisplayName(lang)
	e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgLangChanged, name))
}

func langDisplayName(lang Language) string {
	switch lang {
	case LangEnglish:
		return "English"
	case LangChinese:
		return "中文"
	case LangTraditionalChinese:
		return "繁體中文"
	case LangJapanese:
		return "日本語"
	case LangSpanish:
		return "Español"
	default:
		return "Auto"
	}
}

func (e *Engine) cmdHelp(p Platform, msg *Message) {
	if supportsCards(p) {
		e.replyWithCard(p, msg.ReplyCtx, e.renderHelpCard())
		return
	}
	e.reply(p, msg.ReplyCtx, e.i18n.T(MsgHelp))
}

func supportsCards(p Platform) bool {
	if p == nil {
		return false
	}
	if _, ok := p.(CardSender); ok {
		return true
	}
	if _, ok := p.(InlineButtonSender); ok {
		return true
	}
	return false
}

func (e *Engine) replyWithCard(p Platform, replyCtx any, card *Card) {
	if card == nil {
		slog.Error("replyWithCard: nil card", "platform", p.Name())
		return
	}
	if cs, ok := p.(CardSender); ok {
		if err := cs.ReplyCard(e.ctx, replyCtx, card); err != nil {
			slog.Error("card reply failed", "platform", p.Name(), "error", err)
		}
		return
	}
	if bs, ok := p.(InlineButtonSender); ok && card.HasButtons() {
		if err := bs.SendWithButtons(e.ctx, replyCtx, card.RenderText(), card.CollectButtons()); err == nil {
			return
		}
	}
	e.reply(p, replyCtx, card.RenderText())
}

func (e *Engine) cardBackButton() CardButton {
	return DefaultBtn("Back", "nav:/help")
}

func (e *Engine) simpleCard(title, color, content string) *Card {
	return NewCard().Title(title, color).Markdown(content).Buttons(e.cardBackButton()).Build()
}

func (e *Engine) renderHelpCard() *Card {
	return NewCard().
		Title("Help", "blue").
		Markdown(e.i18n.T(MsgHelp)).
		ButtonsEqual(
			DefaultBtn("/cron", "nav:/cron"),
		).
		Build()
}

// GetAllCommands returns all available commands for bot menu registration.
// It includes built-in commands (with localized descriptions) and custom commands.
func (e *Engine) GetAllCommands() []BotCommandInfo {
	var commands []BotCommandInfo

	// Collect built-in  commands (use primary name, first in names list)
	seenCmds := make(map[string]bool)
	for _, c := range builtinCommands {
		if len(c.names) == 0 {
			continue
		}
		// Use id as primary
		primaryName := c.id
		if seenCmds[primaryName] {
			continue
		}
		seenCmds[primaryName] = true

		// Skip disabled commands
		if e.disabledCmds[c.id] {
			continue
		}

		commands = append(commands, BotCommandInfo{
			Command:     primaryName,
			Description: e.i18n.T(MsgKey(primaryName)),
		})
	}

	// Collect custom commands from CommandRegistry
	for _, c := range e.commands.ListAll() {
		if seenCmds[strings.ToLower(c.Name)] {
			continue
		}
		seenCmds[strings.ToLower(c.Name)] = true

		desc := c.Description
		if desc == "" {
			desc = "Custom command"
		}

		commands = append(commands, BotCommandInfo{
			Command:     c.Name,
			Description: desc,
		})
	}

	// Collect skills
	for _, s := range e.skills.ListAll() {
		if seenCmds[strings.ToLower(s.Name)] {
			continue
		}
		seenCmds[strings.ToLower(s.Name)] = true

		desc := s.Description
		if desc == "" {
			desc = "Skill"
		}

		commands = append(commands, BotCommandInfo{
			Command:     s.Name,
			Description: desc,
		})
	}

	return commands
}

func (e *Engine) cmdModel(p Platform, msg *Message, args []string) {
	switcher, ok := e.agent.(ModelSwitcher)
	if !ok {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgModelNotSupported))
		return
	}

	fetchCtx, cancel := context.WithTimeout(e.ctx, 10*time.Second)
	defer cancel()
	models := switcher.AvailableModels(fetchCtx)

	if len(args) == 0 {
		var sb strings.Builder
		current := switcher.GetModel()
		if current == "" {
			sb.WriteString(e.i18n.T(MsgModelDefault))
		} else {
			sb.WriteString(e.i18n.Tf(MsgModelCurrent, current))
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
		sb.WriteString(e.i18n.T(MsgModelListTitle))
		for i, m := range models {
			marker := "  "
			if m.Name == current {
				marker = "> "
			}
			desc := m.Desc
			if desc != "" {
				desc = " — " + desc
			}
			sb.WriteString(fmt.Sprintf("%s%d. %s%s\n", marker, i+1, m.Name, desc))
		}
		sb.WriteString("\n")
		sb.WriteString(e.i18n.T(MsgModelUsage))

		// Build button rows (max 3 per row); use numeric index to stay within
		// Telegram's 64-byte callback_data limit for long model names.
		var buttons [][]ButtonOption
		var row []ButtonOption
		for i, m := range models {
			label := m.Name
			if m.Name == current {
				label = "▶ " + label
			}
			row = append(row, ButtonOption{Text: label, Data: fmt.Sprintf("cmd:/model %d", i+1)})
			if len(row) >= 3 {
				buttons = append(buttons, row)
				row = nil
			}
		}
		if len(row) > 0 {
			buttons = append(buttons, row)
		}
		e.replyWithButtons(p, msg.ReplyCtx, sb.String(), buttons)
		return
	}

	target := args[0]
	if idx, err := strconv.Atoi(target); err == nil && idx >= 1 && idx <= len(models) {
		target = models[idx-1].Name
	}

	switcher.SetModel(target)
	e.cleanupInteractiveState(msg.SessionKey)

	s := e.sessions.GetOrCreateActive(msg.SessionKey)
	s.AgentSessionID = ""
	s.ClearHistory()
	e.sessions.Save()

	e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgModelChanged, target))
}

func (e *Engine) cmdMode(p Platform, msg *Message, args []string) {
	switcher, ok := e.agent.(ModeSwitcher)
	if !ok {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgModeNotSupported))
		return
	}

	if len(args) == 0 {
		current := switcher.GetMode()
		modes := switcher.PermissionModes()
		var sb strings.Builder
		zhLike := e.i18n.IsZhLike()
		for _, m := range modes {
			marker := "  "
			if m.Key == current {
				marker = "▶ "
			}
			if zhLike {
				sb.WriteString(fmt.Sprintf("%s**%s** — %s\n", marker, m.NameZh, m.DescZh))
			} else {
				sb.WriteString(fmt.Sprintf("%s**%s** — %s\n", marker, m.Name, m.Desc))
			}
		}
		sb.WriteString(e.modeUsageText(modes))

		var buttons [][]ButtonOption
		var row []ButtonOption
		for _, m := range modes {
			label := m.Name
			if zhLike {
				label = m.NameZh
			}
			if m.Key == current {
				label = "▶ " + label
			}
			row = append(row, ButtonOption{Text: label, Data: "cmd:/mode " + m.Key})
			if len(row) >= 2 {
				buttons = append(buttons, row)
				row = nil
			}
		}
		if len(row) > 0 {
			buttons = append(buttons, row)
		}
		e.replyWithButtons(p, msg.ReplyCtx, sb.String(), buttons)
		return
	}

	target := strings.ToLower(args[0])
	switcher.SetMode(target)
	newMode := switcher.GetMode()

	e.cleanupInteractiveState(msg.SessionKey)

	modes := switcher.PermissionModes()
	displayName := newMode
	zhLike := e.i18n.IsZhLike()
	for _, m := range modes {
		if m.Key == newMode {
			if zhLike {
				displayName = m.NameZh
			} else {
				displayName = m.Name
			}
			break
		}
	}
	e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgModeChanged), displayName))
}

func (e *Engine) cmdQuiet(p Platform, msg *Message, args []string) {
	// /quiet global — toggle global quiet for all sessions
	if len(args) > 0 && args[0] == "global" {
		e.quietMu.Lock()
		e.quiet = !e.quiet
		quiet := e.quiet
		e.quietMu.Unlock()

		if quiet {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgQuietGlobalOn))
		} else {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgQuietGlobalOff))
		}
		return
	}

	// /quiet — toggle per-session quiet
	e.interactiveMu.Lock()
	state, ok := e.interactiveStates[msg.SessionKey]
	e.interactiveMu.Unlock()

	if !ok || state == nil {
		state = newInteractiveState(nil, p, msg.ReplyCtx, true)
		e.interactiveMu.Lock()
		e.interactiveStates[msg.SessionKey] = state
		e.interactiveMu.Unlock()
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgQuietOn))
		return
	}

	state.mu.Lock()
	state.quiet = !state.quiet
	quiet := state.quiet
	state.mu.Unlock()

	if quiet {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgQuietOn))
	} else {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgQuietOff))
	}
}

func (e *Engine) cmdTTS(p Platform, msg *Message, args []string) {
	if e.tts == nil || !e.tts.Enabled || e.tts.TTS == nil {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTTSNotEnabled))
		return
	}
	if len(args) == 0 {
		providerStr := e.tts.Provider
		if providerStr == "" {
			providerStr = "unknown"
		}
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgTTSStatus), e.tts.GetTTSMode(), providerStr))
		return
	}
	switch args[0] {
	case "always", "voice_only":
		mode := args[0]
		e.tts.SetTTSMode(mode)
		if e.ttsSaveFunc != nil {
			if err := e.ttsSaveFunc(mode); err != nil {
				slog.Warn("tts: failed to persist mode", "error", err)
			}
		}
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgTTSSwitched), mode))
	default:
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTTSUsage))
	}
}

func (e *Engine) cmdStop(p Platform, msg *Message) {
	e.interactiveMu.Lock()
	state, ok := e.interactiveStates[msg.SessionKey]
	e.interactiveMu.Unlock()

	if !ok || state == nil {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgNoExecution))
		return
	}

	// Cancel pending permission if any
	state.mu.Lock()
	pending := state.pending
	quietMode := state.quiet
	if pending != nil {
		state.pending = nil
	}
	state.mu.Unlock()
	if pending != nil {
		pending.resolve()
	}

	e.cleanupInteractiveState(msg.SessionKey)

	// Preserve quiet preference across stop
	if quietMode {
		e.interactiveMu.Lock()
		if s, ok := e.interactiveStates[msg.SessionKey]; ok {
			s.mu.Lock()
			s.quiet = quietMode
			s.mu.Unlock()
		} else {
			e.interactiveStates[msg.SessionKey] = newInteractiveState(nil, p, msg.ReplyCtx, quietMode)
		}
		e.interactiveMu.Unlock()
	}

	e.reply(p, msg.ReplyCtx, e.i18n.T(MsgExecutionStopped))
}

func (e *Engine) cmdCompress(p Platform, msg *Message) {
	compressor, ok := e.agent.(ContextCompressor)
	if !ok || compressor.CompressCommand() == "" {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCompressNotSupported))
		return
	}

	e.interactiveMu.Lock()
	state, hasState := e.interactiveStates[msg.SessionKey]
	e.interactiveMu.Unlock()

	if !hasState || state == nil || state.agentSession == nil || !state.agentSession.Alive() {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCompressNoSession))
		return
	}

	session := e.sessions.GetOrCreateActive(msg.SessionKey)
	if !session.TryLock() {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgPreviousProcessing))
		return
	}

	e.send(p, msg.ReplyCtx, e.i18n.T(MsgCompressing))

	go func() {
		defer session.Unlock()

		state.mu.Lock()
		state.platform = p
		state.replyCtx = msg.ReplyCtx
		state.mu.Unlock()

		drainEvents(state.agentSession.Events())

		cmd := compressor.CompressCommand()
		if err := state.agentSession.Send(cmd, nil, nil); err != nil {
			e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgError), err))
			if !state.agentSession.Alive() {
				e.cleanupInteractiveState(msg.SessionKey)
			}
			return
		}

		e.processCompressEvents(state, msg.SessionKey, p, msg.ReplyCtx)
	}()
}

// processCompressEvents drains agent events after a compress command.
// Unlike processInteractiveEvents it does NOT record history and treats
// an empty result as success rather than "(empty response)".
func (e *Engine) processCompressEvents(state *interactiveState, sessionKey string, p Platform, replyCtx any) {
	var textParts []string
	events := state.agentSession.Events()

	var idleTimer *time.Timer
	var idleCh <-chan time.Time
	if e.eventIdleTimeout > 0 {
		idleTimer = time.NewTimer(e.eventIdleTimeout)
		defer idleTimer.Stop()
		idleCh = idleTimer.C
	}

	for {
		var event Event
		var ok bool

		select {
		case event, ok = <-events:
			if !ok {
				e.cleanupInteractiveState(sessionKey)
				if len(textParts) > 0 {
					e.send(p, replyCtx, strings.Join(textParts, ""))
				} else {
					e.reply(p, replyCtx, e.i18n.T(MsgCompressDone))
				}
				return
			}
		case <-state.stopped:
			return
		case <-idleCh:
			e.send(p, replyCtx, fmt.Sprintf(e.i18n.T(MsgError), "compress timed out"))
			e.cleanupInteractiveState(sessionKey)
			return
		case <-e.ctx.Done():
			return
		}

		if idleTimer != nil {
			if !idleTimer.Stop() {
				select {
				case <-idleTimer.C:
				default:
				}
			}
			idleTimer.Reset(e.eventIdleTimeout)
		}

		switch event.Type {
		case EventText:
			if event.Content != "" {
				textParts = append(textParts, event.Content)
			}
		case EventResult:
			result := event.Content
			if result == "" && len(textParts) > 0 {
				result = strings.Join(textParts, "")
			}
			if result != "" {
				e.send(p, replyCtx, result)
			} else {
				e.reply(p, replyCtx, e.i18n.T(MsgCompressDone))
			}
			return
		case EventError:
			if event.Error != nil {
				e.reply(p, replyCtx, fmt.Sprintf(e.i18n.T(MsgError), event.Error))
			}
			return
		case EventPermissionRequest:
			_ = state.agentSession.RespondPermission(event.RequestID, PermissionResult{
				Behavior:     "allow",
				UpdatedInput: event.ToolInputRaw,
			})
		}
	}
}

func (e *Engine) cmdAllow(p Platform, msg *Message, args []string) {
	if len(args) == 0 {
		if auth, ok := e.agent.(ToolAuthorizer); ok {
			tools := auth.GetAllowedTools()
			if len(tools) == 0 {
				e.reply(p, msg.ReplyCtx, e.i18n.T(MsgNoToolsAllowed))
			} else {
				e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgCurrentTools), strings.Join(tools, ", ")))
			}
		} else {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgToolAuthNotSupported))
		}
		return
	}

	toolName := strings.TrimSpace(args[0])
	if auth, ok := e.agent.(ToolAuthorizer); ok {
		if err := auth.AddAllowedTools(toolName); err != nil {
			e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgToolAllowFailed), err))
			return
		}
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgToolAllowedNew), toolName))
	} else {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgToolAuthNotSupported))
	}
}

func (e *Engine) cmdProvider(p Platform, msg *Message, args []string) {
	switcher, ok := e.agent.(ProviderSwitcher)
	if !ok {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgProviderNotSupported))
		return
	}

	if len(args) == 0 {
		current := switcher.GetActiveProvider()
		providers := switcher.ListProviders()
		if current == nil && len(providers) == 0 {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgProviderNone))
			return
		}
		if current != nil {
			e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgProviderCurrent), current.Name))
			return
		}
		// Has providers but no active one — show the list
		var sb strings.Builder
		sb.WriteString(e.i18n.T(MsgProviderListTitle))
		for _, prov := range providers {
			marker := "  "
			if current != nil && prov.Name == current.Name {
				marker = "▶ "
			}
			detail := prov.Name
			if prov.BaseURL != "" {
				detail += " (" + prov.BaseURL + ")"
			}
			if prov.Model != "" {
				detail += " [" + prov.Model + "]"
			}
			sb.WriteString(fmt.Sprintf("%s%s\n", marker, detail))
		}
		sb.WriteString("\n" + e.i18n.T(MsgProviderSwitchHint))
		e.reply(p, msg.ReplyCtx, sb.String())
		return
	}

	sub := matchSubCommand(strings.ToLower(args[0]), []string{
		"list", "add", "remove", "switch", "current", "clear", "reset", "none",
	})
	switch sub {
	case "list":
		providers := switcher.ListProviders()
		if len(providers) == 0 {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgProviderListEmpty))
			return
		}
		current := switcher.GetActiveProvider()
		var sb strings.Builder
		sb.WriteString(e.i18n.T(MsgProviderListTitle))
		for _, prov := range providers {
			marker := "  "
			if current != nil && prov.Name == current.Name {
				marker = "▶ "
			}
			detail := prov.Name
			if prov.BaseURL != "" {
				detail += " (" + prov.BaseURL + ")"
			}
			if prov.Model != "" {
				detail += " [" + prov.Model + "]"
			}
			sb.WriteString(fmt.Sprintf("%s%s\n", marker, detail))
		}
		sb.WriteString("\n" + e.i18n.T(MsgProviderSwitchHint))
		e.reply(p, msg.ReplyCtx, sb.String())

	case "add":
		e.cmdProviderAdd(p, msg, switcher, args[1:])

	case "remove", "rm", "delete":
		e.cmdProviderRemove(p, msg, switcher, args[1:])

	case "switch":
		if len(args) < 2 {
			e.reply(p, msg.ReplyCtx, "Usage: /provider switch <name>")
			return
		}
		e.switchProvider(p, msg, switcher, args[1])

	case "current":
		current := switcher.GetActiveProvider()
		if current == nil {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgProviderNone))
			return
		}
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgProviderCurrent), current.Name))

	case "clear", "reset", "none":
		switcher.SetActiveProvider("")
		e.cleanupInteractiveState(msg.SessionKey)
		if e.providerSaveFunc != nil {
			if err := e.providerSaveFunc(""); err != nil {
				slog.Error("failed to save provider", "error", err)
			}
		}
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgProviderCleared))

	default:
		e.switchProvider(p, msg, switcher, args[0])
	}
}

func (e *Engine) cmdProviderAdd(p Platform, msg *Message, switcher ProviderSwitcher, args []string) {
	if len(args) == 0 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgProviderAddUsage))
		return
	}

	var prov ProviderConfig

	// Join args back; detect JSON (starts with '{') vs positional
	raw := strings.Join(args, " ")
	raw = strings.TrimSpace(raw)

	if strings.HasPrefix(raw, "{") {
		// JSON format: /provider add {"name":"relay","api_key":"sk-xxx",...}
		var jp struct {
			Name    string            `json:"name"`
			APIKey  string            `json:"api_key"`
			BaseURL string            `json:"base_url"`
			Model   string            `json:"model"`
			Env     map[string]string `json:"env"`
		}
		if err := json.Unmarshal([]byte(raw), &jp); err != nil {
			e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgProviderAddFailed), "invalid JSON: "+err.Error()))
			return
		}
		if jp.Name == "" {
			e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgProviderAddFailed), "\"name\" is required"))
			return
		}
		prov = ProviderConfig{Name: jp.Name, APIKey: jp.APIKey, BaseURL: jp.BaseURL, Model: jp.Model, Env: jp.Env}
	} else {
		// Positional: /provider add <name> <api_key> [base_url] [model]
		if len(args) < 2 {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgProviderAddUsage))
			return
		}
		prov.Name = args[0]
		prov.APIKey = args[1]
		if len(args) > 2 {
			prov.BaseURL = args[2]
		}
		if len(args) > 3 {
			prov.Model = args[3]
		}
	}

	// Check for duplicates
	for _, existing := range switcher.ListProviders() {
		if existing.Name == prov.Name {
			e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgProviderAddFailed), fmt.Sprintf("provider %q already exists", prov.Name)))
			return
		}
	}

	// Add to runtime
	updated := append(switcher.ListProviders(), prov)
	switcher.SetProviders(updated)

	// Persist to config
	if e.providerAddSaveFunc != nil {
		if err := e.providerAddSaveFunc(prov); err != nil {
			slog.Error("failed to persist provider", "error", err)
		}
	}

	e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgProviderAdded), prov.Name, prov.Name))
}

func (e *Engine) cmdProviderRemove(p Platform, msg *Message, switcher ProviderSwitcher, args []string) {
	if len(args) == 0 {
		e.reply(p, msg.ReplyCtx, "Usage: /provider remove <name>")
		return
	}
	name := args[0]

	providers := switcher.ListProviders()
	found := false
	var remaining []ProviderConfig
	for _, prov := range providers {
		if prov.Name == name {
			found = true
		} else {
			remaining = append(remaining, prov)
		}
	}

	if !found {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgProviderNotFound), name))
		return
	}

	// If removing the active provider, clear it
	active := switcher.GetActiveProvider()
	switcher.SetProviders(remaining)
	if active != nil && active.Name == name {
		// No active provider after removal
		slog.Info("removed active provider, clearing selection", "name", name)
	}

	// Persist
	if e.providerRemoveSaveFunc != nil {
		if err := e.providerRemoveSaveFunc(name); err != nil {
			slog.Error("failed to persist provider removal", "error", err)
		}
	}

	e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgProviderRemoved), name))
}

func (e *Engine) switchProvider(p Platform, msg *Message, switcher ProviderSwitcher, name string) {
	if !switcher.SetActiveProvider(name) {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgProviderNotFound), name))
		return
	}
	e.cleanupInteractiveState(msg.SessionKey)

	if e.providerSaveFunc != nil {
		if err := e.providerSaveFunc(name); err != nil {
			slog.Error("failed to save provider", "error", err)
		}
	}

	e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgProviderSwitched), name))
}

// ──────────────────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────────────────

// SendToSession sends a message to an active session from an external caller (API/CLI).
// If sessionKey is empty, it picks the first active session.
func (e *Engine) SendToSession(sessionKey, message string) error {
	e.interactiveMu.Lock()
	defer e.interactiveMu.Unlock()

	var state *interactiveState
	if sessionKey != "" {
		state = e.interactiveStates[sessionKey]
	} else {
		// Pick the first active session
		for _, s := range e.interactiveStates {
			state = s
			break
		}
	}

	if state == nil {
		return fmt.Errorf("no active session found (key=%q)", sessionKey)
	}

	state.mu.Lock()
	p := state.platform
	replyCtx := state.replyCtx
	state.mu.Unlock()

	if p == nil {
		return fmt.Errorf("no active session found (key=%q)", sessionKey)
	}

	return p.Send(e.ctx, replyCtx, message)
}

// sendPermissionPrompt sends a permission prompt, using inline buttons when the platform supports them.
func (e *Engine) sendPermissionPrompt(sessionKey string, p Platform, replyCtx any, prompt string) {
	e.replyWithInteraction(sessionKey, p, replyCtx, prompt, [][]interactionChoice{
		{
			{ID: "allow", Label: e.i18n.T(MsgPermBtnAllow), SendText: "allow", MatchTexts: []string{"allow", "yes", "approve"}},
			{ID: "deny", Label: e.i18n.T(MsgPermBtnDeny), SendText: "deny", MatchTexts: []string{"deny", "no", "reject"}},
		},
		{
			{ID: "allow_all", Label: e.i18n.T(MsgPermBtnAllowAll), SendText: "allow all", MatchTexts: []string{"allow all", "approve all"}},
		},
	}, true)
}

// send wraps p.Send with error logging and slow-operation warnings.
func (e *Engine) send(p Platform, replyCtx any, content string) {
	start := time.Now()
	if err := p.Send(e.ctx, replyCtx, content); err != nil {
		slog.Error("platform send failed", "platform", p.Name(), "error", err, "content_len", len(content))
	}
	if elapsed := time.Since(start); elapsed >= slowPlatformSend {
		slog.Warn("slow platform send", "platform", p.Name(), "elapsed", elapsed, "content_len", len(content))
	}
}

// drainEvents discards any buffered events from the channel.
// Called before a new turn to prevent stale events from a previous turn's
// agent process from being mistaken for the new turn's response.
func drainEvents(ch <-chan Event) {
	drained := 0
	for {
		select {
		case <-ch:
			drained++
		default:
			if drained > 0 {
				slog.Warn("drained stale events from previous turn", "count", drained)
			}
			return
		}
	}
}

// reply wraps p.Reply with error logging and slow-operation warnings.
func (e *Engine) reply(p Platform, replyCtx any, content string) {
	start := time.Now()
	if err := p.Reply(e.ctx, replyCtx, content); err != nil {
		slog.Error("platform reply failed", "platform", p.Name(), "error", err, "content_len", len(content))
	}
	if elapsed := time.Since(start); elapsed >= slowPlatformSend {
		slog.Warn("slow platform reply", "platform", p.Name(), "elapsed", elapsed, "content_len", len(content))
	}
}

// replyWithButtons sends a reply with inline buttons if the platform supports it,
// otherwise falls back to plain text reply.
func (e *Engine) replyWithButtons(p Platform, replyCtx any, content string, buttons [][]ButtonOption) {
	if bs, ok := p.(InlineButtonSender); ok {
		if err := bs.SendWithButtons(e.ctx, replyCtx, content, buttons); err == nil {
			return
		}
	}
	e.reply(p, replyCtx, content)
}

// ──────────────────────────────────────────────────────────────
// /memory command
// ──────────────────────────────────────────────────────────────

func (e *Engine) cmdMemory(p Platform, msg *Message, args []string) {
	mp, ok := e.agent.(MemoryFileProvider)
	if !ok {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgMemoryNotSupported))
		return
	}

	if len(args) == 0 {
		// /memory — show project memory
		e.showMemoryFile(p, msg, mp.ProjectMemoryFile(), false)
		return
	}

	sub := matchSubCommand(strings.ToLower(args[0]), []string{"add", "global", "show", "help"})
	switch sub {
	case "add":
		text := strings.TrimSpace(strings.Join(args[1:], " "))
		if text == "" {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgMemoryAddUsage))
			return
		}
		e.appendMemoryFile(p, msg, mp.ProjectMemoryFile(), text)

	case "global":
		if len(args) == 1 {
			// /memory global — show global memory
			e.showMemoryFile(p, msg, mp.GlobalMemoryFile(), true)
			return
		}
		if strings.ToLower(args[1]) == "add" {
			text := strings.TrimSpace(strings.Join(args[2:], " "))
			if text == "" {
				e.reply(p, msg.ReplyCtx, e.i18n.T(MsgMemoryAddUsage))
				return
			}
			e.appendMemoryFile(p, msg, mp.GlobalMemoryFile(), text)
		} else {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgMemoryAddUsage))
		}

	case "show":
		e.showMemoryFile(p, msg, mp.ProjectMemoryFile(), false)

	case "help", "--help", "-h":
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgMemoryAddUsage))

	default:
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgMemoryAddUsage))
	}
}

func (e *Engine) showMemoryFile(p Platform, msg *Message, filePath string, isGlobal bool) {
	if filePath == "" {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgMemoryNotSupported))
		return
	}

	data, err := os.ReadFile(filePath)
	if err != nil || len(strings.TrimSpace(string(data))) == 0 {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgMemoryEmpty), filePath))
		return
	}

	content := string(data)
	if len([]rune(content)) > 2000 {
		content = string([]rune(content)[:2000]) + "\n\n... (truncated)"
	}

	if isGlobal {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgMemoryShowGlobal), filePath, content))
	} else {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgMemoryShowProject), filePath, content))
	}
}

func (e *Engine) appendMemoryFile(p Platform, msg *Message, filePath, text string) {
	if filePath == "" {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgMemoryNotSupported))
		return
	}

	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgMemoryAddFailed), err))
		return
	}

	f, err := os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgMemoryAddFailed), err))
		return
	}
	defer f.Close()

	entry := "\n- " + text + "\n"
	if _, err := f.WriteString(entry); err != nil {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgMemoryAddFailed), err))
		return
	}

	e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgMemoryAdded), filePath))
}

// ──────────────────────────────────────────────────────────────
// /cron command
// ──────────────────────────────────────────────────────────────

func (e *Engine) handleCardNav(action, sessionKey string) *Card {
	var prefix, body string
	if i := strings.Index(action, ":"); i >= 0 {
		prefix = action[:i]
		body = action[i+1:]
	} else {
		return nil
	}

	cmd, args := body, ""
	if i := strings.IndexByte(body, ' '); i >= 0 {
		cmd = body[:i]
		args = strings.TrimSpace(body[i+1:])
	}

	var notice string
	if prefix == "act" {
		notice = e.executeCardAction(cmd, args, sessionKey)
	}

	switch cmd {
	case "/help":
		return e.renderHelpCard()
	case "/cron":
		return e.renderCronCard(sessionKey, notice)
	case "/sessions":
		return e.renderSessionCard(sessionKey, parseSessionCardPage(args), notice)
	case "/review":
		if strings.HasPrefix(strings.TrimSpace(args), "start ") {
			return e.simpleCard(e.i18n.T(MsgReviewActionsTitle), "blue", notice)
		}
		return e.resultActionCard()
	}
	return nil
}

func (e *Engine) executeCardAction(cmd, args, sessionKey string) string {
	switch cmd {
	case "/cron":
		if e.cronScheduler == nil {
			return ""
		}
		fields := strings.Fields(args)
		if len(fields) < 2 {
			return ""
		}
		sub, id := fields[0], fields[1]
		job := e.cronScheduler.Store().Get(id)
		if job == nil || job.SessionKey != sessionKey {
			return "Cron job not found for this session."
		}
		switch sub {
		case "enable":
			if err := e.cronScheduler.EnableJob(id); err != nil {
				return fmt.Sprintf("Failed to enable `%s`: %v", id, err)
			}
			return fmt.Sprintf("Enabled `%s`.", id)
		case "disable":
			if err := e.cronScheduler.DisableJob(id); err != nil {
				return fmt.Sprintf("Failed to disable `%s`: %v", id, err)
			}
			return fmt.Sprintf("Disabled `%s`.", id)
		case "delete":
			if ok := e.cronScheduler.RemoveJob(id); !ok {
				return fmt.Sprintf("Failed to delete `%s`.", id)
			}
			return fmt.Sprintf("Deleted `%s`.", id)
		case "mute":
			if err := e.cronScheduler.UpdateJob(id, "mute", true); err != nil {
				return fmt.Sprintf("Failed to mute `%s`: %v", id, err)
			}
			return fmt.Sprintf("Muted `%s`.", id)
		case "unmute":
			if err := e.cronScheduler.UpdateJob(id, "mute", false); err != nil {
				return fmt.Sprintf("Failed to unmute `%s`: %v", id, err)
			}
			return fmt.Sprintf("Unmuted `%s`.", id)
		case "editprompt":
			e.setPendingCronPromptEdit(sessionKey, id)
			sentExistingPrompt := false
			if strings.TrimSpace(job.Prompt) != "" {
				if err := e.sendTextToSession(sessionKey, fmt.Sprintf("Current prompt for `%s`:\n\n%s", id, job.Prompt)); err != nil {
					slog.Warn("cron: failed to send current prompt for edit", "session_key", sessionKey, "job_id", id, "error", err)
				} else {
					sentExistingPrompt = true
				}
			}
			if sentExistingPrompt {
				return fmt.Sprintf("Send your next message to replace the prompt for `%s`. The current prompt was sent above. Send /cancel to abort.", id)
			}
			return fmt.Sprintf("Send your next message to replace the prompt for `%s`. Send /cancel to abort.", id)
		}
	case "/review":
		fields := strings.Fields(args)
		if len(fields) == 0 {
			return ""
		}
		switch fields[0] {
		case "open":
			if err := e.sendCardToSession(sessionKey, e.reviewerSelectorCard()); err != nil {
				slog.Warn("review: failed to send selector card", "session_key", sessionKey, "error", err)
				return fmt.Sprintf(e.i18n.T(MsgError), err)
			}
			return ""
		case "start":
			if len(fields) < 2 {
				return ""
			}
			reviewerProject := fields[1]
			go func() {
				if err := e.startReviewCycle(sessionKey, reviewerProject); err != nil {
					slog.Error("review: cycle failed", "session_key", sessionKey, "reviewer", reviewerProject, "error", err)
					if sendErr := e.sendTextToSession(sessionKey, fmt.Sprintf(e.i18n.T(MsgError), err)); sendErr != nil {
						slog.Warn("review: failed to send error", "session_key", sessionKey, "error", sendErr)
					}
				}
			}()
			return e.i18n.Tf(MsgReviewStarted, reviewerProject)
		}
	case "/sessions":
		fields := strings.Fields(args)
		if len(fields) < 2 {
			return ""
		}
		sub, id := fields[0], fields[1]
		switch sub {
		case "switch":
			return e.switchSessionByID(sessionKey, id)
		case "delete":
			return e.deleteSessionByID(sessionKey, id)
		}
	}
	return ""
}

func (e *Engine) renderCronCard(sessionKey string, notice string) *Card {
	if e.cronScheduler == nil {
		return e.simpleCard("Cron Jobs", "orange", e.i18n.T(MsgCronNotAvailable))
	}
	if strings.TrimSpace(sessionKey) == "" {
		return e.simpleCard("Cron Jobs", "orange", "Invalid session")
	}
	jobs := e.cronScheduler.Store().ListBySessionKey(sessionKey)
	if len(jobs) == 0 {
		return e.simpleCard("Cron Jobs", "orange", e.i18n.T(MsgCronEmpty))
	}

	lang := e.i18n.CurrentLang()
	now := time.Now()
	cb := NewCard().Title("Cron Jobs", "orange")
	cb.Markdownf(e.i18n.T(MsgCronListTitle), len(jobs))
	if strings.TrimSpace(notice) != "" {
		cb.Markdown(notice)
	}

	for _, j := range jobs {
		status := "✅"
		if !j.Enabled {
			status = "⏸"
		}
		desc := j.Description
		if desc == "" {
			if j.IsShellJob() {
				desc = "🖥 " + firstNonEmptyLine(j.Exec)
			} else {
				desc = firstNonEmptyLine(j.Prompt)
			}
		}
		if j.Mute {
			desc += " [mute]"
		}
		human := CronExprToHuman(j.CronExpr, lang)

		var sb strings.Builder
		fmt.Fprintf(&sb, "%s %s\n", status, desc)
		fmt.Fprintf(&sb, "ID: `%s`\n", j.ID)
		sb.WriteString(e.i18n.Tf(MsgCronScheduleLabel, human, j.CronExpr))
		nextRun := e.cronScheduler.NextRun(j.ID)
		if !nextRun.IsZero() {
			fmtStr := cronTimeFormat(nextRun, now)
			sb.WriteString(e.i18n.Tf(MsgCronNextRunLabel, nextRun.Format(fmtStr)))
		}
		if !j.LastRun.IsZero() {
			fmtStr := cronTimeFormat(j.LastRun, now)
			sb.WriteString(e.i18n.Tf(MsgCronLastRunLabel, j.LastRun.Format(fmtStr)))
			if j.LastError != "" {
				fmt.Fprintf(&sb, " (failed: %s)", truncateStr(j.LastError, 40))
			}
			sb.WriteString("\n")
		}
		cb.Markdown(sb.String())

		var btns []CardButton
		btns = append(btns, DefaultBtn("Edit Prompt", fmt.Sprintf("act:/cron editprompt %s", j.ID)))
		if j.Enabled {
			btns = append(btns, DefaultBtn("Disable", fmt.Sprintf("act:/cron disable %s", j.ID)))
		} else {
			btns = append(btns, PrimaryBtn("Enable", fmt.Sprintf("act:/cron enable %s", j.ID)))
		}
		if j.Mute {
			btns = append(btns, DefaultBtn("Unmute", fmt.Sprintf("act:/cron unmute %s", j.ID)))
		} else {
			btns = append(btns, DefaultBtn("Mute", fmt.Sprintf("act:/cron mute %s", j.ID)))
		}
		btns = append(btns, DangerBtn("Delete", fmt.Sprintf("act:/cron delete %s", j.ID)))
		for i := 0; i < len(btns); i += 2 {
			end := i + 2
			if end > len(btns) {
				end = len(btns)
			}
			cb.ButtonsEqual(btns[i:end]...)
		}
	}

	cb.Divider()
	cb.Note("Tip: tap Edit Prompt, then send your next message as the new prompt.")
	cb.Buttons(e.cardBackButton())
	return cb.Build()
}

func (e *Engine) replyCronCardIfSupported(p Platform, msg *Message, notice string) bool {
	if p == nil || msg == nil || !supportsCards(p) {
		return false
	}
	e.replyWithCard(p, msg.ReplyCtx, e.renderCronCard(msg.SessionKey, notice))
	return true
}

func (e *Engine) cmdCron(p Platform, msg *Message, args []string) {
	if e.cronScheduler == nil {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCronNotAvailable))
		return
	}

	if len(args) == 0 {
		e.cmdCronList(p, msg)
		return
	}

	sub := matchSubCommand(strings.ToLower(args[0]), []string{
		"add", "addexec", "list", "del", "delete", "rm", "remove", "enable", "disable", "setup",
	})
	switch sub {
	case "add":
		e.cmdCronAdd(p, msg, args[1:])
	case "addexec":
		e.cmdCronAddExec(p, msg, args[1:])
	case "list":
		e.cmdCronList(p, msg)
	case "del", "delete", "rm", "remove":
		e.cmdCronDel(p, msg, args[1:])
	case "enable":
		e.cmdCronToggle(p, msg, args[1:], true)
	case "disable":
		e.cmdCronToggle(p, msg, args[1:], false)
	case "setup":
		e.cmdCronSetup(p, msg)
	default:
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCronUsage))
	}
}

func (e *Engine) cmdCronAdd(p Platform, msg *Message, args []string) {
	// /cron add <min> <hour> <day> <month> <weekday> <prompt...>
	if len(args) < 6 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCronAddUsage))
		return
	}

	cronExpr := strings.Join(args[:5], " ")
	prompt := strings.Join(args[5:], " ")

	job := &CronJob{
		ID:         GenerateCronID(),
		Project:    e.name,
		SessionKey: msg.SessionKey,
		CronExpr:   cronExpr,
		Prompt:     prompt,
		Enabled:    true,
		CreatedAt:  time.Now(),
	}

	if err := e.cronScheduler.AddJob(job); err != nil {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf("❌ %v", err))
		return
	}

	e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgCronAdded), job.ID, cronExpr, truncateStr(prompt, 60)))
}

func (e *Engine) cmdCronAddExec(p Platform, msg *Message, args []string) {
	if len(args) < 6 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCronAddExecUsage))
		return
	}

	cronExpr := strings.Join(args[:5], " ")
	shellCmd := strings.Join(args[5:], " ")
	job := &CronJob{
		ID:         GenerateCronID(),
		Project:    e.name,
		SessionKey: msg.SessionKey,
		CronExpr:   cronExpr,
		Exec:       shellCmd,
		Enabled:    true,
		CreatedAt:  time.Now(),
	}

	if err := e.cronScheduler.AddJob(job); err != nil {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf("❌ %v", err))
		return
	}

	e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgCronAddedExec), job.ID, cronExpr, truncateStr(shellCmd, 60)))
}

func (e *Engine) cmdCronList(p Platform, msg *Message) {
	if e.replyCronCardIfSupported(p, msg, "") {
		return
	}
	jobs := e.cronScheduler.Store().ListBySessionKey(msg.SessionKey)
	if len(jobs) == 0 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCronEmpty))
		return
	}

	lang := e.i18n.CurrentLang()
	now := time.Now()
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(e.i18n.T(MsgCronListTitle), len(jobs)))
	sb.WriteString("\n")
	sb.WriteString("\n")

	for i, j := range jobs {
		if i > 0 {
			sb.WriteString("\n")
		}

		status := "✅"
		if !j.Enabled {
			status = "⏸"
		}
		desc := j.Description
		if desc == "" {
			if j.IsShellJob() {
				desc = firstNonEmptyLine(j.Exec)
			} else {
				desc = firstNonEmptyLine(j.Prompt)
			}
		}
		sb.WriteString(fmt.Sprintf("%s %s\n", status, desc))

		sb.WriteString(fmt.Sprintf("ID: %s\n", j.ID))

		human := CronExprToHuman(j.CronExpr, lang)
		sb.WriteString(e.i18n.Tf(MsgCronScheduleLabel, human, j.CronExpr))

		nextRun := e.cronScheduler.NextRun(j.ID)
		if !nextRun.IsZero() {
			fmtStr := cronTimeFormat(nextRun, now)
			sb.WriteString(e.i18n.Tf(MsgCronNextRunLabel, nextRun.Format(fmtStr)))
		}

		if !j.LastRun.IsZero() {
			fmtStr := cronTimeFormat(j.LastRun, now)
			sb.WriteString(e.i18n.Tf(MsgCronLastRunLabel, j.LastRun.Format(fmtStr)))
			if j.LastError != "" {
				sb.WriteString(fmt.Sprintf(" (failed: %s)", truncateStr(j.LastError, 40)))
			}
			sb.WriteString("\n")
		}
	}

	sb.WriteString(fmt.Sprintf("\n%s", e.i18n.T(MsgCronListFooter)))
	e.reply(p, msg.ReplyCtx, sb.String())
}

func (e *Engine) cmdCronDel(p Platform, msg *Message, args []string) {
	if len(args) == 0 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCronDelUsage))
		return
	}
	id := args[0]
	if e.cronScheduler.RemoveJob(id) {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgCronDeleted), id))
	} else {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgCronNotFound), id))
	}
}

func (e *Engine) cmdCronToggle(p Platform, msg *Message, args []string, enable bool) {
	if len(args) == 0 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCronDelUsage))
		return
	}
	id := args[0]
	var err error
	if enable {
		err = e.cronScheduler.EnableJob(id)
	} else {
		err = e.cronScheduler.DisableJob(id)
	}
	if err != nil {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf("❌ %v", err))
		return
	}
	if enable {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgCronEnabled), id))
	} else {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgCronDisabled), id))
	}
}

func (e *Engine) cmdCronSetup(p Platform, msg *Message) {
	result, baseName, err := e.setupMemoryFile()
	switch result {
	case setupNative:
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgSetupNative))
	case setupNoMemory:
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgRelaySetupNoMemory))
	case setupExists:
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgRelaySetupExists), baseName))
	case setupError:
		e.reply(p, msg.ReplyCtx, fmt.Sprintf("❌ %v", err))
	case setupOK:
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgCronSetupOK), baseName))
	}
}

// ──────────────────────────────────────────────────────────────
// Custom command execution & management
// ──────────────────────────────────────────────────────────────

func (e *Engine) executeCustomCommand(p Platform, msg *Message, cmd *CustomCommand, args []string) {
	// If this is an exec command, run shell command directly
	if cmd.Exec != "" {
		go e.executeShellCommand(p, msg, cmd, args)
		return
	}

	// Otherwise, use prompt template
	prompt := ExpandPrompt(cmd.Prompt, args)

	session := e.sessions.GetOrCreateActive(msg.SessionKey)
	if !session.TryLock() {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgPreviousProcessing))
		return
	}

	slog.Info("executing custom command",
		"command", cmd.Name,
		"source", cmd.Source,
		"user", msg.UserName,
	)

	msg.Content = prompt
	go e.processInteractiveMessage(p, msg, session)
}

// executeShellCommand runs a shell command and sends the output to the user.
func (e *Engine) executeShellCommand(p Platform, msg *Message, cmd *CustomCommand, args []string) {
	slog.Info("executing shell command",
		"command", cmd.Name,
		"exec", cmd.Exec,
		"user", msg.UserName,
	)

	// Expand placeholders in exec command
	execCmd := ExpandPrompt(cmd.Exec, args)

	// Determine working directory
	workDir := cmd.WorkDir
	if workDir == "" {
		// Default to agent's work_dir if available
		if e.agent != nil {
			if agentOpts, ok := e.agent.(interface{ GetWorkDir() string }); ok {
				workDir = agentOpts.GetWorkDir()
			}
		}
	}
	if workDir == "" {
		workDir, _ = os.Getwd()
	}

	// Create context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Execute command using shell
	shellCmd := exec.CommandContext(ctx, "sh", "-c", execCmd)
	shellCmd.Dir = workDir
	output, err := shellCmd.CombinedOutput()

	if ctx.Err() == context.DeadlineExceeded {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgCommandExecTimeout), cmd.Name))
		return
	}

	if err != nil {
		errMsg := string(output)
		if errMsg == "" {
			errMsg = err.Error()
		}
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgCommandExecError), cmd.Name, truncateStr(errMsg, 1000)))
		return
	}

	result := strings.TrimSpace(string(output))
	if result == "" {
		result = e.i18n.T(MsgCommandExecSuccess)
	} else if len(result) > 4000 {
		result = result[:3997] + "..."
	}

	e.reply(p, msg.ReplyCtx, result)
}

func (e *Engine) cmdCommands(p Platform, msg *Message, args []string) {
	if len(args) == 0 {
		e.cmdCommandsList(p, msg)
		return
	}

	sub := matchSubCommand(strings.ToLower(args[0]), []string{
		"list", "add", "addexec", "del", "delete", "rm", "remove",
	})
	switch sub {
	case "list":
		e.cmdCommandsList(p, msg)
	case "add":
		e.cmdCommandsAdd(p, msg, args[1:])
	case "addexec":
		e.cmdCommandsAddExec(p, msg, args[1:])
	case "del", "delete", "rm", "remove":
		e.cmdCommandsDel(p, msg, args[1:])
	default:
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCommandsUsage))
	}
}

func (e *Engine) cmdCommandsList(p Platform, msg *Message) {
	cmds := e.commands.ListAll()
	if len(cmds) == 0 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCommandsEmpty))
		return
	}

	var sb strings.Builder
	sb.WriteString(e.i18n.Tf(MsgCommandsTitle, len(cmds)))

	for _, c := range cmds {
		// Tag
		tag := ""
		if c.Source == "agent" {
			tag = " [agent]"
		} else if c.Exec != "" {
			tag = " [shell]"
		}
		sb.WriteString(fmt.Sprintf("/%s%s\n", c.Name, tag))

		// Description or fallback
		desc := c.Description
		if desc == "" {
			if c.Exec != "" {
				desc = "$ " + truncateStr(c.Exec, 60)
			} else {
				desc = truncateStr(c.Prompt, 60)
			}
		}
		sb.WriteString(fmt.Sprintf("  %s\n\n", desc))
	}

	sb.WriteString(e.i18n.T(MsgCommandsHint))
	e.reply(p, msg.ReplyCtx, sb.String())
}

func (e *Engine) cmdCommandsAdd(p Platform, msg *Message, args []string) {
	// /commands add <name> <prompt...>
	if len(args) < 2 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCommandsAddUsage))
		return
	}

	name := strings.ToLower(args[0])
	prompt := strings.Join(args[1:], " ")

	if _, exists := e.commands.Resolve(name); exists {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgCommandsAddExists), name, name))
		return
	}

	e.commands.Add(name, "", prompt, "", "", "config")

	if e.commandSaveAddFunc != nil {
		if err := e.commandSaveAddFunc(name, "", prompt, "", ""); err != nil {
			slog.Error("failed to persist command", "error", err)
		}
	}

	e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgCommandsAdded), name, truncateStr(prompt, 80)))
}

func (e *Engine) cmdCommandsAddExec(p Platform, msg *Message, args []string) {
	// /commands addexec <name> <shell command...>
	// /commands addexec --work-dir <dir> <name> <shell command...>
	if len(args) < 2 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCommandsAddExecUsage))
		return
	}

	// Parse --work-dir flag
	workDir := ""
	i := 0
	if args[0] == "--work-dir" && len(args) >= 3 {
		workDir = args[1]
		i = 2
	}

	if i >= len(args) {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCommandsAddExecUsage))
		return
	}

	name := strings.ToLower(args[i])
	execCmd := ""
	if i+1 < len(args) {
		execCmd = strings.Join(args[i+1:], " ")
	}

	if execCmd == "" {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCommandsAddExecUsage))
		return
	}

	if _, exists := e.commands.Resolve(name); exists {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgCommandsAddExists), name, name))
		return
	}

	e.commands.Add(name, "", "", execCmd, workDir, "config")

	if e.commandSaveAddFunc != nil {
		if err := e.commandSaveAddFunc(name, "", "", execCmd, workDir); err != nil {
			slog.Error("failed to persist command", "error", err)
		}
	}

	e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgCommandsExecAdded), name, truncateStr(execCmd, 80)))
}

func (e *Engine) cmdCommandsDel(p Platform, msg *Message, args []string) {
	if len(args) == 0 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCommandsDelUsage))
		return
	}
	name := strings.ToLower(args[0])

	if !e.commands.Remove(name) {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgCommandsNotFound), name))
		return
	}

	if e.commandSaveDelFunc != nil {
		if err := e.commandSaveDelFunc(name); err != nil {
			slog.Error("failed to persist command removal", "error", err)
		}
	}

	e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgCommandsDeleted), name))
}

// ──────────────────────────────────────────────────────────────
// Skill discovery & execution
// ──────────────────────────────────────────────────────────────

func (e *Engine) executeSkill(p Platform, msg *Message, skill *Skill, args []string) {
	prompt := BuildSkillInvocationPrompt(skill, args)

	session := e.sessions.GetOrCreateActive(msg.SessionKey)
	if !session.TryLock() {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgPreviousProcessing))
		return
	}

	slog.Info("executing skill",
		"skill", skill.Name,
		"source", skill.Source,
		"user", msg.UserName,
	)

	msg.Content = prompt
	go e.processInteractiveMessage(p, msg, session)
}

func (e *Engine) cmdSkills(p Platform, msg *Message) {
	skills := e.skills.ListAll()
	if len(skills) == 0 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgSkillsEmpty))
		return
	}

	var sb strings.Builder
	sb.WriteString(e.i18n.Tf(MsgSkillsTitle, e.agent.Name(), len(skills)))

	for _, s := range skills {
		desc := s.Description
		name := s.Name
		sb.WriteString(fmt.Sprintf("  /%s — %s\n", name, desc))
	}

	sb.WriteString("\n" + e.i18n.T(MsgSkillsHint))
	e.reply(p, msg.ReplyCtx, sb.String())
}

// ── /config command ──────────────────────────────────────────

// configItem describes a configurable runtime parameter.
type configItem struct {
	key     string
	desc    string // en description
	descZh  string // zh description
	getFunc func() string
	setFunc func(string) error
}

func (ci configItem) description(isZh bool) string {
	if isZh && ci.descZh != "" {
		return ci.descZh
	}
	return ci.desc
}

func (e *Engine) configItems() []configItem {
	return []configItem{
		{
			key:    "thinking_max_len",
			desc:   "Max chars for thinking messages (0=no truncation)",
			descZh: "思考消息最大长度 (0=不截断)",
			getFunc: func() string {
				return fmt.Sprintf("%d", e.display.ThinkingMaxLen)
			},
			setFunc: func(v string) error {
				n, err := strconv.Atoi(v)
				if err != nil {
					return fmt.Errorf("invalid integer: %s", v)
				}
				if n < 0 {
					return fmt.Errorf("value must be >= 0")
				}
				e.display.ThinkingMaxLen = n
				if e.displaySaveFunc != nil {
					return e.displaySaveFunc(&n, nil)
				}
				return nil
			},
		},
		{
			key:    "tool_max_len",
			desc:   "Max chars for tool use messages (0=no truncation)",
			descZh: "工具消息最大长度 (0=不截断)",
			getFunc: func() string {
				return fmt.Sprintf("%d", e.display.ToolMaxLen)
			},
			setFunc: func(v string) error {
				n, err := strconv.Atoi(v)
				if err != nil {
					return fmt.Errorf("invalid integer: %s", v)
				}
				if n < 0 {
					return fmt.Errorf("value must be >= 0")
				}
				e.display.ToolMaxLen = n
				if e.displaySaveFunc != nil {
					return e.displaySaveFunc(nil, &n)
				}
				return nil
			},
		},
	}
}

func (e *Engine) cmdConfig(p Platform, msg *Message, args []string) {
	items := e.configItems()
	isZh := e.i18n.IsZhLike()

	if len(args) == 0 {
		var sb strings.Builder
		sb.WriteString(e.i18n.T(MsgConfigTitle))
		for _, item := range items {
			sb.WriteString(fmt.Sprintf("`%s` = `%s`\n  %s\n\n", item.key, item.getFunc(), item.description(isZh)))
		}
		sb.WriteString(e.i18n.T(MsgConfigHint))
		e.reply(p, msg.ReplyCtx, sb.String())
		return
	}

	sub := matchSubCommand(strings.ToLower(args[0]), []string{"get", "set", "reload"})

	switch sub {
	case "reload":
		e.cmdConfigReload(p, msg)
		return
	case "get":
		if len(args) < 2 {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgConfigGetUsage))
			return
		}
		key := strings.ToLower(args[1])
		for _, item := range items {
			if item.key == key {
				e.reply(p, msg.ReplyCtx, fmt.Sprintf("`%s` = `%s`\n  %s", key, item.getFunc(), item.description(isZh)))
				return
			}
		}
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgConfigKeyNotFound, key))

	case "set":
		if len(args) < 3 {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgConfigSetUsage))
			return
		}
		key := strings.ToLower(args[1])
		value := args[2]
		for _, item := range items {
			if item.key == key {
				if err := item.setFunc(value); err != nil {
					e.reply(p, msg.ReplyCtx, fmt.Sprintf("❌ %v", err))
					return
				}
				e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgConfigUpdated, key, item.getFunc()))
				return
			}
		}
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgConfigKeyNotFound, key))

	default:
		key := strings.ToLower(sub)
		for _, item := range items {
			if item.key == key {
				if len(args) >= 2 {
					if err := item.setFunc(args[1]); err != nil {
						e.reply(p, msg.ReplyCtx, fmt.Sprintf("❌ %v", err))
						return
					}
					e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgConfigUpdated, key, item.getFunc()))
				} else {
					e.reply(p, msg.ReplyCtx, fmt.Sprintf("`%s` = `%s`\n  %s", key, item.getFunc(), item.description(isZh)))
				}
				return
			}
		}
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgConfigKeyNotFound, key))
	}
}

// ── /doctor command ─────────────────────────────────────────

func (e *Engine) cmdDoctor(p Platform, msg *Message) {
	results := RunDoctorChecks(e.ctx, e.agent, e.platforms)
	report := FormatDoctorResults(results, e.i18n)
	e.reply(p, msg.ReplyCtx, report)
}

func (e *Engine) cmdUpgrade(p Platform, msg *Message, args []string) {
	subCmd := ""
	if len(args) > 0 {
		subCmd = matchSubCommand(args[0], []string{"confirm", "check"})
	}

	if subCmd == "confirm" {
		e.cmdUpgradeConfirm(p, msg)
		return
	}

	// Default: check for updates
	e.reply(p, msg.ReplyCtx, e.i18n.T(MsgUpgradeChecking))

	cur := CurrentVersion
	if cur == "" || cur == "dev" {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgUpgradeDevBuild))
		return
	}

	useGitee := e.i18n.IsZhLike()
	release, err := CheckForUpdate(cur, useGitee)
	if err != nil {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf("❌ %s", err))
		return
	}
	if release == nil {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgUpgradeUpToDate), cur))
		return
	}

	body := release.Body
	if len([]rune(body)) > 300 {
		body = string([]rune(body)[:300]) + "…"
	}

	e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgUpgradeAvailable), cur, release.TagName, body))
}

func (e *Engine) cmdUpgradeConfirm(p Platform, msg *Message) {
	cur := CurrentVersion
	if cur == "" || cur == "dev" {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgUpgradeDevBuild))
		return
	}

	useGitee := e.i18n.IsZhLike()
	release, err := CheckForUpdate(cur, useGitee)
	if err != nil {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf("❌ %s", err))
		return
	}
	if release == nil {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgUpgradeUpToDate), cur))
		return
	}

	e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgUpgradeDownloading), release.TagName))

	if err := SelfUpdate(release.TagName, useGitee); err != nil {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf("❌ %s", err))
		return
	}

	e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgUpgradeSuccess), release.TagName))

	// Auto-restart to apply the update
	select {
	case RestartCh <- RestartRequest{
		SessionKey: msg.SessionKey,
		Platform:   p.Name(),
	}:
	default:
	}
}

func (e *Engine) cmdConfigReload(p Platform, msg *Message) {
	if e.configReloadFunc == nil {
		e.reply(p, msg.ReplyCtx, "❌ Config reload not available")
		return
	}
	result, err := e.configReloadFunc()
	if err != nil {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf("❌ %v", err))
		return
	}
	e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgConfigReloaded),
		result.DisplayUpdated, result.ProvidersUpdated, result.CommandsUpdated))
}

func (e *Engine) cmdRestart(p Platform, msg *Message) {
	e.reply(p, msg.ReplyCtx, e.i18n.T(MsgRestarting))
	select {
	case RestartCh <- RestartRequest{
		SessionKey: msg.SessionKey,
		Platform:   p.Name(),
	}:
	default:
	}
}

func (e *Engine) cmdAlias(p Platform, msg *Message, args []string) {
	if len(args) == 0 {
		e.cmdAliasList(p, msg)
		return
	}

	sub := matchSubCommand(strings.ToLower(args[0]), []string{"list", "add", "del", "delete", "remove"})
	switch sub {
	case "list":
		e.cmdAliasList(p, msg)
	case "add":
		e.cmdAliasAdd(p, msg, args[1:])
	case "del", "delete", "remove":
		e.cmdAliasDel(p, msg, args[1:])
	default:
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgAliasUsage))
	}
}

func (e *Engine) cmdAliasList(p Platform, msg *Message) {
	e.aliasMu.RLock()
	defer e.aliasMu.RUnlock()

	if len(e.aliases) == 0 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgAliasEmpty))
		return
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(e.i18n.T(MsgAliasListHeader), len(e.aliases)))
	sb.WriteString("\n")

	names := make([]string, 0, len(e.aliases))
	for n := range e.aliases {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, n := range names {
		sb.WriteString(fmt.Sprintf("  %s → %s\n", n, e.aliases[n]))
	}
	e.reply(p, msg.ReplyCtx, strings.TrimRight(sb.String(), "\n"))
}

func (e *Engine) cmdAliasAdd(p Platform, msg *Message, args []string) {
	if len(args) < 2 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgAliasUsage))
		return
	}
	name := args[0]
	command := strings.Join(args[1:], " ")
	if !strings.HasPrefix(command, "/") {
		command = "/" + command
	}

	e.aliasMu.Lock()
	e.aliases[name] = command
	e.aliasMu.Unlock()

	if e.aliasSaveAddFunc != nil {
		if err := e.aliasSaveAddFunc(name, command); err != nil {
			slog.Error("alias: save failed", "error", err)
		}
	}

	e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgAliasAdded), name, command))
}

func (e *Engine) cmdAliasDel(p Platform, msg *Message, args []string) {
	if len(args) < 1 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgAliasUsage))
		return
	}
	name := args[0]

	e.aliasMu.Lock()
	_, exists := e.aliases[name]
	if exists {
		delete(e.aliases, name)
	}
	e.aliasMu.Unlock()

	if !exists {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgAliasNotFound), name))
		return
	}

	if e.aliasSaveDelFunc != nil {
		if err := e.aliasSaveDelFunc(name); err != nil {
			slog.Error("alias: save failed", "error", err)
		}
	}

	e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgAliasDeleted), name))
}

func (e *Engine) cmdDelete(p Platform, msg *Message, args []string) {
	deleter, ok := e.agent.(SessionDeleter)
	if !ok {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgDeleteNotSupported))
		return
	}

	if len(args) == 0 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgDeleteUsage))
		return
	}

	agentSessions, err := e.agent.ListSessions(e.ctx)
	if err != nil {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf("❌ %v", err))
		return
	}

	prefix := strings.TrimSpace(args[0])
	var matched *AgentSessionInfo

	if idx, err := strconv.Atoi(prefix); err == nil && idx >= 1 && idx <= len(agentSessions) {
		matched = &agentSessions[idx-1]
	} else {
		for i := range agentSessions {
			if strings.HasPrefix(agentSessions[i].ID, prefix) {
				matched = &agentSessions[i]
				break
			}
		}
	}

	if matched == nil {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgSwitchNoMatch), prefix))
		return
	}

	// Prevent deleting the currently active session
	activeSession := e.sessions.GetOrCreateActive(msg.SessionKey)
	if activeSession.AgentSessionID == matched.ID {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgDeleteActiveDenied))
		return
	}

	displayName := e.sessions.GetSessionName(matched.ID)
	if displayName == "" {
		displayName = matched.Summary
	}
	if displayName == "" {
		shortID := matched.ID
		if len(shortID) > 12 {
			shortID = shortID[:12]
		}
		displayName = shortID
	}

	if err := deleter.DeleteSession(e.ctx, matched.ID); err != nil {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf("❌ %v", err))
		return
	}

	e.sessions.SetSessionName(matched.ID, "")
	e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgDeleteSuccess), displayName))
}

func (e *Engine) deleteSessionByID(sessionKey, id string) string {
	deleter, ok := e.agent.(SessionDeleter)
	if !ok {
		return e.i18n.T(MsgDeleteNotSupported)
	}

	agentSessions, err := e.agent.ListSessions(e.ctx)
	if err != nil {
		return fmt.Sprintf("❌ %v", err)
	}

	var matched *AgentSessionInfo
	for i := range agentSessions {
		if agentSessions[i].ID == id {
			matched = &agentSessions[i]
			break
		}
	}
	if matched == nil {
		return fmt.Sprintf(e.i18n.T(MsgSwitchNoMatch), id)
	}

	activeSession := e.sessions.GetOrCreateActive(sessionKey)
	if activeSession.AgentSessionID == matched.ID {
		return e.i18n.T(MsgDeleteActiveDenied)
	}

	displayName := e.sessions.GetSessionName(matched.ID)
	if displayName == "" {
		displayName = matched.Summary
	}
	if displayName == "" {
		shortID := matched.ID
		if len(shortID) > 12 {
			shortID = shortID[:12]
		}
		displayName = shortID
	}

	if err := deleter.DeleteSession(e.ctx, matched.ID); err != nil {
		return fmt.Sprintf("❌ %v", err)
	}
	e.sessions.SetSessionName(matched.ID, "")
	return fmt.Sprintf(e.i18n.T(MsgDeleteSuccess), displayName)
}

// truncateIf truncates s to maxLen runes. 0 means no truncation.
func truncateIf(s string, maxLen int) string {
	if maxLen <= 0 {
		return s
	}
	if utf8.RuneCountInString(s) <= maxLen {
		return s
	}
	return string([]rune(s)[:maxLen]) + "..."
}

func splitMessage(text string, maxLen int) []string {
	if len(text) <= maxLen {
		return []string{text}
	}
	var chunks []string

	for len(text) > 0 {
		if len(text) <= maxLen {
			chunks = append(chunks, text)
			break
		}

		end := maxLen

		// Try to split at newline boundary
		if idx := strings.LastIndex(text[:end], "\n"); idx > 0 && idx >= end/2 {
			end = idx + 1
		}

		chunk := text[:end]
		text = text[end:]

		chunks = append(chunks, chunk)
	}
	return chunks
}

// sendTTSReply synthesizes fullResponse text and sends audio to the platform.
// Called asynchronously after EventResult; text reply is always sent first.
func (e *Engine) sendTTSReply(p Platform, replyCtx any, text string) {
	if err := e.synthesizeAndSendTTSReply(p, replyCtx, text); err != nil {
		slog.Error("tts: send reply failed", "platform", p.Name(), "error", err)
	}
}

func buildReviewPrompt(originProject, reviewerProject, summary string) string {
	return fmt.Sprintf(
		"Please review the following review packet.\n\nOriginal agent: %s\nReviewer: %s\n\nReview packet:\n%s\n\nUse the packet to determine whether you should inspect working tree changes, the latest commit, or summary-only context. Review it critically. Focus on bugs, regressions, risky assumptions, unclear implementation details, and missing validation or tests. If no major issue is found, say that explicitly.",
		originProject,
		reviewerProject,
		strings.TrimSpace(summary),
	)
}

func buildReviewPacketPrompt(originProject, reviewerProject, summary string) string {
	return fmt.Sprintf(
		"Prepare a structured review packet for a second agent.\n\nOriginal agent: %s\nReviewer: %s\n\nOriginal summary:\n%s\n\nReturn only a <review_packet>...</review_packet> block.\nDecide whether the task is repo-related.\nIf repo-related, specify the correct review target:\n- working_tree if there are relevant uncommitted changes\n- last_commit if the work is already committed\n- summary_only if code state is not needed\nInclude only the files, commit IDs, repo path, branch, and context necessary for review. Do not include prose outside the XML block.",
		originProject,
		reviewerProject,
		strings.TrimSpace(summary),
	)
}

func extractReviewPacket(content string) string {
	text := strings.TrimSpace(content)
	if text == "" {
		return ""
	}
	lower := strings.ToLower(text)
	start := strings.Index(lower, "<review_packet>")
	end := strings.LastIndex(lower, "</review_packet>")
	if start >= 0 && end > start {
		end += len("</review_packet>")
		return strings.TrimSpace(text[start:end])
	}
	return text
}

func buildRevisionPrompt(originProject, reviewerProject, originSummary, reviewSummary string) string {
	return fmt.Sprintf(
		"Please revise your previous work based on the following review feedback.\n\nYour previous summary:\n%s\n\nReviewer feedback from %s:\n%s\n\nUpdate the work accordingly and provide an updated final summary.",
		strings.TrimSpace(originSummary),
		reviewerProject,
		strings.TrimSpace(reviewSummary),
	)
}

func (e *Engine) startReviewCycle(sessionKey, reviewerProject string) error {
	allowed := false
	for _, name := range e.reviewerProjects() {
		if name == reviewerProject {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf(e.i18n.T(MsgReviewReviewerNotFound), reviewerProject)
	}

	e.reviewMu.Lock()
	key := reviewFlowMapKey(e.name, sessionKey)
	flow := e.reviewFlows[key]
	if flow != nil && flow.Running {
		e.reviewMu.Unlock()
		return fmt.Errorf("%s", e.i18n.T(MsgReviewAlreadyRunning))
	}
	if flow == nil {
		flow = &reviewFlow{}
		e.reviewFlows[key] = flow
	}
	flow.ReviewerProject = reviewerProject
	flow.LastOriginSummary = ""
	flow.LastReviewSummary = ""
	flow.Running = true
	flowRef := flow
	e.reviewMu.Unlock()

	defer func() {
		e.reviewMu.Lock()
		if e.reviewFlows[key] == flowRef {
			flowRef.Running = false
		}
		e.reviewMu.Unlock()
	}()

	reviewerEngine := (*Engine)(nil)
	if e.relayManager != nil {
		e.relayManager.mu.RLock()
		reviewerEngine = e.relayManager.engines[reviewerProject]
		e.relayManager.mu.RUnlock()
	}
	if reviewerEngine == nil {
		return fmt.Errorf(e.i18n.T(MsgReviewReviewerUnavailable), reviewerProject)
	}

	originPlatform, originReplyCtx, err := e.reconstructPlatformReply(sessionKey)
	if err != nil {
		return fmt.Errorf("review reconstruct origin reply: %w", err)
	}
	originSession := e.sessions.GetOrCreateActive(sessionKey)
	if !originSession.TryLock() {
		return fmt.Errorf("%s", e.i18n.T(MsgPreviousProcessing))
	}
	defer func() {
		originSession.mu.Lock()
		locked := originSession.busy
		originSession.mu.Unlock()
		if locked {
			originSession.Unlock()
		}
	}()

	originSummary := strings.TrimSpace(originSession.LatestAssistantMessage())
	if originSummary == "" {
		return fmt.Errorf("%s", e.i18n.T(MsgReviewNoContent))
	}
	if utf8.RuneCountInString(originSummary) > maxReviewPromptLen {
		return fmt.Errorf("%s", e.i18n.Tf(MsgReviewPromptTooLong, maxReviewPromptLen))
	}

	e.reviewMu.Lock()
	if e.reviewFlows[key] == flowRef {
		flowRef.LastOriginSummary = originSummary
		flowRef.LastReviewSummary = ""
	}
	e.reviewMu.Unlock()

	packetPrompt := buildReviewPacketPrompt(e.name, reviewerProject, originSummary)
	reviewPacket, err := e.runManagedTurn(originStateForReviewPacket(e, sessionKey, originPlatform, originReplyCtx, originSession), originSession, sessionKey, packetPrompt, managedTurnOpts{
		AutoApprove: true,
		Silent:      true,
	})
	if err != nil {
		return err
	}
	reviewPacket = extractReviewPacket(reviewPacket)
	if reviewPacket == "" {
		return fmt.Errorf("origin agent returned empty review packet")
	}
	if utf8.RuneCountInString(reviewPacket) > maxReviewPromptLen {
		return fmt.Errorf("%s", e.i18n.Tf(MsgReviewPromptTooLong, maxReviewPromptLen))
	}

	reviewKey := reviewerSessionKey(e.name, reviewerProject, sessionKey)
	reviewerSession := reviewerEngine.sessions.GetOrCreateActive(reviewKey)
	if !reviewerSession.TryLock() {
		return fmt.Errorf("%s", e.i18n.Tf(MsgReviewReviewerBusy, reviewerProject))
	}
	defer reviewerSession.Unlock()

	reviewState := reviewerEngine.getOrCreateInteractiveState(reviewKey, originPlatform, originReplyCtx, reviewerSession)
	reviewState.mu.Lock()
	reviewState.platform = originPlatform
	reviewState.replyCtx = originReplyCtx
	reviewState.mu.Unlock()
	reviewPrompt := buildReviewPrompt(e.name, reviewerProject, reviewPacket)
	reviewSummary, err := reviewerEngine.runManagedTurn(reviewState, reviewerSession, reviewKey, reviewPrompt, managedTurnOpts{
		Prefix:      reviewerProject + ": ",
		AutoApprove: true,
	})
	if err != nil {
		return err
	}

	e.reviewMu.Lock()
	if e.reviewFlows[key] == flowRef {
		flowRef.LastReviewSummary = reviewSummary
	}
	e.reviewMu.Unlock()

	revisionPrompt := buildRevisionPrompt(e.name, reviewerProject, originSummary, reviewSummary)
	originState := e.getOrCreateInteractiveState(sessionKey, originPlatform, originReplyCtx, originSession)
	if originState == nil || originState.agentSession == nil {
		return fmt.Errorf("failed to restart origin agent session")
	}
	originState.mu.Lock()
	originState.platform = originPlatform
	originState.replyCtx = originReplyCtx
	originState.mu.Unlock()
	_, err = e.runManagedTurn(originState, originSession, sessionKey, revisionPrompt, managedTurnOpts{
		AutoApprove:       true,
		OfferFollowUpCard: true,
	})
	return err
}

func originStateForReviewPacket(e *Engine, sessionKey string, p Platform, replyCtx any, session *Session) *interactiveState {
	state := e.getOrCreateInteractiveState(sessionKey, p, replyCtx, session)
	if state == nil || state.agentSession == nil {
		return nil
	}
	state.mu.Lock()
	state.platform = p
	state.replyCtx = replyCtx
	state.mu.Unlock()
	return state
}

func (e *Engine) resultActionCard() *Card {
	buttons := []CardButton{
		DefaultBtn(e.i18n.T(MsgTTSReadButton), "tts:read_last"),
		PrimaryBtn(e.i18n.T(MsgReviewButton), "act:/review open"),
	}
	return NewCard().
		Title(e.i18n.T(MsgReviewActionsTitle), "blue").
		ButtonsEqual(buttons...).
		Build()
}

func (e *Engine) reviewerSelectorCard() *Card {
	reviewers := e.reviewerProjects()
	cb := NewCard().
		Title(e.i18n.T(MsgReviewChooseTitle), "orange").
		Markdown(e.i18n.T(MsgReviewChoosePrompt))
	if len(reviewers) == 0 {
		return cb.Note(e.i18n.T(MsgReviewNoCandidates)).Build()
	}
	buttons := make([]CardButton, 0, len(reviewers))
	for _, reviewer := range reviewers {
		buttons = append(buttons, DefaultBtn(reviewer, "act:/review start "+reviewer))
	}
	for i := 0; i < len(buttons); i += 2 {
		end := i + 2
		if end > len(buttons) {
			end = len(buttons)
		}
		cb.ButtonsEqual(buttons[i:end]...)
	}
	return cb.Build()
}

func (e *Engine) offerResultActionCard(p Platform, replyCtx any, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	if supportsCards(p) {
		e.replyWithCard(p, replyCtx, e.resultActionCard())
		return
	}
	if e.tts == nil || !e.tts.Enabled || e.tts.TTS == nil || !e.tts.OfferReadButton {
		return
	}
	if _, ok := p.(AudioSender); !ok {
		return
	}
	buttons := [][]ButtonOption{{
		{Text: e.i18n.T(MsgTTSReadButton), Data: "tts:read_last"},
	}}
	e.replyWithButtons(p, replyCtx, e.i18n.T(MsgTTSReadPrompt), buttons)
}

func (e *Engine) handleReadAloudRequest(p Platform, msg *Message, content string) bool {
	if strings.TrimSpace(strings.ToLower(content)) != "tts read last" {
		return false
	}
	if e.tts == nil || !e.tts.Enabled || e.tts.TTS == nil {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTTSNotEnabled))
		return true
	}
	if _, ok := p.(AudioSender); !ok {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTTSNotEnabled))
		return true
	}

	session := e.sessions.GetOrCreateActive(msg.SessionKey)
	text := session.LatestAssistantMessage()
	if text == "" {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTTSNoContent))
		return true
	}

	e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTTSGenerating))
	go func() {
		if err := e.synthesizeAndSendTTSReply(p, msg.ReplyCtx, text); err != nil {
			slog.Error("tts: on-demand synthesis failed", "platform", p.Name(), "session", msg.SessionKey, "error", err)
			e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgError), err))
		}
	}()
	return true
}

func (e *Engine) synthesizeAndSendTTSReply(p Platform, replyCtx any, text string) error {
	if e.tts == nil || e.tts.TTS == nil {
		return fmt.Errorf("tts is not enabled")
	}
	as, ok := p.(AudioSender)
	if !ok {
		return fmt.Errorf("platform %s does not support audio sending", p.Name())
	}
	chunks := splitTTSChunks(text, e.tts.MaxTextLen)
	opts := TTSSynthesisOpts{Voice: e.tts.Voice}
	type ttsResult struct {
		audio  []byte
		format string
	}
	results := make([]ttsResult, len(chunks))

	parallelism := len(chunks)
	if parallelism > maxParallelTTSRequests {
		parallelism = maxParallelTTSRequests
	}
	if parallelism < 1 {
		parallelism = 1
	}

	sem := make(chan struct{}, parallelism)
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()

	var wg sync.WaitGroup
	var firstErr error
	var firstErrMu sync.Mutex

	for i, chunk := range chunks {
		wg.Add(1)
		go func(idx int, part string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			if ctx.Err() != nil {
				return
			}
			audioData, format, err := e.tts.TTS.Synthesize(ctx, part, opts)
			if err != nil {
				firstErrMu.Lock()
				if firstErr == nil {
					firstErr = err
					cancel()
				}
				firstErrMu.Unlock()
				return
			}
			results[idx] = ttsResult{audio: audioData, format: format}
		}(i, chunk)
	}
	wg.Wait()

	if firstErr != nil {
		return firstErr
	}

	for _, result := range results {
		if err := as.SendAudio(e.ctx, replyCtx, result.audio, result.format); err != nil {
			return err
		}
	}
	return nil
}

func splitTTSChunks(text string, maxLen int) []string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	if maxLen <= 0 || utf8.RuneCountInString(trimmed) <= maxLen {
		return []string{trimmed}
	}

	var chunks []string
	for _, paragraph := range splitTTSParagraphs(trimmed) {
		if utf8.RuneCountInString(paragraph) <= maxLen {
			chunks = append(chunks, paragraph)
			continue
		}
		chunks = append(chunks, splitTTSParagraph(paragraph, maxLen)...)
	}
	return chunks
}

func splitTTSParagraphs(text string) []string {
	lines := strings.Split(text, "\n")
	var paragraphs []string
	var current []string
	flush := func() {
		if len(current) == 0 {
			return
		}
		paragraph := strings.TrimSpace(strings.Join(current, "\n"))
		if paragraph != "" {
			paragraphs = append(paragraphs, paragraph)
		}
		current = nil
	}
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		current = append(current, line)
	}
	flush()
	if len(paragraphs) == 0 {
		return []string{strings.TrimSpace(text)}
	}
	return paragraphs
}

func splitTTSParagraph(paragraph string, maxLen int) []string {
	if utf8.RuneCountInString(paragraph) <= maxLen {
		return []string{paragraph}
	}

	sentences := splitTTSSentences(paragraph)
	var chunks []string
	var current strings.Builder
	currentLen := 0
	flush := func() {
		chunk := strings.TrimSpace(current.String())
		if chunk != "" {
			chunks = append(chunks, chunk)
		}
		current.Reset()
		currentLen = 0
	}

	for _, sentence := range sentences {
		sentence = strings.TrimSpace(sentence)
		if sentence == "" {
			continue
		}
		sentenceLen := utf8.RuneCountInString(sentence)
		if sentenceLen > maxLen {
			flush()
			chunks = append(chunks, splitTTSByLength(sentence, maxLen)...)
			continue
		}
		if currentLen > 0 && currentLen+1+sentenceLen > maxLen {
			flush()
		}
		if currentLen > 0 {
			current.WriteRune(' ')
			currentLen++
		}
		current.WriteString(sentence)
		currentLen += sentenceLen
	}
	flush()
	return chunks
}

func splitTTSSentences(text string) []string {
	var parts []string
	var current strings.Builder
	for _, r := range text {
		current.WriteRune(r)
		switch r {
		case '\n', '。', '！', '？', '.', '!', '?', ';', '；':
			part := strings.TrimSpace(current.String())
			if part != "" {
				parts = append(parts, part)
			}
			current.Reset()
		}
	}
	if tail := strings.TrimSpace(current.String()); tail != "" {
		parts = append(parts, tail)
	}
	if len(parts) == 0 {
		return []string{text}
	}
	return parts
}

func splitTTSByLength(text string, maxLen int) []string {
	runes := []rune(strings.TrimSpace(text))
	var chunks []string
	for len(runes) > 0 {
		if len(runes) <= maxLen {
			chunk := strings.TrimSpace(string(runes))
			if chunk != "" {
				chunks = append(chunks, chunk)
			}
			break
		}

		splitAt := maxLen
		minSplit := maxLen / 2
		if minSplit < 1 {
			minSplit = 1
		}
		for i := maxLen; i >= minSplit; i-- {
			switch runes[i-1] {
			case '\n', '。', '！', '？', '.', '!', '?', ';', '；', ',', '，', ' ':
				splitAt = i
				i = minSplit
			}
		}

		chunk := strings.TrimSpace(string(runes[:splitAt]))
		if chunk != "" {
			chunks = append(chunks, chunk)
		}
		runes = []rune(strings.TrimSpace(string(runes[splitAt:])))
	}
	return chunks
}

// ──────────────────────────────────────────────────────────────
// Bot-to-bot relay
// ──────────────────────────────────────────────────────────────

// HandleRelay processes a relay message synchronously: starts or resumes a
// dedicated relay session, sends the message to the agent, and blocks until
// the complete response is collected.
func (e *Engine) HandleRelay(ctx context.Context, fromProject, chatID, message string) (string, error) {
	relaySessionKey := "relay:" + fromProject + ":" + chatID
	session := e.sessions.GetOrCreateActive(relaySessionKey)

	envVars := e.sessionEnv(relaySessionKey)

	agentSession, err := e.startAgentSession(ctx, session.AgentSessionID, envVars)
	if err != nil {
		return "", fmt.Errorf("start relay session: %w", err)
	}

	if session.AgentSessionID == "" {
		session.AgentSessionID = agentSession.CurrentSessionID()
		e.sessions.Save()
	}

	if err := agentSession.Send(message, nil, nil); err != nil {
		return "", fmt.Errorf("send relay message: %w", err)
	}

	var textParts []string
	for event := range agentSession.Events() {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		switch event.Type {
		case EventText:
			if event.Content != "" {
				textParts = append(textParts, event.Content)
			}
			if event.SessionID != "" && session.AgentSessionID == "" {
				session.AgentSessionID = event.SessionID
				e.sessions.Save()
			}
		case EventResult:
			if event.SessionID != "" {
				session.AgentSessionID = event.SessionID
				e.sessions.Save()
			}
			resp := event.Content
			if resp == "" && len(textParts) > 0 {
				resp = strings.Join(textParts, "")
			}
			if resp == "" {
				resp = "(empty response)"
			}
			slog.Info("relay: turn complete", "from", fromProject, "to", e.name, "response_len", len(resp))
			return resp, nil
		case EventError:
			if event.Error != nil {
				return "", event.Error
			}
			return "", fmt.Errorf("agent error (no details)")
		case EventPermissionRequest:
			// Auto-approve all permissions in relay mode
			_ = agentSession.RespondPermission(event.RequestID, PermissionResult{
				Behavior:     "allow",
				UpdatedInput: event.ToolInputRaw,
			})
		}
	}

	if len(textParts) > 0 {
		return strings.Join(textParts, ""), nil
	}
	return "", fmt.Errorf("relay: agent process exited without response")
}

// cmdBind handles /bind — establishes a relay binding between bots in a group chat.
//
// Usage:
//
//	/bind <project>           — bind current bot with another project in this group
//	/bind remove              — remove all bindings for this group
//	/bind -<project>          — remove specific project from binding
//	/bind                     — show current binding status
//
// The <project> argument is the project name from config.toml [[projects]].
// Multiple projects can be bound together for relay.
func (e *Engine) cmdBind(p Platform, msg *Message, args []string) {
	if e.relayManager == nil {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgRelayNotAvailable))
		return
	}

	_, chatID, err := parseSessionKeyParts(msg.SessionKey)
	if err != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgRelayNotAvailable))
		return
	}

	if len(args) == 0 {
		e.cmdBindStatus(p, msg.ReplyCtx, chatID)
		return
	}

	otherProject := args[0]

	// Handle removal commands
	if otherProject == "remove" || otherProject == "rm" || otherProject == "unbind" || otherProject == "del" || otherProject == "clear" {
		e.relayManager.Unbind(chatID)
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgRelayUnbound))
		return
	}

	if otherProject == "setup" {
		e.cmdBindSetup(p, msg)
		return
	}

	if otherProject == "help" || otherProject == "-h" || otherProject == "--help" {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgRelayUsage))
		return
	}

	// Handle removal with - prefix: /bind -project
	if strings.HasPrefix(otherProject, "-") {
		projectToRemove := strings.TrimPrefix(otherProject, "-")
		if e.relayManager.RemoveFromBind(chatID, projectToRemove) {
			e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgRelayBindRemoved), projectToRemove))
		} else {
			e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgRelayBindNotFound), projectToRemove))
		}
		return
	}

	if otherProject == e.name {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgRelayBindSelf))
		return
	}

	// Validate the target project exists
	if !e.relayManager.HasEngine(otherProject) {
		available := e.relayManager.ListEngineNames()
		var others []string
		for _, n := range available {
			if n != e.name {
				others = append(others, n)
			}
		}
		if len(others) == 0 {
			e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgRelayNoTarget), otherProject))
		} else {
			e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgRelayNotFound), otherProject, strings.Join(others, ", ")))
		}
		return
	}

	// Add current project and target project to binding
	e.relayManager.AddToBind(p.Name(), chatID, e.name)
	e.relayManager.AddToBind(p.Name(), chatID, otherProject)

	// Get all bound projects for status message
	binding := e.relayManager.GetBinding(chatID)
	var boundProjects []string
	for proj := range binding.Bots {
		boundProjects = append(boundProjects, proj)
	}

	reply := fmt.Sprintf(e.i18n.T(MsgRelayBindSuccess), strings.Join(boundProjects, " ↔ "), otherProject, otherProject)

	if _, ok := e.agent.(SystemPromptSupporter); !ok {
		if mp, ok := e.agent.(MemoryFileProvider); ok {
			reply += fmt.Sprintf(e.i18n.T(MsgRelaySetupHint), filepath.Base(mp.ProjectMemoryFile()))
		}
	}

	e.reply(p, msg.ReplyCtx, reply)
}

func (e *Engine) modeUsageText(modes []PermissionModeInfo) string {
	keys := make([]string, 0, len(modes))
	for _, mode := range modes {
		keys = append(keys, "`"+mode.Key+"`")
	}
	return e.i18n.Tf(MsgModeUsage, strings.Join(keys, " / "))
}

func (e *Engine) cmdBindStatus(p Platform, replyCtx any, chatID string) {
	binding := e.relayManager.GetBinding(chatID)
	if binding == nil {
		e.reply(p, replyCtx, e.i18n.T(MsgRelayNoBinding))
		return
	}
	var parts []string
	for proj := range binding.Bots {
		parts = append(parts, proj)
	}
	e.reply(p, replyCtx, fmt.Sprintf(e.i18n.T(MsgRelayBound), strings.Join(parts, " ↔ ")))
}

const ccConnectInstructionMarker = "<!-- cc-connect-instructions -->"

type setupResult int

const (
	setupOK setupResult = iota
	setupExists
	setupNative
	setupNoMemory
	setupError
)

func (e *Engine) setupMemoryFile() (setupResult, string, error) {
	if _, ok := e.agent.(SystemPromptSupporter); ok {
		return setupNative, "", nil
	}

	mp, ok := e.agent.(MemoryFileProvider)
	if !ok {
		return setupNoMemory, "", nil
	}

	filePath := mp.ProjectMemoryFile()
	if filePath == "" {
		return setupNoMemory, "", nil
	}

	baseName := filepath.Base(filePath)
	existing, _ := os.ReadFile(filePath)
	if strings.Contains(string(existing), ccConnectInstructionMarker) {
		return setupExists, baseName, nil
	}

	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		return setupError, baseName, err
	}

	f, err := os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return setupError, baseName, err
	}
	defer f.Close()

	block := "\n" + ccConnectInstructionMarker + "\n" + AgentSystemPrompt() + "\n"
	if _, err := f.WriteString(block); err != nil {
		return setupError, baseName, err
	}

	return setupOK, baseName, nil
}

func (e *Engine) cmdBindSetup(p Platform, msg *Message) {
	result, baseName, err := e.setupMemoryFile()
	switch result {
	case setupNative:
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgSetupNative))
	case setupNoMemory:
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgRelaySetupNoMemory))
	case setupExists:
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgRelaySetupExists), baseName))
	case setupError:
		e.reply(p, msg.ReplyCtx, fmt.Sprintf("❌ %v", err))
	case setupOK:
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgRelaySetupOK), baseName))
	}
}
