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

func TestLoadAllowsHeadlessProjectsWhenRoleConfigured(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	content := `
[[projects]]
name = "gemini-reviewer"
role = "reviewer"

[projects.agent]
type = "gemini"
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Projects) != 1 {
		t.Fatalf("unexpected project count: %d", len(cfg.Projects))
	}
	if cfg.Projects[0].Role != "reviewer" {
		t.Fatalf("unexpected project role: %q", cfg.Projects[0].Role)
	}
	if len(cfg.Projects[0].Platforms) != 0 {
		t.Fatalf("expected headless role project without platforms, got %#v", cfg.Projects[0].Platforms)
	}
}

func TestLoadRejectsHeadlessProjectsForNonReviewerRoleWithoutEcho(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	content := `
[[projects]]
name = "misc-worker"
role = "support"

[projects.agent]
type = "gemini"
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if _, err := Load(configPath); err == nil {
		t.Fatal("expected headless non-reviewer role to be rejected without echo")
	}
}

func TestLoadParsesProjectSkillDirs(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	content := `
[[projects]]
name = "demo"
skill_dirs = ["/tmp/tester-skills", "/tmp/shared-skills"]
include_default_skill_dirs = true

[projects.agent]
type = "claudecode"

[[projects.platforms]]
type = "telegram"
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(cfg.Projects) != 1 {
		t.Fatalf("unexpected project count: %d", len(cfg.Projects))
	}
	proj := cfg.Projects[0]
	if len(proj.SkillDirs) != 2 || proj.SkillDirs[0] != "/tmp/tester-skills" || proj.SkillDirs[1] != "/tmp/shared-skills" {
		t.Fatalf("unexpected skill dirs: %#v", proj.SkillDirs)
	}
	if proj.IncludeDefaultSkillDirs == nil || !*proj.IncludeDefaultSkillDirs {
		t.Fatalf("expected include_default_skill_dirs=true, got %#v", proj.IncludeDefaultSkillDirs)
	}
}

func TestLoadParsesProjectLevelRole(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	content := `
[[projects]]
name = "tester"
role = "test_engineer"

[projects.agent]
type = "claudecode"

[[projects.platforms]]
type = "telegram"
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(cfg.Projects) != 1 {
		t.Fatalf("unexpected project count: %d", len(cfg.Projects))
	}
	if cfg.Projects[0].Role != "test_engineer" {
		t.Fatalf("unexpected project role: %q", cfg.Projects[0].Role)
	}
}

func TestLoadRejectsRelativeProjectSkillDir(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	content := `
[[projects]]
name = "demo"
skill_dirs = ["relative/skills"]

[projects.agent]
type = "claudecode"

[[projects.platforms]]
type = "telegram"
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if _, err := Load(configPath); err == nil {
		t.Fatal("expected relative skill dir validation error")
	}
}

func TestLoadTrimsProjectSkillDirsBeforeValidation(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	content := `
[[projects]]
name = "demo"
skill_dirs = ["  /tmp/tester-skills  "]

[projects.agent]
type = "claudecode"

[[projects.platforms]]
type = "telegram"
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(cfg.Projects) != 1 || len(cfg.Projects[0].SkillDirs) != 1 {
		t.Fatalf("unexpected parsed skill dirs: %#v", cfg.Projects)
	}
	if cfg.Projects[0].SkillDirs[0] != "  /tmp/tester-skills  " {
		t.Fatalf("Load should preserve original TOML value, got %q", cfg.Projects[0].SkillDirs[0])
	}
}

func TestLoadParsesAgentUpdatesConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	content := minimalConfigTOML + `
[agent_updates]
enabled = true
timeout_secs = 900

[agent_updates.codex]
strategy = "package_manager"
update_command = "npm install -g @openai/codex@latest"
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AgentUpdates.Enabled == nil || !*cfg.AgentUpdates.Enabled {
		t.Fatalf("expected agent_updates.enabled=true, got %#v", cfg.AgentUpdates.Enabled)
	}
	if cfg.AgentUpdates.TimeoutSecs != 900 {
		t.Fatalf("timeout_secs = %d, want 900", cfg.AgentUpdates.TimeoutSecs)
	}
	if cfg.AgentUpdates.Codex.Strategy != "package_manager" {
		t.Fatalf("codex strategy = %q", cfg.AgentUpdates.Codex.Strategy)
	}
	if cfg.AgentUpdates.Codex.UpdateCommand != "npm install -g @openai/codex@latest" {
		t.Fatalf("codex update command = %q", cfg.AgentUpdates.Codex.UpdateCommand)
	}
}

func TestLoadRejectsInvalidAgentUpdateStrategy(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	content := minimalConfigTOML + `
[agent_updates]
enabled = true

[agent_updates.gemini]
strategy = "magic"
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if _, err := Load(configPath); err == nil {
		t.Fatal("expected invalid agent update strategy to be rejected")
	}
}

func TestLoadParsesAgentUpdateRunAuthorization(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	content := minimalConfigTOML + `
[agent_updates]
enabled = true
run_enabled = true
allowed_user_ids = ["ou_admin", " ou_backup "]
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AgentUpdates.RunEnabled == nil || !*cfg.AgentUpdates.RunEnabled {
		t.Fatalf("run_enabled = %#v, want true", cfg.AgentUpdates.RunEnabled)
	}
	if len(cfg.AgentUpdates.AllowedUserIDs) != 2 {
		t.Fatalf("allowed_user_ids = %#v", cfg.AgentUpdates.AllowedUserIDs)
	}
}

func TestLoadRejectsAgentUpdateRunWithoutAllowedUsers(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	content := minimalConfigTOML + `
[agent_updates]
enabled = true
run_enabled = true
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if _, err := Load(configPath); err == nil {
		t.Fatal("expected missing allowed_user_ids validation error")
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
