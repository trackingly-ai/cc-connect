package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type cronShellAgent struct {
	stubAgent
	workDir string
}

func (a *cronShellAgent) GetWorkDir() string { return a.workDir }

type cronReplyPlatform struct {
	stubPlatformEngine
}

func (p *cronReplyPlatform) ReconstructReplyCtx(sessionKey string) (any, error) {
	return sessionKey, nil
}

type blockingCronAgent struct {
	mu      sync.Mutex
	session *blockingCronSession
}

func (a *blockingCronAgent) Name() string { return "blocking" }
func (a *blockingCronAgent) StartSession(_ context.Context, _ string) (AgentSession, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.session == nil {
		a.session = newBlockingCronSession()
	}
	return a.session, nil
}
func (a *blockingCronAgent) ListSessions(_ context.Context) ([]AgentSessionInfo, error) {
	return nil, nil
}
func (a *blockingCronAgent) Stop() error { return nil }

type blockingCronSession struct {
	events  chan Event
	closeCh chan struct{}
	once    sync.Once
}

func newBlockingCronSession() *blockingCronSession {
	return &blockingCronSession{
		events:  make(chan Event),
		closeCh: make(chan struct{}),
	}
}

func (s *blockingCronSession) Send(_ string, _ []ImageAttachment, _ []FileAttachment) error {
	<-s.closeCh
	return nil
}
func (s *blockingCronSession) RespondPermission(_ string, _ PermissionResult) error { return nil }
func (s *blockingCronSession) Events() <-chan Event                                 { return s.events }
func (s *blockingCronSession) CurrentSessionID() string                             { return "blocking-cron" }
func (s *blockingCronSession) Alive() bool                                          { return true }
func (s *blockingCronSession) Close() error {
	s.once.Do(func() {
		close(s.closeCh)
		close(s.events)
	})
	return nil
}

func TestAPIServerHandleCronAddRequiresSessionKey(t *testing.T) {
	store, err := NewCronStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewCronStore: %v", err)
	}

	engine := NewEngine("test", &stubAgent{}, nil, "", LangEnglish)
	server := &APIServer{
		engines: map[string]*Engine{"test": engine},
		cron:    NewCronScheduler(store),
	}

	body := strings.NewReader(`{"project":"test","cron_expr":"0 6 * * *","prompt":"hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/cron/add", body)
	rec := httptest.NewRecorder()

	server.handleCronAdd(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "session_key is required") {
		t.Fatalf("body = %q, want session_key error", rec.Body.String())
	}
}

func TestEngineExecuteCronShell(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	engine := NewEngine("test", &cronShellAgent{workDir: t.TempDir()}, []Platform{p}, "", LangEnglish)

	job := &CronJob{
		Exec:    "printf hello",
		WorkDir: t.TempDir(),
	}
	if err := engine.executeCronShell(p, nil, job); err != nil {
		t.Fatalf("executeCronShell: %v", err)
	}
	if len(p.sent) != 1 {
		t.Fatalf("sent count = %d, want 1", len(p.sent))
	}
	if !strings.Contains(p.sent[0], "hello") {
		t.Fatalf("sent message = %q, want command output", p.sent[0])
	}
}

func TestAPIServerHandleCronAddAcceptsExec(t *testing.T) {
	store, err := NewCronStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewCronStore: %v", err)
	}

	engine := NewEngine("test", &stubAgent{}, nil, "", LangEnglish)
	engine.interactiveStates["telegram:1:1"] = &interactiveState{}
	server := &APIServer{
		engines: map[string]*Engine{"test": engine},
		cron:    NewCronScheduler(store),
	}

	body := strings.NewReader(`{"project":"test","session_key":"telegram:1:1","cron_expr":"0 6 * * *","exec":"printf hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/cron/add", body)
	rec := httptest.NewRecorder()

	server.handleCronAdd(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var job CronJob
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if job.Exec != "printf hello" {
		t.Fatalf("job.Exec = %q, want %q", job.Exec, "printf hello")
	}
}

func TestEngineActiveSessionKeys(t *testing.T) {
	engine := NewEngine("test", &stubAgent{}, nil, "", LangEnglish)
	engine.interactiveStates["b"] = &interactiveState{}
	engine.interactiveStates["a"] = &interactiveState{}

	got := engine.ActiveSessionKeys()
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("ActiveSessionKeys() = %v, want [a b]", got)
	}
}

func TestCronStoreUpdateRejectsReadOnlyFieldAliases(t *testing.T) {
	store, err := NewCronStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewCronStore: %v", err)
	}
	job := &CronJob{
		ID:         "job-1",
		Project:    "test",
		SessionKey: "test:session",
		CronExpr:   "* * * * *",
		Prompt:     "hello",
		Enabled:    true,
		CreatedAt:  time.Now(),
		LastError:  "original",
	}
	if err := store.Add(job); err != nil {
		t.Fatalf("store.Add: %v", err)
	}

	if ok := store.Update(job.ID, "ID", "hacked"); ok {
		t.Fatal("expected uppercase read-only alias update to be rejected")
	}
	got := store.Get(job.ID)
	if got == nil || got.ID != "job-1" {
		t.Fatalf("job ID mutated unexpectedly: %#v", got)
	}
	if ok := store.Update(job.ID, "LastError", "forged"); ok {
		t.Fatal("expected LastError alias update to be rejected")
	}
}

func TestAPIServerHandleCronInfoRequiresGET(t *testing.T) {
	store, err := NewCronStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewCronStore: %v", err)
	}
	scheduler := NewCronScheduler(store)
	server := &APIServer{cron: scheduler}

	req := httptest.NewRequest(http.MethodPost, "/cron/info?id=job-1", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	server.handleCronInfo(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestEngineExecuteCronJobCancelsTimedOutInteractiveRun(t *testing.T) {
	agent := &blockingCronAgent{}
	p := &cronReplyPlatform{stubPlatformEngine{n: "test"}}
	engine := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	job := &CronJob{
		ID:         "job-1",
		Project:    "test",
		SessionKey: "test:session",
		CronExpr:   "* * * * *",
		Prompt:     "hello",
		Enabled:    true,
		CreatedAt:  time.Now(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- engine.ExecuteCronJob(ctx, job)
	}()

	select {
	case err := <-done:
		if err != context.DeadlineExceeded {
			t.Fatalf("ExecuteCronJob err = %v, want %v", err, context.DeadlineExceeded)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ExecuteCronJob did not finish after timeout cleanup")
	}
	engine.interactiveMu.Lock()
	defer engine.interactiveMu.Unlock()
	if _, ok := engine.interactiveStates[job.SessionKey]; ok {
		t.Fatalf("interactive state for %q was not cleaned up", job.SessionKey)
	}
}

func TestCronSchedulerUpdateJobDoesNotPersistInvalidSessionMode(t *testing.T) {
	store, err := NewCronStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewCronStore: %v", err)
	}
	scheduler := NewCronScheduler(store)
	job := &CronJob{
		ID:         "job-1",
		Project:    "test",
		SessionKey: "test:session",
		CronExpr:   "* * * * *",
		Prompt:     "hello",
		Enabled:    true,
		CreatedAt:  time.Now(),
	}
	if err := store.Add(job); err != nil {
		t.Fatalf("store.Add: %v", err)
	}

	if err := scheduler.UpdateJob(job.ID, "session_mode", "bogus-mode"); err == nil {
		t.Fatal("expected invalid session_mode to be rejected")
	}
	got := store.Get(job.ID)
	if got == nil {
		t.Fatal("expected stored job")
	}
	if got.SessionMode != "" {
		t.Fatalf("SessionMode = %q, want unchanged empty string", got.SessionMode)
	}
}
