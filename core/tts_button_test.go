package core

import (
	"context"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

type stubTTS struct {
	mu           sync.Mutex
	audio        []byte
	format       string
	texts        []string
	delays       map[string]time.Duration
	audioForText func(string) []byte
}

func (s *stubTTS) Synthesize(_ context.Context, text string, _ TTSSynthesisOpts) ([]byte, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.texts = append(s.texts, text)
	if d := s.delays[text]; d > 0 {
		time.Sleep(d)
	}
	if s.audioForText != nil {
		return s.audioForText(text), s.format, nil
	}
	return s.audio, s.format, nil
}

func (s *stubTTS) textsSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.texts...)
}

func TestFinalReplyOffersReadAloudButton(t *testing.T) {
	p := &stubButtonPlatform{n: "test"}
	session := newVoiceTestSession()
	e := NewEngine("test", &voiceTestAgent{session: session}, []Platform{p}, "", LangEnglish)
	e.SetTTSConfig(&TTSCfg{
		Enabled:         true,
		TTS:             &stubTTS{audio: []byte("audio"), format: "mp3"},
		OfferReadButton: true,
	})

	e.handleMessage(p, &Message{
		SessionKey: "test:user1",
		Platform:   "test",
		ReplyCtx:   "ctx",
		Content:    "summarize this",
	})

	deadline := time.Now().Add(2 * time.Second)
	for len(p.buttonDataSnapshot()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	buttonData := p.buttonDataSnapshot()
	if len(buttonData) != 2 || buttonData[0] != "tts:read_last" || buttonData[1] != "act:/review open" {
		t.Fatalf("expected read/review buttons, got %#v", buttonData)
	}
	buttonTexts := p.buttonTextsSnapshot()
	if len(buttonTexts) != 2 || buttonTexts[0] != "Read Aloud" || buttonTexts[1] != "Review" {
		t.Fatalf("expected read/review button labels, got %#v", buttonTexts)
	}
}

func TestReadAloudRequestSynthesizesLatestAssistantReply(t *testing.T) {
	p := &stubButtonPlatform{n: "test"}
	tts := &stubTTS{audio: []byte("audio"), format: "mp3"}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.SetTTSConfig(&TTSCfg{
		Enabled:         true,
		TTS:             tts,
		OfferReadButton: true,
	})

	session := e.sessions.GetOrCreateActive("test:user1")
	session.AddHistory("assistant", "Final summary")

	e.handleMessage(p, &Message{
		SessionKey: "test:user1",
		Platform:   "test",
		ReplyCtx:   "ctx",
		Content:    "tts read last",
	})

	deadline := time.Now().Add(2 * time.Second)
	for len(p.audioSnapshot()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	texts := tts.textsSnapshot()
	if len(texts) != 1 || texts[0] != "Final summary" {
		t.Fatalf("expected latest assistant reply to be synthesized, got %#v", texts)
	}
	audio := p.audioSnapshot()
	if len(audio) != 1 || string(audio[0]) != "audio" {
		t.Fatalf("expected synthesized audio to be sent, got %#v", audio)
	}
	formats := p.audioFormatSnapshot()
	if len(formats) != 1 || formats[0] != "mp3" {
		t.Fatalf("expected mp3 audio format, got %#v", formats)
	}
}

func TestReadAloudRequestWithoutAssistantReply(t *testing.T) {
	p := &stubButtonPlatform{n: "test"}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.SetTTSConfig(&TTSCfg{
		Enabled:         true,
		TTS:             &stubTTS{audio: []byte("audio"), format: "mp3"},
		OfferReadButton: true,
	})

	e.handleMessage(p, &Message{
		SessionKey: "test:user1",
		Platform:   "test",
		ReplyCtx:   "ctx",
		Content:    "tts read last",
	})

	sent := p.sentSnapshot()
	if len(sent) == 0 || sent[len(sent)-1] != "There is no recent assistant reply to read aloud yet." {
		t.Fatalf("expected no-content warning, got %#v", sent)
	}
}

func TestReadAloudRequestSplitsLongTextIntoMultipleChunks(t *testing.T) {
	p := &stubButtonPlatform{n: "test"}
	tts := &stubTTS{audio: []byte("audio"), format: "mp3"}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.SetTTSConfig(&TTSCfg{
		Enabled:         true,
		TTS:             tts,
		Voice:           "Cherry",
		MaxTextLen:      12,
		OfferReadButton: true,
	})

	session := e.sessions.GetOrCreateActive("test:user1")
	session.AddHistory("assistant", "第一段比较长，需要拆开。第二段也比较长，需要继续拆开。")

	e.handleMessage(p, &Message{
		SessionKey: "test:user1",
		Platform:   "test",
		ReplyCtx:   "ctx",
		Content:    "tts read last",
	})

	deadline := time.Now().Add(2 * time.Second)
	for {
		audio := p.audioSnapshot()
		texts := tts.textsSnapshot()
		if len(texts) >= 2 && len(audio) == len(texts) {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	texts := tts.textsSnapshot()
	if len(texts) < 2 {
		t.Fatalf("expected long text to be split into multiple synthesize calls, got %#v", texts)
	}
	for _, chunk := range texts {
		if utf8.RuneCountInString(chunk) > 12 {
			t.Fatalf("chunk %q exceeds max len", chunk)
		}
	}
	audio := p.audioSnapshot()
	if len(audio) != len(texts) {
		t.Fatalf("expected one audio send per chunk, got audio=%d chunks=%d", len(audio), len(texts))
	}
}

func TestSplitTTSChunksPrefersParagraphs(t *testing.T) {
	text := "第一段比较短。\n\n第二段也比较短。\n\n第三段还是比较短。"
	chunks := splitTTSChunks(text, 20)
	if len(chunks) != 3 {
		t.Fatalf("expected 3 paragraph chunks, got %#v", chunks)
	}
	if chunks[0] != "第一段比较短。" || chunks[1] != "第二段也比较短。" || chunks[2] != "第三段还是比较短。" {
		t.Fatalf("unexpected paragraph chunks: %#v", chunks)
	}
}

func TestReadAloudRequestSynthesizesInParallelButSendsInOrder(t *testing.T) {
	p := &stubButtonPlatform{n: "test"}
	tts := &stubTTS{
		format: "mp3",
		delays: map[string]time.Duration{
			"第一段比较短。":   120 * time.Millisecond,
			"第二段也比较短。":  10 * time.Millisecond,
			"第三段还是比较短。": 60 * time.Millisecond,
		},
		audioForText: func(text string) []byte { return []byte(text) },
	}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.SetTTSConfig(&TTSCfg{
		Enabled:         true,
		TTS:             tts,
		Voice:           "Cherry",
		MaxTextLen:      12,
		OfferReadButton: true,
	})

	session := e.sessions.GetOrCreateActive("test:user1")
	session.AddHistory("assistant", "第一段比较短。\n\n第二段也比较短。\n\n第三段还是比较短。")

	e.handleMessage(p, &Message{
		SessionKey: "test:user1",
		Platform:   "test",
		ReplyCtx:   "ctx",
		Content:    "tts read last",
	})

	deadline := time.Now().Add(3 * time.Second)
	for len(p.audioSnapshot()) < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	audio := p.audioSnapshot()
	if len(audio) != 3 {
		t.Fatalf("expected 3 audio chunks, got %#v", audio)
	}
	if string(audio[0]) != "第一段比较短。" || string(audio[1]) != "第二段也比较短。" || string(audio[2]) != "第三段还是比较短。" {
		t.Fatalf("expected audio sends to preserve text order, got %#v", audio)
	}
}
