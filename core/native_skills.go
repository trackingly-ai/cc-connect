package core

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type nativeSkillEntry struct {
	Name       string `json:"name"`
	Rel        string `json:"rel"`
	SourceDir  string `json:"source_dir"`
	SkillMDMD5 string `json:"skill_md_md5"`
}

type nativeSkillsManifest struct {
	Project          string             `json:"project"`
	SessionKeyHash   string             `json:"session_key_hash"`
	SkillFingerprint string             `json:"skill_fingerprint"`
	WorkspacePath    string             `json:"workspace_path"`
	NativeTarget     string             `json:"native_target"`
	Entries          []nativeSkillEntry `json:"entries"`
}

func nativeSkillEntriesFromRoots(roots []string) ([]nativeSkillEntry, error) {
	var entries []nativeSkillEntry
	seen := make(map[string]struct{})
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		dirEntries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, entry := range dirEntries {
			if !entry.IsDir() {
				continue
			}
			rel := entry.Name()
			skillDir := filepath.Join(root, rel)
			mdPath := filepath.Join(skillDir, "SKILL.md")
			data, err := os.ReadFile(mdPath)
			if err != nil {
				continue
			}
			skill := parseSkillMD(rel, string(data), root)
			if skill == nil {
				continue
			}
			lower := strings.ToLower(skill.Name)
			if _, ok := seen[lower]; ok {
				continue
			}
			sum := md5.Sum(data)
			entries = append(entries, nativeSkillEntry{
				Name:       skill.Name,
				Rel:        filepath.ToSlash(rel),
				SourceDir:  skillDir,
				SkillMDMD5: hex.EncodeToString(sum[:]),
			})
			seen[lower] = struct{}{}
		}
	}
	return entries, nil
}

func nativeSkillFingerprint(entries []nativeSkillEntry) string {
	normalized := append([]nativeSkillEntry(nil), entries...)
	sort.Slice(normalized, func(i, j int) bool {
		if normalized[i].Name != normalized[j].Name {
			return normalized[i].Name < normalized[j].Name
		}
		return normalized[i].Rel < normalized[j].Rel
	})
	var sb strings.Builder
	for i, entry := range normalized {
		fmt.Fprintf(&sb, "skill[%d].rel=%s\n", i, entry.Rel)
		fmt.Fprintf(&sb, "skill[%d].name=%s\n", i, entry.Name)
		fmt.Fprintf(&sb, "skill[%d].skill_md_md5=%s\n", i, entry.SkillMDMD5)
	}
	sum := md5.Sum([]byte(sb.String()))
	return hex.EncodeToString(sum[:])
}

func sessionKeyHash(sessionKey string) string {
	sum := md5.Sum([]byte(strings.TrimSpace(sessionKey)))
	return hex.EncodeToString(sum[:])
}

func nativeSkillTargetDir(agentName, workspacePath string) string {
	switch strings.ToLower(strings.TrimSpace(agentName)) {
	case "claudecode":
		return filepath.Join(workspacePath, ".claude", "skills")
	case "codex", "gemini":
		return filepath.Join(workspacePath, ".agents", "skills")
	case "qoder":
		return filepath.Join(workspacePath, ".qoder", "skills")
	default:
		return ""
	}
}

func ensureManagedWorkspace(
	dataDir string,
	projectName string,
	agentName string,
	sessionKey string,
	roots []string,
) (string, error) {
	entries, err := nativeSkillEntriesFromRoots(roots)
	if err != nil {
		return "", err
	}
	fingerprint := nativeSkillFingerprint(entries)
	keyHash := sessionKeyHash(sessionKey)
	workspacePath := filepath.Join(dataDir, "workspaces", projectName, keyHash, fingerprint)
	targetDir := nativeSkillTargetDir(agentName, workspacePath)
	if targetDir == "" {
		return "", fmt.Errorf("agent %q does not have a managed native skill target", agentName)
	}
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return "", err
	}
	if err := clearDir(targetDir); err != nil {
		return "", err
	}
	for _, entry := range entries {
		dst := filepath.Join(targetDir, entry.Name)
		if err := os.Symlink(entry.SourceDir, dst); err != nil {
			if err := copyDir(entry.SourceDir, dst); err != nil {
				return "", err
			}
		}
	}
	manifestDir := filepath.Join(workspacePath, ".cc-connect")
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		return "", err
	}
	manifest := nativeSkillsManifest{
		Project:          projectName,
		SessionKeyHash:   keyHash,
		SkillFingerprint: fingerprint,
		WorkspacePath:    workspacePath,
		NativeTarget:     targetDir,
		Entries:          entries,
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(manifestDir, "skills-manifest.json"), data, 0o644); err != nil {
		return "", err
	}
	return workspacePath, nil
}

func clearDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func copyDir(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if err := copyDir(srcPath, dstPath); err != nil {
				return err
			}
			continue
		}
		data, err := os.ReadFile(srcPath)
		if err != nil {
			return err
		}
		if err := os.WriteFile(dstPath, data, info.Mode()); err != nil {
			return err
		}
	}
	return nil
}
