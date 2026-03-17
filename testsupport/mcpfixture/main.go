package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

const startupRecoveryRemoteRunningSmoke = "startup-recovery-remote-running-smoke"

func main() {
	listen := flag.String("listen", "127.0.0.1:9820", "listen address")
	project := flag.String("project", "echo", "project name to register")
	flag.Parse()

	dataDir, err := os.MkdirTemp("", "cc-connect-mcpfixture-*")
	if err != nil {
		log.Fatalf("mkdir temp: %v", err)
	}
	defer os.RemoveAll(dataDir)

	jobMgr, err := core.NewJobManager(dataDir)
	if err != nil {
		log.Fatalf("job manager: %v", err)
	}

	engine := core.NewEngine(*project, &fixtureAgent{}, nil, "", core.LangEnglish)
	jobMgr.RegisterRunner(*project, engine.JobRunner())

	mcpSrv := core.NewMCPServer(jobMgr, "")
	if err := mcpSrv.Start(*listen); err != nil {
		log.Fatalf("start mcp server: %v", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := mcpSrv.Stop(shutdownCtx); err != nil {
			log.Printf("shutdown mcp server: %v", err)
		}
	}()

	fmt.Printf("ready %s\n", *listen)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
}

type fixtureAgent struct {
	env []string
}

func (a *fixtureAgent) Name() string { return "fixture-agent" }

func (a *fixtureAgent) SetSessionEnv(env []string) {
	a.env = append([]string(nil), env...)
}

func (a *fixtureAgent) StartSession(
	_ context.Context,
	_ string,
) (core.AgentSession, error) {
	return &fixtureSession{
		events: make(chan core.Event, 2),
		env:    append([]string(nil), a.env...),
	}, nil
}

func (a *fixtureAgent) ListSessions(
	_ context.Context,
) ([]core.AgentSessionInfo, error) {
	return nil, nil
}

func (a *fixtureAgent) Stop() error { return nil }

type fixtureSession struct {
	events chan core.Event
	env    []string
}

func (s *fixtureSession) Send(prompt string, _ []core.ImageAttachment, _ []core.FileAttachment) error {
	go func() {
		time.Sleep(renderDelay(prompt))
		s.events <- core.Event{
			Type:    core.EventResult,
			Content: renderEchoResult(prompt, s.env),
		}
		close(s.events)
	}()
	return nil
}

func (s *fixtureSession) RespondPermission(
	_ string,
	_ core.PermissionResult,
) error {
	return nil
}

func (s *fixtureSession) Events() <-chan core.Event { return s.events }

func (s *fixtureSession) CurrentSessionID() string { return "fixture-session" }

func (s *fixtureSession) Alive() bool { return true }

func (s *fixtureSession) Close() error { return nil }

func renderEchoResult(prompt string, env []string) string {
	workspaceSummary := renderWorkspaceSummary(env)
	if strings.Contains(prompt, "human-resolution-smoke") {
		switch {
		case strings.Contains(prompt, "continuation_of_task_id"):
			return mustRenderEchoResult(map[string]string{
				"status":  "completed",
				"summary": "fixture manager continuation completed" + workspaceSummary,
			})
		case strings.Contains(prompt, "continuation_context"), strings.Contains(prompt, "human_resolution"):
			return mustRenderEchoResult(map[string]string{
				"status":  "completed",
				"summary": "fixture resumed after human resolution" + workspaceSummary,
			})
		default:
			return mustRenderEchoResult(map[string]string{
				"status":            "needs_human_input",
				"summary":           "fixture requires human input" + workspaceSummary,
				"blocked_reason":    "Need operator approval for rollout",
				"continuation_hint": "Approve the Friday rollout window",
			})
		}
	}
	if strings.Contains(prompt, "startup-recovery-remote-failure-smoke") {
		return mustRenderEchoResult(map[string]string{
			"status":  "failed",
			"summary": "fixture failed during startup recovery" + workspaceSummary,
		})
	}
	if strings.Contains(prompt, "- Type: review") {
		reviewOutcome := promptHint(prompt, "review_outcome_hint")
		if reviewOutcome == "changes_requested" {
			return mustRenderEchoResult(map[string]string{
				"status":  "changes_requested",
				"summary": "fixture requested changes" + workspaceSummary,
			})
		}
		sourceBranch := promptHint(prompt, "source_branch_hint")
		if sourceBranch == "" {
			sourceBranch = "fixture/review-branch"
		}
		sourceWorkspaceID := promptHint(prompt, "source_workspace_id_hint")
		if sourceWorkspaceID == "" {
			sourceWorkspaceID = "fixture-workspace-1"
		}
		return mustRenderEchoResult(map[string]string{
			"status":              "approved",
			"summary":             "fixture approved review" + workspaceSummary,
			"source_branch":       sourceBranch,
			"source_workspace_id": sourceWorkspaceID,
		})
	}
	if strings.Contains(prompt, "- Type: land") {
		sourceBranch := promptLineValue(prompt, "- Source Branch: ")
		if strings.Contains(sourceBranch, "fixture/fail-land/") {
			return mustRenderEchoResult(map[string]string{
				"status":  "failed",
				"summary": "fixture land failed due to rebase conflict" + workspaceSummary,
			})
		}
		return mustRenderEchoResult(map[string]string{
			"status":  "completed",
			"summary": "fixture landed approved change" + workspaceSummary,
		})
	}
	return mustRenderEchoResult(map[string]string{
		"status":  "completed",
		"summary": "fixture completed: " + prompt + workspaceSummary,
	})
}

func renderDelay(prompt string) time.Duration {
	// The recovery smoke needs the remote job to stay in "running" long enough
	// for Echo to observe and refresh the heartbeat before the fixture completes.
	if strings.Contains(prompt, startupRecoveryRemoteRunningSmoke) {
		return 500 * time.Millisecond
	}
	return 25 * time.Millisecond
}

func mustRenderEchoResult(payload map[string]string) string {
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		panic(fmt.Sprintf("marshal echo result: %v", err))
	}
	return "```echo-result\n" + string(payloadBytes) + "\n```"
}

func renderWorkspaceSummary(env []string) string {
	repoPath := lookupEnv(env, "CC_REPO_PATH")
	worktreePath := lookupEnv(env, "CC_WORKTREE_PATH")
	branch := lookupEnv(env, "CC_BRANCH")
	if repoPath == "" && worktreePath == "" && branch == "" {
		return ""
	}
	return fmt.Sprintf(
		" [repo=%s worktree=%s branch=%s]",
		repoPath,
		worktreePath,
		branch,
	)
}

func lookupEnv(env []string, key string) string {
	prefix := key + "="
	for i := len(env) - 1; i >= 0; i-- {
		entry := env[i]
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix)
		}
	}
	return ""
}

func promptHint(prompt string, key string) string {
	pattern := regexp.MustCompile(fmt.Sprintf(`['"]%s['"]:\s*['"]([^'"]+)['"]`, regexp.QuoteMeta(key)))
	matches := pattern.FindStringSubmatch(prompt)
	if len(matches) != 2 {
		return ""
	}
	return matches[1]
}

func promptLineValue(prompt string, prefix string) string {
	for _, line := range strings.Split(prompt, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return ""
}
