package core

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

type mcpTestJobRunner struct {
	run func(
		ctx context.Context,
		req JobRequest,
		jobID string,
		onEvent func(JobEvent),
	) (*JobResult, error)
}

func (r mcpTestJobRunner) Run(
	ctx context.Context,
	req JobRequest,
	jobID string,
	onEvent func(JobEvent),
) (*JobResult, error) {
	return r.run(ctx, req, jobID, onEvent)
}

type jobPayload struct {
	ID         string `json:"id"`
	Project    string `json:"project"`
	TaskID     string `json:"task_id"`
	Status     string `json:"status"`
	Summary    string `json:"summary"`
	SessionID  string `json:"session_id"`
	TimeoutSec int    `json:"timeout_sec"`
	Error      string `json:"error"`
	ErrorCode  string `json:"error_code"`
	Workspace  struct {
		RepoPath     string `json:"repo_path"`
		WorktreePath string `json:"worktree_path"`
		Branch       string `json:"branch"`
	} `json:"workspace_ref"`
}

type workspacePayload struct {
	Status       string `json:"status"`
	RepoPath     string `json:"repo_path"`
	BranchName   string `json:"branch_name"`
	WorktreePath string `json:"worktree_path"`
}

type repoCheckoutPayload struct {
	Status        string `json:"status"`
	RepoURL       string `json:"repo_url"`
	RepoPath      string `json:"repo_path"`
	DefaultBranch string `json:"default_branch"`
}

type listAgentsPayload struct {
	Agents []RegisteredProject `json:"agents"`
}

func TestMCPServerTaskRunLifecycle(t *testing.T) {
	jm, err := NewJobManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewJobManager: %v", err)
	}

	done := make(chan struct{})
	jm.RegisterRunner("demo", mcpTestJobRunner{run: func(ctx context.Context, req JobRequest, jobID string, onEvent func(JobEvent)) (*JobResult, error) {
		_ = onEvent
		_ = ctx
		_ = req
		_ = jobID
		close(done)
		return &JobResult{
			Output:    "raw output",
			Summary:   "finished",
			SessionID: "session-123",
		}, nil
	}})

	httpServer := httptest.NewServer(NewMCPServer(jm, "").Handler())
	defer httpServer.Close()

	mcpClient := startMCPClient(t, httpServer.URL+"/mcp")
	defer func() {
		_ = mcpClient.Close()
		time.Sleep(25 * time.Millisecond)
	}()

	var startReq mcp.CallToolRequest
	startReq.Params.Name = "start_task_run"
	startReq.Params.Arguments = map[string]any{
		"project":       "demo",
		"task_id":       "task-1",
		"prompt":        "fix the tests",
		"timeout_sec":   15,
		"repo_path":     "/repo",
		"worktree_path": "/repo/.echo/workspaces/task-1",
		"branch":        "echo/task-1",
	}
	startResult, err := mcpClient.CallTool(context.Background(), startReq)
	if err != nil {
		t.Fatalf("start_task_run: %v", err)
	}

	var started jobPayload
	if err := decodeStructuredResult(startResult, &started); err != nil {
		t.Fatalf("decode start result: %v", err)
	}
	if started.Project != "demo" || started.TaskID != "task-1" {
		t.Fatalf("unexpected started payload: %+v", started)
	}
	if started.TimeoutSec != 15 {
		t.Fatalf("unexpected timeout: %d", started.TimeoutSec)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("job did not complete")
	}
	waitForJobStatus(t, jm, started.ID, JobStatusCompleted)

	var getReq mcp.CallToolRequest
	getReq.Params.Name = "get_task_run"
	getReq.Params.Arguments = map[string]any{"job_id": started.ID}
	getResult, err := mcpClient.CallTool(context.Background(), getReq)
	if err != nil {
		t.Fatalf("get_task_run: %v", err)
	}

	var fetched jobPayload
	if err := decodeStructuredResult(getResult, &fetched); err != nil {
		t.Fatalf("decode get result: %v", err)
	}
	if fetched.Status != JobStatusCompleted {
		t.Fatalf("unexpected fetched status: %+v", fetched)
	}
	if fetched.Summary != "finished" || fetched.SessionID != "session-123" {
		t.Fatalf("unexpected fetched payload: %+v", fetched)
	}
	if fetched.Workspace.RepoPath != "/repo" || fetched.Workspace.WorktreePath != "/repo/.echo/workspaces/task-1" || fetched.Workspace.Branch != "echo/task-1" {
		t.Fatalf("unexpected workspace payload: %+v", fetched.Workspace)
	}
}

func TestMCPServerTaskRunLifecycleAcceptsNestedWorkspaceRef(t *testing.T) {
	jm, err := NewJobManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewJobManager: %v", err)
	}

	done := make(chan struct{})
	jm.RegisterRunner("demo", mcpTestJobRunner{run: func(ctx context.Context, req JobRequest, jobID string, onEvent func(JobEvent)) (*JobResult, error) {
		_ = onEvent
		_ = ctx
		_ = jobID
		if req.WorkspaceRef.RepoPath != "/repo" || req.WorkspaceRef.WorktreePath != "/repo/.echo/workspaces/task-2" || req.WorkspaceRef.Branch != "echo/task-2" {
			t.Fatalf("unexpected workspace ref: %+v", req.WorkspaceRef)
		}
		close(done)
		return &JobResult{Summary: "finished"}, nil
	}})

	httpServer := httptest.NewServer(NewMCPServer(jm, "").Handler())
	defer httpServer.Close()

	mcpClient := startMCPClient(t, httpServer.URL+"/mcp")
	defer func() {
		_ = mcpClient.Close()
		time.Sleep(25 * time.Millisecond)
	}()

	var startReq mcp.CallToolRequest
	startReq.Params.Name = "start_task_run"
	startReq.Params.Arguments = map[string]any{
		"project": "demo",
		"task_id": "task-2",
		"prompt":  "fix the tests",
		"workspace_ref": map[string]any{
			"repo_path":     "/repo",
			"worktree_path": "/repo/.echo/workspaces/task-2",
			"branch":        "echo/task-2",
		},
	}
	startResult, err := mcpClient.CallTool(context.Background(), startReq)
	if err != nil {
		t.Fatalf("start_task_run: %v", err)
	}

	var started jobPayload
	if err := decodeStructuredResult(startResult, &started); err != nil {
		t.Fatalf("decode start result: %v", err)
	}
	if started.Workspace.RepoPath != "/repo" || started.Workspace.WorktreePath != "/repo/.echo/workspaces/task-2" || started.Workspace.Branch != "echo/task-2" {
		t.Fatalf("unexpected workspace payload: %+v", started.Workspace)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("job did not complete")
	}
	waitForJobStatus(t, jm, started.ID, JobStatusCompleted)
}

func TestMCPServerCancelTaskRun(t *testing.T) {
	jm, err := NewJobManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewJobManager: %v", err)
	}

	blocked := make(chan struct{})
	release := make(chan struct{})
	jm.RegisterRunner("demo", mcpTestJobRunner{run: func(ctx context.Context, req JobRequest, jobID string, onEvent func(JobEvent)) (*JobResult, error) {
		_ = onEvent
		_ = req
		_ = jobID
		close(blocked)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return &JobResult{Summary: "unexpected"}, nil
		}
	}})

	httpServer := httptest.NewServer(NewMCPServer(jm, "").Handler())
	defer httpServer.Close()

	mcpClient := startMCPClient(t, httpServer.URL+"/mcp")
	defer func() {
		_ = mcpClient.Close()
		time.Sleep(25 * time.Millisecond)
	}()

	var startReq mcp.CallToolRequest
	startReq.Params.Name = "start_task_run"
	startReq.Params.Arguments = map[string]any{
		"project": "demo",
		"prompt":  "long running work",
	}
	startResult, err := mcpClient.CallTool(context.Background(), startReq)
	if err != nil {
		t.Fatalf("start_task_run: %v", err)
	}

	var started jobPayload
	if err := decodeStructuredResult(startResult, &started); err != nil {
		t.Fatalf("decode start result: %v", err)
	}

	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("job did not start")
	}

	var cancelReq mcp.CallToolRequest
	cancelReq.Params.Name = "cancel_task_run"
	cancelReq.Params.Arguments = map[string]any{"job_id": started.ID}
	cancelResult, err := mcpClient.CallTool(context.Background(), cancelReq)
	if err != nil {
		t.Fatalf("cancel_task_run: %v", err)
	}

	var cancelled jobPayload
	if err := decodeStructuredResult(cancelResult, &cancelled); err != nil {
		t.Fatalf("decode cancel result: %v", err)
	}
	if cancelled.Status != JobStatusCancelled {
		t.Fatalf("unexpected cancelled payload: %+v", cancelled)
	}
	close(release)
	waitForJobStatus(t, jm, started.ID, JobStatusCancelled)
}

func TestMCPServerRequiresBearerToken(t *testing.T) {
	jm, err := NewJobManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewJobManager: %v", err)
	}
	jm.RegisterRunner("demo", mcpTestJobRunner{run: func(ctx context.Context, req JobRequest, jobID string, onEvent func(JobEvent)) (*JobResult, error) {
		_ = onEvent
		_ = ctx
		_ = req
		_ = jobID
		return &JobResult{Summary: "ok"}, nil
	}})

	httpServer := httptest.NewServer(NewMCPServer(jm, "secret").Handler())
	defer httpServer.Close()

	unauthorizedClient, err := client.NewStreamableHttpClient(httpServer.URL + "/mcp")
	if err != nil {
		t.Fatalf("NewStreamableHttpClient: %v", err)
	}
	defer func() {
		_ = unauthorizedClient.Close()
		time.Sleep(25 * time.Millisecond)
	}()
	if err := unauthorizedClient.Start(context.Background()); err != nil {
		t.Fatalf("Start unauthorized client: %v", err)
	}
	_, err = unauthorizedClient.Initialize(context.Background(), initializeRequest())
	if err == nil {
		t.Fatal("expected unauthorized initialize to fail")
	}

	authorizedClient := startMCPClient(
		t,
		httpServer.URL+"/mcp",
		transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer secret"}),
	)
	defer func() {
		_ = authorizedClient.Close()
		time.Sleep(25 * time.Millisecond)
	}()

	var req mcp.CallToolRequest
	req.Params.Name = "start_task_run"
	req.Params.Arguments = map[string]any{
		"project": "demo",
		"prompt":  "work",
	}
	result, err := authorizedClient.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("authorized start_task_run: %v", err)
	}

	var payload jobPayload
	if err := decodeStructuredResult(result, &payload); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if payload.Project != "demo" {
		t.Fatalf("unexpected payload: %+v", payload)
	}
}

func TestMCPServerWorkspaceLifecycle(t *testing.T) {
	jm, err := NewJobManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewJobManager: %v", err)
	}
	repoPath := initGitRepo(t)
	worktreePath := t.TempDir() + "/task-1"

	httpServer := httptest.NewServer(NewMCPServer(jm, "").Handler())
	defer httpServer.Close()

	mcpClient := startMCPClient(t, httpServer.URL+"/mcp")
	defer func() {
		_ = mcpClient.Close()
		time.Sleep(25 * time.Millisecond)
	}()

	var setupReq mcp.CallToolRequest
	setupReq.Params.Name = "setup_workspace"
	setupReq.Params.Arguments = map[string]any{
		"repo_path":     repoPath,
		"base_branch":   "main",
		"branch_name":   "echo/task-1",
		"worktree_path": worktreePath,
	}
	setupResult, err := mcpClient.CallTool(context.Background(), setupReq)
	if err != nil {
		t.Fatalf("setup_workspace: %v", err)
	}

	var setupPayload workspacePayload
	if err := decodeStructuredResult(setupResult, &setupPayload); err != nil {
		t.Fatalf("decode setup result: %v", err)
	}
	if setupPayload.Status != "ready" || setupPayload.WorktreePath != worktreePath {
		t.Fatalf("unexpected setup payload: %+v", setupPayload)
	}

	var cleanupReq mcp.CallToolRequest
	cleanupReq.Params.Name = "cleanup_workspace"
	cleanupReq.Params.Arguments = map[string]any{"worktree_path": worktreePath}
	cleanupResult, err := mcpClient.CallTool(context.Background(), cleanupReq)
	if err != nil {
		t.Fatalf("cleanup_workspace: %v", err)
	}

	var cleanupPayload workspacePayload
	if err := decodeStructuredResult(cleanupResult, &cleanupPayload); err != nil {
		t.Fatalf("decode cleanup result: %v", err)
	}
	if cleanupPayload.Status != "cleaned" || cleanupPayload.WorktreePath != worktreePath {
		t.Fatalf("unexpected cleanup payload: %+v", cleanupPayload)
	}
}

func TestMCPServerEnsureRepoCheckout(t *testing.T) {
	jm, err := NewJobManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewJobManager: %v", err)
	}
	sourceRepo := initGitRepo(t)
	checkoutPath := filepath.Join(t.TempDir(), "clones", "frontend")

	httpServer := httptest.NewServer(NewMCPServer(jm, "").Handler())
	defer httpServer.Close()

	mcpClient := startMCPClient(t, httpServer.URL+"/mcp")
	defer func() {
		_ = mcpClient.Close()
		time.Sleep(25 * time.Millisecond)
	}()

	var req mcp.CallToolRequest
	req.Params.Name = "ensure_repo_checkout"
	req.Params.Arguments = map[string]any{
		"repo_url":       sourceRepo,
		"repo_path":      checkoutPath,
		"default_branch": "main",
	}
	result, err := mcpClient.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("ensure_repo_checkout: %v", err)
	}

	var payload repoCheckoutPayload
	if err := decodeStructuredResult(result, &payload); err != nil {
		t.Fatalf("decode ensure_repo_checkout result: %v", err)
	}
	if payload.Status != "ready" || payload.RepoPath != checkoutPath || payload.RepoURL != sourceRepo {
		t.Fatalf("unexpected repo checkout payload: %+v", payload)
	}
	if _, err := os.Stat(filepath.Join(checkoutPath, "README.md")); err != nil {
		t.Fatalf("expected checkout README: %v", err)
	}
}

func TestMCPServerEnsureRepoCheckoutDefaultsBranch(t *testing.T) {
	jm, err := NewJobManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewJobManager: %v", err)
	}
	sourceRepo := initGitRepo(t)
	checkoutPath := filepath.Join(t.TempDir(), "clones", "frontend-default")

	httpServer := httptest.NewServer(NewMCPServer(jm, "").Handler())
	defer httpServer.Close()

	mcpClient := startMCPClient(t, httpServer.URL+"/mcp")
	defer func() {
		_ = mcpClient.Close()
		time.Sleep(25 * time.Millisecond)
	}()

	var req mcp.CallToolRequest
	req.Params.Name = "ensure_repo_checkout"
	req.Params.Arguments = map[string]any{
		"repo_url":  sourceRepo,
		"repo_path": checkoutPath,
	}
	result, err := mcpClient.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("ensure_repo_checkout: %v", err)
	}

	var payload repoCheckoutPayload
	if err := decodeStructuredResult(result, &payload); err != nil {
		t.Fatalf("decode ensure_repo_checkout result: %v", err)
	}
	if payload.DefaultBranch != "main" {
		t.Fatalf("default branch = %q, want main", payload.DefaultBranch)
	}
}

func TestMCPServerListAgents(t *testing.T) {
	jm, err := NewJobManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewJobManager: %v", err)
	}

	blocked := make(chan struct{})
	release := make(chan struct{})
	jm.RegisterProject("alpha", "codex", mcpTestJobRunner{
		run: func(ctx context.Context, req JobRequest, jobID string, onEvent func(JobEvent)) (*JobResult, error) {
			_ = onEvent
			_ = req
			_ = jobID
			close(blocked)
			<-release
			return &JobResult{Summary: "done"}, ctx.Err()
		},
	})
	jm.RegisterProject("beta", "claudecode", mcpTestJobRunner{
		run: func(ctx context.Context, req JobRequest, jobID string, onEvent func(JobEvent)) (*JobResult, error) {
			_ = onEvent
			_ = ctx
			_ = req
			_ = jobID
			return &JobResult{Summary: "ok"}, nil
		},
	})

	if _, err := jm.Start(JobRequest{Project: "alpha", Prompt: "busy"}); err != nil {
		t.Fatalf("Start alpha: %v", err)
	}
	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("alpha job did not start")
	}

	httpServer := httptest.NewServer(NewMCPServer(jm, "").Handler())
	defer httpServer.Close()
	mcpClient := startMCPClient(t, httpServer.URL+"/mcp")
	defer func() {
		close(release)
		for _, job := range jm.List() {
			if job.Project == "alpha" {
				waitForJobStatus(t, jm, job.ID, JobStatusCompleted)
			}
		}
		_ = mcpClient.Close()
		time.Sleep(25 * time.Millisecond)
	}()

	var req mcp.CallToolRequest
	req.Params.Name = "list_agents"
	result, err := mcpClient.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("list_agents: %v", err)
	}

	var payload listAgentsPayload
	if err := decodeStructuredResult(result, &payload); err != nil {
		t.Fatalf("decode list_agents result: %v", err)
	}
	if len(payload.Agents) != 2 {
		t.Fatalf("agent count = %d, want 2", len(payload.Agents))
	}
	if payload.Agents[0].Project != "alpha" || payload.Agents[0].Status != "busy" {
		t.Fatalf("unexpected alpha payload: %+v", payload.Agents[0])
	}
	if payload.Agents[0].AgentType != "codex" || payload.Agents[0].ActiveJobs != 1 {
		t.Fatalf("unexpected alpha job counts: %+v", payload.Agents[0])
	}
	if payload.Agents[1].Project != "beta" || payload.Agents[1].Status != "idle" {
		t.Fatalf("unexpected beta payload: %+v", payload.Agents[1])
	}
	if payload.Agents[1].AgentType != "claudecode" {
		t.Fatalf("unexpected beta agent type: %+v", payload.Agents[1])
	}
}

func TestMCPServerConcurrentJobsAcrossProjects(t *testing.T) {
	jm, err := NewJobManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewJobManager: %v", err)
	}

	alphaStarted := make(chan struct{})
	betaStarted := make(chan struct{})
	alphaRelease := make(chan struct{})
	betaRelease := make(chan struct{})

	jm.RegisterProject("alpha", "codex", mcpTestJobRunner{
		run: func(ctx context.Context, req JobRequest, jobID string, onEvent func(JobEvent)) (*JobResult, error) {
			_ = onEvent
			if req.Project != "alpha" {
				t.Fatalf("alpha req.Project = %q", req.Project)
			}
			if jobID == "" {
				t.Fatal("expected alpha jobID")
			}
			close(alphaStarted)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-alphaRelease:
				return &JobResult{Summary: "alpha done", SessionID: "alpha-session"}, nil
			}
		},
	})
	jm.RegisterProject("beta", "claudecode", mcpTestJobRunner{
		run: func(ctx context.Context, req JobRequest, jobID string, onEvent func(JobEvent)) (*JobResult, error) {
			_ = onEvent
			if req.Project != "beta" {
				t.Fatalf("beta req.Project = %q", req.Project)
			}
			if jobID == "" {
				t.Fatal("expected beta jobID")
			}
			close(betaStarted)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-betaRelease:
				return &JobResult{Summary: "beta done", SessionID: "beta-session"}, nil
			}
		},
	})

	httpServer := httptest.NewServer(NewMCPServer(jm, "").Handler())
	defer httpServer.Close()

	mcpClient := startMCPClient(t, httpServer.URL+"/mcp")
	defer func() {
		_ = mcpClient.Close()
		time.Sleep(25 * time.Millisecond)
	}()

	startJob := func(project string, taskID string) jobPayload {
		t.Helper()
		var req mcp.CallToolRequest
		req.Params.Name = "start_task_run"
		req.Params.Arguments = map[string]any{
			"project": project,
			"task_id": taskID,
			"prompt":  "run concurrently",
		}
		result, err := mcpClient.CallTool(context.Background(), req)
		if err != nil {
			t.Fatalf("start_task_run(%s): %v", project, err)
		}
		var payload jobPayload
		if err := decodeStructuredResult(result, &payload); err != nil {
			t.Fatalf("decode start payload: %v", err)
		}
		return payload
	}

	alphaJob := startJob("alpha", "task-alpha")
	betaJob := startJob("beta", "task-beta")

	select {
	case <-alphaStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("alpha job did not start")
	}
	select {
	case <-betaStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("beta job did not start")
	}

	var listReq mcp.CallToolRequest
	listReq.Params.Name = "list_agents"
	listResult, err := mcpClient.CallTool(context.Background(), listReq)
	if err != nil {
		t.Fatalf("list_agents: %v", err)
	}
	var listPayload listAgentsPayload
	if err := decodeStructuredResult(listResult, &listPayload); err != nil {
		t.Fatalf("decode list_agents: %v", err)
	}
	if len(listPayload.Agents) != 2 {
		t.Fatalf("agent count = %d, want 2", len(listPayload.Agents))
	}
	for _, agent := range listPayload.Agents {
		if agent.Status != "busy" || agent.ActiveJobs != 1 {
			t.Fatalf("expected busy agent with 1 active job, got %+v", agent)
		}
	}

	close(alphaRelease)
	close(betaRelease)

	waitForJobStatus(t, jm, alphaJob.ID, JobStatusCompleted)
	waitForJobStatus(t, jm, betaJob.ID, JobStatusCompleted)

	getJob := func(jobID string) jobPayload {
		t.Helper()
		var req mcp.CallToolRequest
		req.Params.Name = "get_task_run"
		req.Params.Arguments = map[string]any{"job_id": jobID}
		result, err := mcpClient.CallTool(context.Background(), req)
		if err != nil {
			t.Fatalf("get_task_run(%s): %v", jobID, err)
		}
		var payload jobPayload
		if err := decodeStructuredResult(result, &payload); err != nil {
			t.Fatalf("decode get payload: %v", err)
		}
		return payload
	}

	fetchedAlpha := getJob(alphaJob.ID)
	fetchedBeta := getJob(betaJob.ID)
	if fetchedAlpha.Project != "alpha" || fetchedAlpha.Summary != "alpha done" {
		t.Fatalf("unexpected alpha payload: %+v", fetchedAlpha)
	}
	if fetchedBeta.Project != "beta" || fetchedBeta.Summary != "beta done" {
		t.Fatalf("unexpected beta payload: %+v", fetchedBeta)
	}
}

func TestDecodeStructuredResultHandlesToolErrors(t *testing.T) {
	err := decodeStructuredResult(mcp.NewToolResultError("boom"), &jobPayload{})
	if err == nil || err.Error() != "boom" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func startMCPClient(
	t *testing.T,
	serverURL string,
	options ...transport.StreamableHTTPCOption,
) *client.Client {
	t.Helper()

	mcpClient, err := client.NewStreamableHttpClient(serverURL, options...)
	if err != nil {
		t.Fatalf("NewStreamableHttpClient: %v", err)
	}
	if err := mcpClient.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := mcpClient.Initialize(context.Background(), initializeRequest()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	return mcpClient
}

func initializeRequest() mcp.InitializeRequest {
	return mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo: mcp.Implementation{
				Name:    "cc-connect-test-client",
				Version: "1.0.0",
			},
		},
	}
}

func TestMCPServerHandlerRespondsUnauthorizedWithoutToken(t *testing.T) {
	handler := withOptionalBearerAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), "secret")

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unexpected status: %d", recorder.Code)
	}
}

func TestValidateMCPListenerSecurity(t *testing.T) {
	if err := validateMCPListenerSecurity(&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 9820}, ""); err != nil {
		t.Fatalf("expected loopback listener to be allowed, got %v", err)
	}
	if err := validateMCPListenerSecurity(&net.TCPAddr{IP: net.ParseIP("::1"), Port: 9820}, ""); err != nil {
		t.Fatalf("expected IPv6 loopback listener to be allowed, got %v", err)
	}
	if err := validateMCPListenerSecurity(&net.TCPAddr{IP: net.ParseIP("0.0.0.0"), Port: 9820}, "secret"); err != nil {
		t.Fatalf("expected auth-protected listener to be allowed, got %v", err)
	}
	err := validateMCPListenerSecurity(&net.TCPAddr{IP: net.ParseIP("0.0.0.0"), Port: 9820}, "")
	if err == nil {
		t.Fatal("expected non-loopback listener without auth to fail")
	}
	if !strings.Contains(err.Error(), "auth_token is required") {
		t.Fatalf("unexpected error: %v", err)
	}
	err = validateMCPListenerSecurity(&net.TCPAddr{IP: net.ParseIP("::"), Port: 9820}, "")
	if err == nil {
		t.Fatal("expected unspecified IPv6 listener without auth to fail")
	}
}
