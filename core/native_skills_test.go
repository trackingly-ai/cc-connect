package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type managedSkillTestAgent struct {
	workDir string
}

func (a *managedSkillTestAgent) Name() string { return "codex" }
func (a *managedSkillTestAgent) StartSession(_ context.Context, _ string) (AgentSession, error) {
	return nil, nil
}
func (a *managedSkillTestAgent) ListSessions(_ context.Context) ([]AgentSessionInfo, error) {
	return nil, nil
}
func (a *managedSkillTestAgent) Stop() error           { return nil }
func (a *managedSkillTestAgent) SetWorkDir(dir string) { a.workDir = dir }
func (a *managedSkillTestAgent) GetWorkDir() string    { return a.workDir }

func TestNativeSkillFingerprintStableSort(t *testing.T) {
	entries := []nativeSkillEntry{
		{Name: "regression-check", Rel: "regression-check", SkillMDMD5: "bbb"},
		{Name: "flaky-pytest", Rel: "flaky-pytest", SkillMDMD5: "aaa"},
	}
	got := nativeSkillFingerprint(entries)
	if got == "" {
		t.Fatal("expected non-empty fingerprint")
	}
	rev := []nativeSkillEntry{entries[1], entries[0]}
	if got2 := nativeSkillFingerprint(rev); got2 != got {
		t.Fatalf("fingerprint should be order-insensitive after sort: %q != %q", got2, got)
	}
}

func TestPrepareManagedSkillEnvCreatesWorkspaceAndExtraDir(t *testing.T) {
	dataDir := t.TempDir()
	skillRoot := filepath.Join(t.TempDir(), "skills")
	if err := os.MkdirAll(filepath.Join(skillRoot, "flaky-pytest"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(skillRoot, "flaky-pytest", "SKILL.md"),
		[]byte("---\nname: flaky-pytest\ndescription: test\n---\nBody"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	agent := &managedSkillTestAgent{workDir: t.TempDir()}
	e := NewEngine("demo", agent, nil, "", LangEnglish)
	e.SetDataDir(dataDir)
	e.SetManagedSkillConfig(true, []string{skillRoot})

	env, err := e.prepareManagedSkillEnv("feishu:chat:user", []string{"CC_PROJECT=demo"})
	if err != nil {
		t.Fatalf("prepareManagedSkillEnv: %v", err)
	}

	workDir := SessionWorkDirFromEnv(env, "")
	if workDir == "" {
		t.Fatal("expected managed workspace path in env")
	}
	expectedPrefix := filepath.Join(dataDir, "workspaces", "demo")
	if !strings.HasPrefix(workDir, expectedPrefix) {
		t.Fatalf("managed workspace %q does not start with %q", workDir, expectedPrefix)
	}

	target := filepath.Join(workDir, ".agents", "skills", "flaky-pytest")
	if _, err := os.Lstat(target); err != nil {
		t.Fatalf("expected materialized skill at %s: %v", target, err)
	}

	extraDirs := SessionExtraDirsFromEnv(env)
	if len(extraDirs) != 1 || extraDirs[0] != agent.workDir {
		t.Fatalf("extra dirs = %#v, want [%q]", extraDirs, agent.workDir)
	}
}
