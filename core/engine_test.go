package core

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// --- stubs for Engine tests ---

type stubAgent struct{}

func (a *stubAgent) Name() string { return "stub" }
func (a *stubAgent) StartSession(_ context.Context, _ string) (AgentSession, error) {
	return &stubAgentSession{}, nil
}
func (a *stubAgent) ListSessions(_ context.Context) ([]AgentSessionInfo, error) { return nil, nil }
func (a *stubAgent) Stop() error                                                { return nil }

type recordingResumeAgent struct {
	resumeIDs []string
}

func (a *recordingResumeAgent) Name() string { return "recording" }
func (a *recordingResumeAgent) StartSession(_ context.Context, sessionID string) (AgentSession, error) {
	a.resumeIDs = append(a.resumeIDs, sessionID)
	return &stubAgentSession{}, nil
}
func (a *recordingResumeAgent) ListSessions(_ context.Context) ([]AgentSessionInfo, error) {
	return nil, nil
}
func (a *recordingResumeAgent) Stop() error { return nil }

type stubAgentSession struct{}

func (s *stubAgentSession) Send(_ string, _ []ImageAttachment, _ []FileAttachment) error {
	return nil
}
func (s *stubAgentSession) RespondPermission(_ string, _ PermissionResult) error { return nil }
func (s *stubAgentSession) Events() <-chan Event                                 { return make(chan Event) }
func (s *stubAgentSession) CurrentSessionID() string                             { return "stub-session" }
func (s *stubAgentSession) Alive() bool                                          { return true }
func (s *stubAgentSession) Close() error                                         { return nil }

type eventfulStubAgentSession struct {
	events chan Event
}

func (s *eventfulStubAgentSession) Send(_ string, _ []ImageAttachment, _ []FileAttachment) error {
	return nil
}
func (s *eventfulStubAgentSession) RespondPermission(_ string, _ PermissionResult) error {
	return nil
}
func (s *eventfulStubAgentSession) Events() <-chan Event { return s.events }
func (s *eventfulStubAgentSession) CurrentSessionID() string {
	return ""
}
func (s *eventfulStubAgentSession) Alive() bool  { return true }
func (s *eventfulStubAgentSession) Close() error { return nil }

type stubWorkDirAgent struct {
	stubAgent
	workDir string
}

func (a *stubWorkDirAgent) SetWorkDir(dir string) { a.workDir = dir }
func (a *stubWorkDirAgent) GetWorkDir() string    { return a.workDir }

type stubPlatformEngine struct {
	n    string
	sent []string
}

func (p *stubPlatformEngine) Name() string               { return p.n }
func (p *stubPlatformEngine) Start(MessageHandler) error { return nil }
func (p *stubPlatformEngine) Reply(_ context.Context, _ any, content string) error {
	p.sent = append(p.sent, content)
	return nil
}
func (p *stubPlatformEngine) Send(_ context.Context, _ any, content string) error {
	p.sent = append(p.sent, content)
	return nil
}
func (p *stubPlatformEngine) Stop() error { return nil }

func newTestEngine() *Engine {
	return NewEngine("test", &stubAgent{}, []Platform{&stubPlatformEngine{n: "test"}}, "", LangEnglish)
}

// --- alias tests ---

func TestEngine_Alias(t *testing.T) {
	e := newTestEngine()
	e.AddAlias("帮助", "/help")
	e.AddAlias("新建", "/new")

	got := e.resolveAlias("帮助")
	if got != "/help" {
		t.Errorf("resolveAlias('帮助') = %q, want /help", got)
	}

	got = e.resolveAlias("新建 my-session")
	if got != "/new my-session" {
		t.Errorf("resolveAlias('新建 my-session') = %q, want '/new my-session'", got)
	}

	got = e.resolveAlias("random text")
	if got != "random text" {
		t.Errorf("resolveAlias should not modify unmatched content, got %q", got)
	}
}

func TestCmdDir_ChangesWorkDirAndClearsSession(t *testing.T) {
	baseDir := t.TempDir()
	targetDir := t.TempDir()
	agent := &stubWorkDirAgent{workDir: baseDir}
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)

	session := e.sessions.GetOrCreateActive("test:session")
	session.AgentSessionID = "agent-session"
	session.AddHistory("assistant", "old")

	e.cmdDir(p, &Message{SessionKey: "test:session", ReplyCtx: nil}, []string{targetDir})

	if agent.workDir != targetDir {
		t.Fatalf("workDir = %q, want %q", agent.workDir, targetDir)
	}
	if session.AgentSessionID != "" {
		t.Fatalf("AgentSessionID = %q, want empty", session.AgentSessionID)
	}
	if got := len(session.GetHistory(0)); got != 0 {
		t.Fatalf("history len = %d, want 0", got)
	}
	if len(p.sent) == 0 || !strings.Contains(p.sent[len(p.sent)-1], targetDir) {
		t.Fatalf("last reply = %q, want changed directory message", strings.Join(p.sent, "\n"))
	}
}

func TestProcessInteractiveEvents_BindsSessionIDFromResultEvent(t *testing.T) {
	e := newTestEngine()
	p := &stubPlatformEngine{n: "test"}
	events := make(chan Event, 1)
	events <- Event{Type: EventResult, SessionID: "codex-thread-123", Done: true}
	close(events)

	session := e.sessions.GetOrCreateActive("test:user1")
	state := &interactiveState{
		agentSession: &eventfulStubAgentSession{events: events},
		platform:     p,
		replyCtx:     "ctx",
	}

	e.processInteractiveEvents(state, session, "test:user1", "msg-1", time.Now())

	if session.AgentSessionID != "codex-thread-123" {
		t.Fatalf("AgentSessionID = %q, want codex-thread-123", session.AgentSessionID)
	}
}

func TestProcessInteractiveEvents_StripsOptionsXMLFromDisplayedReply(t *testing.T) {
	p := &stubButtonPlatform{n: "test"}
	tts := &stubTTS{audio: []byte("audio"), format: "mp3"}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.SetTTSConfig(&TTSCfg{
		Enabled:         true,
		TTS:             tts,
		OfferReadButton: true,
	})

	events := make(chan Event, 1)
	events <- Event{
		Type:    EventResult,
		Content: "我建议下一步先做这两件事。\n<options>\n  <option>写单机部署方案</option>\n  <option>写 agent roster 配置草案</option>\n</options>",
		Done:    true,
	}
	close(events)

	session := &Session{Name: "default"}
	state := &interactiveState{
		agentSession: &eventfulStubAgentSession{events: events},
		platform:     p,
		replyCtx:     "ctx",
	}

	e.processInteractiveEvents(state, session, "test:user1", "msg-1", time.Now())

	if len(p.sent) < 2 {
		t.Fatalf("expected sanitized reply and follow-up buttons, got %#v", p.sent)
	}
	if got := p.sent[0]; strings.Contains(got, "<options>") || strings.Contains(got, "<option>") {
		t.Fatalf("expected first reply without XML options, got %q", got)
	}
	if got := p.sent[0]; got != "我建议下一步先做这两件事。" {
		t.Fatalf("unexpected sanitized reply: %q", got)
	}
	if !slices.Contains(p.buttonData, "tts:read_last") {
		t.Fatalf("expected read-aloud button to still be offered, got %#v", p.buttonData)
	}
	history := session.GetHistory(0)
	if len(history) != 1 || history[0].Content != "我建议下一步先做这两件事。" {
		t.Fatalf("expected sanitized assistant history, got %#v", history)
	}
}

func TestSessionIDPersistsAcrossReloadForAgentPatterns(t *testing.T) {
	cases := []struct {
		name      string
		events    []Event
		sessionID string
	}{
		{
			name: "claudecode",
			events: []Event{
				{Type: EventText, SessionID: "claude-session-1"},
				{Type: EventResult, Done: true},
			},
			sessionID: "claude-session-1",
		},
		{
			name: "gemini",
			events: []Event{
				{Type: EventText, SessionID: "gemini-session-1"},
				{Type: EventResult, SessionID: "gemini-session-1", Done: true},
			},
			sessionID: "gemini-session-1",
		},
		{
			name: "cursor",
			events: []Event{
				{Type: EventText, SessionID: "cursor-session-1"},
				{Type: EventResult, SessionID: "cursor-session-1", Done: true},
			},
			sessionID: "cursor-session-1",
		},
		{
			name: "codex",
			events: []Event{
				{Type: EventResult, SessionID: "codex-thread-1", Done: true},
			},
			sessionID: "codex-thread-1",
		},
		{
			name: "qoder",
			events: []Event{
				{Type: EventResult, SessionID: "qoder-session-1", Done: true},
			},
			sessionID: "qoder-session-1",
		},
		{
			name: "opencode",
			events: []Event{
				{Type: EventResult, SessionID: "opencode-session-1", Done: true},
			},
			sessionID: "opencode-session-1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			storePath := filepath.Join(t.TempDir(), "sessions.json")
			sessionKey := "feishu:chat:user"
			p := &stubPlatformEngine{n: "test"}

			e1 := NewEngine("test", &stubAgent{}, []Platform{p}, storePath, LangEnglish)
			session1 := e1.sessions.GetOrCreateActive(sessionKey)
			eventCh := make(chan Event, len(tc.events))
			for _, evt := range tc.events {
				eventCh <- evt
			}
			close(eventCh)

			state := &interactiveState{
				agentSession: &eventfulStubAgentSession{events: eventCh},
				platform:     p,
				replyCtx:     "ctx",
			}
			e1.processInteractiveEvents(state, session1, sessionKey, "msg-1", time.Now())

			if session1.AgentSessionID != tc.sessionID {
				t.Fatalf("stored AgentSessionID = %q, want %q", session1.AgentSessionID, tc.sessionID)
			}

			agent2 := &recordingResumeAgent{}
			e2 := NewEngine("test", agent2, []Platform{p}, storePath, LangEnglish)
			session2 := e2.sessions.GetOrCreateActive(sessionKey)

			if session2.AgentSessionID != tc.sessionID {
				t.Fatalf("reloaded AgentSessionID = %q, want %q", session2.AgentSessionID, tc.sessionID)
			}

			state2 := e2.getOrCreateInteractiveState(sessionKey, p, "ctx", session2)
			if state2 == nil || state2.agentSession == nil {
				t.Fatal("expected restarted interactive state to have an agent session")
			}
			if len(agent2.resumeIDs) != 1 || agent2.resumeIDs[0] != tc.sessionID {
				t.Fatalf("resumeIDs = %#v, want [%q]", agent2.resumeIDs, tc.sessionID)
			}
		})
	}
}

func TestEngine_ClearAliases(t *testing.T) {
	e := newTestEngine()
	e.AddAlias("帮助", "/help")
	e.ClearAliases()

	got := e.resolveAlias("帮助")
	if got != "帮助" {
		t.Errorf("after ClearAliases, should not resolve, got %q", got)
	}
}

// --- banned words tests ---

func TestEngine_BannedWords(t *testing.T) {
	e := newTestEngine()
	e.SetBannedWords([]string{"spam", "BadWord"})

	if w := e.matchBannedWord("this is spam content"); w != "spam" {
		t.Errorf("expected 'spam', got %q", w)
	}
	if w := e.matchBannedWord("CONTAINS BADWORD HERE"); w != "badword" {
		t.Errorf("expected case-insensitive match 'badword', got %q", w)
	}
	if w := e.matchBannedWord("clean message"); w != "" {
		t.Errorf("expected empty, got %q", w)
	}
}

func TestEngine_BannedWordsEmpty(t *testing.T) {
	e := newTestEngine()
	if w := e.matchBannedWord("anything"); w != "" {
		t.Errorf("no banned words set, should return empty, got %q", w)
	}
}

// --- disabled commands tests ---

func TestEngine_DisabledCommands(t *testing.T) {
	e := newTestEngine()
	e.SetDisabledCommands([]string{"upgrade", "restart"})

	if !e.disabledCmds["upgrade"] {
		t.Error("upgrade should be disabled")
	}
	if !e.disabledCmds["restart"] {
		t.Error("restart should be disabled")
	}
	if e.disabledCmds["help"] {
		t.Error("help should not be disabled")
	}
}

func TestEngine_DisabledCommandsWithSlash(t *testing.T) {
	e := newTestEngine()
	e.SetDisabledCommands([]string{"/upgrade"})

	if !e.disabledCmds["upgrade"] {
		t.Error("upgrade should be disabled even when prefixed with /")
	}
}

// --- quiet tests ---

func TestQuietSessionToggle(t *testing.T) {
	e := newTestEngine()
	p := &stubPlatformEngine{n: "test"}
	msg := &Message{SessionKey: "test:user1", ReplyCtx: "ctx"}

	// /quiet — per-session toggle on
	e.cmdQuiet(p, msg, nil)

	e.interactiveMu.Lock()
	state := e.interactiveStates["test:user1"]
	e.interactiveMu.Unlock()

	if state == nil {
		t.Fatal("expected interactiveState to be created")
	}
	state.mu.Lock()
	q := state.quiet
	state.mu.Unlock()
	if !q {
		t.Fatal("expected session quiet to be true")
	}

	// /quiet — per-session toggle off
	e.cmdQuiet(p, msg, nil)
	state.mu.Lock()
	q = state.quiet
	state.mu.Unlock()
	if q {
		t.Fatal("expected session quiet to be false after second toggle")
	}
}

func TestQuietSessionResetsOnNewSession(t *testing.T) {
	e := newTestEngine()
	p := &stubPlatformEngine{n: "test"}
	msg := &Message{SessionKey: "test:user1", ReplyCtx: "ctx"}

	// Enable per-session quiet
	e.cmdQuiet(p, msg, nil)

	// Simulate /new
	e.cleanupInteractiveState("test:user1")

	// State should be gone, quiet resets
	e.interactiveMu.Lock()
	state := e.interactiveStates["test:user1"]
	e.interactiveMu.Unlock()
	if state != nil {
		t.Fatal("expected interactiveState to be cleaned up")
	}

	// Global quiet should still be off
	e.quietMu.RLock()
	gq := e.quiet
	e.quietMu.RUnlock()
	if gq {
		t.Fatal("expected global quiet to be false")
	}
}

func TestQuietGlobalToggle(t *testing.T) {
	e := newTestEngine()
	p := &stubPlatformEngine{n: "test"}
	msg := &Message{SessionKey: "test:user1", ReplyCtx: "ctx"}

	// Default: global quiet is off
	if e.quiet {
		t.Fatal("expected global quiet to be false by default")
	}

	// /quiet global — toggle on
	e.cmdQuiet(p, msg, []string{"global"})
	e.quietMu.RLock()
	q := e.quiet
	e.quietMu.RUnlock()
	if !q {
		t.Fatal("expected global quiet to be true")
	}

	// /quiet global — toggle off
	e.cmdQuiet(p, msg, []string{"global"})
	e.quietMu.RLock()
	q = e.quiet
	e.quietMu.RUnlock()
	if q {
		t.Fatal("expected global quiet to be false after second toggle")
	}
}

func TestQuietGlobalPersistsAcrossSessions(t *testing.T) {
	e := newTestEngine()
	p := &stubPlatformEngine{n: "test"}
	msg := &Message{SessionKey: "test:user1", ReplyCtx: "ctx"}

	// Enable global quiet
	e.cmdQuiet(p, msg, []string{"global"})

	// Simulate /new
	e.cleanupInteractiveState("test:user1")

	// Global quiet should still be on
	e.quietMu.RLock()
	q := e.quiet
	e.quietMu.RUnlock()
	if !q {
		t.Fatal("expected global quiet to remain true after session cleanup")
	}
}

func TestQuietGlobalAndSessionCombined(t *testing.T) {
	e := newTestEngine()
	p := &stubPlatformEngine{n: "test"}
	msg := &Message{SessionKey: "test:user1", ReplyCtx: "ctx"}

	// Only global quiet on — should suppress
	e.cmdQuiet(p, msg, []string{"global"})
	e.quietMu.RLock()
	gq := e.quiet
	e.quietMu.RUnlock()
	if !gq {
		t.Fatal("expected global quiet on")
	}

	// Session quiet is off (no state yet) — global alone should be enough
	e.interactiveMu.Lock()
	state := e.interactiveStates["test:user1"]
	e.interactiveMu.Unlock()
	if state != nil {
		t.Fatal("expected no session state yet")
	}

	// Turn off global, turn on session
	e.cmdQuiet(p, msg, []string{"global"}) // global off
	e.cmdQuiet(p, msg, nil)                // session on

	e.quietMu.RLock()
	gq = e.quiet
	e.quietMu.RUnlock()
	if gq {
		t.Fatal("expected global quiet off")
	}

	e.interactiveMu.Lock()
	state = e.interactiveStates["test:user1"]
	e.interactiveMu.Unlock()
	state.mu.Lock()
	sq := state.quiet
	state.mu.Unlock()
	if !sq {
		t.Fatal("expected session quiet on")
	}
}

func TestCmdNameRenamesCurrentSessionBeforeAgentStarts(t *testing.T) {
	e := newTestEngine()
	p := &stubPlatformEngine{n: "test"}
	msg := &Message{SessionKey: "test:user1", ReplyCtx: "ctx"}

	s := e.sessions.NewSession(msg.SessionKey, "draft")
	if s.AgentSessionID != "" {
		t.Fatal("expected no agent session id yet")
	}

	e.cmdName(p, msg, []string{"renamed"})

	current := e.sessions.GetOrCreateActive(msg.SessionKey)
	if current.Name != "renamed" {
		t.Fatalf("expected current session to be renamed, got %q", current.Name)
	}
}
