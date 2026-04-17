package claudecode

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
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
		"",
		"357aaa11-8e6b-49dd-a6e3-b954cad2ca8d",
		"bypassPermissions",
		[]string{"Read", "Bash"},
		nil,
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

func TestAgentSetReasoningLevelAutoClearsExplicitEffort(t *testing.T) {
	agent := &Agent{reasoningLevel: "high"}
	agent.SetReasoningLevel("auto")

	if got := agent.GetReasoningLevel(); got != "" {
		t.Fatalf("GetReasoningLevel() = %q, want empty default", got)
	}

	args := buildClaudeSessionArgs("sonnet", agent.GetReasoningLevel(), "", "default", nil, nil)
	if slices.Contains(args, "--effort") {
		t.Fatalf("args unexpectedly included --effort after auto reset: %v", args)
	}
}

func TestBuildClaudeSessionArgsOmitsOptionalValues(t *testing.T) {
	args := buildClaudeSessionArgs("", "", "", "default", nil, nil)

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
	args := buildClaudeSessionArgs("", "", "echo-job-job_bb5b7c746d3b2298", "default", nil, nil)

	if slices.Contains(args, "--resume") {
		t.Fatalf("args unexpectedly resumed synthetic session key: %v", args)
	}
}

func TestBuildClaudeSessionArgsIncludesEffort(t *testing.T) {
	args := buildClaudeSessionArgs("sonnet", "high", "", "default", nil, nil)

	idx := slices.Index(args, "--effort")
	if idx < 0 || idx+1 >= len(args) || args[idx+1] != "high" {
		t.Fatalf("args missing --effort high: %v", args)
	}
}

func TestBuildClaudeSessionArgsOmitsEffortForAuto(t *testing.T) {
	args := buildClaudeSessionArgs("sonnet", "", "", "default", nil, nil)

	if slices.Contains(args, "--effort") {
		t.Fatalf("args unexpectedly included --effort: %v", args)
	}
}

func TestNormalizeClaudeImage_ReencodesToJPEG(t *testing.T) {
	raw := mustPNGBytes(t)

	img := normalizeClaudeImage(core.ImageAttachment{
		MimeType: "image/png",
		Data:     raw,
		FileName: "tiny.png",
	})

	if img.MimeType != "image/jpeg" {
		t.Fatalf("mime = %q, want image/jpeg", img.MimeType)
	}
	if img.FileName != "tiny.jpg" {
		t.Fatalf("fileName = %q, want tiny.jpg", img.FileName)
	}
	if len(img.Data) == 0 {
		t.Fatal("normalized image data is empty")
	}
	if len(img.Data) < 2 || img.Data[0] != 0xff || img.Data[1] != 0xd8 {
		t.Fatalf("normalized image is not a JPEG")
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

func TestLooksLikeHEICByHeaderAndName(t *testing.T) {
	img := core.ImageAttachment{
		FileName: "cover.jpg",
		Data:     append([]byte{0, 0, 0, 24}, append([]byte("ftypheic"), []byte("rest")...)...),
	}
	if !looksLikeHEIC(img) {
		t.Fatal("expected HEIC header to be detected")
	}

	img = core.ImageAttachment{FileName: "photo.heic"}
	if !looksLikeHEIC(img) {
		t.Fatal("expected .heic filename to be detected")
	}
}

func TestNormalizeClaudeImage_UsesHEICConverter(t *testing.T) {
	orig := heicImageConverter
	t.Cleanup(func() { heicImageConverter = orig })

	heicImageConverter = func(img core.ImageAttachment) (core.ImageAttachment, error) {
		return core.ImageAttachment{
			MimeType: "image/jpeg",
			Data:     []byte{0xff, 0xd8, 0xff, 0xd9},
			FileName: "cover.jpg",
		}, nil
	}

	img := normalizeClaudeImage(core.ImageAttachment{
		MimeType: "image/jpeg",
		FileName: "cover.jpg",
		Data:     append([]byte{0, 0, 0, 24}, append([]byte("ftypheic"), []byte("rest")...)...),
	})

	if img.MimeType != "image/jpeg" {
		t.Fatalf("mime = %q, want image/jpeg", img.MimeType)
	}
	if img.FileName != "cover.jpg" {
		t.Fatalf("fileName = %q, want cover.jpg", img.FileName)
	}
	if len(img.Data) < 2 || img.Data[0] != 0xff || img.Data[1] != 0xd8 {
		t.Fatalf("expected converted JPEG, got %#v", img.Data)
	}
}

func TestClaudeSend_UsesLocalFilePathsOnlyForImages(t *testing.T) {
	workDir := t.TempDir()
	var buf bytes.Buffer
	cs := &claudeSession{
		workDir: workDir,
		stdin:   nopWriteCloser{Writer: &buf},
	}
	cs.alive.Store(true)

	raw := mustPNGBytes(t)

	if err := cs.Send("check this", []core.ImageAttachment{{
		MimeType: "image/png",
		Data:     raw,
		FileName: "tiny.png",
	}}, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}

	payload := buf.String()
	if strings.Contains(payload, "\"type\":\"image\"") || strings.Contains(payload, "\"base64\"") {
		t.Fatalf("payload unexpectedly contains image block: %s", payload)
	}
	if !strings.Contains(payload, ".cc-connect/images") || !strings.Contains(payload, ".jpg") {
		t.Fatalf("payload missing local image path: %s", payload)
	}

	matches, err := filepath.Glob(filepath.Join(workDir, ".cc-connect", "images", "*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("saved image count = %d, want 1", len(matches))
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read saved image: %v", err)
	}
	if len(data) < 2 || data[0] != 0xff || data[1] != 0xd8 {
		t.Fatalf("saved image is not JPEG")
	}
}

type nopWriteCloser struct {
	io.Writer
}

func (n nopWriteCloser) Close() error { return nil }

func mustPNGBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.NRGBA{R: 255, G: 0, B: 0, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	return buf.Bytes()
}

func TestContainsClaudeImageError(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{name: "plain text", content: "all good", want: false},
		{name: "could not process image", content: "API Error: 400 {\"message\":\"Could not process image\"}", want: true},
		{name: "api error unrelated", content: "API Error: 400 bad request", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := containsClaudeImageError(tc.content); got != tc.want {
				t.Fatalf("containsClaudeImageError(%q) = %v, want %v", tc.content, got, tc.want)
			}
		})
	}
}

func TestRecordToolUseKeepsRecentTail(t *testing.T) {
	var cs claudeSession
	for i := 0; i < 20; i++ {
		cs.recordToolUse("Bash", strings.Repeat("x", i+1))
	}

	got := cs.snapshotRecentTools()
	if len(got) != 12 {
		t.Fatalf("recent tool count = %d, want 12", len(got))
	}
	if !strings.Contains(got[0], strings.Repeat("x", 9)) {
		t.Fatalf("unexpected oldest retained entry: %q", got[0])
	}
	if !strings.Contains(got[len(got)-1], strings.Repeat("x", 20)) {
		t.Fatalf("unexpected newest retained entry: %q", got[len(got)-1])
	}
}
