package core

import (
	"strings"
	"testing"
)

func TestReplyWithInteractionRegistersGenericCallbacks(t *testing.T) {
	p := &stubButtonPlatform{n: "test"}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)

	e.replyWithInteraction("test:user1", p, "ctx", "choose", [][]interactionChoice{{
		{ID: "yes", Label: "Yes", SendText: "yes", MatchTexts: []string{"yes"}},
		{ID: "no", Label: "No", SendText: "no", MatchTexts: []string{"no"}},
	}}, false)

	if len(p.buttonData) != 2 {
		t.Fatalf("expected 2 button callbacks, got %d", len(p.buttonData))
	}
	for _, data := range p.buttonData {
		if !strings.HasPrefix(data, interactionCallbackPrefix) {
			t.Fatalf("expected generic interaction callback prefix, got %q", data)
		}
	}
}

func TestResolvePendingInteractionMapsCallbackToSendText(t *testing.T) {
	p := &stubButtonPlatform{n: "test"}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)

	e.replyWithInteraction("test:user1", p, "ctx", "choose", [][]interactionChoice{{
		{ID: "allow", Label: "Allow", SendText: "allow", MatchTexts: []string{"allow"}},
	}}, true)

	got, consumed := e.resolvePendingInteraction("test:user1", p.buttonData[0])
	if consumed {
		t.Fatal("expected callback to resolve into send text, not be consumed")
	}
	if got != "allow" {
		t.Fatalf("expected allow, got %q", got)
	}
	if _, ok := e.prompts["test:user1"]; ok {
		t.Fatal("expected prompt to be cleared after resolving callback")
	}
}

func TestResolvePendingInteractionClearsNonStrictOnFreeform(t *testing.T) {
	p := &stubButtonPlatform{n: "test"}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)

	e.replyWithInteraction("test:user1", p, "ctx", "choose", [][]interactionChoice{{
		{ID: "one", Label: "One", SendText: "1", MatchTexts: []string{"1"}},
		{ID: "two", Label: "Two", SendText: "2", MatchTexts: []string{"2"}},
	}}, false)

	got, consumed := e.resolvePendingInteraction("test:user1", "something else")
	if consumed {
		t.Fatal("unexpected consume for freeform response")
	}
	if got != "something else" {
		t.Fatalf("expected original content, got %q", got)
	}
	if _, ok := e.prompts["test:user1"]; ok {
		t.Fatal("expected non-strict prompt to clear after freeform response")
	}
}

func TestDetectTextInteractionPromptYesNo(t *testing.T) {
	prompt := detectTextInteractionPrompt("Would you like me to proceed with the refactor?")
	if prompt == nil {
		t.Fatal("expected yes/no prompt to be detected")
	}
	if len(prompt.Choices) != 1 || len(prompt.Choices[0]) != 2 {
		t.Fatalf("unexpected choices: %#v", prompt.Choices)
	}
	if prompt.Choices[0][0].SendText != "yes" || prompt.Choices[0][1].SendText != "no" {
		t.Fatalf("unexpected choice send texts: %#v", prompt.Choices[0])
	}
}

func TestDetectTextInteractionPromptNumberedChoices(t *testing.T) {
	text := "Which option do you prefer?\n1. Update the API handler\n2. Ship a minimal fix"
	prompt := detectTextInteractionPrompt(text)
	if prompt == nil {
		t.Fatal("expected numbered choice prompt to be detected")
	}
	if len(prompt.Choices) != 1 || len(prompt.Choices[0]) != 2 {
		t.Fatalf("unexpected choices: %#v", prompt.Choices)
	}
	if prompt.Choices[0][0].SendText != "1" || prompt.Choices[0][1].SendText != "2" {
		t.Fatalf("unexpected numbered send texts: %#v", prompt.Choices[0])
	}
}

func TestDetectTextInteractionPromptOptionsXML(t *testing.T) {
	text := "Choose a path:\n<options>\n  <option>Update the API handler</option>\n  <option>Ship a minimal fix</option>\n</options>"
	prompt := detectTextInteractionPrompt(text)
	if prompt == nil {
		t.Fatal("expected XML options prompt to be detected")
	}
	if len(prompt.Choices) != 1 || len(prompt.Choices[0]) != 2 {
		t.Fatalf("unexpected choices: %#v", prompt.Choices)
	}
	if prompt.Choices[0][0].SendText != "Update the API handler" || prompt.Choices[0][1].SendText != "Ship a minimal fix" {
		t.Fatalf("unexpected XML option send texts: %#v", prompt.Choices[0])
	}
}

func TestDetectTextInteractionPromptIgnoresSummaryNumberedList(t *testing.T) {
	text := "Summary:\n1. We can keep the current API.\n2. Option A is to patch the parser.\n3. Option B is to remove the feature."
	prompt := detectTextInteractionPrompt(text)
	if prompt != nil {
		t.Fatalf("expected summary numbered list to be ignored, got %#v", prompt)
	}
}
