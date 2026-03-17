package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveFilesToDiskAndAppendFileRefs(t *testing.T) {
	workDir := t.TempDir()
	paths := SaveFilesToDisk(workDir, []FileAttachment{{
		MimeType: "text/plain",
		Data:     []byte("hello"),
		FileName: "note.txt",
	}})

	if len(paths) != 1 {
		t.Fatalf("paths len = %d, want 1", len(paths))
	}
	if filepath.Base(paths[0]) != "note.txt" {
		t.Fatalf("basename = %q, want note.txt", filepath.Base(paths[0]))
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("file contents = %q, want %q", string(data), "hello")
	}

	prompt := AppendFileRefs("analyze this", paths)
	if !strings.Contains(prompt, "analyze this") || !strings.Contains(prompt, paths[0]) {
		t.Fatalf("prompt = %q, want original text and file path", prompt)
	}
}
