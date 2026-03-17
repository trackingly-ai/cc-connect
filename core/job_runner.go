package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const maxJobSummaryLen = 240

type engineJobRunner struct {
	engine *Engine
}

func (e *Engine) JobRunner() JobRunner {
	return engineJobRunner{engine: e}
}

func (r engineJobRunner) Run(
	ctx context.Context,
	req JobRequest,
	jobID string,
) (*JobResult, error) {
	agentSession, err := r.engine.StartJobSession(ctx, req, jobID)
	if err != nil {
		return nil, fmt.Errorf("start job session: %w", err)
	}
	defer agentSession.Close()

	if err := agentSession.Send(req.Prompt, nil, nil); err != nil {
		return nil, fmt.Errorf("send job prompt: %w", err)
	}

	sessionID := agentSession.CurrentSessionID()
	var textParts []string

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case event, ok := <-agentSession.Events():
			if !ok {
				output := strings.TrimSpace(strings.Join(textParts, ""))
				if output == "" {
					return nil, fmt.Errorf("job session exited without result")
				}
				return &JobResult{
					Output:    output,
					Summary:   summarizeJobOutput(output),
					SessionID: sessionID,
				}, nil
			}

			if event.SessionID != "" {
				sessionID = event.SessionID
			}

			switch event.Type {
			case EventText:
				if event.Content != "" {
					textParts = append(textParts, event.Content)
				}
			case EventResult:
				output := event.Content
				if output == "" {
					output = strings.TrimSpace(strings.Join(textParts, ""))
				}
				return &JobResult{
					Output:    output,
					Summary:   summarizeJobOutput(output),
					SessionID: sessionID,
				}, nil
			case EventError:
				if event.Error != nil {
					return nil, event.Error
				}
				return nil, fmt.Errorf("job runner received agent error event")
			case EventPermissionRequest:
				return nil, fmt.Errorf(
					"job runner cannot satisfy permission request for tool %q",
					event.ToolName,
				)
			}
		}
	}
}

func (e *Engine) startAgentSession(
	ctx context.Context,
	sessionID string,
	envVars []string,
) (AgentSession, error) {
	e.agentStartMu.Lock()
	defer e.agentStartMu.Unlock()

	if inj, ok := e.agent.(SessionEnvInjector); ok {
		inj.SetSessionEnv(envVars)
	}
	return e.agent.StartSession(ctx, sessionID)
}

func (e *Engine) StartJobSession(
	ctx context.Context,
	req JobRequest,
	jobID string,
) (AgentSession, error) {
	sessionID := "echo-job-" + strings.TrimSpace(jobID)
	if sessionID == "echo-job-" {
		sessionID = "echo-job-anonymous"
	}
	if err := validateJobWorkspace(req.WorkspaceRef); err != nil {
		return nil, err
	}
	return e.startAgentSession(ctx, sessionID, e.jobEnv(req, jobID, sessionID))
}

func (e *Engine) sessionEnv(sessionKey string) []string {
	envVars := []string{
		"CC_PROJECT=" + e.name,
		"CC_SESSION_KEY=" + sessionKey,
	}
	if exePath, err := os.Executable(); err == nil {
		binDir := filepath.Dir(exePath)
		if curPath := os.Getenv("PATH"); curPath != "" {
			envVars = append(
				envVars,
				"PATH="+binDir+string(filepath.ListSeparator)+curPath,
			)
		} else {
			envVars = append(envVars, "PATH="+binDir)
		}
	}
	return envVars
}

func (e *Engine) jobEnv(req JobRequest, jobID string, sessionID string) []string {
	envVars := e.sessionEnv(sessionID)
	if req.TaskID != "" {
		envVars = append(envVars, "CC_TASK_ID="+req.TaskID)
		envVars = append(envVars, "ECHO_TASK_ID="+req.TaskID)
	}
	if strings.TrimSpace(jobID) != "" {
		envVars = append(envVars, "ECHO_JOB_ID="+strings.TrimSpace(jobID))
	}
	if req.WorkspaceRef.RepoPath != "" {
		envVars = append(envVars, "CC_REPO_PATH="+req.WorkspaceRef.RepoPath)
	}
	if req.WorkspaceRef.WorktreePath != "" {
		envVars = append(envVars, "CC_WORKTREE_PATH="+req.WorkspaceRef.WorktreePath)
	}
	if req.WorkspaceRef.Branch != "" {
		envVars = append(envVars, "CC_BRANCH="+req.WorkspaceRef.Branch)
	}
	return envVars
}

func validateJobWorkspace(workspaceRef JobWorkspaceRef) error {
	worktreePath := strings.TrimSpace(workspaceRef.WorktreePath)
	if worktreePath == "" {
		return nil
	}
	info, err := os.Stat(worktreePath)
	if err != nil {
		return fmt.Errorf("stat worktree_path: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("worktree_path is not a directory: %s", worktreePath)
	}
	return nil
}

func summarizeJobOutput(output string) string {
	summary := strings.TrimSpace(output)
	if len([]rune(summary)) <= maxJobSummaryLen {
		return summary
	}
	runes := []rune(summary)
	return string(runes[:maxJobSummaryLen-3]) + "..."
}
