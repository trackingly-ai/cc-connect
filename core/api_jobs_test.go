package core

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func waitForJobCompletion(t *testing.T, jm *JobManager, jobID string) *Job {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		job, ok := jm.Get(jobID)
		if ok && isTerminalJobStatus(job.Status) {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s did not complete", jobID)
	return nil
}

func TestAPIServerJobEndpoints(t *testing.T) {
	jm, err := NewJobManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewJobManager: %v", err)
	}

	session := &jobTestSession{events: make(chan Event, 3)}
	session.onSend = func(prompt string) {
		session.events <- Event{Type: EventText, Content: "partial "}
		session.events <- Event{Type: EventResult, Content: "done", SessionID: "session-9"}
		close(session.events)
	}
	engine := NewEngine("proj-jobs", &jobTestAgent{session: session}, nil, "", LangEnglish)

	api := &APIServer{
		mux:     http.NewServeMux(),
		engines: make(map[string]*Engine),
	}
	api.SetJobManager(jm)
	api.RegisterEngine("proj-jobs", engine)

	body := strings.NewReader(`{
		"project":"proj-jobs",
		"task_id":"task-42",
		"prompt":"implement task",
		"timeout_sec":30,
		"workspace_ref":{"repo_path":"/repo","branch":"echo/task-42"}
	}`)
	req := httptest.NewRequest(http.MethodPost, "/jobs", body)
	rec := httptest.NewRecorder()

	api.handleJobs(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST /jobs status = %d, want %d", rec.Code, http.StatusAccepted)
	}

	var started Job
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatalf("decode start response: %v", err)
	}
	if started.Project != "proj-jobs" {
		t.Fatalf("Project = %q, want proj-jobs", started.Project)
	}

	completed := waitForJobCompletion(t, jm, started.ID)
	if completed.Status != JobStatusCompleted {
		t.Fatalf("final status = %s, want %s", completed.Status, JobStatusCompleted)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/jobs/"+started.ID, nil)
	getRec := httptest.NewRecorder()
	api.handleJobByID(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET /jobs/{id} status = %d, want %d", getRec.Code, http.StatusOK)
	}

	var fetched Job
	if err := json.Unmarshal(getRec.Body.Bytes(), &fetched); err != nil {
		t.Fatalf("decode get response: %v", err)
	}
	if fetched.ID != started.ID {
		t.Fatalf("fetched id = %q, want %q", fetched.ID, started.ID)
	}
	if fetched.Output != "done" {
		t.Fatalf("fetched output = %q, want done", fetched.Output)
	}
	if len(fetched.Events) != 2 {
		t.Fatalf("len(fetched.Events) = %d, want 2", len(fetched.Events))
	}
	if fetched.Events[0].Type != string(EventText) {
		t.Fatalf("fetched first event type = %q, want %q", fetched.Events[0].Type, EventText)
	}
	if fetched.Events[1].Type != string(EventResult) {
		t.Fatalf("fetched second event type = %q, want %q", fetched.Events[1].Type, EventResult)
	}
	if fetched.EventCount != 2 {
		t.Fatalf("fetched event_count = %d, want 2", fetched.EventCount)
	}

	getSlashReq := httptest.NewRequest(http.MethodGet, "/jobs/"+started.ID+"/", nil)
	getSlashRec := httptest.NewRecorder()
	api.handleJobByID(getSlashRec, getSlashReq)
	if getSlashRec.Code != http.StatusOK {
		t.Fatalf(
			"GET /jobs/{id}/ status = %d, want %d",
			getSlashRec.Code,
			http.StatusOK,
		)
	}

	listReq := httptest.NewRequest(http.MethodGet, "/jobs", nil)
	listRec := httptest.NewRecorder()
	api.handleJobs(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("GET /jobs status = %d, want %d", listRec.Code, http.StatusOK)
	}
	var jobs []Job
	if err := json.Unmarshal(listRec.Body.Bytes(), &jobs); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("len(jobs) = %d, want 1", len(jobs))
	}
}

func TestAPIServerJobEndpointsRequireManager(t *testing.T) {
	api := &APIServer{}

	req := httptest.NewRequest(http.MethodGet, "/jobs", nil)
	rec := httptest.NewRecorder()
	api.handleJobs(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /jobs status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestAPIServerJobEndpointsRejectUnknownJob(t *testing.T) {
	jm, err := NewJobManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewJobManager: %v", err)
	}
	api := &APIServer{}
	api.SetJobManager(jm)

	req := httptest.NewRequest(http.MethodGet, "/jobs/missing", nil)
	rec := httptest.NewRecorder()
	api.handleJobByID(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET missing job status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestAPIServerJobEndpointsRejectUnknownProject(t *testing.T) {
	jm, err := NewJobManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewJobManager: %v", err)
	}
	api := &APIServer{}
	api.SetJobManager(jm)

	req := httptest.NewRequest(
		http.MethodPost,
		"/jobs",
		strings.NewReader(`{"project":"missing","prompt":"hi"}`),
	)
	rec := httptest.NewRecorder()
	api.handleJobs(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /jobs unknown project = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestAPIServerRegisterEngineRegistersJobRunner(t *testing.T) {
	jm, err := NewJobManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewJobManager: %v", err)
	}

	api := &APIServer{engines: make(map[string]*Engine)}
	api.SetJobManager(jm)
	session := &jobTestSession{events: make(chan Event, 1)}
	session.onSend = func(prompt string) {
		session.events <- Event{Type: EventResult, Content: "ok"}
		close(session.events)
	}
	api.RegisterEngine("proj", NewEngine("proj", &jobTestAgent{
		session: session,
	}, nil, "", LangEnglish))

	job, err := jm.Start(JobRequest{Project: "proj", Prompt: "ping"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	completed := waitForJobCompletion(t, jm, job.ID)
	if completed.Project != "proj" {
		t.Fatalf("job project = %q, want proj", completed.Project)
	}
}
