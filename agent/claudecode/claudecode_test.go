package claudecode

import (
	"context"
	"slices"
	"testing"
)

func TestAvailableModelsFallsBackToCurrentClaudeFamily(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")

	agent := &Agent{}
	models := agent.AvailableModels(context.Background())
	var names []string
	for _, model := range models {
		names = append(names, model.Name)
	}

	want := []string{
		"claude-opus-4-7",
		"claude-opus-4-7-1m",
		"claude-sonnet-4-6",
		"claude-haiku-4-5",
	}
	if !slices.Equal(names, want) {
		t.Fatalf("AvailableModels() = %#v, want %#v", names, want)
	}
}
