package feishu

import (
	"encoding/json"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func decodeRenderedCard(t *testing.T, card *core.Card) map[string]any {
	t.Helper()

	var got map[string]any
	if err := json.Unmarshal([]byte(renderCard(card, "")), &got); err != nil {
		t.Fatalf("renderCard JSON decode failed: %v", err)
	}
	return got
}

func TestRenderCardMap_EqualColumnsActionsUseColumnSet(t *testing.T) {
	buttons := []core.CardButton{
		core.PrimaryBtn("Session Management", "nav:/help session"),
		core.DefaultBtn("Agent Configuration", "nav:/help agent"),
	}
	card := core.NewCard().ButtonsEqual(buttons...).Build()
	got := decodeRenderedCard(t, card)

	elements, ok := got["elements"].([]any)
	if !ok || len(elements) != 1 {
		t.Fatalf("elements = %#v, want one element", got["elements"])
	}
	columnSet, ok := elements[0].(map[string]any)
	if !ok {
		t.Fatalf("first element = %#v, want object", elements[0])
	}
	if tag := columnSet["tag"]; tag != "column_set" {
		t.Fatalf("tag = %#v, want column_set", tag)
	}
	if flexMode := columnSet["flex_mode"]; flexMode != "bisect" {
		t.Fatalf("flex_mode = %#v, want bisect", flexMode)
	}
}

func TestRenderCardMap_DefaultActionsStayActionRow(t *testing.T) {
	card := core.NewCard().
		Buttons(core.PrimaryBtn("Yes", "act:/yes"), core.DefaultBtn("No", "act:/no")).
		Build()
	got := decodeRenderedCard(t, card)

	elements, ok := got["elements"].([]any)
	if !ok || len(elements) != 1 {
		t.Fatalf("elements = %#v, want one element", got["elements"])
	}
	actionRow, ok := elements[0].(map[string]any)
	if !ok {
		t.Fatalf("first element = %#v, want object", elements[0])
	}
	if tag := actionRow["tag"]; tag != "action" {
		t.Fatalf("tag = %#v, want action", tag)
	}
}

func TestRenderCardMap_InjectsSessionKeyIntoCallbacks(t *testing.T) {
	card := core.NewCard().
		Buttons(core.PrimaryBtn("Open", "nav:/help session")).
		ListItem("Choose", "Confirm", "act:/confirm").
		Build()

	got := renderCardMap(card, "feishu:oc_chat:user")
	elements, ok := got["elements"].([]map[string]any)
	if ok {
		_ = elements
	}

	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal rendered card failed: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode rendered card failed: %v", err)
	}
	parts, ok := decoded["elements"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("elements = %#v, want 2 elements", decoded["elements"])
	}

	actionRow := parts[0].(map[string]any)
	actions := actionRow["actions"].([]any)
	firstButton := actions[0].(map[string]any)
	value := firstButton["value"].(map[string]any)
	if value["session_key"] != "feishu:oc_chat:user" {
		t.Fatalf("button session_key = %#v, want thread session key", value["session_key"])
	}
}

func TestRenderDeleteModeCheckerCard_InvalidCardReturnsFalse(t *testing.T) {
	card := core.NewCard().
		Markdown("plain markdown should not be transformed").
		Build()

	base := map[string]any{"config": map[string]any{"wide_screen_mode": true}}
	if transformed, ok := renderDeleteModeCheckerCard(card, base); ok || transformed != nil {
		t.Fatalf("got transformed=%#v ok=%v, want nil false", transformed, ok)
	}
}
