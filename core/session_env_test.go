package core

import "testing"

func TestSessionWorkDirFromEnv(t *testing.T) {
	tests := []struct {
		name       string
		env        []string
		defaultDir string
		want       string
	}{
		{
			name:       "default when no override",
			env:        []string{"CC_PROJECT=echo"},
			defaultDir: "/repo",
			want:       "/repo",
		},
		{
			name: "last worktree override wins",
			env: []string{
				"CC_WORKTREE_PATH=/repo/old",
				"CC_WORKTREE_PATH=/repo/new",
			},
			defaultDir: "/repo",
			want:       "/repo/new",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SessionWorkDirFromEnv(tt.env, tt.defaultDir)
			if got != tt.want {
				t.Fatalf("SessionWorkDirFromEnv() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSessionExtraDirsFromEnv(t *testing.T) {
	env := []string{
		"CC_EXTRA_WORK_DIRS=/old/one:/old/two",
		"CC_EXTRA_WORK_DIRS=/new/one:/new/two",
	}
	got := SessionExtraDirsFromEnv(env)
	want := []string{"/new/one", "/new/two"}
	if len(got) != len(want) {
		t.Fatalf("len(got) = %d, want %d (%#v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
