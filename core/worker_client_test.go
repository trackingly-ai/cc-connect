package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/config"
	"github.com/gorilla/websocket"
)

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
				_ = conn.WriteJSON(map[string]any{"type": "hello_ack"})
			case "register_host":
				host, _ := payload["host"].(map[string]any)
				hostID, _ := host["id"].(string)
				receivedHostID = hostID
				_ = conn.WriteJSON(map[string]any{"type": "host_registered", "host_id": hostID, "status": "online"})
			case "register_agents":
				agents, _ := payload["agents"].([]any)
				receivedAgentCount = len(agents)
				_ = conn.WriteJSON(map[string]any{"type": "agents_registered", "host_id": "host-local", "count": len(agents)})
			case "heartbeat":
				_ = conn.WriteJSON(map[string]any{"type": "heartbeat_ack", "host_id": "host-local", "agent_count": 1})
			default:
				_ = conn.WriteJSON(map[string]any{"type": "error", "message": "unexpected"})
			}
		}
	}))
	defer server.Close()

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
		mu.Unlock()
		if count >= 4 {
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
}
