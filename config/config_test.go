package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSetsDefaultMCPListenAddress(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(configPath, []byte(minimalConfigTOML), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.MCP.Listen != "127.0.0.1:9820" {
		t.Fatalf("unexpected MCP listen default: %q", cfg.MCP.Listen)
	}
}

func TestLoadParsesMCPConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	content := minimalConfigTOML + `
[mcp]
enabled = true
listen = "127.0.0.1:19000"
auth_token = "secret-token"
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if !cfg.MCP.Enabled {
		t.Fatal("expected MCP to be enabled")
	}
	if cfg.MCP.Listen != "127.0.0.1:19000" {
		t.Fatalf("unexpected MCP listen: %q", cfg.MCP.Listen)
	}
	if cfg.MCP.AuthToken != "secret-token" {
		t.Fatalf("unexpected MCP auth token: %q", cfg.MCP.AuthToken)
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
