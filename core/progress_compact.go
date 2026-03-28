package core

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"
)

const (
	progressStyleLegacy  = "legacy"
	progressStyleCompact = "compact"
	progressStyleCard    = "card"
)

type compactProgressWriter struct {
	ctx      context.Context
	platform Platform
	replyCtx any
	style    string
	starter  PreviewStarter
	updater  MessageUpdater
	handle   any
	entries  []string
	lastSent string
	enabled  bool
	failed   bool
}

func newCompactProgressWriter(ctx context.Context, p Platform, replyCtx any) *compactProgressWriter {
	w := &compactProgressWriter{ctx: ctx, platform: p, replyCtx: replyCtx, style: progressStyleLegacy}
	if sp, ok := p.(ProgressStyleProvider); ok {
		switch strings.ToLower(strings.TrimSpace(sp.ProgressStyle())) {
		case progressStyleCompact:
			w.style = progressStyleCompact
		case progressStyleCard:
			w.style = progressStyleCard
		}
	}
	if w.style == progressStyleLegacy {
		return w
	}
	updater, ok := p.(MessageUpdater)
	if !ok {
		return w
	}
	starter, ok := p.(PreviewStarter)
	if !ok {
		return w
	}
	w.updater = updater
	w.starter = starter
	w.enabled = true
	return w
}

func (w *compactProgressWriter) Append(entry string) bool {
	if !w.enabled || w.failed {
		return false
	}
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return true
	}
	w.entries = append(w.entries, entry)
	if len(w.entries) > 8 {
		w.entries = w.entries[len(w.entries)-8:]
	}

	content := w.render()
	if content == "" || content == w.lastSent {
		return true
	}
	if w.handle == nil {
		handle, err := w.starter.SendPreviewStart(w.ctx, w.replyCtx, content)
		if err != nil || handle == nil {
			slog.Warn("progress writer: preview start failed", "platform", w.platform.Name(), "style", w.style, "error", err)
			w.failed = true
			return false
		}
		w.handle = handle
		w.lastSent = content
		return true
	}
	if err := w.updater.UpdateMessage(w.ctx, w.handle, content); err != nil {
		slog.Warn("progress writer: update failed", "platform", w.platform.Name(), "style", w.style, "error", err)
		w.failed = true
		return false
	}
	w.lastSent = content
	return true
}

func (w *compactProgressWriter) Finalize() {}

func (w *compactProgressWriter) render() string {
	switch w.style {
	case progressStyleCard:
		var b strings.Builder
		b.WriteString("⏳ **Progress**")
		for i, entry := range w.entries {
			fmt.Fprintf(&b, "\n\n%d. %s", i+1, strings.ReplaceAll(entry, "\n", "\n   "))
		}
		return trimCompactProgressText(b.String(), maxPlatformMessageLen-200)
	case progressStyleCompact:
		return trimCompactProgressText(strings.Join(w.entries, "\n\n"), maxPlatformMessageLen-200)
	default:
		return ""
	}
}

func trimCompactProgressText(s string, maxRunes int) string {
	if maxRunes <= 0 || utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	rs := []rune(s)
	return "…\n" + strings.TrimLeft(string(rs[len(rs)-maxRunes:]), "\n")
}
