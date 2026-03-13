package core

import (
	"context"
	"testing"
	"time"
)

type stubTTS struct {
	audio  []byte
	format string
	texts  []string
}

func (s *stubTTS) Synthesize(_ context.Context, text string, _ TTSSynthesisOpts) ([]byte, string, error) {
	s.texts = append(s.texts, text)
	return s.audio, s.format, nil
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
	for len(p.buttonData) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	if len(p.buttonData) != 1 || p.buttonData[0] != "tts:read_last" {
		t.Fatalf("expected read-aloud button, got %#v", p.buttonData)
	}
	if len(p.buttonTexts) != 1 || p.buttonTexts[0] != "Read Aloud" {
		t.Fatalf("expected read-aloud button label, got %#v", p.buttonTexts)
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
	for len(p.audio) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	if len(tts.texts) != 1 || tts.texts[0] != "Final summary" {
		t.Fatalf("expected latest assistant reply to be synthesized, got %#v", tts.texts)
	}
	if len(p.audio) != 1 || string(p.audio[0]) != "audio" {
		t.Fatalf("expected synthesized audio to be sent, got %#v", p.audio)
	}
	if len(p.audioFormat) != 1 || p.audioFormat[0] != "mp3" {
		t.Fatalf("expected mp3 audio format, got %#v", p.audioFormat)
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

	if len(p.sent) == 0 || p.sent[len(p.sent)-1] != "There is no recent assistant reply to read aloud yet." {
		t.Fatalf("expected no-content warning, got %#v", p.sent)
	}
}
