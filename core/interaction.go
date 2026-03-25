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

	if prompt := detectOptionsXMLPrompt(trimmed); prompt != nil {
		return prompt
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

func detectOptionsXMLPrompt(text string) *textInteractionPrompt {
	reBlock := regexp.MustCompile(`(?is)<options>\s*([\s\S]*?)\s*</options>`)
	match := reBlock.FindStringSubmatch(text)
	if len(match) != 2 {
		return nil
	}

	reOption := regexp.MustCompile(`(?is)<option>\s*([\s\S]*?)\s*</option>`)
	optionMatches := reOption.FindAllStringSubmatch(match[1], -1)
	if len(optionMatches) < 2 {
		return nil
	}

	row := make([]interactionChoice, 0, len(optionMatches))
	fallback := make([]string, 0, len(optionMatches))
	for idx, m := range optionMatches {
		label := normalizeChoiceText(strings.TrimSpace(m[1]))
		if label == "" {
			continue
		}
		row = append(row, interactionChoice{
			ID:         fmt.Sprintf("opt_%d", idx+1),
			Label:      truncateChoiceLabel(label),
			SendText:   label,
			MatchTexts: []string{strings.ToLower(label)},
		})
		fallback = append(fallback, label)
	}
	if len(row) < 2 {
		return nil
	}

	promptText := strings.TrimSpace(reBlock.ReplaceAllString(text, ""))

	return &textInteractionPrompt{
		Prompt:   promptText,
		Choices:  [][]interactionChoice{row},
		Fallback: fallback,
	}
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
	trimmedLines := make([]string, 0, len(lines))
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line != "" {
			trimmedLines = append(trimmedLines, line)
		}
	}
	if len(trimmedLines) < 3 {
		return nil
	}

	type choiceLine struct {
		number string
		label  string
	}
	var choices []choiceLine
	re := regexp.MustCompile(`^\s*(\d+)[\.\)]\s+(.+)$`)
	start := -1
	for idx, line := range trimmedLines {
		m := re.FindStringSubmatch(line)
		if len(m) == 3 {
			if start == -1 {
				start = idx
			}
			label := strings.TrimSpace(strings.TrimSuffix(m[2], "."))
			if label == "" || len([]rune(label)) > 100 {
				return nil
			}
			choices = append(choices, choiceLine{number: m[1], label: label})
			if len(choices) == 4 {
				break
			}
			continue
		}
		if start != -1 {
			// Once the numbered block starts, any following non-numbered line means
			// this is content, not a clean choice prompt.
			return nil
		}
	}
	if len(choices) < 2 {
		return nil
	}
	if start <= 0 {
		return nil
	}

	intro := strings.ToLower(strings.Join(trimmedLines[:start], "\n"))
	lastIntro := strings.ToLower(trimmedLines[start-1])
	if !looksLikeChoiceQuestion(intro, lastIntro) {
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

func looksLikeChoiceQuestion(fullIntro, lastLine string) bool {
	keywords := []string{
		"choose",
		"pick",
		"select",
		"prefer",
		"which option",
		"which one",
		"what should i",
		"what do you want",
		"let me know which",
	}
	matched := false
	for _, keyword := range keywords {
		if strings.Contains(fullIntro, keyword) {
			matched = true
			break
		}
	}
	if !matched {
		return false
	}
	return strings.Contains(lastLine, "?") ||
		strings.HasPrefix(lastLine, "choose") ||
		strings.HasPrefix(lastLine, "pick") ||
		strings.HasPrefix(lastLine, "select") ||
		strings.HasPrefix(lastLine, "which")
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
	return strings.TrimSpace(s)
}
