package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

type mcpTestJobRunner struct {
	run func(ctx context.Context, req JobRequest) (*JobResult, error)
}

func (r mcpTestJobRunner) Run(ctx context.Context, req JobRequest) (*JobResult, error) {
	return r.run(ctx, req)
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
}

func TestMCPServerTaskRunLifecycle(t *testing.T) {
	jm, err := NewJobManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewJobManager: %v", err)
	}

	done := make(chan struct{})
	jm.RegisterRunner("demo", mcpTestJobRunner{run: func(ctx context.Context, req JobRequest) (*JobResult, error) {
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
}

func TestMCPServerCancelTaskRun(t *testing.T) {
	jm, err := NewJobManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewJobManager: %v", err)
	}

	blocked := make(chan struct{})
	release := make(chan struct{})
	jm.RegisterRunner("demo", mcpTestJobRunner{run: func(ctx context.Context, req JobRequest) (*JobResult, error) {
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
}

func TestMCPServerRequiresBearerToken(t *testing.T) {
	jm, err := NewJobManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewJobManager: %v", err)
	}
	jm.RegisterRunner("demo", mcpTestJobRunner{run: func(ctx context.Context, req JobRequest) (*JobResult, error) {
		return &JobResult{Summary: "ok"}, nil
	}})

	httpServer := httptest.NewServer(NewMCPServer(jm, "secret").Handler())
	defer httpServer.Close()

	unauthorizedClient, err := client.NewStreamableHttpClient(httpServer.URL + "/mcp")
	if err != nil {
		t.Fatalf("NewStreamableHttpClient: %v", err)
	}
	defer unauthorizedClient.Close()
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
	t.Cleanup(func() {
		mcpClient.Close()
	})
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
