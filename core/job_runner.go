package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

const maxJobSummaryLen = 240
const maxBufferedJobTextBytes = 64 * 1024

type codedJobError struct {
	code string
	msg  string
}

func (e *codedJobError) Error() string {
	return e.msg
}

func (e *codedJobError) Code() string {
	return e.code
}

type jobTextBuffer struct {
	parts        []string
	total        int
	truncated    bool
	droppedBytes int
}

func (b *jobTextBuffer) Append(text string) {
	if text == "" {
		return
	}
	b.parts = append(b.parts, text)
	b.total += len(text)
	for b.total > maxBufferedJobTextBytes && len(b.parts) > 0 {
		overflow := b.total - maxBufferedJobTextBytes
		head := b.parts[0]
		if len(head) <= overflow {
			b.parts = b.parts[1:]
			b.total -= len(head)
			b.droppedBytes += len(head)
			b.truncated = true
			continue
		}
		for overflow < len(head) && !utf8.RuneStart(head[overflow]) {
			overflow++
		}
		b.parts[0] = head[overflow:]
		b.total -= overflow
		b.droppedBytes += overflow
		b.truncated = true
		break
	}
}

func (b *jobTextBuffer) String() string {
	joined := strings.Join(b.parts, "")
	output := strings.TrimSpace(joined)
	if !b.truncated {
		return output
	}
	if output == "" {
		return fmt.Sprintf("[truncated %d bytes]", b.droppedBytes)
	}
	return fmt.Sprintf("[truncated %d bytes]\n%s", b.droppedBytes, output)
}

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
	onEvent func(JobEvent),
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
	var textBuffer jobTextBuffer

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case event, ok := <-agentSession.Events():
			if !ok {
				output := textBuffer.String()
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
					textBuffer.Append(event.Content)
					if onEvent != nil {
						onEvent(JobEvent{
							Type:      string(EventText),
							Content:   event.Content,
							SessionID: sessionID,
							CreatedAt: time.Now().UTC(),
						})
					}
				}
			case EventThinking:
				if event.Content != "" && onEvent != nil {
					onEvent(JobEvent{
						Type:      string(EventThinking),
						Content:   event.Content,
						SessionID: sessionID,
						CreatedAt: time.Now().UTC(),
					})
				}
			case EventToolUse:
				if onEvent != nil {
					onEvent(JobEvent{
						Type:      string(EventToolUse),
						Content:   event.Content,
						ToolName:  event.ToolName,
						ToolInput: event.ToolInput,
						SessionID: sessionID,
						CreatedAt: time.Now().UTC(),
					})
				}
			case EventToolResult:
				if onEvent != nil {
					content := event.ToolResult
					if content == "" {
						content = event.Content
					}
					onEvent(JobEvent{
						Type:      string(EventToolResult),
						Content:   content,
						ToolName:  event.ToolName,
						SessionID: sessionID,
						CreatedAt: time.Now().UTC(),
					})
				}
			case EventResult:
				output := event.Content
				if output == "" {
					output = textBuffer.String()
				}
				if onEvent != nil {
					onEvent(JobEvent{
						Type:      string(EventResult),
						Content:   output,
						SessionID: sessionID,
						CreatedAt: time.Now().UTC(),
					})
				}
				return &JobResult{
					Output:    output,
					Summary:   summarizeJobOutput(output),
					SessionID: sessionID,
				}, nil
			case EventError:
				errValue := event.Error
				if errValue == nil {
					errValue = fmt.Errorf("job runner received agent error event")
				}
				if onEvent != nil {
					onEvent(JobEvent{
						Type:      string(EventError),
						Content:   errValue.Error(),
						SessionID: sessionID,
						CreatedAt: time.Now().UTC(),
					})
				}
				return nil, errValue
			case EventPermissionRequest:
				if onEvent != nil {
					onEvent(JobEvent{
						Type:      string(EventPermissionRequest),
						Content:   event.Content,
						ToolName:  event.ToolName,
						ToolInput: event.ToolInput,
						SessionID: sessionID,
						CreatedAt: time.Now().UTC(),
					})
				}
				return nil, &codedJobError{
					code: JobErrorCodePermissionRequired,
					msg: fmt.Sprintf(
						"job runner cannot satisfy permission request for tool %q",
						event.ToolName,
					),
				}
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
	sessionKey := "echo-job-" + strings.TrimSpace(jobID)
	if sessionKey == "echo-job-" {
		sessionKey = "echo-job-anonymous"
	}
	if err := validateJobWorkspace(req.WorkspaceRef); err != nil {
		return nil, err
	}
	return e.startAgentSession(ctx, "", e.jobEnv(req, jobID, sessionKey))
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
	if summary == "" {
		return ""
	}
	lines := strings.Split(summary, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			summary = line
			break
		}
	}
	if len([]rune(summary)) <= maxJobSummaryLen {
		return summary
	}
	runes := []rune(summary)
	return "..." + string(runes[len(runes)-(maxJobSummaryLen-3):])
}
