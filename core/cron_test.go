package core

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type cronShellAgent struct {
	stubAgent
	workDir string
}

func (a *cronShellAgent) GetWorkDir() string { return a.workDir }

func TestAPIServerHandleCronAddRequiresSessionKey(t *testing.T) {
	store, err := NewCronStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewCronStore: %v", err)
	}

	engine := NewEngine("test", &stubAgent{}, nil, "", LangEnglish)
	server := &APIServer{
		engines: map[string]*Engine{"test": engine},
		cron:    NewCronScheduler(store),
	}

	body := strings.NewReader(`{"project":"test","cron_expr":"0 6 * * *","prompt":"hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/cron/add", body)
	rec := httptest.NewRecorder()

	server.handleCronAdd(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "session_key is required") {
		t.Fatalf("body = %q, want session_key error", rec.Body.String())
	}
}

func TestEngineExecuteCronShell(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	engine := NewEngine("test", &cronShellAgent{workDir: t.TempDir()}, []Platform{p}, "", LangEnglish)

	job := &CronJob{
		Exec:    "printf hello",
		WorkDir: t.TempDir(),
	}
	if err := engine.executeCronShell(p, nil, job); err != nil {
		t.Fatalf("executeCronShell: %v", err)
	}
	if len(p.sent) != 1 {
		t.Fatalf("sent count = %d, want 1", len(p.sent))
	}
	if !strings.Contains(p.sent[0], "hello") {
		t.Fatalf("sent message = %q, want command output", p.sent[0])
	}
}

func TestAPIServerHandleCronAddAcceptsExec(t *testing.T) {
	store, err := NewCronStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewCronStore: %v", err)
	}

	engine := NewEngine("test", &stubAgent{}, nil, "", LangEnglish)
	engine.interactiveStates["telegram:1:1"] = &interactiveState{}
	server := &APIServer{
		engines: map[string]*Engine{"test": engine},
		cron:    NewCronScheduler(store),
	}

	body := strings.NewReader(`{"project":"test","session_key":"telegram:1:1","cron_expr":"0 6 * * *","exec":"printf hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/cron/add", body)
	rec := httptest.NewRecorder()

	server.handleCronAdd(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var job CronJob
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if job.Exec != "printf hello" {
		t.Fatalf("job.Exec = %q, want %q", job.Exec, "printf hello")
	}
}

func TestEngineActiveSessionKeys(t *testing.T) {
	engine := NewEngine("test", &stubAgent{}, nil, "", LangEnglish)
	engine.interactiveStates["b"] = &interactiveState{}
	engine.interactiveStates["a"] = &interactiveState{}

	got := engine.ActiveSessionKeys()
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("ActiveSessionKeys() = %v, want [a b]", got)
	}
}
