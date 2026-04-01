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

type stubNoFilePlatform struct {
	n string
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

func (p *stubNoFilePlatform) Name() string               { return p.n }
func (p *stubNoFilePlatform) Start(MessageHandler) error { return nil }
func (p *stubNoFilePlatform) Reply(context.Context, any, string) error {
	return nil
}
func (p *stubNoFilePlatform) Send(context.Context, any, string) error { return nil }
func (p *stubNoFilePlatform) Stop() error                             { return nil }

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

func TestEngineSendFileToSessionRejectsRelativePath(t *testing.T) {
	p := &stubFilePlatform{n: "stub"}
	e := NewEngine("proj", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.interactiveStates["session-1"] = &interactiveState{
		platform: p,
		replyCtx: "reply",
	}

	err := e.SendFileToSession("session-1", "report.pdf", "latest report")
	if err == nil || !strings.Contains(err.Error(), "path must be absolute") {
		t.Fatalf("SendFileToSession relative path err = %v, want absolute-path error", err)
	}
}

func TestEngineSendFileToSessionRejectsUnsupportedPlatform(t *testing.T) {
	p := &stubNoFilePlatform{n: "stub"}
	e := NewEngine("proj", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.interactiveStates["session-1"] = &interactiveState{
		platform: p,
		replyCtx: "reply",
	}

	err := e.SendFileToSession("session-1", "/tmp/report.pdf", "latest report")
	if err == nil || !strings.Contains(err.Error(), "does not support sending files") {
		t.Fatalf("SendFileToSession unsupported platform err = %v", err)
	}
}

func TestAPIServerHandleSendFileRejectsEmptyPath(t *testing.T) {
	api := &APIServer{
		mux:     http.NewServeMux(),
		engines: map[string]*Engine{},
	}

	req := httptest.NewRequest(http.MethodPost, "/send-file", strings.NewReader(`{"project":"proj","path":"   "}`))
	rec := httptest.NewRecorder()

	api.handleSendFile(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /send-file empty path status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestAgentSystemPromptMentionsSendFile(t *testing.T) {
	prompt := AgentSystemPrompt()
	if !strings.Contains(prompt, "cc-connect send-file") || !strings.Contains(prompt, "--path <absolute_file_path>") {
		t.Fatalf("AgentSystemPrompt missing send-file instructions: %q", prompt)
	}
}
