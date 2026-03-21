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

const defaultEchoOrgID = "coding-team"

type WorkerClient struct {
	serverURL         string
	authToken         string
	hostID            string
	label             string
	tags              []string
	heartbeatInterval time.Duration
	agents            []workerAgentRegistration

	mu     sync.Mutex
	conn   *websocket.Conn
	doneCh chan struct{}
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
) (*WorkerClient, error) {
	serverURL := strings.TrimSpace(echoCfg.ServerURL)
	if serverURL == "" {
		return nil, nil
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
		doneCh:            make(chan struct{}),
	}, nil
}

func (c *WorkerClient) Start(ctx context.Context) {
	go c.run(ctx)
}

func (c *WorkerClient) Stop(ctx context.Context) error {
	c.closeConn()
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

	if err := c.sendAndExpect(conn, map[string]any{
		"type":           "hello",
		"worker_version": "cc-connect",
	}, "hello_ack"); err != nil {
		return err
	}
	if err := c.sendAndExpect(conn, map[string]any{
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
	if err := c.sendAndExpect(conn, map[string]any{
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
		case <-ticker.C:
			if err := c.sendAndExpect(conn, map[string]any{
				"type":      "heartbeat",
				"host_id":   c.hostID,
				"agent_ids": c.agentIDs(),
			}, "heartbeat_ack"); err != nil {
				return err
			}
		}
	}
}

func (c *WorkerClient) sendAndExpect(conn *websocket.Conn, payload map[string]any, wantType string) error {
	if err := conn.WriteJSON(payload); err != nil {
		return fmt.Errorf("write %s: %w", payload["type"], err)
	}
	var response map[string]any
	if err := conn.ReadJSON(&response); err != nil {
		return fmt.Errorf("read %s response: %w", payload["type"], err)
	}
	gotType, _ := response["type"].(string)
	if gotType == "error" {
		raw, _ := json.Marshal(response)
		return fmt.Errorf("echo worker gateway error: %s", strings.TrimSpace(string(raw)))
	}
	if gotType != wantType {
		return fmt.Errorf("unexpected worker gateway response type %q, want %q", gotType, wantType)
	}
	return nil
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
