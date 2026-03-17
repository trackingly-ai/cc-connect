package claudecode

import "testing"

func TestNormalizePermissionMode(t *testing.T) {
	tests := map[string]string{
		"":                   "default",
		"default":            "default",
		"edit":               "acceptEdits",
		"accept-edits":       "acceptEdits",
		"accept_edits":       "acceptEdits",
		"plan":               "plan",
		"yolo":               "bypassPermissions",
		"auto":               "bypassPermissions",
		"bypass-permissions": "bypassPermissions",
		"bypass_permissions": "bypassPermissions",
		"dontAsk":            "dontAsk",
		"dont-ask":           "dontAsk",
		"dont_ask":           "dontAsk",
		"  DONTASK  ":        "dontAsk",
		"something-unknown":  "default",
	}

	for input, want := range tests {
		if got := normalizePermissionMode(input); got != want {
			t.Fatalf("normalizePermissionMode(%q) = %q, want %q", input, got, want)
		}
	}
}
