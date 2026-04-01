package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type stubFilePlatform struct {
	n           string
	sentPath    string
	sentCaption string
}

func (p *stubFilePlatform) Name() string               { return p.n }
func (p *stubFilePlatform) Start(MessageHandler) error { return nil }
func (p *stubFilePlatform) Reply(context.Context, any, string) error {
	return nil
}
func (p *stubFilePlatform) Send(context.Context, any, string) error { return nil }
func (p *stubFilePlatform) Stop() error                             { return nil }
func (p *stubFilePlatform) SendFile(_ context.Context, _ any, path string, caption string) error {
	p.sentPath = path
	p.sentCaption = caption
	return nil
}

func TestEngineSendFileToSession(t *testing.T) {
	p := &stubFilePlatform{n: "stub"}
	e := NewEngine("proj", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.interactiveStates["session-1"] = &interactiveState{
		platform: p,
		replyCtx: "reply",
	}

	err := e.SendFileToSession("session-1", "/tmp/report.pdf", "latest report")
	if err != nil {
		t.Fatalf("SendFileToSession: %v", err)
	}
	if p.sentPath != "/tmp/report.pdf" {
		t.Fatalf("sentPath = %q, want /tmp/report.pdf", p.sentPath)
	}
	if p.sentCaption != "latest report" {
		t.Fatalf("sentCaption = %q, want latest report", p.sentCaption)
	}
}

func TestAPIServerHandleSendFile(t *testing.T) {
	p := &stubFilePlatform{n: "stub"}
	e := NewEngine("proj", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.interactiveStates["session-1"] = &interactiveState{
		platform: p,
		replyCtx: "reply",
	}

	api := &APIServer{
		mux:     http.NewServeMux(),
		engines: map[string]*Engine{"proj": e},
	}

	req := httptest.NewRequest(http.MethodPost, "/send-file", strings.NewReader(`{
		"project":"proj",
		"session_key":"session-1",
		"path":"/tmp/report.pdf",
		"caption":"latest report"
	}`))
	rec := httptest.NewRecorder()

	api.handleSendFile(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /send-file status = %d, want %d", rec.Code, http.StatusOK)
	}
	if p.sentPath != "/tmp/report.pdf" {
		t.Fatalf("sentPath = %q, want /tmp/report.pdf", p.sentPath)
	}
	if p.sentCaption != "latest report" {
		t.Fatalf("sentCaption = %q, want latest report", p.sentCaption)
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("status = %q, want ok", body["status"])
	}
}

func TestAgentSystemPromptMentionsSendFile(t *testing.T) {
	prompt := AgentSystemPrompt()
	if !strings.Contains(prompt, "cc-connect send-file") || !strings.Contains(prompt, "--path <absolute_file_path>") {
		t.Fatalf("AgentSystemPrompt missing send-file instructions: %q", prompt)
	}
}
