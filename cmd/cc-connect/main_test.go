package main

import (
	"context"
	"testing"

	"github.com/chenhg5/cc-connect/config"
	"github.com/chenhg5/cc-connect/core"
)

type buildJobManagerTestAgent struct{}

func (a *buildJobManagerTestAgent) Name() string { return "test-agent" }
func (a *buildJobManagerTestAgent) StartSession(
	_ context.Context,
	_ string,
) (core.AgentSession, error) {
	return &buildJobManagerTestSession{}, nil
}
func (a *buildJobManagerTestAgent) ListSessions(
	_ context.Context,
) ([]core.AgentSessionInfo, error) {
	return nil, nil
}
func (a *buildJobManagerTestAgent) Stop() error { return nil }

type buildJobManagerTestSession struct{}

func (s *buildJobManagerTestSession) Send(_ string, _ []core.ImageAttachment) error {
	return nil
}
func (s *buildJobManagerTestSession) RespondPermission(
	_ string,
	_ core.PermissionResult,
) error {
	return nil
}
func (s *buildJobManagerTestSession) Events() <-chan core.Event {
	ch := make(chan core.Event, 1)
	ch <- core.Event{Type: core.EventResult, Content: "job finished"}
	close(ch)
	return ch
}
func (s *buildJobManagerTestSession) CurrentSessionID() string { return "session-1" }
func (s *buildJobManagerTestSession) Alive() bool              { return true }
func (s *buildJobManagerTestSession) Close() error             { return nil }

func TestBuildJobManagerRegistersProjectRunners(t *testing.T) {
	projects := []config.ProjectConfig{
		{Name: "proj-a"},
		{Name: "proj-b"},
	}
	engines := []*core.Engine{
		core.NewEngine("proj-a", &buildJobManagerTestAgent{}, nil, "", core.LangEnglish),
		core.NewEngine("proj-b", &buildJobManagerTestAgent{}, nil, "", core.LangEnglish),
	}

	jobMgr, err := buildJobManager(t.TempDir(), projects, engines)
	if err != nil {
		t.Fatalf("buildJobManager: %v", err)
	}

	job, err := jobMgr.Start(core.JobRequest{Project: "proj-b", Prompt: "run"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if job.Project != "proj-b" {
		t.Fatalf("unexpected project: %q", job.Project)
	}
}
