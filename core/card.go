package core

import (
	"fmt"
	"strings"
)

// Card represents a structured rich message that can be rendered as
// platform-specific cards or degraded to plain text.
type Card struct {
	Header   *CardHeader
	Elements []CardElement
}

// CardHeader is the optional colored title bar of a card.
type CardHeader struct {
	Title string
	Color string
}

// CardElement is the interface satisfied by all card content elements.
type CardElement interface {
	cardElement()
}

// CardMarkdown renders markdown-formatted text.
type CardMarkdown struct{ Content string }

// CardDivider renders a horizontal rule.
type CardDivider struct{}

// CardActions renders a row of clickable buttons.
type CardActions struct {
	Buttons []CardButton
	Layout  CardActionLayout
}

// CardNote renders small footnote text at the bottom.
type CardNote struct {
	Text string
	Tag  string
}

// CardListItem renders a row with description text on the left and a button on the right.
type CardListItem struct {
	Text     string
	BtnText  string
	BtnType  string
	BtnValue string
	Extra    map[string]string
}

// CardSelect renders a dropdown selector.
type CardSelect struct {
	Placeholder string
	Options     []CardSelectOption
	InitValue   string
}

// CardSelectOption is one item in a CardSelect dropdown.
type CardSelectOption struct {
	Text  string
	Value string
}

func (CardMarkdown) cardElement() {}
func (CardDivider) cardElement()  {}
func (CardActions) cardElement()  {}
func (CardNote) cardElement()     {}
func (CardListItem) cardElement() {}
func (CardSelect) cardElement()   {}

// CardButton represents a clickable button inside a CardActions element.
type CardButton struct {
	Text  string
	Type  string
	Value string
	Extra map[string]string
}

// CardActionLayout controls how a CardActions row should be rendered.
type CardActionLayout string

const (
	CardActionLayoutRow          CardActionLayout = "row"
	CardActionLayoutEqualColumns CardActionLayout = "equal_columns"
)

func Btn(text, typ, value string) CardButton {
	return CardButton{Text: text, Type: typ, Value: value}
}

func PrimaryBtn(text, value string) CardButton {
	return CardButton{Text: text, Type: "primary", Value: value}
}

func DefaultBtn(text, value string) CardButton {
	return CardButton{Text: text, Type: "default", Value: value}
}

func DangerBtn(text, value string) CardButton {
	return CardButton{Text: text, Type: "danger", Value: value}
}

// CardBuilder provides a fluent API for constructing Card instances.
type CardBuilder struct {
	card Card
}

func NewCard() *CardBuilder {
	return &CardBuilder{}
}

func (b *CardBuilder) Title(title, color string) *CardBuilder {
	b.card.Header = &CardHeader{Title: title, Color: color}
	return b
}

func (b *CardBuilder) Markdown(content string) *CardBuilder {
	if content != "" {
		b.card.Elements = append(b.card.Elements, CardMarkdown{Content: content})
	}
	return b
}

func (b *CardBuilder) Markdownf(format string, args ...any) *CardBuilder {
	return b.Markdown(fmt.Sprintf(format, args...))
}

func (b *CardBuilder) Divider() *CardBuilder {
	b.card.Elements = append(b.card.Elements, CardDivider{})
	return b
}

func (b *CardBuilder) Buttons(buttons ...CardButton) *CardBuilder {
	if len(buttons) > 0 {
		b.card.Elements = append(b.card.Elements, CardActions{Buttons: buttons, Layout: CardActionLayoutRow})
	}
	return b
}

func (b *CardBuilder) ButtonsEqual(buttons ...CardButton) *CardBuilder {
	if len(buttons) > 0 {
		b.card.Elements = append(b.card.Elements, CardActions{Buttons: buttons, Layout: CardActionLayoutEqualColumns})
	}
	return b
}

func (b *CardBuilder) ListItem(desc, btnText, btnValue string) *CardBuilder {
	b.card.Elements = append(b.card.Elements, CardListItem{
		Text: desc, BtnText: btnText, BtnType: "default", BtnValue: btnValue,
	})
	return b
}

func (b *CardBuilder) ListItemBtn(desc, btnText, btnType, btnValue string) *CardBuilder {
	b.card.Elements = append(b.card.Elements, CardListItem{
		Text: desc, BtnText: btnText, BtnType: btnType, BtnValue: btnValue,
	})
	return b
}

func (b *CardBuilder) ListItemBtnExtra(desc, btnText, btnType, btnValue string, extra map[string]string) *CardBuilder {
	b.card.Elements = append(b.card.Elements, CardListItem{
		Text: desc, BtnText: btnText, BtnType: btnType, BtnValue: btnValue, Extra: extra,
	})
	return b
}

func (b *CardBuilder) Select(placeholder string, options []CardSelectOption, initValue string) *CardBuilder {
	if len(options) > 0 {
		b.card.Elements = append(b.card.Elements, CardSelect{
			Placeholder: placeholder, Options: options, InitValue: initValue,
		})
	}
	return b
}

func (b *CardBuilder) Note(text string) *CardBuilder {
	if text != "" {
		b.card.Elements = append(b.card.Elements, CardNote{Text: text})
	}
	return b
}

func (b *CardBuilder) TaggedNote(tag, text string) *CardBuilder {
	if text != "" {
		b.card.Elements = append(b.card.Elements, CardNote{Text: text, Tag: tag})
	}
	return b
}

func (b *CardBuilder) Build() *Card {
	c := b.card
	return &c
}

// RenderText converts the card to a plain-text representation.
func (c *Card) RenderText() string {
	var sb strings.Builder

	if c.Header != nil && c.Header.Title != "" {
		sb.WriteString("**")
		sb.WriteString(c.Header.Title)
		sb.WriteString("**\n\n")
	}

	for _, elem := range c.Elements {
		switch e := elem.(type) {
		case CardMarkdown:
			sb.WriteString(e.Content)
			sb.WriteString("\n\n")
		case CardDivider:
			sb.WriteString("---\n\n")
		case CardActions:
			for i, btn := range e.Buttons {
				if i > 0 {
					sb.WriteString("  ")
				}
				sb.WriteString("[")
				sb.WriteString(btn.Text)
				sb.WriteString("]")
			}
			sb.WriteString("\n\n")
		case CardListItem:
			sb.WriteString(e.Text)
			sb.WriteString("  [")
			sb.WriteString(e.BtnText)
			sb.WriteString("]\n")
		case CardSelect:
			sb.WriteString(e.Placeholder)
			sb.WriteString(": ")
			for i, opt := range e.Options {
				if i > 0 {
					sb.WriteString(" | ")
				}
				sb.WriteString(opt.Text)
			}
			sb.WriteString("\n\n")
		case CardNote:
			sb.WriteString(e.Text)
			sb.WriteString("\n")
		}
	}

	return strings.TrimRight(sb.String(), "\n")
}

// HasButtons returns true if the card contains any interactive elements.
func (c *Card) HasButtons() bool {
	for _, elem := range c.Elements {
		switch elem.(type) {
		case CardActions, CardListItem, CardSelect:
			return true
		}
	}
	return false
}

// CollectButtons extracts buttons from the card as rows suitable for inline-button platforms.
func (c *Card) CollectButtons() [][]ButtonOption {
	var rows [][]ButtonOption
	for _, elem := range c.Elements {
		switch e := elem.(type) {
		case CardActions:
			var row []ButtonOption
			for _, btn := range e.Buttons {
				row = append(row, ButtonOption{Text: btn.Text, Data: btn.Value})
			}
			if len(row) > 0 {
				rows = append(rows, row)
			}
		case CardListItem:
			rows = append(rows, []ButtonOption{{Text: e.BtnText, Data: e.BtnValue}})
		}
	}
	return rows
}
