package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	JobStatusQueued    = "queued"
	JobStatusRunning   = "running"
	JobStatusCompleted = "completed"
	JobStatusFailed    = "failed"
	JobStatusCancelled = "cancelled"
	JobStatusTimedOut  = "timed_out"
	JobStatusOrphaned  = "orphaned"
)

const JobErrorCodePermissionRequired = "permission_required"

var newJobIDFunc = newJobID

type JobWorkspaceRef struct {
	RepoPath     string `json:"repo_path,omitempty"`
	WorktreePath string `json:"worktree_path,omitempty"`
	Branch       string `json:"branch,omitempty"`
}

type JobRequest struct {
	Project      string          `json:"project"`
	TaskID       string          `json:"task_id,omitempty"`
	TaskType     string          `json:"task_type,omitempty"`
	Prompt       string          `json:"prompt"`
	Timeout      time.Duration   `json:"timeout,omitempty"`
	WorkspaceRef JobWorkspaceRef `json:"workspace_ref,omitempty"`
}

type JobResult struct {
	Output    string `json:"output,omitempty"`
	Summary   string `json:"summary,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

type JobEvent struct {
	Index     int            `json:"index"`
	Type      string         `json:"type"`
	Content   string         `json:"content,omitempty"`
	ToolName  string         `json:"tool_name,omitempty"`
	ToolInput string         `json:"tool_input,omitempty"`
	SessionID string         `json:"session_id,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
}

type Job struct {
	ID           string          `json:"id"`
	Project      string          `json:"project"`
	TaskID       string          `json:"task_id,omitempty"`
	TaskType     string          `json:"task_type,omitempty"`
	Prompt       string          `json:"prompt"`
	WorkspaceRef JobWorkspaceRef `json:"workspace_ref,omitempty"`
	Status       string          `json:"status"`
	Output       string          `json:"output,omitempty"`
	Summary      string          `json:"summary,omitempty"`
	SessionID    string          `json:"session_id,omitempty"`
	Error        string          `json:"error,omitempty"`
	ErrorCode    string          `json:"error_code,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	StartedAt    *time.Time      `json:"started_at,omitempty"`
	FinishedAt   *time.Time      `json:"finished_at,omitempty"`
	TimeoutSec   int             `json:"timeout_sec,omitempty"`
	EventCount   int             `json:"event_count,omitempty"`
	Events       []JobEvent      `json:"events,omitempty"`
}

func (j *Job) clone() *Job {
	if j == nil {
		return nil
	}
	dup := *j
	if j.StartedAt != nil {
		started := *j.StartedAt
		dup.StartedAt = &started
	}
	if j.FinishedAt != nil {
		finished := *j.FinishedAt
		dup.FinishedAt = &finished
	}
	if len(j.Events) > 0 {
		dup.Events = append([]JobEvent(nil), j.Events...)
	}
	return &dup
}

type JobRunner interface {
	Run(
		ctx context.Context,
		req JobRequest,
		jobID string,
		onEvent func(JobEvent),
	) (*JobResult, error)
}

type managedJob struct {
	job    *Job
	req    JobRequest
	runner JobRunner
	ctx    context.Context
	cancel context.CancelFunc
}

type RegisteredProject struct {
	Project     string `json:"project"`
	AgentType   string `json:"agent_type"`
	Status      string `json:"status"`
	ActiveJobs  int    `json:"active_jobs"`
	RunningJobs int    `json:"running_jobs"`
	QueuedJobs  int    `json:"queued_jobs"`
}

type JobManager struct {
	baseDir string

	mu      sync.RWMutex
	jobs    map[string]*managedJob
	runners map[string]JobRunner
	agents  map[string]string
	running map[string]string
	queues  map[string][]string
}

const maxBufferedJobEvents = 200

func NewJobManager(dataDir string) (*JobManager, error) {
	jobsDir := filepath.Join(dataDir, "jobs")
	if err := os.MkdirAll(jobsDir, 0o755); err != nil {
		return nil, fmt.Errorf("create jobs dir: %w", err)
	}

	jm := &JobManager{
		baseDir: jobsDir,
		jobs:    make(map[string]*managedJob),
		runners: make(map[string]JobRunner),
		agents:  make(map[string]string),
		running: make(map[string]string),
		queues:  make(map[string][]string),
	}
	if err := jm.load(); err != nil {
		return nil, err
	}
	return jm, nil
}

func (jm *JobManager) RegisterRunner(project string, runner JobRunner) {
	jm.RegisterProject(project, "", runner)
}

func (jm *JobManager) RegisterProject(
	project string,
	agentType string,
	runner JobRunner,
) {
	jm.mu.Lock()
	jm.runners[project] = runner
	jm.agents[project] = agentType
	for _, managed := range jm.jobs {
		if managed.job.Project == project {
			managed.runner = runner
		}
	}
	dispatch := jm.prepareNextJobLocked(project)
	jm.mu.Unlock()
	if dispatch != nil {
		go jm.runJob(dispatch)
	}
}

func (jm *JobManager) Start(req JobRequest) (*Job, error) {
	if req.Project == "" {
		return nil, fmt.Errorf("project is required")
	}
	if req.Prompt == "" {
		return nil, fmt.Errorf("prompt is required")
	}

	jm.mu.Lock()
	runner, ok := jm.runners[req.Project]
	if !ok {
		jm.mu.Unlock()
		return nil, fmt.Errorf("project %q is not registered", req.Project)
	}

	jobID, err := jm.createUniqueJobIDLocked()
	if err != nil {
		jm.mu.Unlock()
		return nil, err
	}

	now := time.Now().UTC()
	job := &Job{
		ID:           jobID,
		Project:      req.Project,
		TaskID:       req.TaskID,
		TaskType:     req.TaskType,
		Prompt:       req.Prompt,
		WorkspaceRef: req.WorkspaceRef,
		Status:       JobStatusQueued,
		CreatedAt:    now,
		TimeoutSec:   int(req.Timeout / time.Second),
	}
	if err := jm.saveJob(job); err != nil {
		jm.mu.Unlock()
		return nil, err
	}

	jm.jobs[job.ID] = &managedJob{
		job:    job,
		req:    req,
		runner: runner,
		cancel: func() {},
	}
	jm.queues[req.Project] = append(jm.queues[req.Project], job.ID)
	dispatch := jm.prepareNextJobLocked(req.Project)
	jm.mu.Unlock()

	if dispatch != nil {
		go jm.runJob(dispatch)
	}
	return job.clone(), nil
}

func (jm *JobManager) Get(jobID string) (*Job, bool) {
	jm.mu.RLock()
	defer jm.mu.RUnlock()
	managed, ok := jm.jobs[jobID]
	if !ok {
		return nil, false
	}
	return managed.job.clone(), true
}

func (jm *JobManager) List() []*Job {
	jm.mu.RLock()
	defer jm.mu.RUnlock()

	jobs := make([]*Job, 0, len(jm.jobs))
	for _, managed := range jm.jobs {
		jobs = append(jobs, managed.job.clone())
	}
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].CreatedAt.Equal(jobs[j].CreatedAt) {
			return jobs[i].ID < jobs[j].ID
		}
		return jobs[i].CreatedAt.Before(jobs[j].CreatedAt)
	})
	return jobs
}

func (jm *JobManager) ListAgents() []RegisteredProject {
	jm.mu.RLock()
	defer jm.mu.RUnlock()

	projects := make([]RegisteredProject, 0, len(jm.runners))
	for project := range jm.runners {
		registered := RegisteredProject{
			Project:   project,
			AgentType: jm.agents[project],
			Status:    "idle",
		}
		for _, managed := range jm.jobs {
			if managed.job.Project != project {
				continue
			}
			switch managed.job.Status {
			case JobStatusQueued:
				registered.ActiveJobs++
				registered.QueuedJobs++
			case JobStatusRunning:
				registered.ActiveJobs++
				registered.RunningJobs++
			}
		}
		if registered.ActiveJobs > 0 {
			registered.Status = "busy"
		}
		if registered.AgentType == "" {
			registered.AgentType = "unknown"
		}
		projects = append(projects, registered)
	}

	sort.Slice(projects, func(i, j int) bool {
		return projects[i].Project < projects[j].Project
	})
	return projects
}

func (jm *JobManager) Cancel(jobID string) (*Job, error) {
	jm.mu.Lock()
	managed, ok := jm.jobs[jobID]
	if !ok {
		jm.mu.Unlock()
		return nil, fmt.Errorf("job %q not found", jobID)
	}
	if isTerminalJobStatus(managed.job.Status) {
		job := managed.job.clone()
		jm.mu.Unlock()
		return job, nil
	}

	wasQueued := managed.job.Status == JobStatusQueued
	wasStarted := managed.job.StartedAt != nil
	now := time.Now().UTC()
	managed.job.Status = JobStatusCancelled
	managed.job.Error = context.Canceled.Error()
	if wasQueued || !wasStarted {
		managed.job.FinishedAt = &now
	}
	jm.removeQueuedJobLocked(managed.job.Project, jobID)
	if err := jm.saveJob(managed.job); err != nil {
		managed.job.Status = JobStatusFailed
		managed.job.Error = fmt.Sprintf("persist cancelled job: %v", err)
	}
	cancel := managed.cancel
	job := managed.job.clone()
	jm.mu.Unlock()
	cancel()
	return job, nil
}

func (jm *JobManager) runJob(managed *managedJob) {
	if managed == nil || managed.job == nil {
		return
	}
	jobID := managed.job.ID
	now := time.Now().UTC()

	jm.mu.Lock()
	current := jm.jobs[jobID]
	if current == nil || current != managed {
		jm.mu.Unlock()
		return
	}
	if isTerminalJobStatus(managed.job.Status) {
		if jm.running[managed.job.Project] == jobID {
			delete(jm.running, managed.job.Project)
		}
		jm.mu.Unlock()
		return
	}
	managed.job.Status = JobStatusRunning
	managed.job.StartedAt = &now
	job := managed.job.clone()
	ctx := managed.ctx
	runner := managed.runner
	req := managed.req
	jm.mu.Unlock()

	if err := jm.saveJob(job); err != nil {
		jm.finishJob(jobID, func(job *Job) {
			job.Status = JobStatusFailed
			job.Error = fmt.Sprintf("persist running job: %v", err)
		})
		return
	}

	result, err := runner.Run(ctx, req, jobID, func(event JobEvent) {
		jm.recordJobEvent(jobID, event)
	})
	jobOutcome := "completed"
	if err != nil {
		jobOutcome = "failed"
	} else if ctx.Err() != nil {
		jobOutcome = "cancelled"
	}
	if shouldCaptureWorkspaceSnapshot(req) {
		if snapshot, snapshotErr := CaptureWorkspaceSnapshot(
			req.WorkspaceRef.RepoPath,
			req.WorkspaceRef.WorktreePath,
			req.WorkspaceRef.Branch,
		); snapshotErr != nil {
			jm.recordJobEvent(jobID, JobEvent{
				Type:      "workspace_snapshot",
				Content:   "workspace snapshot failed",
				CreatedAt: time.Now().UTC(),
				Metadata: map[string]any{
					"error":          snapshotErr.Error(),
					"job_outcome":    jobOutcome,
					"snapshot_stage": "post_run",
				},
			})
		} else if snapshot != nil {
			metadata := snapshot.Metadata()
			metadata["job_outcome"] = jobOutcome
			metadata["snapshot_stage"] = "post_run"
			jm.recordJobEvent(jobID, JobEvent{
				Type:      "workspace_snapshot",
				Content:   snapshot.Summary(),
				CreatedAt: time.Now().UTC(),
				Metadata:  metadata,
			})
		}
	}
	jm.finishJob(jobID, func(job *Job) {
		now := time.Now().UTC()
		job.FinishedAt = &now
		if ctx.Err() != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				job.Status = JobStatusTimedOut
			} else {
				job.Status = JobStatusCancelled
			}
			job.Error = ctx.Err().Error()
			return
		}
		if err != nil {
			job.Status = JobStatusFailed
			job.Error = err.Error()
			if coded, ok := err.(interface{ Code() string }); ok {
				job.ErrorCode = coded.Code()
			}
			return
		}
		job.Status = JobStatusCompleted
		if result != nil {
			job.Output = result.Output
			job.Summary = result.Summary
			job.SessionID = result.SessionID
		}
	})
}

func shouldCaptureWorkspaceSnapshot(req JobRequest) bool {
	switch strings.TrimSpace(req.TaskType) {
	case "research", "design", "implementation":
		return strings.TrimSpace(req.WorkspaceRef.WorktreePath) != ""
	default:
		return false
	}
}

func (jm *JobManager) recordJobEvent(jobID string, event JobEvent) {
	jm.mu.Lock()
	managed := jm.jobs[jobID]
	if managed == nil {
		jm.mu.Unlock()
		return
	}
	event.Index = managed.job.EventCount
	managed.job.EventCount++
	managed.job.Events = append(managed.job.Events, event)
	if extra := len(managed.job.Events) - maxBufferedJobEvents; extra > 0 {
		managed.job.Events = append([]JobEvent(nil), managed.job.Events[extra:]...)
	}
	job := managed.job.clone()
	jm.mu.Unlock()

	if err := jm.saveJob(job); err != nil {
		slog.Warn("persist job event failed", "job_id", jobID, "error", err)
	}
}

func (jm *JobManager) finishJob(jobID string, mutate func(job *Job)) {
	jm.mu.Lock()
	managed := jm.jobs[jobID]
	if managed == nil {
		jm.mu.Unlock()
		return
	}
	mutate(managed.job)
	// Persist the terminal state before unlocking so a concurrent restart does not
	// observe an in-memory completion that was never durably written to disk.
	if err := jm.saveJob(managed.job); err != nil {
		managed.job.Status = JobStatusFailed
		managed.job.Error = fmt.Sprintf("persist finished job: %v", err)
	}
	project := managed.job.Project
	cancel := managed.cancel
	var dispatch *managedJob
	if jm.running[project] == jobID {
		delete(jm.running, project)
		dispatch = jm.prepareNextJobLocked(project)
	}
	jm.mu.Unlock()
	cancel()
	if dispatch != nil {
		go jm.runJob(dispatch)
	}
}

func (jm *JobManager) createUniqueJobIDLocked() (string, error) {
	for range 16 {
		jobID, err := newJobIDFunc()
		if err != nil {
			return "", err
		}
		if _, exists := jm.jobs[jobID]; exists {
			continue
		}
		path := filepath.Join(jm.baseDir, jobID+".json")
		if _, err := os.Stat(path); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return "", fmt.Errorf("stat job %s: %w", jobID, err)
		}
		return jobID, nil
	}
	return "", fmt.Errorf("generate unique job id: too many collisions")
}

func (jm *JobManager) load() error {
	entries, err := os.ReadDir(jm.baseDir)
	if err != nil {
		return fmt.Errorf("read jobs dir: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(jm.baseDir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read job %s: %w", path, err)
		}
		var job Job
		if err := json.Unmarshal(data, &job); err != nil {
			return fmt.Errorf("decode job %s: %w", path, err)
		}
		if !isTerminalJobStatus(job.Status) {
			if job.Status == JobStatusQueued && job.StartedAt == nil {
				// Jobs that were accepted but never started can stay queued and
				// be dispatched again after projects register on restart.
			} else {
				now := time.Now().UTC()
				job.Status = JobStatusOrphaned
				job.Error = "job interrupted by process restart"
				if job.FinishedAt == nil {
					job.FinishedAt = &now
				}
				if err := jm.saveJob(&job); err != nil {
					return err
				}
			}
		}
		managed := &managedJob{
			job: &job,
			req: JobRequest{
				Project:      job.Project,
				TaskID:       job.TaskID,
				Prompt:       job.Prompt,
				Timeout:      time.Duration(job.TimeoutSec) * time.Second,
				WorkspaceRef: job.WorkspaceRef,
			},
			cancel: func() {},
		}
		if runner, ok := jm.runners[job.Project]; ok {
			managed.runner = runner
		}
		jm.jobs[job.ID] = managed
		if job.Status == JobStatusQueued {
			jm.queues[job.Project] = append(jm.queues[job.Project], job.ID)
		}
	}
	return nil
}

func (jm *JobManager) saveJob(job *Job) error {
	data, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal job: %w", err)
	}
	path := filepath.Join(jm.baseDir, job.ID+".json")
	if err := AtomicWriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write job %s: %w", job.ID, err)
	}
	return nil
}

func isTerminalJobStatus(status string) bool {
	switch status {
	case JobStatusCompleted, JobStatusFailed, JobStatusCancelled, JobStatusTimedOut, JobStatusOrphaned:
		return true
	default:
		return false
	}
}

func newJobID() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate job id: %w", err)
	}
	return "job_" + hex.EncodeToString(raw[:]), nil
}

func (jm *JobManager) prepareNextJobLocked(project string) *managedJob {
	if project == "" || jm.running[project] != "" {
		return nil
	}
	queue := jm.queues[project]
	for len(queue) > 0 {
		jobID := queue[0]
		queue = queue[1:]
		managed := jm.jobs[jobID]
		if managed == nil || managed.job == nil || managed.job.Status != JobStatusQueued || managed.runner == nil {
			continue
		}
		ctx, cancel := newManagedJobContext(managed.req.Timeout)
		managed.ctx = ctx
		managed.cancel = cancel
		jm.running[project] = jobID
		jm.queues[project] = queue
		return managed
	}
	jm.queues[project] = queue
	return nil
}

func (jm *JobManager) removeQueuedJobLocked(project string, jobID string) {
	queue := jm.queues[project]
	if len(queue) == 0 {
		return
	}
	filtered := queue[:0]
	for _, queuedID := range queue {
		if queuedID != jobID {
			filtered = append(filtered, queuedID)
		}
	}
	if len(filtered) == 0 {
		delete(jm.queues, project)
		return
	}
	jm.queues[project] = filtered
}

func newManagedJobContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx := context.Background()
	if timeout > 0 {
		return context.WithTimeout(ctx, timeout)
	}
	return context.WithCancel(ctx)
}
