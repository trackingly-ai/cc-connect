package core

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

type jobTestAgent struct {
	session *jobTestSession
	env     []string
}

func (a *jobTestAgent) Name() string { return "job-test" }

func (a *jobTestAgent) StartSession(_ context.Context, _ string) (AgentSession, error) {
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

func (s *jobTestSession) Send(prompt string, _ []ImageAttachment) error {
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
			WorktreePath: "/repo/.echo/task-7",
			Branch:       "echo/task-7",
		},
	})
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
		"CC_TASK_ID=task-7",
		"CC_REPO_PATH=/repo",
		"CC_WORKTREE_PATH=/repo/.echo/task-7",
		"CC_BRANCH=echo/task-7",
	} {
		if !strings.Contains(env, want) {
			t.Fatalf("env missing %q: %s", want, env)
		}
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
	})
	if err == nil {
		t.Fatal("expected permission request error")
	}
	if !strings.Contains(err.Error(), "permission request") {
		t.Fatalf("error = %v, want permission request", err)
	}
}

func TestSummarizeJobOutput(t *testing.T) {
	short := summarizeJobOutput("short")
	if short != "short" {
		t.Fatalf("short summary = %q", short)
	}

	longInput := strings.Repeat("a", maxJobSummaryLen+20)
	summary := summarizeJobOutput(longInput)
	if len([]rune(summary)) != maxJobSummaryLen {
		t.Fatalf("summary len = %d, want %d", len([]rune(summary)), maxJobSummaryLen)
	}
	if !strings.HasSuffix(summary, "...") {
		t.Fatalf("summary = %q, want ellipsis", summary)
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
	})
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
		})
		errCh <- err
	}()

	<-agent.firstStartReady

	go func() {
		_, err := engine.JobRunner().Run(context.Background(), JobRequest{
			Project: "proj-serial",
			TaskID:  "task-b",
			Prompt:  "second",
		})
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
