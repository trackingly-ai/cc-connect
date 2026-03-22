package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadParsesEchoConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	content := minimalConfigTOML + `
[echo]
server_url = "https://echo.example.com"
auth_token = "echo-token"
host_id = "host-local"
label = "Local Host"
org_id = "coding-team"
tags = ["local", "primary"]
heartbeat_interval_sec = 12
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Echo.ServerURL != "https://echo.example.com" {
		t.Fatalf("unexpected echo server url: %q", cfg.Echo.ServerURL)
	}
	if cfg.Echo.AuthToken != "echo-token" {
		t.Fatalf("unexpected echo auth token: %q", cfg.Echo.AuthToken)
	}
	if cfg.Echo.HostID != "host-local" {
		t.Fatalf("unexpected echo host id: %q", cfg.Echo.HostID)
	}
	if cfg.Echo.HeartbeatIntervalSec != 12 {
		t.Fatalf("unexpected heartbeat interval: %d", cfg.Echo.HeartbeatIntervalSec)
	}
	if len(cfg.Echo.Tags) != 2 || cfg.Echo.Tags[0] != "local" || cfg.Echo.Tags[1] != "primary" {
		t.Fatalf("unexpected echo tags: %#v", cfg.Echo.Tags)
	}
}

func TestLoadAllowsHeadlessProjectsWhenEchoEnabled(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	content := `
[echo]
server_url = "https://echo.example.com"

[[projects]]
name = "echo-manager-claude"

[projects.agent]
type = "claudecode"
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Echo.ServerURL != "https://echo.example.com" {
		t.Fatalf("unexpected echo server url: %q", cfg.Echo.ServerURL)
	}
	if len(cfg.Projects) != 1 {
		t.Fatalf("unexpected project count: %d", len(cfg.Projects))
	}
	if len(cfg.Projects[0].Platforms) != 0 {
		t.Fatalf("expected headless project without platforms, got %#v", cfg.Projects[0].Platforms)
	}
}

const minimalConfigTOML = `
[[projects]]
name = "demo"

[projects.agent]
type = "codex"

[[projects.platforms]]
type = "telegram"
`
