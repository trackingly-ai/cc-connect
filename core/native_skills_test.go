package core

import (
	"context"
	"os"
	"os/exec"
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

func TestPrepareManagedSkillEnvDisabledKeepsOriginalEnv(t *testing.T) {
	agent := &managedSkillTestAgent{workDir: t.TempDir()}
	e := NewEngine("demo", agent, nil, "", LangEnglish)
	e.SetDataDir(t.TempDir())
	e.SetManagedSkillConfig(false, nil)

	env, err := e.prepareManagedSkillEnv("feishu:chat:user", []string{"CC_PROJECT=demo"})
	if err != nil {
		t.Fatalf("prepareManagedSkillEnv: %v", err)
	}
	if got := SessionWorkDirFromEnv(env, ""); got != "" {
		t.Fatalf("unexpected managed workspace override: %q", got)
	}
	if extra := SessionExtraDirsFromEnv(env); len(extra) != 0 {
		t.Fatalf("unexpected extra dirs: %#v", extra)
	}
}

func TestPrepareManagedSkillEnvMaterializesIntoExistingWorktreeAndAddsGitExclude(t *testing.T) {
	dataDir := t.TempDir()
	repoDir := initNativeSkillGitRepo(t)
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

	defaultWorkDir := t.TempDir()
	agent := &managedSkillTestAgent{workDir: defaultWorkDir}
	e := NewEngine("demo", agent, nil, "", LangEnglish)
	e.SetDataDir(dataDir)
	e.SetManagedSkillConfig(true, []string{skillRoot})

	env, err := e.prepareManagedSkillEnv("echo-job-1", []string{"CC_WORKTREE_PATH=" + repoDir})
	if err != nil {
		t.Fatalf("prepareManagedSkillEnv: %v", err)
	}
	if got := SessionWorkDirFromEnv(env, ""); got != repoDir {
		t.Fatalf("worktree override = %q, want %q", got, repoDir)
	}
	target := filepath.Join(repoDir, ".agents", "skills", "flaky-pytest")
	if _, err := os.Lstat(target); err != nil {
		t.Fatalf("expected materialized skill at %s: %v", target, err)
	}
	extraDirs := SessionExtraDirsFromEnv(env)
	if len(extraDirs) != 1 || extraDirs[0] != defaultWorkDir {
		t.Fatalf("extra dirs = %#v, want [%q]", extraDirs, defaultWorkDir)
	}
	skillsMeta := SessionNativeSkillsMetaFromEnv(env)
	if skillsMeta["workspace_path"] != repoDir {
		t.Fatalf("workspace_path = %#v, want %q", skillsMeta["workspace_path"], repoDir)
	}
	if skillsMeta["native_target_kind"] != ".agents/skills" {
		t.Fatalf("native_target_kind = %#v", skillsMeta["native_target_kind"])
	}
	if skillsMeta["skill_count"] != float64(1) {
		t.Fatalf("skill_count = %#v", skillsMeta["skill_count"])
	}
	excludeData, err := os.ReadFile(filepath.Join(repoDir, ".git", "info", "exclude"))
	if err != nil {
		t.Fatalf("read git exclude: %v", err)
	}
	exclude := string(excludeData)
	for _, want := range []string{".agents/skills/", ".cc-connect/skills-manifest.json"} {
		if !strings.Contains(exclude, want) {
			t.Fatalf("exclude missing %q: %s", want, exclude)
		}
	}
}

func TestPrepareManagedSkillEnvMaterializesIntoExistingNonGitWorktree(t *testing.T) {
	dataDir := t.TempDir()
	worktreeDir := t.TempDir()
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

	env, err := e.prepareManagedSkillEnv("echo-job-2", []string{"CC_WORKTREE_PATH=" + worktreeDir})
	if err != nil {
		t.Fatalf("prepareManagedSkillEnv: %v", err)
	}
	if got := SessionWorkDirFromEnv(env, ""); got != worktreeDir {
		t.Fatalf("worktree override = %q, want %q", got, worktreeDir)
	}
	if _, err := os.Lstat(filepath.Join(worktreeDir, ".agents", "skills", "flaky-pytest")); err != nil {
		t.Fatalf("expected materialized skill in non-git worktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(worktreeDir, ".git", "info", "exclude")); !os.IsNotExist(err) {
		t.Fatalf("expected no git exclude in non-git worktree, got err=%v", err)
	}
}

func TestPrepareManagedSkillEnvWritesExcludeForGitWorktree(t *testing.T) {
	dataDir := t.TempDir()
	repoDir := initNativeSkillGitRepo(t)
	worktreeDir := filepath.Join(t.TempDir(), "worktree")
	if out, err := exec.Command("git", "-C", repoDir, "worktree", "add", "-b", "echo-skill-test", worktreeDir).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v (%s)", err, string(out))
	}
	gitFileData, err := os.ReadFile(filepath.Join(worktreeDir, ".git"))
	if err != nil {
		t.Fatalf("read worktree .git file: %v", err)
	}
	if !strings.Contains(string(gitFileData), "gitdir:") {
		t.Fatalf("expected worktree .git file indirection, got %q", string(gitFileData))
	}
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

	env, err := e.prepareManagedSkillEnv("echo-job-3", []string{"CC_WORKTREE_PATH=" + worktreeDir})
	if err != nil {
		t.Fatalf("prepareManagedSkillEnv: %v", err)
	}
	if got := SessionWorkDirFromEnv(env, ""); got != worktreeDir {
		t.Fatalf("worktree override = %q, want %q", got, worktreeDir)
	}

	excludePathBytes, err := exec.Command("git", "-C", worktreeDir, "rev-parse", "--git-path", "info/exclude").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse --git-path info/exclude: %v (%s)", err, string(excludePathBytes))
	}
	excludePath := strings.TrimSpace(string(excludePathBytes))
	excludeData, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read worktree exclude: %v", err)
	}
	exclude := string(excludeData)
	for _, want := range []string{".agents/skills/", ".cc-connect/skills-manifest.json"} {
		if !strings.Contains(exclude, want) {
			t.Fatalf("exclude missing %q: %s", want, exclude)
		}
	}
}

func TestNativeSkillTargetDirIsCaseInsensitive(t *testing.T) {
	got := nativeSkillTargetDir("Codex", "/tmp/work")
	want := "/tmp/work/.agents/skills"
	if got != want {
		t.Fatalf("nativeSkillTargetDir() = %q, want %q", got, want)
	}
}

func TestBuildNativeSkillsMetaIncludesManifestAndSkillNames(t *testing.T) {
	entries := []nativeSkillEntry{
		{Name: "playwright-smoke", Rel: "playwright-smoke"},
		{Name: "env-readiness", Rel: "env-readiness"},
	}
	meta := buildNativeSkillsMeta(
		"echo-test-engineer-claude",
		"claudecode",
		"/tmp/worktree",
		entries,
		"abc123",
		[]string{"/skills/shared", "/skills/tester"},
	)
	if got := meta["project"]; got != "echo-test-engineer-claude" {
		t.Fatalf("project = %#v", got)
	}
	if got := meta["native_target_kind"]; got != ".claude/skills" {
		t.Fatalf("native_target_kind = %#v", got)
	}
	if got := meta["manifest_path"]; got != "/tmp/worktree/.cc-connect/skills-manifest.json" {
		t.Fatalf("manifest_path = %#v", got)
	}
	names, ok := meta["skill_names"].([]string)
	if !ok {
		t.Fatalf("skill_names type = %T", meta["skill_names"])
	}
	if len(names) != 2 || names[0] != "env-readiness" || names[1] != "playwright-smoke" {
		t.Fatalf("skill_names = %#v", names)
	}
}

func initNativeSkillGitRepo(t *testing.T) string {
	t.Helper()

	repoDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repoDir, "init", "--initial-branch=main").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, string(out))
	}
	if out, err := exec.Command("git", "-C", repoDir, "config", "user.email", "fixture@example.com").CombinedOutput(); err != nil {
		t.Fatalf("git config user.email: %v (%s)", err, string(out))
	}
	if out, err := exec.Command("git", "-C", repoDir, "config", "user.name", "Fixture User").CombinedOutput(); err != nil {
		t.Fatalf("git config user.name: %v (%s)", err, string(out))
	}
	readmePath := filepath.Join(repoDir, "README.md")
	if err := os.WriteFile(readmePath, []byte("# fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repoDir, "add", "README.md").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v (%s)", err, string(out))
	}
	if out, err := exec.Command("git", "-C", repoDir, "commit", "-m", "initial commit").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v (%s)", err, string(out))
	}
	return repoDir
}
