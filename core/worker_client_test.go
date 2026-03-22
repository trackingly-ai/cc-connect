package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/config"
	"github.com/gorilla/websocket"
)

type workerTestRunner struct{}

func (workerTestRunner) Run(
	ctx context.Context,
	req JobRequest,
	jobID string,
	onEvent func(JobEvent),
) (*JobResult, error) {
	if onEvent != nil {
		onEvent(JobEvent{
			Type:      string(EventThinking),
			Content:   "planning",
			SessionID: "session-1",
			CreatedAt: time.Now().UTC(),
		})
		onEvent(JobEvent{
			Type:      string(EventText),
			Content:   "partial output",
			SessionID: "session-1",
			CreatedAt: time.Now().UTC(),
		})
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(700 * time.Millisecond):
	}
	return &JobResult{
		Output:    "final output",
		Summary:   summarizeJobOutput("final output"),
		SessionID: "session-1",
	}, nil
}

type workerBlockingRunner struct{}

func (workerBlockingRunner) Run(
	ctx context.Context,
	req JobRequest,
	jobID string,
	onEvent func(JobEvent),
) (*JobResult, error) {
	if onEvent != nil {
		onEvent(JobEvent{
			Type:      string(EventThinking),
			Content:   "waiting",
			SessionID: "session-blocking",
			CreatedAt: time.Now().UTC(),
		})
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestBuildWorkerAgentRegistrations(t *testing.T) {
	enabled := true
	agents, err := buildWorkerAgentRegistrations(
		config.EchoConfig{OrgID: "coding-team"},
		[]config.ProjectConfig{
			{
				Name: "echo-manager-claude",
				Agent: config.AgentConfig{
					Type: "claudecode",
				},
			},
			{
				Name: "custom-review",
				Agent: config.AgentConfig{
					Type: "qoder",
				},
				Echo: config.EchoProjectConfig{
					Enabled:        &enabled,
					Role:           "reviewer",
					AgentID:        "agent-reviewer-qoder-9",
					PromptTemplate: "prompts/coding/reviewer.md",
				},
			},
		},
	)
	if err != nil {
		t.Fatalf("buildWorkerAgentRegistrations: %v", err)
	}
	if len(agents) != 2 {
		t.Fatalf("unexpected agent count: %d", len(agents))
	}
	if agents[0].ID != "agent-manager-claudecode-1" {
		t.Fatalf("unexpected inferred agent id: %q", agents[0].ID)
	}
	if agents[0].Role != "manager" {
		t.Fatalf("unexpected inferred role: %q", agents[0].Role)
	}
	if agents[1].ID != "agent-reviewer-qoder-9" {
		t.Fatalf("unexpected explicit agent id: %q", agents[1].ID)
	}
}

func TestWorkerGatewayWebSocketURL(t *testing.T) {
	got, err := workerGatewayWebSocketURL("https://echo.example.com")
	if err != nil {
		t.Fatalf("workerGatewayWebSocketURL: %v", err)
	}
	if got != "wss://echo.example.com/api/v1/workers/ws" {
		t.Fatalf("unexpected ws url: %q", got)
	}
}

func TestWorkerClientRegistersAndHeartbeats(t *testing.T) {
	upgrader := websocket.Upgrader{}
	var mu sync.Mutex
	receivedTypes := []string{}
	receivedHostID := ""
	receivedAgentCount := 0
	taskProgressSeen := false
	taskResultSeen := false

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer conn.Close()
		for {
			var payload map[string]any
			if err := conn.ReadJSON(&payload); err != nil {
				return
			}
			msgType, _ := payload["type"].(string)
			mu.Lock()
			receivedTypes = append(receivedTypes, msgType)
			mu.Unlock()
			switch msgType {
			case "hello":
				_ = conn.WriteJSON(map[string]any{"type": "hello_ack", "request_id": payload["request_id"]})
			case "register_host":
				host, _ := payload["host"].(map[string]any)
				hostID, _ := host["id"].(string)
				receivedHostID = hostID
				_ = conn.WriteJSON(map[string]any{"type": "host_registered", "request_id": payload["request_id"], "host_id": hostID, "status": "online"})
			case "register_agents":
				agents, _ := payload["agents"].([]any)
				receivedAgentCount = len(agents)
				_ = conn.WriteJSON(map[string]any{"type": "agents_registered", "request_id": payload["request_id"], "host_id": "host-local", "count": len(agents)})
				_ = conn.WriteJSON(map[string]any{
					"type":          "assign_task",
					"request_id":    "assign-1",
					"agent_project": "echo-manager-claude",
					"task_id":       "task-1",
					"prompt":        "do the task",
					"timeout_sec":   30,
					"workspace_ref": map[string]any{},
				})
			case "heartbeat":
				_ = conn.WriteJSON(map[string]any{"type": "heartbeat_ack", "request_id": payload["request_id"], "host_id": "host-local", "agent_count": 1})
			case "task_progress":
				mu.Lock()
				taskProgressSeen = true
				mu.Unlock()
			case "task_result":
				mu.Lock()
				taskResultSeen = true
				mu.Unlock()
			default:
				_ = conn.WriteJSON(map[string]any{"type": "error", "message": "unexpected"})
			}
		}
	}))
	defer server.Close()

	jobMgr, err := NewJobManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewJobManager: %v", err)
	}
	jobMgr.RegisterProject("echo-manager-claude", "claudecode", workerTestRunner{})

	client, err := NewWorkerClient(
		config.EchoConfig{
			ServerURL:            server.URL,
			HostID:               "host-local",
			HeartbeatIntervalSec: 1,
		},
		[]config.ProjectConfig{
			{
				Name: "echo-manager-claude",
				Agent: config.AgentConfig{
					Type: "claudecode",
				},
			},
		},
		jobMgr,
	)
	if err != nil {
		t.Fatalf("NewWorkerClient: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	client.Start(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		count := len(receivedTypes)
		progressSeen := taskProgressSeen
		resultSeen := taskResultSeen
		mu.Unlock()
		if count >= 5 && progressSeen && resultSeen {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	if err := client.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if receivedHostID != "host-local" {
		t.Fatalf("unexpected host id: %q", receivedHostID)
	}
	if receivedAgentCount != 1 {
		t.Fatalf("unexpected registered agent count: %d", receivedAgentCount)
	}
	if len(receivedTypes) < 4 {
		t.Fatalf("expected hello/register_host/register_agents/heartbeat, got %#v", receivedTypes)
	}
	if !taskProgressSeen {
		t.Fatal("expected task_progress to be sent")
	}
	if !taskResultSeen {
		t.Fatal("expected task_result to be sent")
	}
}

func TestWorkerClientCancelsAssignedTask(t *testing.T) {
	upgrader := websocket.Upgrader{}
	var mu sync.Mutex
	cancelAckSeen := false
	cancelledResultSeen := false

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer conn.Close()
		for {
			var payload map[string]any
			if err := conn.ReadJSON(&payload); err != nil {
				return
			}
			msgType, _ := payload["type"].(string)
			switch msgType {
			case "hello":
				_ = conn.WriteJSON(map[string]any{"type": "hello_ack", "request_id": payload["request_id"]})
			case "register_host":
				host := payload["host"].(map[string]any)
				_ = conn.WriteJSON(map[string]any{"type": "host_registered", "request_id": payload["request_id"], "host_id": host["id"], "status": "online"})
			case "register_agents":
				_ = conn.WriteJSON(map[string]any{"type": "agents_registered", "request_id": payload["request_id"], "host_id": "host-local", "count": 1})
				_ = conn.WriteJSON(map[string]any{
					"type":          "assign_task",
					"request_id":    "assign-1",
					"agent_project": "echo-manager-claude",
					"task_id":       "task-2",
					"prompt":        "block forever",
					"timeout_sec":   30,
					"workspace_ref": map[string]any{},
				})
			case "task_assigned":
				jobID := payload["job"].(map[string]any)["id"]
				_ = conn.WriteJSON(map[string]any{
					"type":       "cancel_task",
					"request_id": "cancel-1",
					"job_id":     jobID,
				})
			case "cancel_ack":
				mu.Lock()
				cancelAckSeen = true
				mu.Unlock()
			case "task_result":
				job := payload["job"].(map[string]any)
				if status, _ := job["status"].(string); status == JobStatusCancelled {
					mu.Lock()
					cancelledResultSeen = true
					mu.Unlock()
					return
				}
			case "heartbeat":
				_ = conn.WriteJSON(map[string]any{"type": "heartbeat_ack", "request_id": payload["request_id"], "host_id": "host-local", "agent_count": 1})
			}
		}
	}))
	defer server.Close()

	jobMgr, err := NewJobManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewJobManager: %v", err)
	}
	jobMgr.RegisterProject("echo-manager-claude", "claudecode", workerBlockingRunner{})

	client, err := NewWorkerClient(
		config.EchoConfig{
			ServerURL:            server.URL,
			HostID:               "host-local",
			HeartbeatIntervalSec: 1,
		},
		[]config.ProjectConfig{
			{
				Name: "echo-manager-claude",
				Agent: config.AgentConfig{
					Type: "claudecode",
				},
			},
		},
		jobMgr,
	)
	if err != nil {
		t.Fatalf("NewWorkerClient: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	client.Start(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := cancelAckSeen && cancelledResultSeen
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	if err := client.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !cancelAckSeen {
		t.Fatal("expected cancel_ack to be sent")
	}
	if !cancelledResultSeen {
		t.Fatal("expected cancelled task_result to be sent")
	}
}

func TestWorkerClientHandlesWorkspaceAndRepoRPCs(t *testing.T) {
	upgrader := websocket.Upgrader{}
	var mu sync.Mutex
	workspaceReadySeen := false
	checkoutReadySeen := false
	repoFileWrittenSeen := false
	workspaceCleanedSeen := false

	repoPath := initGitRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "worktree")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer conn.Close()
		for {
			var payload map[string]any
			if err := conn.ReadJSON(&payload); err != nil {
				return
			}
			msgType, _ := payload["type"].(string)
			switch msgType {
			case "hello":
				_ = conn.WriteJSON(map[string]any{"type": "hello_ack", "request_id": payload["request_id"]})
			case "register_host":
				host := payload["host"].(map[string]any)
				_ = conn.WriteJSON(map[string]any{"type": "host_registered", "request_id": payload["request_id"], "host_id": host["id"], "status": "online"})
			case "register_agents":
				_ = conn.WriteJSON(map[string]any{"type": "agents_registered", "request_id": payload["request_id"], "host_id": "host-local", "count": 1})
				_ = conn.WriteJSON(map[string]any{
					"type":          "setup_workspace",
					"request_id":    "setup-1",
					"repo_path":     repoPath,
					"base_branch":   "main",
					"branch_name":   "echo/task-1",
					"worktree_path": worktreePath,
				})
			case "workspace_ready":
				mu.Lock()
				workspaceReadySeen = true
				mu.Unlock()
				_ = conn.WriteJSON(map[string]any{
					"type":           "ensure_repo_checkout",
					"request_id":     "checkout-1",
					"repo_url":       repoPath,
					"repo_path":      filepath.Join(t.TempDir(), "checkout"),
					"default_branch": "main",
				})
			case "repo_checkout_ready":
				mu.Lock()
				checkoutReadySeen = true
				mu.Unlock()
				_ = conn.WriteJSON(map[string]any{
					"type":          "write_repo_file",
					"request_id":    "write-1",
					"repo_path":     repoPath,
					"relative_path": "docs/design.md",
					"content":       "# hello\n",
				})
			case "repo_file_written":
				mu.Lock()
				repoFileWrittenSeen = true
				mu.Unlock()
				_ = conn.WriteJSON(map[string]any{
					"type":          "cleanup_workspace",
					"request_id":    "cleanup-1",
					"worktree_path": worktreePath,
				})
			case "workspace_cleaned":
				mu.Lock()
				workspaceCleanedSeen = true
				mu.Unlock()
				return
			case "heartbeat":
				_ = conn.WriteJSON(map[string]any{"type": "heartbeat_ack", "request_id": payload["request_id"], "host_id": "host-local", "agent_count": 1})
			}
		}
	}))
	defer server.Close()

	jobMgr, err := NewJobManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewJobManager: %v", err)
	}
	jobMgr.RegisterProject("echo-manager-claude", "claudecode", workerTestRunner{})

	client, err := NewWorkerClient(
		config.EchoConfig{
			ServerURL:            server.URL,
			HostID:               "host-local",
			HeartbeatIntervalSec: 1,
		},
		[]config.ProjectConfig{
			{
				Name: "echo-manager-claude",
				Agent: config.AgentConfig{
					Type: "claudecode",
				},
			},
		},
		jobMgr,
	)
	if err != nil {
		t.Fatalf("NewWorkerClient: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	client.Start(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := workspaceReadySeen && checkoutReadySeen && repoFileWrittenSeen && workspaceCleanedSeen
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	if err := client.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !workspaceReadySeen || !checkoutReadySeen || !repoFileWrittenSeen || !workspaceCleanedSeen {
		t.Fatalf(
			"expected all rpc acknowledgements, got workspace=%v checkout=%v file=%v cleanup=%v",
			workspaceReadySeen,
			checkoutReadySeen,
			repoFileWrittenSeen,
			workspaceCleanedSeen,
		)
	}
	if _, err := os.Stat(filepath.Join(repoPath, "docs", "design.md")); err != nil {
		t.Fatalf("expected repo file to be written: %v", err)
	}
}
