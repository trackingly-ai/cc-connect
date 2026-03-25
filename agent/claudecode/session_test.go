package claudecode

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestReadJSONLines_AllowsLargeJSONLRecords(t *testing.T) {
	large := bytes.Repeat([]byte("x"), 2*1024*1024)
	line, err := json.Marshal(map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []map[string]any{{
				"type": "text",
				"text": string(large),
			}},
		},
	})
	if err != nil {
		t.Fatalf("marshal large json line: %v", err)
	}

	input := append(line, '\n')
	calls := 0
	if err := readJSONLines(bytes.NewReader(input), func(got []byte) error {
		calls++
		if !bytes.Equal(got, line) {
			t.Fatalf("readJSONLines returned unexpected payload length=%d want=%d", len(got), len(line))
		}
		return nil
	}); err != nil {
		t.Fatalf("readJSONLines returned error: %v", err)
	}

	if calls != 1 {
		t.Fatalf("readJSONLines calls = %d, want 1", calls)
	}
}

func TestBuildClaudeSessionArgsUsesExpectedDefaults(t *testing.T) {
	args := buildClaudeSessionArgs(
		"claude-sonnet",
		"357aaa11-8e6b-49dd-a6e3-b954cad2ca8d",
		"bypassPermissions",
		[]string{"Read", "Bash"},
	)

	requiredPairs := [][2]string{
		{"--output-format", "stream-json"},
		{"--input-format", "stream-json"},
		{"--permission-prompt-tool", "stdio"},
		{"--permission-mode", "bypassPermissions"},
		{"--resume", "357aaa11-8e6b-49dd-a6e3-b954cad2ca8d"},
		{"--model", "claude-sonnet"},
		{"--allowedTools", "Read,Bash"},
	}
	for _, pair := range requiredPairs {
		idx := slices.Index(args, pair[0])
		if idx < 0 || idx+1 >= len(args) || args[idx+1] != pair[1] {
			t.Fatalf("args missing %q=%q: %v", pair[0], pair[1], args)
		}
	}

	if !slices.Contains(args, "--verbose") {
		t.Fatalf("args missing --verbose: %v", args)
	}
}

func TestBuildClaudeSessionArgsOmitsOptionalValues(t *testing.T) {
	args := buildClaudeSessionArgs("", "", "default", nil)

	for _, forbidden := range []string{
		"--permission-mode",
		"--resume",
		"--model",
		"--allowedTools",
	} {
		if slices.Contains(args, forbidden) {
			t.Fatalf("args unexpectedly included %q: %v", forbidden, args)
		}
	}
}

func TestBuildClaudeSessionArgsDoesNotResumeSyntheticSessionKey(t *testing.T) {
	args := buildClaudeSessionArgs("", "echo-job-job_bb5b7c746d3b2298", "default", nil)

	if slices.Contains(args, "--resume") {
		t.Fatalf("args unexpectedly resumed synthetic session key: %v", args)
	}
}

func TestNormalizeClaudeImage_ReencodesToPNG(t *testing.T) {
	// Valid 1x1 RGBA PNG that reproduces Claude's "Could not process image" error
	// when sent without normalization.
	raw := []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d,
		0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4, 0x89, 0x00, 0x00, 0x00,
		0x0d, 0x49, 0x44, 0x41, 0x54, 0x08, 0x63, 0xf8, 0xff, 0xff, 0xff, 0x19,
		0x00, 0x09, 0xfb, 0x03, 0xff, 0xda, 0x13, 0x7c, 0x37, 0x00, 0x00, 0x00,
		0x00, 0x49, 0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
	}

	img := normalizeClaudeImage(core.ImageAttachment{
		MimeType: "image/png",
		Data:     raw,
		FileName: "tiny.png",
	})

	if img.MimeType != "image/png" {
		t.Fatalf("mime = %q, want image/png", img.MimeType)
	}
	if img.FileName != "tiny.png" {
		t.Fatalf("fileName = %q, want tiny.png", img.FileName)
	}
	if len(img.Data) == 0 {
		t.Fatal("normalized image data is empty")
	}
	if !bytes.HasPrefix(img.Data, []byte{0x89, 0x50, 0x4e, 0x47}) {
		t.Fatalf("normalized image is not a PNG")
	}
}

func TestNormalizeClaudeImage_KeepsUndecodableBytes(t *testing.T) {
	img := normalizeClaudeImage(core.ImageAttachment{
		MimeType: "image/webp",
		Data:     []byte("not-an-image"),
		FileName: "bad.webp",
	})

	if img.MimeType != "image/webp" || !bytes.Equal(img.Data, []byte("not-an-image")) || !strings.HasSuffix(img.FileName, ".webp") {
		t.Fatalf("unexpected fallback image: %#v", img)
	}
}
