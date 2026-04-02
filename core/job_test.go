package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type stubJobRunner struct {
	run func(
		ctx context.Context,
		req JobRequest,
		jobID string,
		onEvent func(JobEvent),
	) (*JobResult, error)
}

func (s stubJobRunner) Run(
	ctx context.Context,
	req JobRequest,
	jobID string,
	onEvent func(JobEvent),
) (*JobResult, error) {
	return s.run(ctx, req, jobID, onEvent)
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

func waitForFinishedAt(t *testing.T, jm *JobManager, jobID string) *Job {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		job, ok := jm.Get(jobID)
		if ok && job.FinishedAt != nil {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}

	job, _ := jm.Get(jobID)
	if job == nil {
		t.Fatalf("job %s not found", jobID)
	}
	t.Fatalf("job %s did not record finished_at", jobID)
	return nil
}

func TestJobManagerStartAndPersistCompletedJob(t *testing.T) {
	dataDir := t.TempDir()
	jm, err := NewJobManager(dataDir)
	if err != nil {
		t.Fatalf("NewJobManager: %v", err)
	}

	jm.RegisterRunner("echo", stubJobRunner{
		run: func(ctx context.Context, req JobRequest, jobID string, onEvent func(JobEvent)) (*JobResult, error) {
			_ = onEvent
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

func TestJobManagerPersistsBufferedEvents(t *testing.T) {
	dataDir := t.TempDir()
	jm, err := NewJobManager(dataDir)
	if err != nil {
		t.Fatalf("NewJobManager: %v", err)
	}

	jm.RegisterRunner("echo", stubJobRunner{
		run: func(
			ctx context.Context,
			req JobRequest,
			jobID string,
			onEvent func(JobEvent),
		) (*JobResult, error) {
			_ = ctx
			_ = req
			_ = jobID
			onEvent(JobEvent{
				Type:      string(EventToolUse),
				ToolName:  "Bash",
				ToolInput: "git status",
				CreatedAt: time.Now().UTC(),
			})
			onEvent(JobEvent{
				Type:      string(EventText),
				Content:   "working",
				CreatedAt: time.Now().UTC(),
			})
			return &JobResult{
				Output:  "done",
				Summary: "done",
			}, nil
		},
	})

	job, err := jm.Start(JobRequest{Project: "echo", Prompt: "run"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	completed := waitForJobStatus(t, jm, job.ID, JobStatusCompleted)
	if completed.EventCount != 2 {
		t.Fatalf("EventCount = %d, want 2", completed.EventCount)
	}
	if len(completed.Events) != 2 {
		t.Fatalf("len(Events) = %d, want 2", len(completed.Events))
	}
	if completed.Events[0].Index != 0 || completed.Events[1].Index != 1 {
		t.Fatalf("event indexes = %#v, want 0/1", completed.Events)
	}

	reloaded, err := NewJobManager(dataDir)
	if err != nil {
		t.Fatalf("reload NewJobManager: %v", err)
	}
	saved, ok := reloaded.Get(job.ID)
	if !ok {
		t.Fatalf("reloaded job %s not found", job.ID)
	}
	if saved.EventCount != 2 || len(saved.Events) != 2 {
		t.Fatalf("reloaded events = count:%d len:%d, want 2/2", saved.EventCount, len(saved.Events))
	}
}

func TestJobManagerCapturesWorkspaceSnapshotForProducerTasks(t *testing.T) {
	dataDir := t.TempDir()
	jm, err := NewJobManager(dataDir)
	if err != nil {
		t.Fatalf("NewJobManager: %v", err)
	}

	jm.RegisterRunner("echo", stubJobRunner{
		run: func(
			ctx context.Context,
			req JobRequest,
			jobID string,
			onEvent func(JobEvent),
		) (*JobResult, error) {
			_ = ctx
			_ = req
			_ = jobID
			_ = onEvent
			return &JobResult{Output: "done", Summary: "done"}, nil
		},
	})

	job, err := jm.Start(JobRequest{
		Project:  "echo",
		TaskType: "research",
		Prompt:   "run",
		WorkspaceRef: JobWorkspaceRef{
			RepoPath:     "/tmp/repo",
			WorktreePath: filepath.Join(dataDir, "missing-worktree"),
			Branch:       "echo/research/test",
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	completed := waitForJobStatus(t, jm, job.ID, JobStatusCompleted)
	if completed.EventCount != 1 {
		t.Fatalf("EventCount = %d, want 1", completed.EventCount)
	}
	if len(completed.Events) != 1 {
		t.Fatalf("len(Events) = %d, want 1", len(completed.Events))
	}
	if completed.Events[0].Type != "workspace_snapshot" {
		t.Fatalf("event type = %q, want workspace_snapshot", completed.Events[0].Type)
	}
	if completed.Events[0].Metadata["snapshot_error"] == "" {
		t.Fatalf("metadata = %#v, want snapshot_error", completed.Events[0].Metadata)
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
		run: func(ctx context.Context, req JobRequest, jobID string, onEvent func(JobEvent)) (*JobResult, error) {
			_ = onEvent
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
	if current.FinishedAt != nil {
		t.Fatalf("running job should not set finished_at until runner exits")
	}

	cancelled := waitForFinishedAt(t, jm, job.ID)
	if cancelled.Error == "" {
		t.Fatalf("expected cancellation error message")
	}
	if cancelled.Status != JobStatusCancelled {
		t.Fatalf("final status = %s, want %s", cancelled.Status, JobStatusCancelled)
	}
	if cancelled.FinishedAt == nil {
		t.Fatalf("expected finished_at after runner exits")
	}
}

func TestJobManagerMarksFailedJob(t *testing.T) {
	dataDir := t.TempDir()
	jm, err := NewJobManager(dataDir)
	if err != nil {
		t.Fatalf("NewJobManager: %v", err)
	}

	jm.RegisterRunner("echo", stubJobRunner{
		run: func(ctx context.Context, req JobRequest, jobID string, onEvent func(JobEvent)) (*JobResult, error) {
			_ = onEvent
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
		run: func(ctx context.Context, req JobRequest, jobID string, onEvent func(JobEvent)) (*JobResult, error) {
			_ = onEvent
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
		run: func(ctx context.Context, req JobRequest, jobID string, onEvent func(JobEvent)) (*JobResult, error) {
			_ = onEvent
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
		run: func(ctx context.Context, req JobRequest, jobID string, onEvent func(JobEvent)) (*JobResult, error) {
			_ = onEvent
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
		run: func(ctx context.Context, req JobRequest, jobID string, onEvent func(JobEvent)) (*JobResult, error) {
			_ = onEvent
			_ = req
			_ = jobID
			close(blocked)
			<-release
			return &JobResult{Summary: "done"}, nil
		},
	})
	jm.RegisterProject("beta", "claudecode", stubJobRunner{
		run: func(ctx context.Context, req JobRequest, jobID string, onEvent func(JobEvent)) (*JobResult, error) {
			_ = onEvent
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

func TestJobManagerQueuesJobsPerProject(t *testing.T) {
	dataDir := t.TempDir()
	jm, err := NewJobManager(dataDir)
	if err != nil {
		t.Fatalf("NewJobManager: %v", err)
	}

	firstStarted := make(chan struct{})
	firstRelease := make(chan struct{})
	secondStarted := make(chan struct{})
	var startsMu sync.Mutex
	startOrder := make([]string, 0, 2)
	jm.RegisterProject("alpha", "codex", stubJobRunner{
		run: func(ctx context.Context, req JobRequest, jobID string, onEvent func(JobEvent)) (*JobResult, error) {
			_ = onEvent
			startsMu.Lock()
			startOrder = append(startOrder, req.Prompt)
			startsMu.Unlock()
			switch req.Prompt {
			case "first":
				close(firstStarted)
				<-firstRelease
			case "second":
				close(secondStarted)
			}
			return &JobResult{Summary: req.Prompt, SessionID: jobID}, nil
		},
	})

	firstJob, err := jm.Start(JobRequest{Project: "alpha", Prompt: "first"})
	if err != nil {
		t.Fatalf("Start first: %v", err)
	}
	<-firstStarted

	secondJob, err := jm.Start(JobRequest{Project: "alpha", Prompt: "second"})
	if err != nil {
		t.Fatalf("Start second: %v", err)
	}
	queued := waitForJobStatus(t, jm, secondJob.ID, JobStatusQueued)
	if queued.StartedAt != nil {
		t.Fatalf("queued job should not have started_at")
	}

	agents := jm.ListAgents()
	if len(agents) != 1 {
		t.Fatalf("agent count = %d, want 1", len(agents))
	}
	if agents[0].RunningJobs != 1 || agents[0].QueuedJobs != 1 {
		t.Fatalf("unexpected queue payload: %+v", agents[0])
	}

	close(firstRelease)
	waitForJobStatus(t, jm, firstJob.ID, JobStatusCompleted)
	<-secondStarted
	waitForJobStatus(t, jm, secondJob.ID, JobStatusCompleted)

	startsMu.Lock()
	defer startsMu.Unlock()
	if len(startOrder) != 2 || startOrder[0] != "first" || startOrder[1] != "second" {
		t.Fatalf("unexpected start order: %+v", startOrder)
	}
}

func TestJobManagerCancelQueuedJob(t *testing.T) {
	dataDir := t.TempDir()
	jm, err := NewJobManager(dataDir)
	if err != nil {
		t.Fatalf("NewJobManager: %v", err)
	}

	firstStarted := make(chan struct{})
	firstRelease := make(chan struct{})
	secondStarted := make(chan struct{})
	jm.RegisterProject("alpha", "codex", stubJobRunner{
		run: func(ctx context.Context, req JobRequest, jobID string, onEvent func(JobEvent)) (*JobResult, error) {
			_ = onEvent
			switch req.Prompt {
			case "first":
				close(firstStarted)
				<-firstRelease
			case "second":
				close(secondStarted)
			}
			return &JobResult{Summary: req.Prompt}, nil
		},
	})

	if _, err := jm.Start(JobRequest{Project: "alpha", Prompt: "first"}); err != nil {
		t.Fatalf("Start first: %v", err)
	}
	<-firstStarted

	secondJob, err := jm.Start(JobRequest{Project: "alpha", Prompt: "second"})
	if err != nil {
		t.Fatalf("Start second: %v", err)
	}
	waitForJobStatus(t, jm, secondJob.ID, JobStatusQueued)

	cancelled, err := jm.Cancel(secondJob.ID)
	if err != nil {
		t.Fatalf("Cancel queued: %v", err)
	}
	if cancelled.Status != JobStatusCancelled {
		t.Fatalf("cancelled status = %s, want cancelled", cancelled.Status)
	}
	if cancelled.FinishedAt == nil {
		t.Fatalf("queued cancel should set finished_at immediately")
	}

	close(firstRelease)
	select {
	case <-secondStarted:
		t.Fatal("queued cancelled job should never start")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestJobManagerReloadKeepsQueuedJobsQueued(t *testing.T) {
	dataDir := t.TempDir()
	jm, err := NewJobManager(dataDir)
	if err != nil {
		t.Fatalf("NewJobManager: %v", err)
	}

	job := &Job{
		ID:        "job_queued_reload",
		Project:   "alpha",
		Prompt:    "queued after restart",
		Status:    JobStatusQueued,
		CreatedAt: time.Now().UTC(),
	}
	if err := jm.saveJob(job); err != nil {
		t.Fatalf("saveJob: %v", err)
	}

	reloaded, err := NewJobManager(dataDir)
	if err != nil {
		t.Fatalf("reload NewJobManager: %v", err)
	}
	loaded, ok := reloaded.Get(job.ID)
	if !ok {
		t.Fatalf("queued job %s not found", job.ID)
	}
	if loaded.Status != JobStatusQueued {
		t.Fatalf("loaded status = %s, want queued", loaded.Status)
	}

	started := make(chan struct{})
	reloaded.RegisterProject("alpha", "codex", stubJobRunner{
		run: func(ctx context.Context, req JobRequest, jobID string, onEvent func(JobEvent)) (*JobResult, error) {
			_ = onEvent
			close(started)
			return &JobResult{Summary: "done"}, nil
		},
	})

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("queued job was not dispatched after project registration")
	}
	waitForJobStatus(t, reloaded, job.ID, JobStatusCompleted)
}
