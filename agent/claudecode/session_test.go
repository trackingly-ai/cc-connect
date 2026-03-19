package claudecode

import (
	"bytes"
	"encoding/json"
	"slices"
	"testing"
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

func TestBuildClaudeSessionArgsUsesHeadlessIsolatedDefaults(t *testing.T) {
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
		{"--agent", "general-purpose"},
		{"--agents", "{}"},
		{"--setting-sources", "project,local"},
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

	if !slices.Contains(args, "--disable-slash-commands") {
		t.Fatalf("args missing --disable-slash-commands: %v", args)
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
