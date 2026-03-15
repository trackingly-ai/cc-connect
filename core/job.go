package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	JobStatusQueued    = "queued"
	JobStatusRunning   = "running"
	JobStatusCompleted = "completed"
	JobStatusFailed    = "failed"
	JobStatusCancelled = "cancelled"
	JobStatusOrphaned  = "orphaned"
)

var newJobIDFunc = newJobID

type JobWorkspaceRef struct {
	RepoPath     string `json:"repo_path,omitempty"`
	WorktreePath string `json:"worktree_path,omitempty"`
	Branch       string `json:"branch,omitempty"`
}

type JobRequest struct {
	Project      string          `json:"project"`
	TaskID       string          `json:"task_id,omitempty"`
	Prompt       string          `json:"prompt"`
	Timeout      time.Duration   `json:"timeout,omitempty"`
	WorkspaceRef JobWorkspaceRef `json:"workspace_ref,omitempty"`
}

type JobResult struct {
	Output    string `json:"output,omitempty"`
	Summary   string `json:"summary,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

type Job struct {
	ID           string          `json:"id"`
	Project      string          `json:"project"`
	TaskID       string          `json:"task_id,omitempty"`
	Prompt       string          `json:"prompt"`
	WorkspaceRef JobWorkspaceRef `json:"workspace_ref,omitempty"`
	Status       string          `json:"status"`
	Output       string          `json:"output,omitempty"`
	Summary      string          `json:"summary,omitempty"`
	SessionID    string          `json:"session_id,omitempty"`
	Error        string          `json:"error,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	StartedAt    *time.Time      `json:"started_at,omitempty"`
	FinishedAt   *time.Time      `json:"finished_at,omitempty"`
	TimeoutSec   int             `json:"timeout_sec,omitempty"`
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
	return &dup
}

type JobRunner interface {
	Run(ctx context.Context, req JobRequest, jobID string) (*JobResult, error)
}

type managedJob struct {
	job    *Job
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
}

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
	defer jm.mu.Unlock()
	jm.runners[project] = runner
	jm.agents[project] = agentType
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

	ctx := context.Background()
	cancel := func() {}
	if req.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}

	jm.jobs[job.ID] = &managedJob{job: job, cancel: cancel}
	jm.mu.Unlock()

	go jm.runJob(ctx, runner, req, job.ID)
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
	jm.mu.RLock()
	managed, ok := jm.jobs[jobID]
	jm.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("job %q not found", jobID)
	}

	jm.mu.Lock()
	defer jm.mu.Unlock()
	if isTerminalJobStatus(managed.job.Status) {
		return managed.job.clone(), nil
	}
	now := time.Now().UTC()
	managed.job.Status = JobStatusCancelled
	managed.job.Error = context.Canceled.Error()
	managed.job.FinishedAt = &now
	if err := jm.saveJob(managed.job); err != nil {
		managed.job.Status = JobStatusFailed
		managed.job.Error = fmt.Sprintf("persist cancelled job: %v", err)
	}
	managed.cancel()
	return managed.job.clone(), nil
}

func (jm *JobManager) runJob(
	ctx context.Context,
	runner JobRunner,
	req JobRequest,
	jobID string,
) {
	now := time.Now().UTC()

	jm.mu.Lock()
	managed := jm.jobs[jobID]
	if managed == nil {
		jm.mu.Unlock()
		return
	}
	managed.job.Status = JobStatusRunning
	managed.job.StartedAt = &now
	job := managed.job.clone()
	jm.mu.Unlock()

	if err := jm.saveJob(job); err != nil {
		jm.finishJob(jobID, func(job *Job) {
			job.Status = JobStatusFailed
			job.Error = fmt.Sprintf("persist running job: %v", err)
		})
		return
	}

	result, err := runner.Run(ctx, req, jobID)
	jm.finishJob(jobID, func(job *Job) {
		now := time.Now().UTC()
		job.FinishedAt = &now
		if ctx.Err() != nil {
			job.Status = JobStatusCancelled
			job.Error = ctx.Err().Error()
			return
		}
		if err != nil {
			job.Status = JobStatusFailed
			job.Error = err.Error()
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

func (jm *JobManager) finishJob(jobID string, mutate func(job *Job)) {
	jm.mu.Lock()
	managed := jm.jobs[jobID]
	if managed == nil {
		jm.mu.Unlock()
		return
	}
	defer managed.cancel()
	mutate(managed.job)
	// Persist the terminal state before unlocking so a concurrent restart does not
	// observe an in-memory completion that was never durably written to disk.
	if err := jm.saveJob(managed.job); err != nil {
		managed.job.Status = JobStatusFailed
		managed.job.Error = fmt.Sprintf("persist finished job: %v", err)
	}
	jm.mu.Unlock()
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
		jm.jobs[job.ID] = &managedJob{
			job:    &job,
			cancel: func() {},
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
	case JobStatusCompleted, JobStatusFailed, JobStatusCancelled, JobStatusOrphaned:
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
