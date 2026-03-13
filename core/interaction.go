package core

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

const interactionCallbackPrefix = "ia:"

type interactionChoice struct {
	ID         string
	Label      string
	SendText   string
	MatchTexts []string
}

type pendingInteractionPrompt struct {
	Token   string
	Choices map[string]interactionChoice
	Strict  bool
}

type textInteractionPrompt struct {
	Prompt      string
	Choices     [][]interactionChoice
	Fallback    []string
	Description string
}

func newInteractionToken() string {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "00000000"
	}
	return hex.EncodeToString(buf[:])
}

func buildInteractionCallbackData(token, choiceID string) string {
	return interactionCallbackPrefix + token + ":" + choiceID
}

func parseInteractionCallbackData(data string) (token, choiceID string, ok bool) {
	if !strings.HasPrefix(data, interactionCallbackPrefix) {
		return "", "", false
	}
	parts := strings.SplitN(strings.TrimPrefix(data, interactionCallbackPrefix), ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func detectTextInteractionPrompt(text string) *textInteractionPrompt {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}

	if prompt := detectReplyWithPrompt(trimmed); prompt != nil {
		return prompt
	}
	if prompt := detectNumberedChoicePrompt(trimmed); prompt != nil {
		return prompt
	}
	if prompt := detectYesNoPrompt(trimmed); prompt != nil {
		return prompt
	}
	return nil
}

func detectReplyWithPrompt(text string) *textInteractionPrompt {
	re := regexp.MustCompile("(?is)(reply|respond)\\s+with\\s+[\"']?([^\"'\\n,/.]+(?:\\s+[^\"'\\n,/.]+)*)[\"']?\\s*(?:or|/)\\s*[\"']?([^\"'\\n]+?)[\"']?(?:[\\.\\n]|$)")
	m := re.FindStringSubmatch(text)
	if len(m) != 4 {
		return nil
	}

	first := normalizeChoiceText(m[2])
	second := normalizeChoiceText(m[3])
	if first == "" || second == "" {
		return nil
	}

	return &textInteractionPrompt{
		Prompt: text,
		Choices: [][]interactionChoice{{
			{
				ID:         "reply_1",
				Label:      first,
				SendText:   first,
				MatchTexts: []string{strings.ToLower(first)},
			},
			{
				ID:         "reply_2",
				Label:      second,
				SendText:   second,
				MatchTexts: []string{strings.ToLower(second)},
			},
		}},
		Fallback: []string{first, second},
	}
}

func detectNumberedChoicePrompt(text string) *textInteractionPrompt {
	lines := strings.Split(text, "\n")
	type choiceLine struct {
		number string
		label  string
	}
	var choices []choiceLine
	re := regexp.MustCompile(`^\s*(\d+)[\.\)]\s+(.+)$`)
	for _, raw := range lines {
		m := re.FindStringSubmatch(strings.TrimSpace(raw))
		if len(m) != 3 {
			continue
		}
		label := strings.TrimSpace(strings.TrimSuffix(m[2], "."))
		if label == "" {
			continue
		}
		choices = append(choices, choiceLine{number: m[1], label: label})
		if len(choices) == 3 {
			break
		}
	}
	if len(choices) < 2 {
		return nil
	}

	lower := strings.ToLower(text)
	if !strings.Contains(lower, "choose") && !strings.Contains(lower, "pick") &&
		!strings.Contains(lower, "option") && !strings.Contains(lower, "prefer") &&
		!strings.Contains(lower, "which") {
		return nil
	}

	row := make([]interactionChoice, 0, len(choices))
	fallback := make([]string, 0, len(choices))
	for _, choice := range choices {
		row = append(row, interactionChoice{
			ID:       "num_" + choice.number,
			Label:    fmt.Sprintf("%s. %s", choice.number, truncateChoiceLabel(choice.label)),
			SendText: choice.number,
			MatchTexts: []string{
				choice.number,
				strings.ToLower(choice.label),
				strings.ToLower(fmt.Sprintf("%s %s", choice.number, choice.label)),
			},
		})
		fallback = append(fallback, choice.number)
	}

	return &textInteractionPrompt{
		Prompt:   text,
		Choices:  [][]interactionChoice{row},
		Fallback: fallback,
	}
}

func detectYesNoPrompt(text string) *textInteractionPrompt {
	lower := strings.ToLower(strings.TrimSpace(text))
	if strings.Count(lower, "?") == 0 {
		return nil
	}
	triggers := []string{
		"would you like",
		"would you prefer",
		"do you want me to",
		"do you want",
		"should i",
		"shall i",
		"can i",
		"may i",
		"should we",
		"shall we",
		"proceed",
		"continue",
		"go ahead",
	}
	matched := false
	for _, trigger := range triggers {
		if strings.Contains(lower, trigger) {
			matched = true
			break
		}
	}
	if !matched {
		return nil
	}
	if strings.Contains(lower, "1.") || strings.Contains(lower, "2.") {
		return nil
	}

	return &textInteractionPrompt{
		Prompt: text,
		Choices: [][]interactionChoice{{
			{
				ID:         "yes",
				Label:      "Yes",
				SendText:   "yes",
				MatchTexts: []string{"yes", "y", "ok", "continue", "proceed"},
			},
			{
				ID:         "no",
				Label:      "No",
				SendText:   "no",
				MatchTexts: []string{"no", "n", "stop", "cancel"},
			},
		}},
		Fallback: []string{"yes", "no"},
	}
}

func normalizeChoiceText(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "\"'`")
	s = strings.TrimSpace(strings.TrimSuffix(s, "."))
	return s
}

func truncateChoiceLabel(s string) string {
	runes := []rune(strings.TrimSpace(s))
	if len(runes) <= 28 {
		return string(runes)
	}
	return string(runes[:25]) + "..."
}
