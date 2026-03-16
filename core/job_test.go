package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type stubJobRunner struct {
	run func(ctx context.Context, req JobRequest, jobID string) (*JobResult, error)
}

func (s stubJobRunner) Run(
	ctx context.Context,
	req JobRequest,
	jobID string,
) (*JobResult, error) {
	return s.run(ctx, req, jobID)
}

func waitForJobStatus(t *testing.T, jm *JobManager, jobID string, want string) *Job {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		job, ok := jm.Get(jobID)
		if ok && job.Status == want {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}

	job, _ := jm.Get(jobID)
	if job == nil {
		t.Fatalf("job %s not found", jobID)
	}
	t.Fatalf("job %s status = %s, want %s", jobID, job.Status, want)
	return nil
}

func TestJobManagerStartAndPersistCompletedJob(t *testing.T) {
	dataDir := t.TempDir()
	jm, err := NewJobManager(dataDir)
	if err != nil {
		t.Fatalf("NewJobManager: %v", err)
	}

	jm.RegisterRunner("echo", stubJobRunner{
		run: func(ctx context.Context, req JobRequest, jobID string) (*JobResult, error) {
			_ = jobID
			if req.TaskID != "task-1" {
				t.Fatalf("TaskID = %q, want task-1", req.TaskID)
			}
			if req.WorkspaceRef.Branch != "echo/task-1" {
				t.Fatalf("branch = %q, want echo/task-1", req.WorkspaceRef.Branch)
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(20 * time.Millisecond):
			}
			return &JobResult{
				Output:    "full output",
				Summary:   "done",
				SessionID: "session-1",
			}, nil
		},
	})

	job, err := jm.Start(JobRequest{
		Project: "echo",
		TaskID:  "task-1",
		Prompt:  "implement feature",
		WorkspaceRef: JobWorkspaceRef{
			RepoPath:     "/repo",
			WorktreePath: "/repo/.echo/task-1",
			Branch:       "echo/task-1",
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if job.Status != JobStatusQueued {
		t.Fatalf("initial status = %s, want %s", job.Status, JobStatusQueued)
	}

	finished := waitForJobStatus(t, jm, job.ID, JobStatusCompleted)
	if finished.Output != "full output" {
		t.Fatalf("Output = %q, want full output", finished.Output)
	}
	if finished.Summary != "done" {
		t.Fatalf("Summary = %q, want done", finished.Summary)
	}
	if finished.SessionID != "session-1" {
		t.Fatalf("SessionID = %q, want session-1", finished.SessionID)
	}
	if finished.StartedAt == nil || finished.FinishedAt == nil {
		t.Fatalf("expected started_at and finished_at to be set")
	}

	reloaded, err := NewJobManager(dataDir)
	if err != nil {
		t.Fatalf("reload NewJobManager: %v", err)
	}
	saved, ok := reloaded.Get(job.ID)
	if !ok {
		t.Fatalf("reloaded job %s not found", job.ID)
	}
	if saved.Status != JobStatusCompleted {
		t.Fatalf("reloaded status = %s, want %s", saved.Status, JobStatusCompleted)
	}

	path := filepath.Join(dataDir, "jobs", job.ID+".json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("job file missing: %v", err)
	}
}

func TestJobManagerCancelRunningJob(t *testing.T) {
	dataDir := t.TempDir()
	jm, err := NewJobManager(dataDir)
	if err != nil {
		t.Fatalf("NewJobManager: %v", err)
	}

	started := make(chan struct{})
	jm.RegisterRunner("echo", stubJobRunner{
		run: func(ctx context.Context, req JobRequest, jobID string) (*JobResult, error) {
			_ = jobID
			if req.Project != "echo" {
				t.Fatalf("Project = %q, want echo", req.Project)
			}
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})

	job, err := jm.Start(JobRequest{Project: "echo", Prompt: "run forever"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-started

	if _, err := jm.Cancel(job.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	current, ok := jm.Get(job.ID)
	if !ok {
		t.Fatalf("job %s not found after cancel", job.ID)
	}
	if current.Status != JobStatusCancelled {
		t.Fatalf("status after cancel = %s, want %s", current.Status, JobStatusCancelled)
	}

	cancelled := waitForJobStatus(t, jm, job.ID, JobStatusCancelled)
	if cancelled.Error == "" {
		t.Fatalf("expected cancellation error message")
	}
}

func TestJobManagerMarksFailedJob(t *testing.T) {
	dataDir := t.TempDir()
	jm, err := NewJobManager(dataDir)
	if err != nil {
		t.Fatalf("NewJobManager: %v", err)
	}

	jm.RegisterRunner("echo", stubJobRunner{
		run: func(ctx context.Context, req JobRequest, jobID string) (*JobResult, error) {
			_ = jobID
			return nil, errors.New("runner failed")
		},
	})

	job, err := jm.Start(JobRequest{Project: "echo", Prompt: "fail"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	failed := waitForJobStatus(t, jm, job.ID, JobStatusFailed)
	if failed.Error != "runner failed" {
		t.Fatalf("Error = %q, want runner failed", failed.Error)
	}
}

func TestJobManagerMarksTimedOutJob(t *testing.T) {
	dataDir := t.TempDir()
	jm, err := NewJobManager(dataDir)
	if err != nil {
		t.Fatalf("NewJobManager: %v", err)
	}

	jm.RegisterRunner("echo", stubJobRunner{
		run: func(ctx context.Context, req JobRequest, jobID string) (*JobResult, error) {
			_ = req
			_ = jobID
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})

	job, err := jm.Start(
		JobRequest{
			Project: "echo",
			Prompt:  "timeout",
			Timeout: 20 * time.Millisecond,
		},
	)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	timedOut := waitForJobStatus(t, jm, job.ID, JobStatusTimedOut)
	if timedOut.Error != context.DeadlineExceeded.Error() {
		t.Fatalf("Error = %q, want %q", timedOut.Error, context.DeadlineExceeded.Error())
	}
}

func TestJobManagerRejectsUnknownProject(t *testing.T) {
	jm, err := NewJobManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewJobManager: %v", err)
	}

	if _, err := jm.Start(JobRequest{Project: "missing", Prompt: "hi"}); err == nil {
		t.Fatal("expected error for unknown project")
	}
}

func TestJobManagerReloadMarksNonTerminalJobsOrphaned(t *testing.T) {
	dataDir := t.TempDir()
	jm, err := NewJobManager(dataDir)
	if err != nil {
		t.Fatalf("NewJobManager: %v", err)
	}

	jm.RegisterRunner("echo", stubJobRunner{
		run: func(ctx context.Context, req JobRequest, jobID string) (*JobResult, error) {
			_ = jobID
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})

	job, err := jm.Start(JobRequest{Project: "echo", Prompt: "hang", Timeout: time.Hour})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForJobStatus(t, jm, job.ID, JobStatusRunning)

	reloaded, err := NewJobManager(dataDir)
	if err != nil {
		t.Fatalf("reload NewJobManager: %v", err)
	}
	recovered, ok := reloaded.Get(job.ID)
	if !ok {
		t.Fatalf("reloaded job %s not found", job.ID)
	}
	if recovered.Status != JobStatusOrphaned {
		t.Fatalf("reloaded status = %s, want %s", recovered.Status, JobStatusOrphaned)
	}
	if recovered.Error != "job interrupted by process restart" {
		t.Fatalf("reloaded error = %q", recovered.Error)
	}
	if recovered.FinishedAt == nil {
		t.Fatal("expected finished_at after reload recovery")
	}

	if _, err := reloaded.Cancel(job.ID); err != nil {
		t.Fatalf("Cancel on recovered job: %v", err)
	}
}

func TestJobManagerStartRetriesJobIDCollision(t *testing.T) {
	dataDir := t.TempDir()
	jm, err := NewJobManager(dataDir)
	if err != nil {
		t.Fatalf("NewJobManager: %v", err)
	}

	jm.RegisterRunner("echo", stubJobRunner{
		run: func(ctx context.Context, req JobRequest, jobID string) (*JobResult, error) {
			_ = jobID
			return &JobResult{Summary: "done"}, nil
		},
	})

	original := newJobIDFunc
	defer func() { newJobIDFunc = original }()

	callCount := 0
	newJobIDFunc = func() (string, error) {
		callCount++
		if callCount == 1 {
			return "job_collision", nil
		}
		return "job_unique", nil
	}

	jm.jobs["job_collision"] = &managedJob{
		job: &Job{ID: "job_collision", Status: JobStatusCompleted},
	}

	job, err := jm.Start(JobRequest{Project: "echo", Prompt: "run"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if job.ID != "job_unique" {
		t.Fatalf("job ID = %q, want job_unique", job.ID)
	}
}

func TestJobManagerListAgents(t *testing.T) {
	dataDir := t.TempDir()
	jm, err := NewJobManager(dataDir)
	if err != nil {
		t.Fatalf("NewJobManager: %v", err)
	}

	blocked := make(chan struct{})
	release := make(chan struct{})
	jm.RegisterProject("alpha", "codex", stubJobRunner{
		run: func(ctx context.Context, req JobRequest, jobID string) (*JobResult, error) {
			_ = req
			_ = jobID
			close(blocked)
			<-release
			return &JobResult{Summary: "done"}, nil
		},
	})
	jm.RegisterProject("beta", "claudecode", stubJobRunner{
		run: func(ctx context.Context, req JobRequest, jobID string) (*JobResult, error) {
			_ = ctx
			_ = req
			_ = jobID
			return &JobResult{Summary: "done"}, nil
		},
	})

	job, err := jm.Start(JobRequest{Project: "alpha", Prompt: "busy"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-blocked

	agents := jm.ListAgents()
	close(release)
	waitForJobStatus(t, jm, job.ID, JobStatusCompleted)

	if len(agents) != 2 {
		t.Fatalf("agent count = %d, want 2", len(agents))
	}
	if agents[0].Project != "alpha" || agents[0].Status != "busy" {
		t.Fatalf("unexpected alpha status: %+v", agents[0])
	}
	if agents[0].AgentType != "codex" || agents[0].ActiveJobs != 1 {
		t.Fatalf("unexpected alpha payload: %+v", agents[0])
	}
	if agents[1].Project != "beta" || agents[1].Status != "idle" {
		t.Fatalf("unexpected beta status: %+v", agents[1])
	}
}
