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
	if base := filepath.Base(paths[0]); !strings.HasPrefix(base, "note-") || !strings.HasSuffix(base, ".txt") {
		t.Fatalf("basename = %q, want note-*.txt", base)
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

func TestSaveFilesToDisk_SanitizesNamesAndAvoidsOverwrite(t *testing.T) {
	workDir := t.TempDir()
	paths := SaveFilesToDisk(workDir, []FileAttachment{
		{
			MimeType: "text/plain",
			Data:     []byte("alpha"),
			FileName: "../../secret.txt",
		},
		{
			MimeType: "text/plain",
			Data:     []byte("beta"),
			FileName: "../../secret.txt",
		},
	})

	if len(paths) != 2 {
		t.Fatalf("paths len = %d, want 2", len(paths))
	}
	if paths[0] == paths[1] {
		t.Fatalf("paths should be unique, got %q", paths[0])
	}
	for _, p := range paths {
		rel, err := filepath.Rel(filepath.Join(workDir, ".cc-connect", "attachments"), p)
		if err != nil {
			t.Fatalf("Rel: %v", err)
		}
		if strings.HasPrefix(rel, "..") {
			t.Fatalf("path escaped attachment dir: %q", p)
		}
		if base := filepath.Base(p); strings.Contains(base, "/") || strings.Contains(base, "\\") {
			t.Fatalf("unsanitized basename: %q", base)
		}
	}

	data0, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatalf("ReadFile first: %v", err)
	}
	data1, err := os.ReadFile(paths[1])
	if err != nil {
		t.Fatalf("ReadFile second: %v", err)
	}
	if string(data0) == string(data1) {
		t.Fatalf("expected distinct file contents, got %q and %q", string(data0), string(data1))
	}
}

func TestSaveImagesToDisk_SanitizesNamesAndAddsExtensions(t *testing.T) {
	workDir := t.TempDir()
	paths := SaveImagesToDisk(workDir, []ImageAttachment{
		{
			MimeType: "image/jpeg",
			Data:     []byte("jpegdata"),
			FileName: "../../screen shot.jpg",
		},
		{
			MimeType: "image/png",
			Data:     []byte("pngdata"),
		},
	})

	if len(paths) != 2 {
		t.Fatalf("paths len = %d, want 2", len(paths))
	}
	if !strings.HasSuffix(paths[0], ".jpg") {
		t.Fatalf("first path = %q, want .jpg suffix", paths[0])
	}
	if !strings.HasSuffix(paths[1], ".png") {
		t.Fatalf("second path = %q, want .png suffix", paths[1])
	}
	for _, p := range paths {
		rel, err := filepath.Rel(filepath.Join(workDir, ".cc-connect", "images"), p)
		if err != nil {
			t.Fatalf("Rel: %v", err)
		}
		if strings.HasPrefix(rel, "..") {
			t.Fatalf("path escaped image dir: %q", p)
		}
	}
}
