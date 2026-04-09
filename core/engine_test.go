package core

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
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

type namedStubAgent struct {
	name string
}

func (a *namedStubAgent) Name() string { return a.name }
func (a *namedStubAgent) StartSession(_ context.Context, _ string) (AgentSession, error) {
	return &stubAgentSession{}, nil
}
func (a *namedStubAgent) ListSessions(_ context.Context) ([]AgentSessionInfo, error) {
	return nil, nil
}
func (a *namedStubAgent) Stop() error { return nil }

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

type scriptedAgentSession struct {
	events chan Event
	queue  []Event
}

func (s *scriptedAgentSession) Send(_ string, _ []ImageAttachment, _ []FileAttachment) error {
	for _, evt := range s.queue {
		s.events <- evt
	}
	close(s.events)
	return nil
}
func (s *scriptedAgentSession) RespondPermission(_ string, _ PermissionResult) error { return nil }
func (s *scriptedAgentSession) Events() <-chan Event                                 { return s.events }
func (s *scriptedAgentSession) CurrentSessionID() string                             { return "" }
func (s *scriptedAgentSession) Alive() bool                                          { return true }
func (s *scriptedAgentSession) Close() error                                         { return nil }

type recordedSend struct {
	prompt string
	images []ImageAttachment
	files  []FileAttachment
}

type recordingSendAgent struct {
	mu      sync.Mutex
	session *recordingSendSession
}

func (a *recordingSendAgent) Name() string { return "recording-send" }
func (a *recordingSendAgent) StartSession(_ context.Context, _ string) (AgentSession, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.session == nil {
		a.session = newRecordingSendSession()
	}
	return a.session, nil
}
func (a *recordingSendAgent) ListSessions(_ context.Context) ([]AgentSessionInfo, error) {
	return nil, nil
}
func (a *recordingSendAgent) Stop() error { return nil }

type scriptedRecordingAgent struct {
	mu        sync.Mutex
	responses []string
	session   *scriptedRecordingSession
}

func (a *scriptedRecordingAgent) Name() string { return "scripted-recording" }
func (a *scriptedRecordingAgent) StartSession(_ context.Context, _ string) (AgentSession, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.session == nil {
		a.session = &scriptedRecordingSession{
			events:    make(chan Event, 32),
			responses: append([]string(nil), a.responses...),
		}
	}
	return a.session, nil
}
func (a *scriptedRecordingAgent) ListSessions(_ context.Context) ([]AgentSessionInfo, error) {
	return nil, nil
}
func (a *scriptedRecordingAgent) Stop() error { return nil }

type listDeleteAgent struct {
	sessions []AgentSessionInfo
	deleted  []string
}

func (a *listDeleteAgent) Name() string { return "list-delete" }
func (a *listDeleteAgent) StartSession(_ context.Context, _ string) (AgentSession, error) {
	return &stubAgentSession{}, nil
}
func (a *listDeleteAgent) ListSessions(_ context.Context) ([]AgentSessionInfo, error) {
	out := make([]AgentSessionInfo, len(a.sessions))
	copy(out, a.sessions)
	return out, nil
}
func (a *listDeleteAgent) Stop() error { return nil }
func (a *listDeleteAgent) DeleteSession(_ context.Context, id string) error {
	a.deleted = append(a.deleted, id)
	filtered := a.sessions[:0]
	for _, s := range a.sessions {
		if s.ID != id {
			filtered = append(filtered, s)
		}
	}
	a.sessions = filtered
	return nil
}

type recordingSendSession struct {
	mu     sync.Mutex
	sends  []recordedSend
	events chan Event
}

func newRecordingSendSession() *recordingSendSession {
	return &recordingSendSession{events: make(chan Event, 8)}
}

func (s *recordingSendSession) Send(prompt string, images []ImageAttachment, files []FileAttachment) error {
	s.mu.Lock()
	s.sends = append(s.sends, recordedSend{
		prompt: prompt,
		images: cloneImages(images),
		files:  append([]FileAttachment(nil), files...),
	})
	s.mu.Unlock()
	s.events <- Event{Type: EventText, Content: "ok"}
	s.events <- Event{Type: EventResult, Done: true}
	return nil
}
func (s *recordingSendSession) RespondPermission(_ string, _ PermissionResult) error { return nil }
func (s *recordingSendSession) Events() <-chan Event                                 { return s.events }
func (s *recordingSendSession) CurrentSessionID() string                             { return "recording-send" }
func (s *recordingSendSession) Alive() bool                                          { return true }
func (s *recordingSendSession) Close() error                                         { close(s.events); return nil }

func (s *recordingSendSession) Sends() []recordedSend {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]recordedSend, len(s.sends))
	copy(out, s.sends)
	return out
}

type scriptedRecordingSession struct {
	mu        sync.Mutex
	sends     []recordedSend
	events    chan Event
	responses []string
}

func (s *scriptedRecordingSession) Send(prompt string, images []ImageAttachment, files []FileAttachment) error {
	s.mu.Lock()
	s.sends = append(s.sends, recordedSend{
		prompt: prompt,
		images: cloneImages(images),
		files:  append([]FileAttachment(nil), files...),
	})
	response := "ok"
	if len(s.responses) > 0 {
		response = s.responses[0]
		s.responses = s.responses[1:]
	}
	s.mu.Unlock()
	s.events <- Event{Type: EventText, Content: response}
	s.events <- Event{Type: EventResult, Done: true}
	return nil
}
func (s *scriptedRecordingSession) RespondPermission(_ string, _ PermissionResult) error { return nil }
func (s *scriptedRecordingSession) Events() <-chan Event                                 { return s.events }
func (s *scriptedRecordingSession) CurrentSessionID() string                             { return "scripted-recording" }
func (s *scriptedRecordingSession) Alive() bool                                          { return true }
func (s *scriptedRecordingSession) Close() error                                         { close(s.events); return nil }
func (s *scriptedRecordingSession) Sends() []recordedSend {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]recordedSend, len(s.sends))
	copy(out, s.sends)
	return out
}

func TestShouldResetAgentSessionOnResult(t *testing.T) {
	e := NewEngine("test", &namedStubAgent{name: "claudecode"}, nil, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)

	if !e.shouldResetAgentSessionOnResult("API Error: 400 {\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"Could not process image\"}}") {
		t.Fatal("expected Claude image failure to trigger reset")
	}
	if !e.shouldResetAgentSessionOnResult("Request too large (max 20MB). Try with a smaller file.") {
		t.Fatal("expected request size limit to trigger reset")
	}
	if !e.shouldResetAgentSessionOnResult("Error: max 20MB exceeded for this request") {
		t.Fatal("expected max 20MB limit to trigger reset")
	}
	if !e.shouldResetAgentSessionOnResult("Payload too large for this request") {
		t.Fatal("expected payload size limit to trigger reset")
	}
	if e.shouldResetAgentSessionOnResult("normal response") {
		t.Fatal("did not expect normal response to trigger reset")
	}

	e.agent = &namedStubAgent{name: "codex"}
	if e.shouldResetAgentSessionOnResult("API Error: 400 {\"message\":\"Could not process image\"}") {
		t.Fatal("did not expect non-Claude agent to trigger reset")
	}
}

func TestBuildAgentPrompt_IncludesReplayContextAfterReset(t *testing.T) {
	e := NewEngine("test", &namedStubAgent{name: "claudecode"}, nil, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
	session := &Session{NeedsReplay: true}
	session.AddHistory("user", "Please review the image set in SocialOps.")
	session.AddHistory("assistant", "I checked the folder and found a few suspicious files.")
	session.AddHistory("assistant", "API Error: 400 {\"type\":\"error\",\"error\":{\"message\":\"Could not process image\"}}")

	got := e.buildAgentPrompt(session, &Message{Content: "Continue with the cleanup plan."})
	if !strings.Contains(got, "Previous conversation context:") {
		t.Fatalf("prompt missing replay header: %q", got)
	}
	if !strings.Contains(got, "User: Please review the image set in SocialOps.") {
		t.Fatalf("prompt missing user history: %q", got)
	}
	if !strings.Contains(got, "Assistant: I checked the folder and found a few suspicious files.") {
		t.Fatalf("prompt missing assistant history: %q", got)
	}
	if strings.Contains(got, "Could not process image") {
		t.Fatalf("prompt should exclude fatal image error history: %q", got)
	}
	if !strings.Contains(got, "Continue from that context.") || !strings.Contains(got, "Continue with the cleanup plan.") {
		t.Fatalf("prompt missing current turn continuation: %q", got)
	}
}

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

type stubCardPlatform struct {
	mu    sync.Mutex
	n     string
	sent  []string
	cards []*Card
}

func (p *stubCardPlatform) Name() string               { return p.n }
func (p *stubCardPlatform) Start(MessageHandler) error { return nil }
func (p *stubCardPlatform) Reply(_ context.Context, _ any, content string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, content)
	return nil
}
func (p *stubCardPlatform) Send(_ context.Context, _ any, content string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, content)
	return nil
}
func (p *stubCardPlatform) Stop() error { return nil }
func (p *stubCardPlatform) ReconstructReplyCtx(sessionKey string) (any, error) {
	return sessionKey, nil
}
func (p *stubCardPlatform) SendCard(_ context.Context, _ any, card *Card) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cards = append(p.cards, card)
	return nil
}
func (p *stubCardPlatform) ReplyCard(_ context.Context, _ any, card *Card) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cards = append(p.cards, card)
	return nil
}

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

func TestCmdCronList_FeishuUsesCardButtons(t *testing.T) {
	store, err := NewCronStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewCronStore: %v", err)
	}
	scheduler := NewCronScheduler(store)
	e := NewEngine("test", &stubAgent{}, nil, "", LangEnglish)
	e.SetCronScheduler(scheduler)
	p := &stubButtonPlatform{n: "feishu"}

	job := &CronJob{
		ID:         "job-1",
		Project:    "test",
		SessionKey: "feishu:chat:user",
		CronExpr:   "* * * * *",
		Prompt:     "Collect daily updates",
		Enabled:    true,
		CreatedAt:  time.Now(),
	}
	if err := scheduler.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	e.cmdCronList(p, &Message{SessionKey: job.SessionKey, ReplyCtx: "ctx"})

	buttons := p.buttonDataSnapshot()
	if !slices.Contains(buttons, "act:/cron editprompt job-1") {
		t.Fatalf("button data = %#v, want editprompt action", buttons)
	}
	sent := p.sentSnapshot()
	if len(sent) == 0 || !strings.Contains(sent[0], "job-1") {
		t.Fatalf("sent = %#v, want cron card content with job id", sent)
	}
}

func TestCmdList_FeishuUsesSessionCardButtons(t *testing.T) {
	agent := &listDeleteAgent{sessions: []AgentSessionInfo{
		{ID: "sess-1", Summary: "First session", MessageCount: 3, ModifiedAt: time.Now()},
		{ID: "sess-2", Summary: "Second session", MessageCount: 5, ModifiedAt: time.Now()},
	}}
	p := &stubCardPlatform{n: "feishu"}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)

	msg := &Message{SessionKey: "feishu:chat:user", ReplyCtx: "ctx"}
	e.cmdList(p, msg, nil)

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.cards) == 0 {
		t.Fatal("expected session card to be sent")
	}
	rendered := p.cards[0].RenderText()
	if !strings.Contains(rendered, "First session") {
		t.Fatalf("card = %q, want session summary", rendered)
	}
	rows := p.cards[0].CollectButtons()
	var values []string
	for _, row := range rows {
		for _, btn := range row {
			values = append(values, btn.Data)
		}
	}
	if !slices.Contains(values, "act:/sessions switch sess-1 1") {
		t.Fatalf("buttons = %#v, want switch action", values)
	}
	if !slices.Contains(values, "act:/sessions delete sess-1 1") {
		t.Fatalf("buttons = %#v, want delete action", values)
	}
}

func TestExecuteCardAction_SessionsSwitchAndDelete(t *testing.T) {
	agent := &listDeleteAgent{sessions: []AgentSessionInfo{
		{ID: "sess-1", Summary: "First session", MessageCount: 3, ModifiedAt: time.Now()},
		{ID: "sess-2", Summary: "Second session", MessageCount: 5, ModifiedAt: time.Now()},
	}}
	e := NewEngine("test", agent, nil, "", LangEnglish)
	sessionKey := "feishu:chat:user"

	notice := e.executeCardAction("/sessions", "switch sess-2", sessionKey)
	if !strings.Contains(notice, "Second session") {
		t.Fatalf("switch notice = %q, want switched session", notice)
	}
	if got := e.sessions.GetOrCreateActive(sessionKey).AgentSessionID; got != "sess-2" {
		t.Fatalf("active agent session = %q, want sess-2", got)
	}

	notice = e.executeCardAction("/sessions", "delete sess-1", sessionKey)
	if !strings.Contains(notice, "First session") {
		t.Fatalf("delete notice = %q, want deleted session", notice)
	}
	if !slices.Contains(agent.deleted, "sess-1") {
		t.Fatalf("deleted = %#v, want sess-1", agent.deleted)
	}
}

func TestHandleCardNav_SessionsActionPreservesPage(t *testing.T) {
	sessions := make([]AgentSessionInfo, 25)
	for i := range sessions {
		sessions[i] = AgentSessionInfo{
			ID:           fmt.Sprintf("sess-%02d", i+1),
			Summary:      fmt.Sprintf("Session %02d", i+1),
			MessageCount: i + 1,
			ModifiedAt:   time.Now(),
		}
	}
	agent := &listDeleteAgent{sessions: sessions}
	e := NewEngine("test", agent, nil, "", LangEnglish)

	card := e.handleCardNav("act:/sessions delete sess-21 2", "feishu:chat:user")
	if card == nil {
		t.Fatal("expected session card")
	}
	rendered := card.RenderText()
	if !strings.Contains(rendered, "(2/2)") {
		t.Fatalf("card = %q, want page 2 title", rendered)
	}
	if strings.Contains(rendered, "Session 01") {
		t.Fatalf("card = %q, should stay on page 2 after action", rendered)
	}
}

func TestRenderSessionCard_PageTwoBackReturnsToPageOne(t *testing.T) {
	sessions := make([]AgentSessionInfo, 25)
	for i := range sessions {
		sessions[i] = AgentSessionInfo{
			ID:           fmt.Sprintf("sess-%02d", i+1),
			Summary:      fmt.Sprintf("Session %02d", i+1),
			MessageCount: i + 1,
			ModifiedAt:   time.Now(),
		}
	}
	agent := &listDeleteAgent{sessions: sessions}
	e := NewEngine("test", agent, nil, "", LangEnglish)

	card := e.renderSessionCard("feishu:chat:user", 2, "")
	if card == nil {
		t.Fatal("expected session card")
	}

	buttonRows := card.CollectButtons()
	var values []string
	for _, row := range buttonRows {
		for _, btn := range row {
			values = append(values, btn.Data)
		}
	}
	if !slices.Contains(values, "nav:/sessions 1") {
		t.Fatalf("buttons = %#v, want back to page 1", values)
	}
}

func TestRenderCronCard_UsesTwoButtonsPerRow(t *testing.T) {
	store, err := NewCronStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewCronStore: %v", err)
	}
	scheduler := NewCronScheduler(store)
	e := NewEngine("test", &stubAgent{}, nil, "", LangEnglish)
	e.SetCronScheduler(scheduler)

	job := &CronJob{
		ID:         "job-1",
		Project:    "test",
		SessionKey: "feishu:chat:user",
		CronExpr:   "* * * * *",
		Prompt:     "Collect daily updates",
		Enabled:    true,
		CreatedAt:  time.Now(),
	}
	if err := scheduler.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	card := e.renderCronCard(job.SessionKey, "")
	if card == nil {
		t.Fatal("expected cron card")
	}

	rows := 0
	for _, el := range card.Elements {
		actions, ok := el.(CardActions)
		if !ok {
			continue
		}
		if len(actions.Buttons) == 0 {
			continue
		}
		rows++
		if len(actions.Buttons) > 2 {
			t.Fatalf("button row size = %d, want <= 2", len(actions.Buttons))
		}
	}
	if rows < 2 {
		t.Fatalf("button rows = %d, want at least 2", rows)
	}
}

func TestRenderCronCard_ShowsFullPromptFirstLine(t *testing.T) {
	store, err := NewCronStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewCronStore: %v", err)
	}
	scheduler := NewCronScheduler(store)
	e := NewEngine("test", &stubAgent{}, nil, "", LangEnglish)
	e.SetCronScheduler(scheduler)

	fullPrompt := "看 tmux pane e2-llm 跑的脚本的状态和输出是否正常，如果失败就总结原因并给出下一步建议"
	job := &CronJob{
		ID:         "job-1",
		Project:    "test",
		SessionKey: "feishu:chat:user",
		CronExpr:   "* * * * *",
		Prompt:     fullPrompt,
		Enabled:    true,
		CreatedAt:  time.Now(),
	}
	if err := scheduler.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	card := e.renderCronCard(job.SessionKey, "")
	if card == nil {
		t.Fatal("expected cron card")
	}
	if got := card.RenderText(); !strings.Contains(got, fullPrompt) {
		t.Fatalf("card = %q, want full prompt line", got)
	}
}

func TestFirstNonEmptyLine(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty", in: "", want: ""},
		{name: "whitespace", in: " \n\t ", want: ""},
		{name: "single line", in: "  hello world  ", want: "hello world"},
		{name: "leading blanks", in: "\n\nDo the thing\nWith details here", want: "Do the thing"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := firstNonEmptyLine(tc.in); got != tc.want {
				t.Fatalf("firstNonEmptyLine(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRenderCronCard_UsesFirstNonEmptyPromptLine(t *testing.T) {
	store, err := NewCronStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewCronStore: %v", err)
	}
	scheduler := NewCronScheduler(store)
	e := NewEngine("test", &stubAgent{}, nil, "", LangEnglish)
	e.SetCronScheduler(scheduler)

	job := &CronJob{
		ID:         "job-1",
		Project:    "test",
		SessionKey: "feishu:chat:user",
		CronExpr:   "* * * * *",
		Prompt:     "\n\nDo the thing\nWith details here",
		Enabled:    true,
		CreatedAt:  time.Now(),
	}
	if err := scheduler.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	card := e.renderCronCard(job.SessionKey, "")
	if card == nil {
		t.Fatal("expected cron card")
	}
	got := card.RenderText()
	if !strings.Contains(got, "Do the thing") {
		t.Fatalf("card = %q, want first non-empty line", got)
	}
	if strings.Contains(got, "With details here") {
		t.Fatalf("card = %q, should not include later prompt lines in title", got)
	}
}

func TestCmdCronList_UsesFirstNonEmptyLineForShellJobs(t *testing.T) {
	store, err := NewCronStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewCronStore: %v", err)
	}
	scheduler := NewCronScheduler(store)
	e := NewEngine("test", &stubAgent{}, nil, "", LangEnglish)
	e.SetCronScheduler(scheduler)
	p := &stubPlatformEngine{n: "test"}

	job := &CronJob{
		ID:         "job-1",
		Project:    "test",
		SessionKey: "test:user",
		CronExpr:   "* * * * *",
		Exec:       "\n\npython run.py --verbose\nsecond line",
		Enabled:    true,
		CreatedAt:  time.Now(),
	}
	if err := scheduler.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	e.cmdCronList(p, &Message{SessionKey: job.SessionKey, ReplyCtx: "ctx"})
	if len(p.sent) == 0 {
		t.Fatal("expected text cron list")
	}
	got := p.sent[len(p.sent)-1]
	if !strings.Contains(got, "python run.py --verbose") {
		t.Fatalf("list = %q, want first non-empty exec line", got)
	}
	if strings.Contains(got, "second line") {
		t.Fatalf("list = %q, should not include later exec lines", got)
	}
}

func TestHandlePendingCronPromptEditUpdatesPrompt(t *testing.T) {
	store, err := NewCronStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewCronStore: %v", err)
	}
	scheduler := NewCronScheduler(store)
	e := NewEngine("test", &stubAgent{}, nil, "", LangEnglish)
	e.SetCronScheduler(scheduler)
	p := &stubButtonPlatform{n: "feishu"}

	job := &CronJob{
		ID:         "job-1",
		Project:    "test",
		SessionKey: "feishu:chat:user",
		CronExpr:   "* * * * *",
		Prompt:     "Old prompt",
		Enabled:    true,
		CreatedAt:  time.Now(),
	}
	if err := scheduler.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	card := e.handleCardNav("act:/cron editprompt job-1", job.SessionKey)
	if card == nil || !strings.Contains(card.RenderText(), "replace the prompt") {
		t.Fatalf("card = %#v, want edit prompt notice", card)
	}
	if edit := e.getPendingCronPromptEdit(job.SessionKey); edit == nil || edit.JobID != "job-1" {
		t.Fatalf("pending edit = %#v, want job-1", edit)
	}

	e.handleMessage(p, &Message{
		SessionKey: job.SessionKey,
		Platform:   "feishu",
		UserID:     "user",
		Content:    "New prompt from follow-up message",
		ReplyCtx:   "ctx",
	})

	got := store.Get(job.ID)
	if got == nil || got.Prompt != "New prompt from follow-up message" {
		t.Fatalf("prompt = %#v, want updated prompt", got)
	}
	if e.getPendingCronPromptEdit(job.SessionKey) != nil {
		t.Fatal("expected pending edit to be cleared")
	}
	if !slices.Contains(p.buttonDataSnapshot(), "act:/cron editprompt job-1") {
		t.Fatalf("expected refreshed cron card buttons, got %#v", p.buttonDataSnapshot())
	}
}

func TestHandleCardNav_CronEditPromptSendsExistingPrompt(t *testing.T) {
	store, err := NewCronStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewCronStore: %v", err)
	}
	scheduler := NewCronScheduler(store)
	p := &stubCardPlatform{n: "feishu"}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.SetCronScheduler(scheduler)

	job := &CronJob{
		ID:         "job-1",
		Project:    "test",
		SessionKey: "feishu:chat:user",
		CronExpr:   "* * * * *",
		Prompt:     "Old prompt content",
		Enabled:    true,
		CreatedAt:  time.Now(),
	}
	if err := scheduler.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	card := e.handleCardNav("act:/cron editprompt job-1", job.SessionKey)
	if card == nil {
		t.Fatal("expected refreshed cron card")
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.sent) == 0 || !strings.Contains(p.sent[len(p.sent)-1], "Old prompt content") {
		t.Fatalf("sent = %#v, want existing prompt content", p.sent)
	}
}

func TestHandleCardNav_CronEditPromptWithoutExistingPromptDoesNotClaimSend(t *testing.T) {
	store, err := NewCronStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewCronStore: %v", err)
	}
	scheduler := NewCronScheduler(store)
	p := &stubCardPlatform{n: "feishu"}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.SetCronScheduler(scheduler)

	job := &CronJob{
		ID:         "job-1",
		Project:    "test",
		SessionKey: "feishu:chat:user",
		CronExpr:   "* * * * *",
		Prompt:     "",
		Enabled:    true,
		CreatedAt:  time.Now(),
	}
	if err := scheduler.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	card := e.handleCardNav("act:/cron editprompt job-1", job.SessionKey)
	if card == nil {
		t.Fatal("expected refreshed cron card")
	}
	if got := card.RenderText(); strings.Contains(got, "sent above") {
		t.Fatalf("card = %q, should not claim prompt was sent", got)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.sent) != 0 {
		t.Fatalf("sent = %#v, want no extra prompt message", p.sent)
	}
}

func TestExecuteCardAction_RejectsForeignCronJob(t *testing.T) {
	store, err := NewCronStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewCronStore: %v", err)
	}
	scheduler := NewCronScheduler(store)
	e := NewEngine("test", &stubAgent{}, nil, "", LangEnglish)
	e.SetCronScheduler(scheduler)

	job := &CronJob{
		ID:         "job-1",
		Project:    "test",
		SessionKey: "feishu:chat:owner",
		CronExpr:   "* * * * *",
		Prompt:     "Old prompt",
		Enabled:    true,
		CreatedAt:  time.Now(),
	}
	if err := scheduler.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	notice := e.executeCardAction("/cron", "delete job-1", "feishu:chat:other")
	if !strings.Contains(notice, "not found for this session") {
		t.Fatalf("notice = %q, want ownership failure", notice)
	}
	if got := store.Get("job-1"); got == nil {
		t.Fatal("job should not be deleted")
	}
}

func TestHandleCardNav_InvalidFormatReturnsNil(t *testing.T) {
	e := NewEngine("test", &stubAgent{}, nil, "", LangEnglish)
	if card := e.handleCardNav("not-a-card-action", "feishu:chat:user"); card != nil {
		t.Fatalf("card = %#v, want nil", card)
	}
}

func TestExecuteCardAction_NilSchedulerReturnsEmpty(t *testing.T) {
	e := NewEngine("test", &stubAgent{}, nil, "", LangEnglish)
	if notice := e.executeCardAction("/cron", "enable job-1", "feishu:chat:user"); notice != "" {
		t.Fatalf("notice = %q, want empty", notice)
	}
}

func TestHandlePendingCronPromptEditRejectsTooLongPrompt(t *testing.T) {
	store, err := NewCronStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewCronStore: %v", err)
	}
	scheduler := NewCronScheduler(store)
	e := NewEngine("test", &stubAgent{}, nil, "", LangEnglish)
	e.SetCronScheduler(scheduler)
	p := &stubButtonPlatform{n: "feishu"}

	job := &CronJob{
		ID:         "job-1",
		Project:    "test",
		SessionKey: "feishu:chat:user",
		CronExpr:   "* * * * *",
		Prompt:     "Old prompt",
		Enabled:    true,
		CreatedAt:  time.Now(),
	}
	if err := scheduler.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	e.setPendingCronPromptEdit(job.SessionKey, job.ID)

	e.handleMessage(p, &Message{
		SessionKey: job.SessionKey,
		Platform:   "feishu",
		UserID:     "user",
		Content:    strings.Repeat("x", maxCronPromptLen+1),
		ReplyCtx:   "ctx",
	})

	got := store.Get(job.ID)
	if got == nil || got.Prompt != "Old prompt" {
		t.Fatalf("prompt = %#v, want unchanged", got)
	}
	sent := p.sentSnapshot()
	if len(sent) == 0 || !strings.Contains(sent[0], "prompt too long") {
		t.Fatalf("sent = %#v, want prompt too long error", sent)
	}
}

func TestResultActionCardContainsReviewButton(t *testing.T) {
	e := NewEngine("codex", &stubAgent{}, nil, "", LangEnglish)
	card := e.resultActionCard()
	if card == nil {
		t.Fatal("expected result action card")
	}
	text := card.RenderText()
	if !strings.Contains(text, "Read Aloud") || !strings.Contains(text, "Review") {
		t.Fatalf("card text = %q, want read and review actions", text)
	}
}

func TestReviewerSelectorCardExcludesCurrentProject(t *testing.T) {
	rm := NewRelayManager("")
	e := NewEngine("codex", &stubAgent{}, nil, "", LangEnglish)
	other1 := NewEngine("qoder", &stubAgent{}, nil, "", LangEnglish)
	other2 := NewEngine("gemini", &stubAgent{}, nil, "", LangEnglish)
	rm.RegisterEngine(e.name, e)
	rm.RegisterEngine(other1.name, other1)
	rm.RegisterEngine(other2.name, other2)
	e.SetRelayManager(rm)

	card := e.reviewerSelectorCard()
	text := card.RenderText()
	if strings.Contains(text, "codex") {
		t.Fatalf("selector should exclude current project, got %q", text)
	}
	if !strings.Contains(text, "qoder") || !strings.Contains(text, "gemini") {
		t.Fatalf("selector missing reviewer projects: %q", text)
	}
}

func TestReviewerProjectsPreferReviewerRoleWhenPresent(t *testing.T) {
	rm := NewRelayManager("")
	origin := NewEngine("codex", &stubAgent{}, nil, "", LangEnglish)
	plain := NewEngine("gemini", &stubAgent{}, nil, "", LangEnglish)
	reviewer1 := NewEngine("qoder-reviewer", &stubAgent{}, nil, "", LangEnglish)
	reviewer2 := NewEngine("gemini-reviewer", &stubAgent{}, nil, "", LangEnglish)
	reviewer1.SetRole("reviewer")
	reviewer2.SetRole("reviewer")
	rm.RegisterEngine(origin.name, origin)
	rm.RegisterEngine(plain.name, plain)
	rm.RegisterEngine(reviewer1.name, reviewer1)
	rm.RegisterEngine(reviewer2.name, reviewer2)
	origin.SetRelayManager(rm)

	got := origin.reviewerProjects()
	want := []string{"gemini-reviewer", "qoder-reviewer"}
	if !slices.Equal(got, want) {
		t.Fatalf("reviewerProjects = %#v, want %#v", got, want)
	}
}

func TestReviewerProjectsFallbackToAllOtherProjectsWhenNoReviewerRole(t *testing.T) {
	rm := NewRelayManager("")
	origin := NewEngine("codex", &stubAgent{}, nil, "", LangEnglish)
	other1 := NewEngine("qoder", &stubAgent{}, nil, "", LangEnglish)
	other2 := NewEngine("gemini", &stubAgent{}, nil, "", LangEnglish)
	rm.RegisterEngine(origin.name, origin)
	rm.RegisterEngine(other1.name, other1)
	rm.RegisterEngine(other2.name, other2)
	origin.SetRelayManager(rm)

	got := origin.reviewerProjects()
	want := []string{"gemini", "qoder"}
	if !slices.Equal(got, want) {
		t.Fatalf("reviewerProjects = %#v, want %#v", got, want)
	}
}

func TestStartReviewCycleRunsReviewerAndOriginRevision(t *testing.T) {
	rm := NewRelayManager("")
	originAgent := &scriptedRecordingAgent{responses: []string{
		"<review_packet><recommended_review_target>working_tree</recommended_review_target><files><file>core/engine.go</file></files></review_packet>",
		"revised summary",
	}}
	reviewerAgent := &scriptedRecordingAgent{responses: []string{"reviewer summary"}}
	originPlatform := &stubCardPlatform{n: "feishu"}
	reviewerPlatform := &stubCardPlatform{n: "worker"}
	origin := NewEngine("codex", originAgent, []Platform{originPlatform}, "", LangEnglish)
	reviewer := NewEngine("qoder", reviewerAgent, []Platform{reviewerPlatform}, "", LangEnglish)
	rm.RegisterEngine(origin.name, origin)
	rm.RegisterEngine(reviewer.name, reviewer)
	origin.SetRelayManager(rm)
	reviewer.SetRelayManager(rm)

	sessionKey := "feishu:chat:user"
	originSession := origin.sessions.GetOrCreateActive(sessionKey)
	originSession.AddHistory("assistant", "Original summary from codex.")

	if err := origin.startReviewCycle(sessionKey, "qoder"); err != nil {
		t.Fatalf("startReviewCycle: %v", err)
	}

	reviewerSends := reviewerAgent.session.Sends()
	if len(reviewerSends) != 1 {
		t.Fatalf("reviewer send count = %d, want 1", len(reviewerSends))
	}
	if !strings.Contains(reviewerSends[0].prompt, "<review_packet>") || !strings.Contains(reviewerSends[0].prompt, "core/engine.go") {
		t.Fatalf("reviewer prompt = %q, want review packet", reviewerSends[0].prompt)
	}
	if !strings.Contains(reviewerSends[0].prompt, "Do not include praise") {
		t.Fatalf("reviewer prompt = %q, want findings-only instruction", reviewerSends[0].prompt)
	}
	if !strings.Contains(reviewerSends[0].prompt, "`comprehensive-review`") || !strings.Contains(reviewerSends[0].prompt, "`code-review`") {
		t.Fatalf("reviewer prompt = %q, want code-review skill hint", reviewerSends[0].prompt)
	}
	if !strings.Contains(reviewerSends[0].prompt, "`security-review`") {
		t.Fatalf("reviewer prompt = %q, want security-review skill hint", reviewerSends[0].prompt)
	}

	originSends := originAgent.session.Sends()
	if len(originSends) != 2 {
		t.Fatalf("origin send count = %d, want 2 (packet + revision)", len(originSends))
	}
	if !strings.Contains(originSends[0].prompt, "Prepare a structured review packet") {
		t.Fatalf("origin packet prompt = %q, want review packet instruction", originSends[0].prompt)
	}
	if !strings.Contains(originSends[0].prompt, "`review-scope`") || !strings.Contains(originSends[0].prompt, "`review-packet`") {
		t.Fatalf("origin packet prompt = %q, want review packet skill hints", originSends[0].prompt)
	}
	if !strings.Contains(originSends[1].prompt, "Reviewer feedback from qoder:") {
		t.Fatalf("origin revision prompt = %q, want reviewer feedback section", originSends[1].prompt)
	}
	if !strings.Contains(originSends[1].prompt, "reviewer summary") {
		t.Fatalf("origin revision prompt = %q, want reviewer summary text", originSends[1].prompt)
	}
	if !strings.Contains(originSends[1].prompt, "`review-feedback-triage`") {
		t.Fatalf("origin revision prompt = %q, want review-feedback-triage skill hint", originSends[1].prompt)
	}

	originPlatform.mu.Lock()
	sentText := strings.Join(originPlatform.sent, "\n")
	originPlatform.mu.Unlock()
	if !strings.Contains(sentText, "codex review-packet: prompt:") || !strings.Contains(sentText, "Prepare a structured review packet for a second agent.") {
		t.Fatalf("origin platform sent = %q, want visible review-packet prompt", sentText)
	}
	if !strings.Contains(sentText, "codex review-packet: <review_packet>") {
		t.Fatalf("origin platform sent = %q, want visible review-packet output", sentText)
	}

	originPlatform.mu.Lock()
	if len(originPlatform.cards) == 0 {
		originPlatform.mu.Unlock()
		t.Fatal("expected follow-up action card after revision turn")
	}
	lastCardText := originPlatform.cards[len(originPlatform.cards)-1].RenderText()
	originPlatform.mu.Unlock()
	if !strings.Contains(lastCardText, "Read Aloud") || !strings.Contains(lastCardText, "Review") {
		t.Fatalf("follow-up card = %q, want result actions", lastCardText)
	}

	flow := origin.getReviewFlow(sessionKey)
	if flow == nil || flow.ReviewerProject != "qoder" {
		t.Fatalf("flow = %#v, want qoder reviewer", flow)
	}
	if flow.LastOriginSummary != "Original summary from codex." {
		t.Fatalf("origin summary = %q", flow.LastOriginSummary)
	}
	if flow.LastReviewSummary != "reviewer summary" {
		t.Fatalf("review summary = %q, want reviewer summary", flow.LastReviewSummary)
	}
	if flow.Running {
		t.Fatal("expected flow to be marked not running after completion")
	}
}

func TestStartReviewCycle_UsesOriginPlatformForReviewerOutput(t *testing.T) {
	rm := NewRelayManager("")
	originAgent := &scriptedRecordingAgent{responses: []string{
		"<review_packet><recommended_review_target>summary_only</recommended_review_target></review_packet>",
		"revised summary",
	}}
	reviewerAgent := &scriptedRecordingAgent{responses: []string{"reviewer summary"}}
	originPlatform := &stubCardPlatform{n: "feishu"}
	reviewerPlatform := &stubCardPlatform{n: "worker"}
	origin := NewEngine("codex", originAgent, []Platform{originPlatform}, "", LangEnglish)
	reviewer := NewEngine("qoder", reviewerAgent, []Platform{reviewerPlatform}, "", LangEnglish)
	rm.RegisterEngine(origin.name, origin)
	rm.RegisterEngine(reviewer.name, reviewer)
	origin.SetRelayManager(rm)
	reviewer.SetRelayManager(rm)

	sessionKey := "feishu:chat:user"
	originSession := origin.sessions.GetOrCreateActive(sessionKey)
	originSession.AddHistory("assistant", "Original summary from codex.")

	if err := origin.startReviewCycle(sessionKey, "qoder"); err != nil {
		t.Fatalf("startReviewCycle: %v", err)
	}

	originPlatform.mu.Lock()
	defer originPlatform.mu.Unlock()
	if len(originPlatform.sent) == 0 {
		t.Fatal("expected reviewer output to be sent via origin platform")
	}
	if !strings.Contains(strings.Join(originPlatform.sent, "\n"), "qoder: reviewer summary") {
		t.Fatalf("origin platform sent = %#v, want prefixed reviewer output", originPlatform.sent)
	}
}

func TestExtractReviewPacket(t *testing.T) {
	input := "prefix\n<review_packet><repo>demo</repo></review_packet>\nsuffix"
	got := extractReviewPacket(input)
	want := "<review_packet><repo>demo</repo></review_packet>"
	if got != want {
		t.Fatalf("extractReviewPacket = %q, want %q", got, want)
	}
}

func TestHandleCardNav_ReviewOpenSendsSelectorCard(t *testing.T) {
	rm := NewRelayManager("")
	originPlatform := &stubCardPlatform{n: "feishu"}
	origin := NewEngine("codex", &stubAgent{}, []Platform{originPlatform}, "", LangEnglish)
	reviewer := NewEngine("qoder", &stubAgent{}, []Platform{&stubCardPlatform{n: "worker"}}, "", LangEnglish)
	rm.RegisterEngine(origin.name, origin)
	rm.RegisterEngine(reviewer.name, reviewer)
	origin.SetRelayManager(rm)
	reviewer.SetRelayManager(rm)

	card := origin.handleCardNav("act:/review open", "feishu:chat:user")
	if card == nil {
		t.Fatal("expected original action card to be returned")
	}

	originPlatform.mu.Lock()
	defer originPlatform.mu.Unlock()
	if len(originPlatform.cards) == 0 {
		t.Fatal("expected reviewer selector card to be sent")
	}
	if got := originPlatform.cards[len(originPlatform.cards)-1].RenderText(); !strings.Contains(got, "qoder") {
		t.Fatalf("selector card = %q, want reviewer option", got)
	}
}

func TestReviewerSelectorCard_GroupsButtonsByRow(t *testing.T) {
	rm := NewRelayManager("")
	origin := NewEngine("codex", &stubAgent{}, nil, "", LangEnglish)
	rm.RegisterEngine("codex", origin)
	rm.RegisterEngine("claude", NewEngine("claude", &stubAgent{}, nil, "", LangEnglish))
	rm.RegisterEngine("gemini", NewEngine("gemini", &stubAgent{}, nil, "", LangEnglish))
	rm.RegisterEngine("qoder", NewEngine("qoder", &stubAgent{}, nil, "", LangEnglish))
	origin.SetRelayManager(rm)

	card := origin.reviewerSelectorCard()
	if card == nil {
		t.Fatal("expected selector card")
	}

	actionRows := 0
	for _, el := range card.Elements {
		if actions, ok := el.(CardActions); ok {
			actionRows++
			if len(actions.Buttons) > 2 {
				t.Fatalf("row button count = %d, want <= 2", len(actions.Buttons))
			}
		}
	}
	if actionRows != 2 {
		t.Fatalf("action rows = %d, want 2 for 3 reviewers", actionRows)
	}
}

func TestRunManagedTurn_UsesToolResultContentAndHonorsQuiet(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	session := e.sessions.GetOrCreateActive("test:user1")
	state := newInteractiveState(&scriptedAgentSession{
		events: make(chan Event, 4),
		queue: []Event{
			{Type: EventToolResult, Content: "tool output from content field"},
			{Type: EventText, Content: "final"},
			{Type: EventResult, Done: true},
		},
	}, p, "ctx", false)

	got, err := e.runManagedTurn(state, session, "test:user1", "prompt", managedTurnOpts{Prefix: "qoder: ", ShowToolResults: true})
	if err != nil {
		t.Fatalf("runManagedTurn: %v", err)
	}
	if got != "final" {
		t.Fatalf("final = %q, want final", got)
	}
	if !strings.Contains(strings.Join(p.sent, "\n"), "qoder: tool output from content field") {
		t.Fatalf("sent = %#v, want tool result content with prefix", p.sent)
	}

	quietPlatform := &stubPlatformEngine{n: "test"}
	quietSession := e.sessions.GetOrCreateActive("test:user2")
	quietState := newInteractiveState(&scriptedAgentSession{
		events: make(chan Event, 3),
		queue: []Event{
			{Type: EventToolResult, Content: "hidden tool output"},
			{Type: EventText, Content: "quiet final"},
			{Type: EventResult, Done: true},
		},
	}, quietPlatform, "ctx", true)

	if _, err := e.runManagedTurn(quietState, quietSession, "test:user2", "prompt", managedTurnOpts{Prefix: "qoder: ", ShowToolResults: true}); err != nil {
		t.Fatalf("runManagedTurn quiet: %v", err)
	}
	if strings.Contains(strings.Join(quietPlatform.sent, "\n"), "hidden tool output") {
		t.Fatalf("quiet sent = %#v, want tool output suppressed", quietPlatform.sent)
	}
}

func TestRunManagedTurn_HidesToolResultByDefault(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	session := e.sessions.GetOrCreateActive("test:user3")
	state := newInteractiveState(&scriptedAgentSession{
		events: make(chan Event, 3),
		queue: []Event{
			{Type: EventToolResult, Content: "default hidden tool output"},
			{Type: EventText, Content: "final"},
			{Type: EventResult, Done: true},
		},
	}, p, "ctx", false)

	got, err := e.runManagedTurn(state, session, "test:user3", "prompt", managedTurnOpts{Prefix: "qoder: "})
	if err != nil {
		t.Fatalf("runManagedTurn: %v", err)
	}
	if got != "final" {
		t.Fatalf("final = %q, want final", got)
	}
	if strings.Contains(strings.Join(p.sent, "\n"), "default hidden tool output") {
		t.Fatalf("sent = %#v, want tool output hidden by default", p.sent)
	}
}

func TestSendChunksWithPrefix_PrefixesEveryChunk(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)

	content := strings.Repeat("a", maxPlatformMessageLen+32)
	prefix := "codex review-packet: "

	if err := e.sendChunksWithPrefix(p, "ctx", prefix, content); err != nil {
		t.Fatalf("sendChunksWithPrefix: %v", err)
	}
	if len(p.sent) < 2 {
		t.Fatalf("sent chunks = %d, want at least 2", len(p.sent))
	}
	for i, msg := range p.sent {
		if !strings.HasPrefix(msg, prefix) {
			t.Fatalf("chunk %d = %q, want prefix %q", i, msg, prefix)
		}
		if len(msg) > maxPlatformMessageLen {
			t.Fatalf("chunk %d len = %d, want <= %d", i, len(msg), maxPlatformMessageLen)
		}
	}
}

func TestSendChunksWithPrefix_EmptyPrefixPreservesChunks(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)

	content := strings.Repeat("a", maxPlatformMessageLen+32)

	if err := e.sendChunksWithPrefix(p, "ctx", "", content); err != nil {
		t.Fatalf("sendChunksWithPrefix: %v", err)
	}
	if len(p.sent) < 2 {
		t.Fatalf("sent chunks = %d, want at least 2", len(p.sent))
	}
	if strings.Join(p.sent, "") != content {
		t.Fatalf("joined chunks != original content")
	}
	for i, msg := range p.sent {
		if len(msg) > maxPlatformMessageLen {
			t.Fatalf("chunk %d len = %d, want <= %d", i, len(msg), maxPlatformMessageLen)
		}
	}
}

func TestSendChunksWithPrefix_OversizedPrefixFallsBackToBoundedChunks(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)

	prefix := strings.Repeat("p", maxPlatformMessageLen+10)
	content := "body"

	if err := e.sendChunksWithPrefix(p, "ctx", prefix, content); err != nil {
		t.Fatalf("sendChunksWithPrefix: %v", err)
	}
	if len(p.sent) < 2 {
		t.Fatalf("sent chunks = %d, want at least 2", len(p.sent))
	}
	if strings.Join(p.sent, "") != prefix+content {
		t.Fatalf("joined chunks != prefixed content")
	}
	for i, msg := range p.sent {
		if len(msg) > maxPlatformMessageLen {
			t.Fatalf("chunk %d len = %d, want <= %d", i, len(msg), maxPlatformMessageLen)
		}
	}
}

func TestSplitMessage_KeepsUTF8Boundaries(t *testing.T) {
	text := strings.Repeat("你", maxPlatformMessageLen/3+8)

	chunks := splitMessage(text, maxPlatformMessageLen)
	if len(chunks) < 2 {
		t.Fatalf("chunks = %d, want at least 2", len(chunks))
	}
	if strings.Join(chunks, "") != text {
		t.Fatalf("joined chunks != original text")
	}
	for i, chunk := range chunks {
		if !utf8.ValidString(chunk) {
			t.Fatalf("chunk %d is not valid UTF-8", i)
		}
		if len(chunk) > maxPlatformMessageLen {
			t.Fatalf("chunk %d len = %d, want <= %d", i, len(chunk), maxPlatformMessageLen)
		}
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

func TestProcessInteractiveMessage_IncludesQuotedContextInPrompt(t *testing.T) {
	agent := &recordingSendAgent{}
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)

	msg := &Message{
		SessionKey:      "test:user1",
		Platform:        "test",
		UserID:          "user1",
		UserName:        "Alice",
		Content:         "Can you act on this?",
		QuotedUserName:  "Codex",
		QuotedContent:   "Please update the deployment script and keep the current env vars.",
		QuotedMessageID: "m-1",
	}

	session := e.sessions.GetOrCreateActive(msg.SessionKey)
	if !session.TryLock() {
		t.Fatal("expected session lock")
	}

	e.processInteractiveMessage(p, msg, session)

	sends := agent.session.Sends()
	if len(sends) != 1 {
		t.Fatalf("expected 1 send, got %d", len(sends))
	}
	got := sends[0].prompt
	if !strings.Contains(got, "Reply context from Codex:") {
		t.Fatalf("prompt missing quoted header: %q", got)
	}
	if !strings.Contains(got, "Please update the deployment script") {
		t.Fatalf("prompt missing quoted content: %q", got)
	}
	if !strings.Contains(got, "User message:\nCan you act on this?") {
		t.Fatalf("prompt missing user message section: %q", got)
	}

	history := session.GetHistory(2)
	if len(history) < 2 || history[0].Role != "user" || history[0].Content != got {
		t.Fatalf("history content = %#v, want enriched prompt as user entry", history)
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

func TestProcessInteractiveEvents_FatalClaudeImageErrorCleansInteractiveState(t *testing.T) {
	e := NewEngine("test", &namedStubAgent{name: "claudecode"}, nil, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
	p := &stubPlatformEngine{n: "test"}
	events := make(chan Event, 1)
	events <- Event{
		Type:    EventResult,
		Content: "API Error: 400 {\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"Could not process image\"}}",
		Done:    true,
	}
	close(events)

	session := &Session{ID: "s14", AgentSessionID: "bad-session"}
	state := newInteractiveState(&eventfulStubAgentSession{events: events}, p, "ctx", false)
	e.interactiveStates["test:user1"] = state

	e.processInteractiveEvents(state, session, "test:user1", "msg-1", time.Now())

	if session.AgentSessionID != "" {
		t.Fatalf("AgentSessionID = %q, want empty", session.AgentSessionID)
	}
	if !session.NeedsReplay {
		t.Fatal("expected session to require replay after fatal image error")
	}
	if _, ok := e.interactiveStates["test:user1"]; ok {
		t.Fatal("expected interactive state to be cleaned up after fatal image error")
	}
}

func TestProcessInteractiveEvents_RequestTooLargeCleansInteractiveState(t *testing.T) {
	e := NewEngine("test", &namedStubAgent{name: "claudecode"}, nil, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
	p := &stubPlatformEngine{n: "test"}
	events := make(chan Event, 1)
	events <- Event{
		Type:    EventResult,
		Content: "Request too large (max 20MB). Try with a smaller file.",
		Done:    true,
	}
	close(events)

	session := &Session{ID: "s15", AgentSessionID: "too-large-session"}
	state := newInteractiveState(&eventfulStubAgentSession{events: events}, p, "ctx", false)
	e.interactiveStates["test:user2"] = state

	e.processInteractiveEvents(state, session, "test:user2", "msg-2", time.Now())

	if session.AgentSessionID != "" {
		t.Fatalf("AgentSessionID = %q, want empty", session.AgentSessionID)
	}
	if !session.NeedsReplay {
		t.Fatal("expected session to require replay after request-too-large error")
	}
	if _, ok := e.interactiveStates["test:user2"]; ok {
		t.Fatal("expected interactive state to be cleaned up after request-too-large error")
	}
}

func TestProcessInteractiveEvents_FirstEventTimeout(t *testing.T) {
	e := newTestEngine()
	e.SetFirstEventTimeout(20 * time.Millisecond)
	e.SetEventIdleTimeout(0)
	e.SetTurnTimeout(0)

	p := &stubPlatformEngine{n: "test"}
	session := e.sessions.GetOrCreateActive("test:user1")
	state := newInteractiveState(&eventfulStubAgentSession{events: make(chan Event)}, p, "ctx", false)

	done := make(chan struct{})
	go func() {
		defer close(done)
		e.processInteractiveEvents(state, session, "test:user1", "msg-1", time.Now())
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("expected first-event timeout to stop processInteractiveEvents")
	}

	if len(p.sent) == 0 || !strings.Contains(p.sent[len(p.sent)-1], "timed out waiting for first response") {
		t.Fatalf("expected timeout reply, got %#v", p.sent)
	}
}

func TestProcessInteractiveEvents_TurnTimeout(t *testing.T) {
	e := newTestEngine()
	e.SetFirstEventTimeout(0)
	e.SetEventIdleTimeout(0)
	e.SetTurnTimeout(30 * time.Millisecond)

	p := &stubPlatformEngine{n: "test"}
	events := make(chan Event, 1)
	events <- Event{Type: EventThinking, Content: "still working"}
	session := e.sessions.GetOrCreateActive("test:user1")
	state := newInteractiveState(&eventfulStubAgentSession{events: events}, p, "ctx", false)

	done := make(chan struct{})
	go func() {
		defer close(done)
		e.processInteractiveEvents(state, session, "test:user1", "msg-1", time.Now())
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("expected turn timeout to stop processInteractiveEvents")
	}

	if len(p.sent) == 0 || !strings.Contains(p.sent[len(p.sent)-1], "timed out before completing the turn") {
		t.Fatalf("expected timeout reply, got %#v", p.sent)
	}
}

func TestHandleMessage_BuffersAttachmentsUntilTextArrives(t *testing.T) {
	agent := &recordingSendAgent{}
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)

	e.handleMessage(p, &Message{
		SessionKey: "test:user1",
		Platform:   "test",
		MessageID:  "img-1",
		Images:     []ImageAttachment{{MimeType: "image/png", Data: []byte("img1")}},
		ReplyCtx:   "ctx",
	})

	if got := len(p.sent); got != 1 {
		t.Fatalf("reply count = %d, want 1 buffering ack", got)
	}
	if !strings.Contains(p.sent[0], "Received 1 attachment") {
		t.Fatalf("buffering ack = %q", p.sent[0])
	}
	if agent.session != nil && len(agent.session.Sends()) != 0 {
		t.Fatalf("expected no agent send for image-only message")
	}

	e.handleMessage(p, &Message{
		SessionKey: "test:user1",
		Platform:   "test",
		MessageID:  "txt-1",
		Content:    "please review this screenshot",
		ReplyCtx:   "ctx",
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if agent.session != nil && len(agent.session.Sends()) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if agent.session == nil {
		t.Fatal("expected agent session to start")
	}
	sends := agent.session.Sends()
	if len(sends) != 1 {
		t.Fatalf("send count = %d, want 1", len(sends))
	}
	if sends[0].prompt != "please review this screenshot" {
		t.Fatalf("prompt = %q", sends[0].prompt)
	}
	if len(sends[0].images) != 1 || string(sends[0].images[0].Data) != "img1" {
		t.Fatalf("images = %#v, want buffered image", sends[0].images)
	}
	if pending := e.getPendingAttachments("test:user1"); pending != nil {
		t.Fatalf("expected pending attachments to be cleared after send, got %#v", pending)
	}
}

func TestHandleMessage_SendsTextAndImagesImmediately(t *testing.T) {
	agent := &recordingSendAgent{}
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)

	e.handleMessage(p, &Message{
		SessionKey: "test:user1",
		Platform:   "test",
		MessageID:  "mix-1",
		Content:    "summarize this",
		Images:     []ImageAttachment{{MimeType: "image/png", Data: []byte("img1")}},
		ReplyCtx:   "ctx",
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if agent.session != nil && len(agent.session.Sends()) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if agent.session == nil {
		t.Fatal("expected agent session to start")
	}
	sends := agent.session.Sends()
	if len(sends) != 1 {
		t.Fatalf("send count = %d, want 1", len(sends))
	}
	for _, sent := range p.sent {
		if strings.Contains(sent, "Received 1 attachment") {
			t.Fatalf("expected no buffering ack for mixed message, got %#v", p.sent)
		}
	}
	if sends[0].prompt != "summarize this" || len(sends[0].images) != 1 {
		t.Fatalf("send = %#v", sends[0])
	}
}

func TestHandleMessage_BufferedAttachmentsStayScopedToSessionKey(t *testing.T) {
	agent := &recordingSendAgent{}
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)

	e.handleMessage(p, &Message{
		SessionKey: "test:user1",
		Platform:   "test",
		MessageID:  "img-1",
		Images:     []ImageAttachment{{MimeType: "image/png", Data: []byte("img1")}},
		ReplyCtx:   "ctx",
	})
	e.handleMessage(p, &Message{
		SessionKey: "test:user2",
		Platform:   "test",
		MessageID:  "txt-2",
		Content:    "no image here",
		ReplyCtx:   "ctx",
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if agent.session != nil && len(agent.session.Sends()) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	sends := agent.session.Sends()
	if len(sends) != 1 {
		t.Fatalf("send count = %d, want 1", len(sends))
	}
	if len(sends[0].images) != 0 || len(sends[0].files) != 0 {
		t.Fatalf("expected no cross-session buffered attachments, got %#v", sends[0])
	}
	if pending := e.getPendingAttachments("test:user1"); pending == nil || len(pending.Images) != 1 {
		t.Fatalf("expected user1 buffered attachment to remain pending, got %#v", pending)
	}
}

func TestHandleMessage_BuffersFileOnlyUntilTextArrives(t *testing.T) {
	agent := &recordingSendAgent{}
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)

	e.handleMessage(p, &Message{
		SessionKey: "test:user1",
		Platform:   "test",
		MessageID:  "file-1",
		Files: []FileAttachment{{
			MimeType: "video/mp4",
			Data:     []byte("video"),
			FileName: "clip.mp4",
		}},
		ReplyCtx: "ctx",
	})

	e.handleMessage(p, &Message{
		SessionKey: "test:user1",
		Platform:   "test",
		MessageID:  "txt-1",
		Content:    "summarize this clip",
		ReplyCtx:   "ctx",
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if agent.session != nil && len(agent.session.Sends()) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	sends := agent.session.Sends()
	if len(sends) != 1 {
		t.Fatalf("send count = %d, want 1", len(sends))
	}
	if len(sends[0].files) != 1 || sends[0].files[0].FileName != "clip.mp4" {
		t.Fatalf("files = %#v, want buffered file", sends[0].files)
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

func TestCmdStopReleasesBusySessionWhenEventLoopIsWaiting(t *testing.T) {
	e := newTestEngine()
	p := &stubPlatformEngine{n: "test"}
	sessionKey := "test:user1"
	session := e.sessions.GetOrCreateActive(sessionKey)
	if !session.TryLock() {
		t.Fatal("expected fresh session lock to succeed")
	}

	state := newInteractiveState(&eventfulStubAgentSession{events: make(chan Event)}, p, "ctx", false)
	e.interactiveMu.Lock()
	e.interactiveStates[sessionKey] = state
	e.interactiveMu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer session.Unlock()
		e.processInteractiveEvents(state, session, sessionKey, "msg-1", time.Now())
	}()

	e.cmdStop(p, &Message{SessionKey: sessionKey, ReplyCtx: "ctx"})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("expected processInteractiveEvents to stop promptly after /stop")
	}

	if !session.TryLock() {
		t.Fatal("expected session lock to be released after /stop")
	}
	session.Unlock()

	if len(p.sent) == 0 || p.sent[len(p.sent)-1] != e.i18n.T(MsgExecutionStopped) {
		t.Fatalf("expected stop confirmation reply, got %#v", p.sent)
	}
}

func TestCmdStopClearsStaleBusyLockWithoutInteractiveState(t *testing.T) {
	e := newTestEngine()
	p := &stubPlatformEngine{n: "test"}
	sessionKey := "test:user-stale-stop"
	session := e.sessions.GetOrCreateActive(sessionKey)
	if !session.TryLock() {
		t.Fatal("expected fresh session lock to succeed")
	}

	e.cmdStop(p, &Message{SessionKey: sessionKey, ReplyCtx: "ctx"})

	if !session.TryLock() {
		t.Fatal("expected stale busy lock to be released by /stop")
	}
	session.Unlock()

	if len(p.sent) == 0 || p.sent[len(p.sent)-1] != e.i18n.T(MsgExecutionStopped) {
		t.Fatalf("expected stop confirmation reply, got %#v", p.sent)
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
