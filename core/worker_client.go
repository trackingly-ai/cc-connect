package core

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/config"
	"github.com/gorilla/websocket"
)

const (
	defaultEchoOrgID          = "coding-team"
	defaultWorkerPollInterval = 500 * time.Millisecond
)

type WorkerClient struct {
	serverURL         string
	authToken         string
	hostID            string
	label             string
	tags              []string
	heartbeatInterval time.Duration
	agents            []workerAgentRegistration
	jobMgr            *JobManager

	mu       sync.Mutex
	conn     *websocket.Conn
	sendMu   sync.Mutex
	doneCh   chan struct{}
	pending  map[string]chan map[string]any
	reqSeq   int
	watchers map[string]context.CancelFunc
}

type workerAgentRegistration struct {
	ID             string `json:"id"`
	OrgID          string `json:"org_id"`
	Role           string `json:"role"`
	CCProject      string `json:"cc_project"`
	AgentType      string `json:"agent_type"`
	PromptTemplate string `json:"prompt_template"`
}

func NewWorkerClient(
	echoCfg config.EchoConfig,
	projects []config.ProjectConfig,
	jobMgr *JobManager,
) (*WorkerClient, error) {
	serverURL := strings.TrimSpace(echoCfg.ServerURL)
	if serverURL == "" {
		return nil, nil
	}
	if jobMgr == nil {
		return nil, fmt.Errorf("echo worker client requires job manager")
	}
	wsURL, err := workerGatewayWebSocketURL(serverURL)
	if err != nil {
		return nil, err
	}
	agents, err := buildWorkerAgentRegistrations(echoCfg, projects)
	if err != nil {
		return nil, err
	}
	if len(agents) == 0 {
		return nil, fmt.Errorf("echo worker client requires at least one enabled echo project")
	}

	hostID := strings.TrimSpace(echoCfg.HostID)
	if hostID == "" {
		hostname, err := os.Hostname()
		if err != nil || strings.TrimSpace(hostname) == "" {
			hostID = "host-local"
		} else {
			hostID = hostname
		}
	}
	label := strings.TrimSpace(echoCfg.Label)
	if label == "" {
		label = hostID
	}
	interval := time.Duration(echoCfg.HeartbeatIntervalSec) * time.Second
	if interval <= 0 {
		interval = 30 * time.Second
	}

	return &WorkerClient{
		serverURL:         wsURL,
		authToken:         strings.TrimSpace(echoCfg.AuthToken),
		hostID:            hostID,
		label:             label,
		tags:              append([]string(nil), echoCfg.Tags...),
		heartbeatInterval: interval,
		agents:            agents,
		jobMgr:            jobMgr,
		doneCh:            make(chan struct{}),
		pending:           make(map[string]chan map[string]any),
		watchers:          make(map[string]context.CancelFunc),
	}, nil
}

func (c *WorkerClient) Start(ctx context.Context) {
	go c.run(ctx)
}

func (c *WorkerClient) Stop(ctx context.Context) error {
	c.closeConn()
	c.stopAllWatchers()
	select {
	case <-c.doneCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *WorkerClient) run(ctx context.Context) {
	defer close(c.doneCh)

	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		if err := c.runSession(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("echo worker client session ended", "error", err)
		}
		if ctx.Err() != nil {
			return
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
}

func (c *WorkerClient) runSession(ctx context.Context) error {
	header := http.Header{}
	if c.authToken != "" {
		header.Set("Authorization", "Bearer "+c.authToken)
	}
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, c.serverURL, header)
	if err != nil {
		return fmt.Errorf("dial echo worker gateway: %w", err)
	}
	c.setConn(conn)
	defer c.closeConn()

	readerErrCh := make(chan error, 1)
	go func() {
		readerErrCh <- c.readLoop(ctx, conn)
	}()

	if _, err := c.request(ctx, map[string]any{
		"type":           "hello",
		"worker_version": "cc-connect",
	}, "hello_ack"); err != nil {
		return err
	}
	if _, err := c.request(ctx, map[string]any{
		"type": "register_host",
		"host": map[string]any{
			"id": c.hostID,
			"metadata": map[string]any{
				"label":    c.label,
				"platform": runtime.GOOS,
				"tags":     c.tags,
			},
		},
	}, "host_registered"); err != nil {
		return err
	}
	if _, err := c.request(ctx, map[string]any{
		"type":    "register_agents",
		"host_id": c.hostID,
		"agents":  c.agents,
	}, "agents_registered"); err != nil {
		return err
	}

	ticker := time.NewTicker(c.heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-readerErrCh:
			return err
		case <-ticker.C:
			if _, err := c.request(ctx, map[string]any{
				"type":      "heartbeat",
				"host_id":   c.hostID,
				"agent_ids": c.agentIDs(),
			}, "heartbeat_ack"); err != nil {
				return err
			}
		}
	}
}

func (c *WorkerClient) request(
	ctx context.Context,
	payload map[string]any,
	wantType string,
) (map[string]any, error) {
	requestID := c.nextRequestID()
	payload = cloneMap(payload)
	payload["request_id"] = requestID
	waitCh := make(chan map[string]any, 1)

	c.mu.Lock()
	c.pending[requestID] = waitCh
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, requestID)
		c.mu.Unlock()
	}()

	if err := c.sendJSON(payload); err != nil {
		return nil, err
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case response := <-waitCh:
		gotType, _ := response["type"].(string)
		if gotType == "error" {
			raw, _ := json.Marshal(response)
			return nil, fmt.Errorf("echo worker gateway error: %s", strings.TrimSpace(string(raw)))
		}
		if gotType != wantType {
			return nil, fmt.Errorf("unexpected worker gateway response type %q, want %q", gotType, wantType)
		}
		return response, nil
	}
}

func (c *WorkerClient) readLoop(ctx context.Context, conn *websocket.Conn) error {
	for {
		var payload map[string]any
		if err := conn.ReadJSON(&payload); err != nil {
			c.failPending(err)
			return err
		}

		requestID, _ := payload["request_id"].(string)
		if requestID != "" {
			c.mu.Lock()
			waitCh := c.pending[requestID]
			c.mu.Unlock()
			if waitCh != nil {
				waitCh <- cloneMap(payload)
				continue
			}
		}

		msgType, _ := payload["type"].(string)
		switch msgType {
		case "assign_task":
			go c.handleAssignTask(ctx, payload)
		case "cancel_task":
			go c.handleCancelTask(payload)
		case "setup_workspace":
			go c.handleSetupWorkspace(payload)
		case "cleanup_workspace":
			go c.handleCleanupWorkspace(payload)
		case "ensure_repo_checkout":
			go c.handleEnsureRepoCheckout(payload)
		case "write_repo_file":
			go c.handleWriteRepoFile(payload)
		default:
			slog.Warn("worker client received unsupported gateway message", "type", msgType)
		}
	}
}

func (c *WorkerClient) handleAssignTask(ctx context.Context, payload map[string]any) {
	requestID, _ := payload["request_id"].(string)
	agentProject, _ := payload["agent_project"].(string)
	taskID, _ := payload["task_id"].(string)
	prompt, _ := payload["prompt"].(string)
	timeoutSec, _ := payload["timeout_sec"].(float64)
	workspaceRef := decodeWorkspaceRef(payload["workspace_ref"])

	job, err := c.jobMgr.Start(JobRequest{
		Project:      strings.TrimSpace(agentProject),
		TaskID:       strings.TrimSpace(taskID),
		Prompt:       prompt,
		Timeout:      time.Duration(int(timeoutSec)) * time.Second,
		WorkspaceRef: workspaceRef,
	})
	if err != nil {
		_ = c.sendJSON(map[string]any{
			"type":       "task_assigned",
			"request_id": requestID,
			"host_id":    c.hostID,
			"error":      err.Error(),
		})
		return
	}

	_ = c.sendJSON(map[string]any{
		"type":       "task_assigned",
		"request_id": requestID,
		"host_id":    c.hostID,
		"job":        workerJobPayload(job),
	})
	c.ensureWatcher(ctx, job.ID)
}

func (c *WorkerClient) handleCancelTask(payload map[string]any) {
	requestID, _ := payload["request_id"].(string)
	jobID, _ := payload["job_id"].(string)
	job, err := c.jobMgr.Cancel(strings.TrimSpace(jobID))
	if err != nil {
		_ = c.sendJSON(map[string]any{
			"type":       "cancel_ack",
			"request_id": requestID,
			"host_id":    c.hostID,
			"error":      err.Error(),
		})
		return
	}
	_ = c.sendJSON(map[string]any{
		"type":       "cancel_ack",
		"request_id": requestID,
		"host_id":    c.hostID,
		"job":        workerJobPayload(job),
	})
	c.ensureWatcher(context.Background(), job.ID)
}

func (c *WorkerClient) handleSetupWorkspace(payload map[string]any) {
	requestID, _ := payload["request_id"].(string)
	repoPath, _ := payload["repo_path"].(string)
	baseBranch, _ := payload["base_branch"].(string)
	branchName, _ := payload["branch_name"].(string)
	worktreePath, _ := payload["worktree_path"].(string)
	if err := SetupWorkspace(
		strings.TrimSpace(repoPath),
		strings.TrimSpace(baseBranch),
		strings.TrimSpace(branchName),
		strings.TrimSpace(worktreePath),
	); err != nil {
		_ = c.sendJSON(map[string]any{
			"type":       "workspace_ready",
			"request_id": requestID,
			"host_id":    c.hostID,
			"error":      err.Error(),
		})
		return
	}
	_ = c.sendJSON(map[string]any{
		"type":       "workspace_ready",
		"request_id": requestID,
		"host_id":    c.hostID,
		"result": map[string]any{
			"repo_path":     strings.TrimSpace(repoPath),
			"base_branch":   strings.TrimSpace(baseBranch),
			"branch_name":   strings.TrimSpace(branchName),
			"worktree_path": strings.TrimSpace(worktreePath),
		},
	})
}

func (c *WorkerClient) handleCleanupWorkspace(payload map[string]any) {
	requestID, _ := payload["request_id"].(string)
	worktreePath, _ := payload["worktree_path"].(string)
	if err := CleanupWorkspace(strings.TrimSpace(worktreePath)); err != nil {
		_ = c.sendJSON(map[string]any{
			"type":       "workspace_cleaned",
			"request_id": requestID,
			"host_id":    c.hostID,
			"error":      err.Error(),
		})
		return
	}
	_ = c.sendJSON(map[string]any{
		"type":       "workspace_cleaned",
		"request_id": requestID,
		"host_id":    c.hostID,
		"result": map[string]any{
			"worktree_path": strings.TrimSpace(worktreePath),
		},
	})
}

func (c *WorkerClient) handleEnsureRepoCheckout(payload map[string]any) {
	requestID, _ := payload["request_id"].(string)
	repoURL, _ := payload["repo_url"].(string)
	repoPath, _ := payload["repo_path"].(string)
	defaultBranch, _ := payload["default_branch"].(string)
	if err := EnsureRepoCheckout(
		strings.TrimSpace(repoURL),
		strings.TrimSpace(repoPath),
		strings.TrimSpace(defaultBranch),
	); err != nil {
		_ = c.sendJSON(map[string]any{
			"type":       "repo_checkout_ready",
			"request_id": requestID,
			"host_id":    c.hostID,
			"error":      err.Error(),
		})
		return
	}
	branch := strings.TrimSpace(defaultBranch)
	if branch == "" {
		branch = "main"
	}
	_ = c.sendJSON(map[string]any{
		"type":       "repo_checkout_ready",
		"request_id": requestID,
		"host_id":    c.hostID,
		"result": map[string]any{
			"repo_url":          strings.TrimSpace(repoURL),
			"repo_path":         strings.TrimSpace(repoPath),
			"default_branch":    branch,
			"checkout_state":    "ready",
			"provisioned":       true,
			"provisioning_mode": "worker_gateway",
		},
	})
}

func (c *WorkerClient) handleWriteRepoFile(payload map[string]any) {
	requestID, _ := payload["request_id"].(string)
	repoPath, _ := payload["repo_path"].(string)
	relativePath, _ := payload["relative_path"].(string)
	content, _ := payload["content"].(string)
	absolutePath, err := WriteRepoFile(
		strings.TrimSpace(repoPath),
		strings.TrimSpace(relativePath),
		content,
	)
	if err != nil {
		_ = c.sendJSON(map[string]any{
			"type":       "repo_file_written",
			"request_id": requestID,
			"host_id":    c.hostID,
			"error":      err.Error(),
		})
		return
	}
	_ = c.sendJSON(map[string]any{
		"type":       "repo_file_written",
		"request_id": requestID,
		"host_id":    c.hostID,
		"result": map[string]any{
			"repo_path":     strings.TrimSpace(repoPath),
			"relative_path": strings.TrimSpace(relativePath),
			"absolute_path": absolutePath,
		},
	})
}

func (c *WorkerClient) ensureWatcher(ctx context.Context, jobID string) {
	c.mu.Lock()
	if _, exists := c.watchers[jobID]; exists {
		c.mu.Unlock()
		return
	}
	watchCtx, cancel := context.WithCancel(ctx)
	c.watchers[jobID] = cancel
	c.mu.Unlock()

	go c.watchJob(watchCtx, jobID)
}

func (c *WorkerClient) watchJob(ctx context.Context, jobID string) {
	defer func() {
		c.mu.Lock()
		delete(c.watchers, jobID)
		c.mu.Unlock()
	}()

	ticker := time.NewTicker(defaultWorkerPollInterval)
	defer ticker.Stop()

	lastFingerprint := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			job, ok := c.jobMgr.Get(jobID)
			if !ok {
				return
			}
			payload := map[string]any{
				"host_id": c.hostID,
				"job":     workerJobPayload(job),
			}
			fingerprint := fingerprintPayload(payload)
			if fingerprint == lastFingerprint {
				if isTerminalWorkerJobStatus(job.Status) {
					return
				}
				continue
			}
			lastFingerprint = fingerprint
			if isTerminalWorkerJobStatus(job.Status) {
				if err := c.sendJSON(map[string]any{
					"type":    "task_result",
					"host_id": c.hostID,
					"job":     workerJobPayload(job),
				}); err != nil {
					slog.Warn("worker client failed to send task result", "job_id", jobID, "error", err)
				}
				return
			}
			if err := c.sendJSON(map[string]any{
				"type":    "task_progress",
				"host_id": c.hostID,
				"job":     workerJobPayload(job),
			}); err != nil {
				slog.Warn("worker client failed to send task progress", "job_id", jobID, "error", err)
				return
			}
		}
	}
}

func (c *WorkerClient) sendJSON(payload map[string]any) error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return fmt.Errorf("worker gateway websocket is not connected")
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if err := conn.WriteJSON(payload); err != nil {
		return fmt.Errorf("write %s: %w", payload["type"], err)
	}
	return nil
}

func (c *WorkerClient) failPending(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for requestID, waitCh := range c.pending {
		select {
		case waitCh <- map[string]any{
			"type":       "error",
			"request_id": requestID,
			"message":    err.Error(),
		}:
		default:
		}
	}
}

func (c *WorkerClient) stopAllWatchers() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, cancel := range c.watchers {
		cancel()
	}
	c.watchers = make(map[string]context.CancelFunc)
}

func (c *WorkerClient) nextRequestID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqSeq++
	return fmt.Sprintf("req-%d", c.reqSeq)
}

func (c *WorkerClient) agentIDs() []string {
	ids := make([]string, 0, len(c.agents))
	for _, agent := range c.agents {
		ids = append(ids, agent.ID)
	}
	return ids
}

func (c *WorkerClient) setConn(conn *websocket.Conn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.conn = conn
}

func (c *WorkerClient) closeConn() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
}

func buildWorkerAgentRegistrations(
	echoCfg config.EchoConfig,
	projects []config.ProjectConfig,
) ([]workerAgentRegistration, error) {
	var agents []workerAgentRegistration
	counts := map[string]int{}
	for _, proj := range projects {
		enabled, role := resolveEchoProjectEnabled(proj)
		if !enabled {
			continue
		}
		if role == "" {
			return nil, fmt.Errorf("project %q must set [projects.echo].role or use an echo-* project name", proj.Name)
		}
		agentType := strings.TrimSpace(proj.Agent.Type)
		if agentType == "" {
			return nil, fmt.Errorf("project %q missing agent type", proj.Name)
		}
		key := role + ":" + agentType
		counts[key]++
		agentID := strings.TrimSpace(proj.Echo.AgentID)
		if agentID == "" {
			agentID = fmt.Sprintf("agent-%s-%s-%d", role, agentType, counts[key])
		}
		promptTemplate := strings.TrimSpace(proj.Echo.PromptTemplate)
		if promptTemplate == "" {
			promptTemplate = fmt.Sprintf("prompts/coding/%s.md", role)
		}
		orgID := strings.TrimSpace(proj.Echo.OrgID)
		if orgID == "" {
			orgID = strings.TrimSpace(echoCfg.OrgID)
		}
		if orgID == "" {
			orgID = defaultEchoOrgID
		}
		agents = append(agents, workerAgentRegistration{
			ID:             agentID,
			OrgID:          orgID,
			Role:           role,
			CCProject:      proj.Name,
			AgentType:      agentType,
			PromptTemplate: promptTemplate,
		})
	}
	return agents, nil
}

func resolveEchoProjectEnabled(proj config.ProjectConfig) (bool, string) {
	if proj.Echo.Enabled != nil && !*proj.Echo.Enabled {
		return false, ""
	}
	role := strings.TrimSpace(proj.Echo.Role)
	if role != "" {
		return true, role
	}
	if inferredRole, ok := inferEchoRole(proj.Name); ok {
		return true, inferredRole
	}
	if proj.Echo.Enabled != nil && *proj.Echo.Enabled {
		return true, ""
	}
	return false, ""
}

func inferEchoRole(projectName string) (string, bool) {
	parts := strings.Split(strings.TrimSpace(projectName), "-")
	if len(parts) < 3 || parts[0] != "echo" {
		return "", false
	}
	role := strings.TrimSpace(parts[1])
	if role == "" {
		return "", false
	}
	return role, true
}

func workerGatewayWebSocketURL(serverURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(serverURL))
	if err != nil {
		return "", fmt.Errorf("parse echo server url: %w", err)
	}
	switch parsed.Scheme {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("unsupported echo server url scheme %q", parsed.Scheme)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/api/v1/workers/ws"
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func decodeWorkspaceRef(raw any) JobWorkspaceRef {
	payload, ok := raw.(map[string]any)
	if !ok {
		return JobWorkspaceRef{}
	}
	ref := JobWorkspaceRef{}
	if value, ok := payload["repo_path"].(string); ok {
		ref.RepoPath = value
	}
	if value, ok := payload["worktree_path"].(string); ok {
		ref.WorktreePath = value
	}
	if value, ok := payload["branch"].(string); ok {
		ref.Branch = value
	}
	return ref
}

func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func fingerprintPayload(payload map[string]any) string {
	data, _ := json.Marshal(payload)
	return string(data)
}

func isTerminalWorkerJobStatus(status string) bool {
	switch status {
	case JobStatusCompleted, JobStatusFailed, JobStatusCancelled, JobStatusTimedOut, JobStatusOrphaned:
		return true
	default:
		return false
	}
}

func workerJobPayload(job *Job) map[string]any {
	if job == nil {
		return map[string]any{}
	}
	payload := map[string]any{
		"id":            job.ID,
		"project":       job.Project,
		"task_id":       job.TaskID,
		"prompt":        job.Prompt,
		"workspace_ref": job.WorkspaceRef,
		"status":        job.Status,
		"output":        job.Output,
		"summary":       job.Summary,
		"session_id":    job.SessionID,
		"error":         job.Error,
		"error_code":    job.ErrorCode,
		"created_at":    job.CreatedAt.Format(time.RFC3339Nano),
		"timeout_sec":   job.TimeoutSec,
		"event_count":   job.EventCount,
		"events":        []JobEvent{},
	}
	if len(job.Events) > 0 {
		payload["events"] = append([]JobEvent(nil), job.Events...)
	}
	if job.StartedAt != nil {
		payload["started_at"] = job.StartedAt.Format(time.RFC3339Nano)
	}
	if job.FinishedAt != nil {
		payload["finished_at"] = job.FinishedAt.Format(time.RFC3339Nano)
	}
	return payload
}
