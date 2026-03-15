package main

import (
	"strings"
	"testing"
)

func TestLookupEnvUsesLastMatch(t *testing.T) {
	t.Parallel()

	env := []string{
		"CC_REPO_PATH=/repo/old",
		"CC_BRANCH=echo/old",
		"CC_REPO_PATH=/repo/new",
	}

	got := lookupEnv(env, "CC_REPO_PATH")
	if got != "/repo/new" {
		t.Fatalf("lookupEnv() = %q, want %q", got, "/repo/new")
	}
}

func TestRenderWorkspaceSummary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		env  []string
		want string
	}{
		{
			name: "empty env",
			env:  nil,
			want: "",
		},
		{
			name: "missing workspace vars",
			env:  []string{"CC_TASK_ID=task-1"},
			want: "",
		},
		{
			name: "full workspace env",
			env: []string{
				"CC_REPO_PATH=/repo",
				"CC_WORKTREE_PATH=/repo/.echo/workspaces/ws-1",
				"CC_BRANCH=echo/ws-1",
			},
			want: " [repo=/repo worktree=/repo/.echo/workspaces/ws-1 branch=echo/ws-1]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := renderWorkspaceSummary(tt.env)
			if got != tt.want {
				t.Fatalf("renderWorkspaceSummary() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRenderEchoResultIncludesWorkspaceMetadata(t *testing.T) {
	t.Parallel()

	got := renderEchoResult("ship it", []string{
		"CC_REPO_PATH=/repo",
		"CC_WORKTREE_PATH=/repo/.echo/workspaces/ws-1",
		"CC_BRANCH=echo/ws-1",
	})

	for _, want := range []string{
		`"status":"completed"`,
		`repo=/repo`,
		`worktree=/repo/.echo/workspaces/ws-1`,
		`branch=echo/ws-1`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("renderEchoResult() missing %q in %q", want, got)
		}
	}
}

func TestRenderEchoResultHumanResolutionFlow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		prompt string
		want   []string
	}{
		{
			name:   "initial request needs human input",
			prompt: "human-resolution-smoke initial implementation task",
			want: []string{
				`"status":"needs_human_input"`,
				`"blocked_reason":"Need operator approval for rollout"`,
				`"continuation_hint":"Approve the Friday rollout window"`,
			},
		},
		{
			name:   "manager continuation completes",
			prompt: "human-resolution-smoke continuation_of_task_id=task-1",
			want: []string{
				`"status":"completed"`,
				`fixture manager continuation completed`,
			},
		},
		{
			name:   "resumed source task completes",
			prompt: "human-resolution-smoke continuation_context human_resolution approved",
			want: []string{
				`"status":"completed"`,
				`fixture resumed after human resolution`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := renderEchoResult(tt.prompt, nil)
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Fatalf("renderEchoResult() missing %q in %q", want, got)
				}
			}
		})
	}
}
