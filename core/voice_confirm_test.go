package core

import (
	"context"
	"sync"
	"testing"
	"time"
)

type stubSpeechToText struct {
	text string
}

func (s *stubSpeechToText) Transcribe(context.Context, []byte, string, string) (string, error) {
	return s.text, nil
}

type stubButtonPlatform struct {
	mu          sync.Mutex
	n           string
	sent        []string
	buttonTexts []string
	buttonData  []string
	audio       [][]byte
	audioFormat []string
}

func (p *stubButtonPlatform) Name() string               { return p.n }
func (p *stubButtonPlatform) Start(MessageHandler) error { return nil }
func (p *stubButtonPlatform) Reply(_ context.Context, _ any, content string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, content)
	return nil
}
func (p *stubButtonPlatform) Send(_ context.Context, _ any, content string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, content)
	return nil
}
func (p *stubButtonPlatform) Stop() error { return nil }
func (p *stubButtonPlatform) SendWithButtons(_ context.Context, _ any, content string, buttons [][]ButtonOption) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, content)
	for _, row := range buttons {
		for _, btn := range row {
			p.buttonTexts = append(p.buttonTexts, btn.Text)
			p.buttonData = append(p.buttonData, btn.Data)
		}
	}
	return nil
}
func (p *stubButtonPlatform) SendAudio(_ context.Context, _ any, audio []byte, format string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.audio = append(p.audio, audio)
	p.audioFormat = append(p.audioFormat, format)
	return nil
}

func (p *stubButtonPlatform) sentSnapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.sent...)
}

func (p *stubButtonPlatform) buttonDataSnapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.buttonData...)
}

func (p *stubButtonPlatform) buttonTextsSnapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.buttonTexts...)
}

func (p *stubButtonPlatform) audioSnapshot() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([][]byte, len(p.audio))
	for i, audio := range p.audio {
		out[i] = append([]byte(nil), audio...)
	}
	return out
}

func (p *stubButtonPlatform) audioFormatSnapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.audioFormat...)
}

type voiceTestAgent struct {
	session *voiceTestSession
}

func (a *voiceTestAgent) Name() string { return "voice-test" }
func (a *voiceTestAgent) StartSession(_ context.Context, _ string) (AgentSession, error) {
	return a.session, nil
}
func (a *voiceTestAgent) ListSessions(_ context.Context) ([]AgentSessionInfo, error) { return nil, nil }
func (a *voiceTestAgent) Stop() error                                                { return nil }

type voiceTestSession struct {
	sendCh chan string
	events chan Event
}

func newVoiceTestSession() *voiceTestSession {
	return &voiceTestSession{
		sendCh: make(chan string, 1),
		events: make(chan Event, 1),
	}
}

func (s *voiceTestSession) Send(prompt string, _ []ImageAttachment, _ []FileAttachment) error {
	s.sendCh <- prompt
	s.events <- Event{Type: EventResult, Content: "done", Done: true}
	return nil
}
func (s *voiceTestSession) RespondPermission(string, PermissionResult) error { return nil }
func (s *voiceTestSession) Events() <-chan Event                             { return s.events }
func (s *voiceTestSession) CurrentSessionID() string                         { return "voice-test-session" }
func (s *voiceTestSession) Alive() bool                                      { return true }
func (s *voiceTestSession) Close() error {
	close(s.events)
	return nil
}

func TestHandleVoiceMessageQueuesConfirmation(t *testing.T) {
	p := &stubButtonPlatform{n: "test"}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.SetSpeechConfig(SpeechCfg{
		Enabled:           true,
		ConfirmBeforeSend: true,
		STT:               &stubSpeechToText{text: "draft request"},
	})

	msg := &Message{
		SessionKey: "test:user1",
		Platform:   "test",
		ReplyCtx:   "ctx",
		Audio:      &AudioAttachment{Format: "mp3", Data: []byte("audio")},
	}

	e.handleVoiceMessage(p, msg)

	sent := p.sentSnapshot()
	if len(sent) < 2 {
		t.Fatalf("expected transcribing message and confirmation prompt, got %d messages", len(sent))
	}
	if got := sent[1]; got == "" || got == "draft request" {
		t.Fatalf("expected formatted confirmation prompt, got %q", got)
	}
	buttonTexts := p.buttonTextsSnapshot()
	if len(buttonTexts) != 2 || buttonTexts[0] != "Confirm" || buttonTexts[1] != "Modify" {
		t.Fatalf("unexpected button labels: %#v", buttonTexts)
	}
	if pending, ok := e.getVoiceConfirmation("test:user1"); !ok || pending.Text != "draft request" {
		t.Fatalf("expected pending voice confirmation, got %#v, ok=%v", pending, ok)
	}
}

func TestVoiceConfirmationModifyThenConfirmSendsUpdatedText(t *testing.T) {
	p := &stubButtonPlatform{n: "test"}
	session := newVoiceTestSession()
	e := NewEngine("test", &voiceTestAgent{session: session}, []Platform{p}, "", LangEnglish)
	e.SetSpeechConfig(SpeechCfg{
		Enabled:           true,
		ConfirmBeforeSend: true,
		STT:               &stubSpeechToText{text: "initial request"},
	})

	voiceMsg := &Message{
		SessionKey: "test:user1",
		Platform:   "test",
		ReplyCtx:   "ctx",
		Audio:      &AudioAttachment{Format: "mp3", Data: []byte("audio")},
	}
	e.handleVoiceMessage(p, voiceMsg)

	e.handleMessage(p, &Message{
		SessionKey: "test:user1",
		Platform:   "test",
		ReplyCtx:   "ctx",
		Content:    "voice modify",
	})
	e.handleMessage(p, &Message{
		SessionKey: "test:user1",
		Platform:   "test",
		ReplyCtx:   "ctx",
		Content:    "updated request",
	})
	e.handleMessage(p, &Message{
		SessionKey: "test:user1",
		Platform:   "test",
		ReplyCtx:   "ctx",
		Content:    "confirm",
	})

	select {
	case got := <-session.sendCh:
		if got != "updated request" {
			t.Fatalf("expected updated request to be sent to agent, got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for agent send")
	}

	if _, ok := e.getVoiceConfirmation("test:user1"); ok {
		t.Fatal("expected pending voice confirmation to be cleared after confirm")
	}
}
