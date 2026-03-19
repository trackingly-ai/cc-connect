package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// MCPServer exposes JobManager operations over MCP streamable HTTP.
type MCPServer struct {
	authToken string
	handler   http.Handler
	server    *http.Server
	listener  net.Listener
}

// NewMCPServer builds an MCP server that exposes asynchronous task-run tools.
func NewMCPServer(jm *JobManager, authToken string) *MCPServer {
	serverVersion := CurrentVersion
	if serverVersion == "" {
		serverVersion = "dev"
	}

	mcpSrv := mcpserver.NewMCPServer(
		"cc-connect",
		serverVersion,
		mcpserver.WithToolCapabilities(true),
	)

	listAgentsTool := mcp.NewTool("list_agents",
		mcp.WithDescription("List registered cc-connect projects and their current job availability."),
	)
	mcpSrv.AddTool(listAgentsTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		_ = ctx
		_ = req
		agents := jm.ListAgents()
		return mcp.NewToolResultStructured(
			map[string]any{"agents": agents},
			fmt.Sprintf("listed %d agents", len(agents)),
		), nil
	})

	startTool := mcp.NewTool("start_task_run",
		mcp.WithDescription("Start an asynchronous task run in a named cc-connect project."),
		mcp.WithString("project", mcp.Description("Registered cc-connect project name."), mcp.Required()),
		mcp.WithString("task_id", mcp.Description("Optional orchestrator task identifier.")),
		mcp.WithString("prompt", mcp.Description("Prompt to send to the agent."), mcp.Required()),
		mcp.WithNumber("timeout_sec", mcp.Description("Optional timeout in seconds.")),
		mcp.WithString("repo_path", mcp.Description("Optional repository path for audit metadata.")),
		mcp.WithString("worktree_path", mcp.Description("Optional worktree path for audit metadata.")),
		mcp.WithString("branch", mcp.Description("Optional git branch for audit metadata.")),
	)
	mcpSrv.AddTool(startTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		project := strings.TrimSpace(req.GetString("project", ""))
		prompt := strings.TrimSpace(req.GetString("prompt", ""))
		if project == "" {
			return mcp.NewToolResultError("project is required"), nil
		}
		if prompt == "" {
			return mcp.NewToolResultError("prompt is required"), nil
		}

		timeoutSec := int(req.GetFloat("timeout_sec", 0))
		if timeoutSec < 0 {
			return mcp.NewToolResultError("timeout_sec must be >= 0"), nil
		}

		workspaceRef, err := workspaceRefFromArguments(req)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		job, err := jm.Start(JobRequest{
			Project:      project,
			TaskID:       strings.TrimSpace(req.GetString("task_id", "")),
			Prompt:       prompt,
			Timeout:      time.Duration(timeoutSec) * time.Second,
			WorkspaceRef: workspaceRef,
		})
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultStructured(jobResponse(job), fmt.Sprintf("started task run %s", job.ID)), nil
	})

	getTool := mcp.NewTool("get_task_run",
		mcp.WithDescription("Fetch the latest state for a previously started task run."),
		mcp.WithString("job_id", mcp.Description("Job identifier returned by start_task_run."), mcp.Required()),
	)
	mcpSrv.AddTool(getTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		jobID := strings.TrimSpace(req.GetString("job_id", ""))
		if jobID == "" {
			return mcp.NewToolResultError("job_id is required"), nil
		}
		job, ok := jm.Get(jobID)
		if !ok {
			return mcp.NewToolResultError(fmt.Sprintf("job %q not found", jobID)), nil
		}
		return mcp.NewToolResultStructured(jobResponse(job), fmt.Sprintf("fetched task run %s", job.ID)), nil
	})

	cancelTool := mcp.NewTool("cancel_task_run",
		mcp.WithDescription("Cancel a running or queued task run."),
		mcp.WithString("job_id", mcp.Description("Job identifier returned by start_task_run."), mcp.Required()),
	)
	mcpSrv.AddTool(cancelTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		jobID := strings.TrimSpace(req.GetString("job_id", ""))
		if jobID == "" {
			return mcp.NewToolResultError("job_id is required"), nil
		}
		job, err := jm.Cancel(jobID)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultStructured(jobResponse(job), fmt.Sprintf("cancelled task run %s", job.ID)), nil
	})

	setupWorkspaceTool := mcp.NewTool("setup_workspace",
		mcp.WithDescription("Create a git worktree workspace for a task."),
		mcp.WithString("repo_path", mcp.Description("Repository root path."), mcp.Required()),
		mcp.WithString("base_branch", mcp.Description("Branch or ref to branch from."), mcp.Required()),
		mcp.WithString("branch_name", mcp.Description("Workspace branch name."), mcp.Required()),
		mcp.WithString("worktree_path", mcp.Description("Destination worktree path."), mcp.Required()),
	)
	mcpSrv.AddTool(setupWorkspaceTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		repoPath := strings.TrimSpace(req.GetString("repo_path", ""))
		baseBranch := strings.TrimSpace(req.GetString("base_branch", ""))
		branchName := strings.TrimSpace(req.GetString("branch_name", ""))
		worktreePath := strings.TrimSpace(req.GetString("worktree_path", ""))
		if err := SetupWorkspace(repoPath, baseBranch, branchName, worktreePath); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultStructured(
			workspaceResponse("ready", repoPath, branchName, worktreePath),
			fmt.Sprintf("prepared workspace %s", worktreePath),
		), nil
	})

	ensureRepoCheckoutTool := mcp.NewTool("ensure_repo_checkout",
		mcp.WithDescription("Ensure a host-local git checkout exists for a project repository."),
		mcp.WithString("repo_url", mcp.Description("Git repository URL to clone or validate."), mcp.Required()),
		mcp.WithString("repo_path", mcp.Description("Destination repository checkout path."), mcp.Required()),
		mcp.WithString("default_branch", mcp.Description("Default branch to clone or checkout.")),
	)
	mcpSrv.AddTool(ensureRepoCheckoutTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		repoURL := strings.TrimSpace(req.GetString("repo_url", ""))
		repoPath := strings.TrimSpace(req.GetString("repo_path", ""))
		defaultBranch := strings.TrimSpace(req.GetString("default_branch", ""))
		if defaultBranch == "" {
			defaultBranch = "main"
		}
		if err := EnsureRepoCheckout(repoURL, repoPath, defaultBranch); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultStructured(
			map[string]any{
				"status":         "ready",
				"repo_url":       repoURL,
				"repo_path":      repoPath,
				"default_branch": defaultBranch,
			},
			fmt.Sprintf("prepared repo checkout %s", repoPath),
		), nil
	})

	cleanupWorkspaceTool := mcp.NewTool("cleanup_workspace",
		mcp.WithDescription("Remove a git worktree workspace for a task."),
		mcp.WithString("worktree_path", mcp.Description("Workspace worktree path."), mcp.Required()),
		mcp.WithBoolean("keep_branch", mcp.Description("Keep the workspace branch after removing the worktree.")),
	)
	mcpSrv.AddTool(cleanupWorkspaceTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		worktreePath := strings.TrimSpace(req.GetString("worktree_path", ""))
		if err := CleanupWorkspaceWithOptions(worktreePath, CleanupWorkspaceOptions{
			KeepBranch: req.GetBool("keep_branch", false),
		}); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultStructured(
			workspaceResponse("cleaned", "", "", worktreePath),
			fmt.Sprintf("cleaned workspace %s", worktreePath),
		), nil
	})

	handler := mcpserver.NewStreamableHTTPServer(mcpSrv)
	return &MCPServer{
		authToken: authToken,
		handler:   withOptionalBearerAuth(handler, authToken),
	}
}

func (s *MCPServer) Handler() http.Handler {
	return s.handler
}

func (s *MCPServer) Start(addr string) error {
	if s.handler == nil {
		return fmt.Errorf("mcp handler is not configured")
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen mcp http: %w", err)
	}
	if err := validateMCPListenerSecurity(listener.Addr(), s.authToken); err != nil {
		_ = listener.Close()
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp", s.handler)
	mux.Handle("/mcp/", s.handler)

	s.listener = listener
	s.server = &http.Server{Handler: mux}
	go func() {
		if err := s.server.Serve(listener); err != nil && err != http.ErrServerClosed {
			slog.Error("mcp server error", "error", err)
		}
	}()
	slog.Info("mcp server started", "listen", addr, "path", "/mcp", "auth", s.authToken != "")
	return nil
}

func (s *MCPServer) Stop(ctx context.Context) error {
	if s.server == nil {
		return nil
	}
	return s.server.Shutdown(ctx)
}

func withOptionalBearerAuth(next http.Handler, authToken string) http.Handler {
	if authToken == "" {
		return next
	}
	expected := "Bearer " + authToken
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != expected {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func workspaceRefFromArguments(req mcp.CallToolRequest) (JobWorkspaceRef, error) {
	workspaceRef := JobWorkspaceRef{
		RepoPath:     strings.TrimSpace(req.GetString("repo_path", "")),
		WorktreePath: strings.TrimSpace(req.GetString("worktree_path", "")),
		Branch:       strings.TrimSpace(req.GetString("branch", "")),
	}
	args, ok := req.Params.Arguments.(map[string]any)
	if !ok {
		return workspaceRef, nil
	}
	raw, ok := args["workspace_ref"]
	if !ok || raw == nil {
		return workspaceRef, nil
	}
	workspaceMap, ok := raw.(map[string]any)
	if !ok {
		return JobWorkspaceRef{}, fmt.Errorf("workspace_ref must be an object")
	}
	if workspaceRef.RepoPath == "" {
		workspaceRef.RepoPath = stringArg(workspaceMap, "repo_path")
	}
	if workspaceRef.WorktreePath == "" {
		workspaceRef.WorktreePath = stringArg(workspaceMap, "worktree_path")
	}
	if workspaceRef.Branch == "" {
		workspaceRef.Branch = stringArg(workspaceMap, "branch")
	}
	return workspaceRef, nil
}

func stringArg(values map[string]any, key string) string {
	value, ok := values[key]
	if !ok {
		return ""
	}
	asString, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(asString)
}

func validateMCPListenerSecurity(addr net.Addr, authToken string) error {
	if strings.TrimSpace(authToken) != "" {
		return nil
	}
	tcpAddr, ok := addr.(*net.TCPAddr)
	if !ok {
		return nil
	}
	ip, ok := netip.AddrFromSlice(tcpAddr.IP)
	if !ok {
		return nil
	}
	if !ip.IsValid() || ip.IsUnspecified() || !ip.IsLoopback() {
		return fmt.Errorf("mcp auth_token is required when binding to non-loopback address %q", addr.String())
	}
	return nil
}

func jobResponse(job *Job) map[string]any {
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
		payload["events"] = job.Events
	}
	if job.StartedAt != nil {
		payload["started_at"] = job.StartedAt.Format(time.RFC3339Nano)
	}
	if job.FinishedAt != nil {
		payload["finished_at"] = job.FinishedAt.Format(time.RFC3339Nano)
	}
	return payload
}

func decodeStructuredResult(result *mcp.CallToolResult, out any) error {
	if result == nil {
		return fmt.Errorf("result is nil")
	}
	if result.IsError {
		return errors.New(resultToText(result))
	}
	data, err := json.Marshal(result.StructuredContent)
	if err != nil {
		return fmt.Errorf("marshal structured content: %w", err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("unmarshal structured content: %w", err)
	}
	return nil
}

func resultToText(result *mcp.CallToolResult) string {
	if result == nil {
		return ""
	}
	var text strings.Builder
	for _, content := range result.Content {
		textContent, ok := content.(mcp.TextContent)
		if !ok {
			continue
		}
		if text.Len() > 0 {
			text.WriteString("\n")
		}
		text.WriteString(textContent.Text)
	}
	return text.String()
}

func workspaceResponse(
	status string,
	repoPath string,
	branchName string,
	worktreePath string,
) map[string]any {
	payload := map[string]any{
		"status":        status,
		"worktree_path": worktreePath,
	}
	if repoPath != "" {
		payload["repo_path"] = repoPath
	}
	if branchName != "" {
		payload["branch_name"] = branchName
	}
	return payload
}
