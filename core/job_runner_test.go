package core

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

type jobTestAgent struct {
	session           *jobTestSession
	env               []string
	capturedSessionID string
}

func (a *jobTestAgent) Name() string { return "job-test" }

func (a *jobTestAgent) StartSession(_ context.Context, sessionID string) (AgentSession, error) {
	a.capturedSessionID = sessionID
	return a.session, nil
}

func (a *jobTestAgent) ListSessions(_ context.Context) ([]AgentSessionInfo, error) {
	return nil, nil
}

func (a *jobTestAgent) Stop() error { return nil }

func (a *jobTestAgent) SetSessionEnv(env []string) {
	a.env = append([]string(nil), env...)
}

type jobTestSession struct {
	events chan Event
	onSend func(prompt string)
}

func (s *jobTestSession) Send(prompt string, _ []ImageAttachment, _ []FileAttachment) error {
	if s.onSend != nil {
		s.onSend(prompt)
	}
	return nil
}

func (s *jobTestSession) RespondPermission(_ string, _ PermissionResult) error { return nil }
func (s *jobTestSession) Events() <-chan Event                                 { return s.events }
func (s *jobTestSession) CurrentSessionID() string                             { return "job-session" }
func (s *jobTestSession) Alive() bool                                          { return true }
func (s *jobTestSession) Close() error                                         { return nil }

func TestEngineJobRunnerCompletes(t *testing.T) {
	session := &jobTestSession{events: make(chan Event, 4)}
	session.onSend = func(prompt string) {
		if prompt != "implement feature" {
			t.Fatalf("prompt = %q, want implement feature", prompt)
		}
		session.events <- Event{Type: EventText, Content: "hello "}
		session.events <- Event{
			Type:      EventResult,
			Content:   "world",
			SessionID: "agent-session-1",
		}
		close(session.events)
	}

	agent := &jobTestAgent{session: session}
	engine := NewEngine("proj-a", agent, nil, "", LangEnglish)

	result, err := engine.JobRunner().Run(context.Background(), JobRequest{
		Project: "proj-a",
		TaskID:  "task-7",
		Prompt:  "implement feature",
		WorkspaceRef: JobWorkspaceRef{
			RepoPath:     "/repo",
			WorktreePath: t.TempDir(),
			Branch:       "echo/task-7",
		},
	}, "job-123", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Output != "world" {
		t.Fatalf("Output = %q, want world", result.Output)
	}
	if result.Summary != "world" {
		t.Fatalf("Summary = %q, want world", result.Summary)
	}
	if result.SessionID != "agent-session-1" {
		t.Fatalf("SessionID = %q, want agent-session-1", result.SessionID)
	}

	env := strings.Join(agent.env, "\n")
	for _, want := range []string{
		"CC_PROJECT=proj-a",
		"CC_SESSION_KEY=echo-job-job-123",
		"CC_TASK_ID=task-7",
		"ECHO_TASK_ID=task-7",
		"ECHO_JOB_ID=job-123",
		"CC_REPO_PATH=/repo",
		"CC_WORKTREE_PATH=",
		"CC_BRANCH=echo/task-7",
	} {
		if !strings.Contains(env, want) {
			t.Fatalf("env missing %q: %s", want, env)
		}
	}
	if agent.capturedSessionID != "" {
		t.Fatalf("sessionID = %q, want empty provider resume id", agent.capturedSessionID)
	}
}

func TestEngineJobRunnerRejectsPermissionRequest(t *testing.T) {
	session := &jobTestSession{events: make(chan Event, 2)}
	session.onSend = func(prompt string) {
		session.events <- Event{
			Type:     EventPermissionRequest,
			ToolName: "Bash",
		}
		close(session.events)
	}

	engine := NewEngine("proj-b", &jobTestAgent{session: session}, nil, "", LangEnglish)

	_, err := engine.JobRunner().Run(context.Background(), JobRequest{
		Project: "proj-b",
		Prompt:  "run dangerous command",
	}, "job-perm", nil)
	if err == nil {
		t.Fatal("expected permission request error")
	}
	if !strings.Contains(err.Error(), "permission request") {
		t.Fatalf("error = %v, want permission request", err)
	}
	coded, ok := err.(interface{ Code() string })
	if !ok || coded.Code() != JobErrorCodePermissionRequired {
		t.Fatalf("error code = %v, want %q", err, JobErrorCodePermissionRequired)
	}
}

func TestEngineJobRunnerEmitsStructuredJobEvents(t *testing.T) {
	session := &jobTestSession{events: make(chan Event, 6)}
	session.onSend = func(prompt string) {
		session.events <- Event{Type: EventThinking, Content: "Planning next step"}
		session.events <- Event{Type: EventToolUse, ToolName: "Bash", ToolInput: "git status"}
		session.events <- Event{Type: EventToolResult, ToolName: "Bash", ToolResult: "clean"}
		session.events <- Event{Type: EventText, Content: "Done."}
		session.events <- Event{Type: EventResult, Content: "final", SessionID: "session-7"}
		close(session.events)
	}

	engine := NewEngine("proj-events", &jobTestAgent{session: session}, nil, "", LangEnglish)
	var captured []JobEvent
	result, err := engine.JobRunner().Run(context.Background(), JobRequest{
		Project: "proj-events",
		Prompt:  "inspect repo",
	}, "job-events", func(event JobEvent) {
		captured = append(captured, event)
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.SessionID != "session-7" {
		t.Fatalf("SessionID = %q, want session-7", result.SessionID)
	}
	if len(captured) != 4 {
		t.Fatalf("captured len = %d, want 4", len(captured))
	}
	if captured[0].Type != string(EventThinking) {
		t.Fatalf("first event type = %q, want thinking", captured[0].Type)
	}
	if captured[1].ToolName != "Bash" || captured[1].ToolInput != "git status" {
		t.Fatalf("tool use = %#v, want Bash git status", captured[1])
	}
	if captured[2].Content != "clean" {
		t.Fatalf("tool result content = %q, want clean", captured[2].Content)
	}
	if captured[3].Content != "Done." {
		t.Fatalf("text content = %q, want Done.", captured[3].Content)
	}
}

func TestSummarizeJobOutput(t *testing.T) {
	short := summarizeJobOutput("short")
	if short != "short" {
		t.Fatalf("short summary = %q", short)
	}

	multiLine := "thinking...\nresult line"
	if summary := summarizeJobOutput(multiLine); summary != "result line" {
		t.Fatalf("summary = %q, want result line", summary)
	}

	longInput := strings.Repeat("a", 20) + "\n" + strings.Repeat("z", maxJobSummaryLen+20)
	summary := summarizeJobOutput(longInput)
	if len([]rune(summary)) != maxJobSummaryLen {
		t.Fatalf("summary len = %d, want %d", len([]rune(summary)), maxJobSummaryLen)
	}
	if !strings.HasPrefix(summary, "...") {
		t.Fatalf("summary = %q, want leading ellipsis", summary)
	}
}

func TestJobTextBufferKeepsOnlyRecentTail(t *testing.T) {
	var buffer jobTextBuffer
	buffer.Append(strings.Repeat("a", maxBufferedJobTextBytes/2))
	buffer.Append(strings.Repeat("b", maxBufferedJobTextBytes/2))
	buffer.Append(strings.Repeat("c", maxBufferedJobTextBytes/2))

	output := buffer.String()
	if !strings.HasPrefix(output, "[truncated ") {
		t.Fatalf("expected truncation notice, got %q", output[:min(len(output), 32)])
	}
	if strings.Contains(output, strings.Repeat("a", 32)) {
		t.Fatalf("expected oldest chunk to be dropped")
	}
	if !strings.Contains(output, strings.Repeat("c", 32)) {
		t.Fatalf("expected newest chunk to be retained")
	}
}

func TestJobTextBufferPreservesRuneBoundariesAndWhitespaceOnlyTail(t *testing.T) {
	var buffer jobTextBuffer
	buffer.Append(strings.Repeat("界", maxBufferedJobTextBytes/4))
	buffer.Append(strings.Repeat(" ", maxBufferedJobTextBytes))

	output := buffer.String()
	if !utf8.ValidString(output) {
		t.Fatalf("buffer output is not valid UTF-8")
	}
	if !strings.HasPrefix(output, "[truncated ") {
		t.Fatalf("output = %q, want truncation notice", output)
	}
}

func TestEngineJobRunnerReturnsAgentError(t *testing.T) {
	session := &jobTestSession{events: make(chan Event, 2)}
	session.onSend = func(prompt string) {
		session.events <- Event{Type: EventError, Error: errors.New("agent failed")}
		close(session.events)
	}

	engine := NewEngine("proj-c", &jobTestAgent{session: session}, nil, "", LangEnglish)
	_, err := engine.JobRunner().Run(context.Background(), JobRequest{
		Project: "proj-c",
		Prompt:  "fail",
	}, "job-fail", nil)
	if err == nil || err.Error() != "agent failed" {
		t.Fatalf("err = %v, want agent failed", err)
	}
}

type serialEnvAgent struct {
	mu                sync.Mutex
	env               []string
	capturedEnvs      [][]string
	startCount        int
	firstStartReady   chan struct{}
	releaseFirstStart chan struct{}
	started           chan int
}

func (a *serialEnvAgent) Name() string { return "serial-env" }

func (a *serialEnvAgent) SetSessionEnv(env []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.env = append([]string(nil), env...)
}

func (a *serialEnvAgent) StartSession(_ context.Context, _ string) (AgentSession, error) {
	a.mu.Lock()
	startIdx := a.startCount
	a.startCount++
	a.mu.Unlock()
	if a.started != nil {
		a.started <- startIdx
	}

	if startIdx == 0 {
		close(a.firstStartReady)
		<-a.releaseFirstStart
	}

	a.mu.Lock()
	captured := append([]string(nil), a.env...)
	a.capturedEnvs = append(a.capturedEnvs, captured)
	a.mu.Unlock()

	session := &jobTestSession{events: make(chan Event, 1)}
	session.onSend = func(prompt string) {
		session.events <- Event{Type: EventResult, Content: prompt}
		close(session.events)
	}
	return session, nil
}

func (a *serialEnvAgent) ListSessions(_ context.Context) ([]AgentSessionInfo, error) {
	return nil, nil
}

func (a *serialEnvAgent) Stop() error { return nil }

func waitForStartIndex(t *testing.T, started <-chan int, want int) {
	t.Helper()
	select {
	case got := <-started:
		if got != want {
			t.Fatalf("start index = %d, want %d", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for start index %d", want)
	}
}

type workDirCaptureAgent struct {
	mu              sync.Mutex
	workDir         string
	sessionEnv      []string
	capturedWorkDir string
}

func (a *workDirCaptureAgent) Name() string { return "workdir-capture" }

func (a *workDirCaptureAgent) SetWorkDir(dir string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.workDir = dir
}

func (a *workDirCaptureAgent) GetWorkDir() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.workDir
}

func (a *workDirCaptureAgent) SetSessionEnv(env []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessionEnv = append([]string(nil), env...)
}

func (a *workDirCaptureAgent) StartSession(_ context.Context, _ string) (AgentSession, error) {
	a.mu.Lock()
	a.capturedWorkDir = SessionWorkDirFromEnv(a.sessionEnv, a.workDir)
	a.mu.Unlock()
	return &jobTestSession{events: make(chan Event)}, nil
}

func (a *workDirCaptureAgent) ListSessions(_ context.Context) ([]AgentSessionInfo, error) {
	return nil, nil
}

func (a *workDirCaptureAgent) Stop() error { return nil }

func TestEngineJobRunnerSerializesSessionEnvInjection(t *testing.T) {
	agent := &serialEnvAgent{
		firstStartReady:   make(chan struct{}),
		releaseFirstStart: make(chan struct{}),
	}
	engine := NewEngine("proj-serial", agent, nil, "", LangEnglish)

	errCh := make(chan error, 2)
	go func() {
		_, err := engine.JobRunner().Run(context.Background(), JobRequest{
			Project: "proj-serial",
			TaskID:  "task-a",
			Prompt:  "first",
		}, "job-a", nil)
		errCh <- err
	}()

	<-agent.firstStartReady

	go func() {
		_, err := engine.JobRunner().Run(context.Background(), JobRequest{
			Project: "proj-serial",
			TaskID:  "task-b",
			Prompt:  "second",
		}, "job-b", nil)
		errCh <- err
	}()

	close(agent.releaseFirstStart)

	for range 2 {
		if err := <-errCh; err != nil {
			t.Fatalf("Run: %v", err)
		}
	}

	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.capturedEnvs) != 2 {
		t.Fatalf("captured env count = %d, want 2", len(agent.capturedEnvs))
	}

	first := strings.Join(agent.capturedEnvs[0], "\n")
	second := strings.Join(agent.capturedEnvs[1], "\n")
	if !strings.Contains(first, "CC_TASK_ID=task-a") {
		t.Fatalf("first env missing task-a: %s", first)
	}
	if !strings.Contains(second, "CC_TASK_ID=task-b") {
		t.Fatalf("second env missing task-b: %s", second)
	}
}

func TestEngineStartJobSessionRejectsMissingWorktree(t *testing.T) {
	session := &jobTestSession{events: make(chan Event, 1)}
	engine := NewEngine("proj-d", &jobTestAgent{session: session}, nil, "", LangEnglish)

	_, err := engine.StartJobSession(context.Background(), JobRequest{
		Project: "proj-d",
		Prompt:  "run",
		WorkspaceRef: JobWorkspaceRef{
			WorktreePath: "/definitely/missing/worktree",
		},
	}, "job-missing")
	if err == nil || !strings.Contains(err.Error(), "stat worktree_path") {
		t.Fatalf("err = %v, want missing worktree error", err)
	}
}

func TestEngineStartJobSessionUsesWorktreeWithoutMutatingDefaultWorkDir(t *testing.T) {
	worktreePath := t.TempDir()
	agent := &workDirCaptureAgent{workDir: "/default/workdir"}
	engine := NewEngine("proj-workdir", agent, nil, "", LangEnglish)

	session, err := engine.StartJobSession(context.Background(), JobRequest{
		Project: "proj-workdir",
		Prompt:  "run",
		WorkspaceRef: JobWorkspaceRef{
			WorktreePath: worktreePath,
		},
	}, "job-workdir")
	if err != nil {
		t.Fatalf("StartJobSession: %v", err)
	}
	defer func() {
		if err := session.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}()

	if got := agent.GetWorkDir(); got != "/default/workdir" {
		t.Fatalf("default workdir = %q, want /default/workdir", got)
	}

	agent.mu.Lock()
	captured := agent.capturedWorkDir
	agent.mu.Unlock()
	if captured != worktreePath {
		t.Fatalf("captured workdir = %q, want %q", captured, worktreePath)
	}
}

func TestEngineSerializesInteractiveAndJobSessionStartup(t *testing.T) {
	agent := &serialEnvAgent{
		firstStartReady:   make(chan struct{}),
		releaseFirstStart: make(chan struct{}),
		started:           make(chan int, 2),
	}
	engine := NewEngine("proj-serial", agent, []Platform{&stubPlatformEngine{n: "test"}}, "", LangEnglish)

	session := engine.sessions.GetOrCreateActive("test:user1")
	stateCh := make(chan *interactiveState, 1)
	go func() {
		stateCh <- engine.getOrCreateInteractiveState("test:user1", &stubPlatformEngine{n: "test"}, nil, session)
	}()

	<-agent.firstStartReady
	waitForStartIndex(t, agent.started, 0)

	jobSessionCh := make(chan AgentSession, 1)
	jobErrCh := make(chan error, 1)
	go func() {
		sess, err := engine.StartJobSession(context.Background(), JobRequest{
			Project: "proj-serial",
			TaskID:  "task-job",
			Prompt:  "run job",
		}, "job-1")
		if err != nil {
			jobErrCh <- err
			return
		}
		jobSessionCh <- sess
	}()

	select {
	case startIdx := <-agent.started:
		t.Fatalf("job start should be blocked until interactive start finishes, got index %d", startIdx)
	case <-time.After(200 * time.Millisecond):
	}

	close(agent.releaseFirstStart)

	state := <-stateCh
	if state == nil || state.agentSession == nil {
		t.Fatal("expected interactive state with agent session")
	}
	defer func() {
		if err := state.agentSession.Close(); err != nil {
			t.Fatalf("close interactive session: %v", err)
		}
	}()

	select {
	case err := <-jobErrCh:
		t.Fatalf("StartJobSession: %v", err)
	case sess := <-jobSessionCh:
		if err := sess.Close(); err != nil {
			t.Fatalf("close job session: %v", err)
		}
	}

	waitForStartIndex(t, agent.started, 1)

	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.capturedEnvs) != 2 {
		t.Fatalf("captured env count = %d, want 2", len(agent.capturedEnvs))
	}

	first := strings.Join(agent.capturedEnvs[0], "\n")
	second := strings.Join(agent.capturedEnvs[1], "\n")
	if !strings.Contains(first, "CC_SESSION_KEY=test:user1") {
		t.Fatalf("first env missing interactive session key: %s", first)
	}
	if !strings.Contains(second, "CC_SESSION_KEY=echo-job-job-1") {
		t.Fatalf("second env missing job session key: %s", second)
	}
	if !strings.Contains(second, "CC_TASK_ID=task-job") {
		t.Fatalf("second env missing task-job: %s", second)
	}
}
