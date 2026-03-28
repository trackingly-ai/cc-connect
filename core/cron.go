package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// CronJob represents a persisted scheduled task.
type CronJob struct {
	ID          string    `json:"id"`
	Project     string    `json:"project"`
	SessionKey  string    `json:"session_key"`
	CronExpr    string    `json:"cron_expr"`
	Prompt      string    `json:"prompt"`
	Exec        string    `json:"exec,omitempty"`     // shell command; mutually exclusive with Prompt
	WorkDir     string    `json:"work_dir,omitempty"` // working directory for exec; empty = agent work_dir
	Description string    `json:"description"`
	Enabled     bool      `json:"enabled"`
	Silent      *bool     `json:"silent,omitempty"` // suppress start notification; nil = use global default
	Mute        bool      `json:"mute,omitempty"`   // suppress all outbound messages for this job
	SessionMode string    `json:"session_mode,omitempty"`
	TimeoutMins *int      `json:"timeout_mins,omitempty"` // nil=30m, 0=unlimited, >0=minutes
	CreatedAt   time.Time `json:"created_at"`
	LastRun     time.Time `json:"last_run,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
}

// IsShellJob returns true if the job executes a shell command directly.
func (j *CronJob) IsShellJob() bool {
	return strings.TrimSpace(j.Exec) != ""
}

const defaultCronJobTimeout = 30 * time.Minute

func (j *CronJob) ExecutionTimeout() time.Duration {
	if j.TimeoutMins == nil {
		return defaultCronJobTimeout
	}
	if *j.TimeoutMins <= 0 {
		return 0
	}
	return time.Duration(*j.TimeoutMins) * time.Minute
}

func NormalizeCronSessionMode(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "", "reuse":
		return ""
	case "new_per_run", "new-per-run":
		return "new_per_run"
	default:
		return s
	}
}

func (j *CronJob) UsesNewSessionPerRun() bool {
	return NormalizeCronSessionMode(j.SessionMode) == "new_per_run"
}

func validateCronJob(j *CronJob) error {
	mode := NormalizeCronSessionMode(j.SessionMode)
	if mode != "" && mode != "new_per_run" {
		return fmt.Errorf("invalid session_mode %q (want reuse, new_per_run, or new-per-run)", j.SessionMode)
	}
	if j.TimeoutMins != nil && *j.TimeoutMins < 0 {
		return fmt.Errorf("timeout_mins must be >= 0")
	}
	return nil
}

// CronStore persists cron jobs to a JSON file.
type CronStore struct {
	path string
	mu   sync.Mutex
	jobs []*CronJob
}

func NewCronStore(dataDir string) (*CronStore, error) {
	dir := filepath.Join(dataDir, "crons")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "jobs.json")
	s := &CronStore{path: path}
	s.load()
	return s, nil
}

func (s *CronStore) load() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	if err := json.Unmarshal(data, &s.jobs); err != nil {
		slog.Error("cron: failed to load jobs", "path", s.path, "error", err)
	}
}

func (s *CronStore) save() error {
	data, err := json.MarshalIndent(s.jobs, "", "  ")
	if err != nil {
		return err
	}
	return AtomicWriteFile(s.path, data, 0o644)
}

func (s *CronStore) Add(job *CronJob) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs = append(s.jobs, job)
	return s.save()
}

func (s *CronStore) Remove(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, j := range s.jobs {
		if j.ID == id {
			s.jobs = append(s.jobs[:i], s.jobs[i+1:]...)
			if err := s.save(); err != nil {
				slog.Error("cron: failed to save jobs after remove", "id", id, "error", err)
			}
			return true
		}
	}
	return false
}

func (s *CronStore) SetEnabled(id string, enabled bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.ID == id {
			j.Enabled = enabled
			if err := s.save(); err != nil {
				slog.Error("cron: failed to save jobs after toggle", "id", id, "enabled", enabled, "error", err)
			}
			return true
		}
	}
	return false
}

func (s *CronStore) MarkRun(id string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.ID == id {
			j.LastRun = time.Now()
			if err != nil {
				j.LastError = err.Error()
			} else {
				j.LastError = ""
			}
			if saveErr := s.save(); saveErr != nil {
				slog.Error("cron: failed to save jobs after mark run", "id", id, "error", saveErr)
			}
			return
		}
	}
}

func (s *CronStore) List() []*CronJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*CronJob, len(s.jobs))
	copy(out, s.jobs)
	return out
}

func (s *CronStore) ListByProject(project string) []*CronJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*CronJob
	for _, j := range s.jobs {
		if j.Project == project {
			out = append(out, j)
		}
	}
	return out
}

func (s *CronStore) ListBySessionKey(sessionKey string) []*CronJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*CronJob
	for _, j := range s.jobs {
		if j.SessionKey == sessionKey {
			out = append(out, j)
		}
	}
	return out
}

func (s *CronStore) Get(id string) *CronJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.ID == id {
			return j
		}
	}
	return nil
}

func (s *CronStore) Update(id, field string, value any) bool {
	readOnly := map[string]bool{
		"id": true, "created_at": true, "last_run": true, "last_error": true,
	}
	if readOnly[strings.ToLower(strings.TrimSpace(field))] {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.ID == id {
			if err := updateCronJobField(j, field, value); err != nil {
				return false
			}
			if err := s.save(); err != nil {
				slog.Error("cron: failed to save jobs after update", "id", id, "field", field, "error", err)
			}
			return true
		}
	}
	return false
}

func updateCronJobField(job *CronJob, field string, value any) error {
	switch field {
	case "project":
		if v, ok := value.(string); ok {
			job.Project = v
			return nil
		}
	case "session_key":
		if v, ok := value.(string); ok {
			job.SessionKey = v
			return nil
		}
	case "cron_expr":
		if v, ok := value.(string); ok {
			job.CronExpr = v
			return nil
		}
	case "prompt":
		if v, ok := value.(string); ok {
			job.Prompt = v
			return nil
		}
	case "exec":
		if v, ok := value.(string); ok {
			job.Exec = v
			return nil
		}
	case "work_dir":
		if v, ok := value.(string); ok {
			job.WorkDir = v
			return nil
		}
	case "description":
		if v, ok := value.(string); ok {
			job.Description = v
			return nil
		}
	case "enabled":
		if v, ok := value.(bool); ok {
			job.Enabled = v
			return nil
		}
	case "silent":
		if v, ok := value.(bool); ok {
			job.Silent = &v
			return nil
		}
	case "mute":
		if v, ok := value.(bool); ok {
			job.Mute = v
			return nil
		}
	case "session_mode":
		if v, ok := value.(string); ok {
			job.SessionMode = NormalizeCronSessionMode(v)
			return nil
		}
	case "timeout_mins":
		switch v := value.(type) {
		case int:
			job.TimeoutMins = &v
			return nil
		case float64:
			n := int(v)
			job.TimeoutMins = &n
			return nil
		}
	}

	return fmt.Errorf("unknown or invalid field: %s", field)
}

// CronScheduler runs cron jobs by injecting synthetic messages into engines.
type CronScheduler struct {
	store         *CronStore
	cron          *cron.Cron
	engines       map[string]*Engine // project name → engine
	mu            sync.RWMutex
	entries       map[string]cron.EntryID // job ID → cron entry
	defaultSilent bool                    // global default for suppressing cron start notifications
}

func NewCronScheduler(store *CronStore) *CronScheduler {
	return &CronScheduler{
		store:   store,
		cron:    cron.New(),
		engines: make(map[string]*Engine),
		entries: make(map[string]cron.EntryID),
	}
}

func (cs *CronScheduler) RegisterEngine(name string, e *Engine) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.engines[name] = e
}

func (cs *CronScheduler) SetDefaultSilent(silent bool) {
	cs.defaultSilent = silent
}

// IsSilent returns whether the cron job should suppress the start notification.
func (cs *CronScheduler) IsSilent(job *CronJob) bool {
	if job.Silent != nil {
		return *job.Silent
	}
	return cs.defaultSilent
}

func (cs *CronScheduler) Start() error {
	jobs := cs.store.List()
	for _, job := range jobs {
		if job.Enabled {
			if err := cs.scheduleJob(job); err != nil {
				slog.Warn("cron: failed to schedule job", "id", job.ID, "error", err)
			}
		}
	}
	cs.cron.Start()
	slog.Info("cron: scheduler started", "jobs", len(jobs))
	return nil
}

func (cs *CronScheduler) Stop() {
	cs.cron.Stop()
}

func (cs *CronScheduler) AddJob(job *CronJob) error {
	if err := validateCronJob(job); err != nil {
		return err
	}
	job.SessionMode = NormalizeCronSessionMode(job.SessionMode)
	if _, err := cron.ParseStandard(job.CronExpr); err != nil {
		return fmt.Errorf("invalid cron expression %q: %w", job.CronExpr, err)
	}
	if err := cs.store.Add(job); err != nil {
		return err
	}
	if job.Enabled {
		return cs.scheduleJob(job)
	}
	return nil
}

func (cs *CronScheduler) RemoveJob(id string) bool {
	cs.mu.Lock()
	if entryID, ok := cs.entries[id]; ok {
		cs.cron.Remove(entryID)
		delete(cs.entries, id)
	}
	cs.mu.Unlock()
	return cs.store.Remove(id)
}

func (cs *CronScheduler) EnableJob(id string) error {
	if !cs.store.SetEnabled(id, true) {
		return fmt.Errorf("job %q not found", id)
	}
	job := cs.store.Get(id)
	if job != nil {
		return cs.scheduleJob(job)
	}
	return nil
}

func (cs *CronScheduler) DisableJob(id string) error {
	if !cs.store.SetEnabled(id, false) {
		return fmt.Errorf("job %q not found", id)
	}
	cs.mu.Lock()
	if entryID, ok := cs.entries[id]; ok {
		cs.cron.Remove(entryID)
		delete(cs.entries, id)
	}
	cs.mu.Unlock()
	return nil
}

func (cs *CronScheduler) UpdateJob(id, field string, value any) error {
	job := cs.store.Get(id)
	if job == nil {
		return fmt.Errorf("job %q not found", id)
	}
	proposed := cloneCronJob(job)
	if field == "cron_expr" {
		expr, ok := value.(string)
		if !ok {
			return fmt.Errorf("cron_expr must be a string")
		}
		if _, err := cron.ParseStandard(expr); err != nil {
			return fmt.Errorf("invalid cron expression %q: %w", expr, err)
		}
	}
	if err := updateCronJobField(proposed, field, value); err != nil {
		return fmt.Errorf("failed to update field %q (may be read-only or invalid type)", field)
	}
	if err := validateCronJob(proposed); err != nil {
		return err
	}

	needsReschedule := field == "cron_expr" || field == "enabled"
	if needsReschedule {
		cs.mu.Lock()
		if entryID, ok := cs.entries[id]; ok {
			cs.cron.Remove(entryID)
			delete(cs.entries, id)
		}
		cs.mu.Unlock()
	}

	if !cs.store.Update(id, field, value) {
		return fmt.Errorf("failed to update field %q (may be read-only or invalid type)", field)
	}

	updated := cs.store.Get(id)
	if needsReschedule && updated != nil && updated.Enabled {
		if err := cs.scheduleJob(updated); err != nil {
			return fmt.Errorf("reschedule failed: %w", err)
		}
	}
	return nil
}

func cloneCronJob(job *CronJob) *CronJob {
	if job == nil {
		return nil
	}
	clone := *job
	if job.TimeoutMins != nil {
		v := *job.TimeoutMins
		clone.TimeoutMins = &v
	}
	if job.Silent != nil {
		v := *job.Silent
		clone.Silent = &v
	}
	return &clone
}

func (cs *CronScheduler) Store() *CronStore {
	return cs.store
}

// NextRun returns the next scheduled run time for a job, or zero if not scheduled.
func (cs *CronScheduler) NextRun(jobID string) time.Time {
	cs.mu.RLock()
	entryID, ok := cs.entries[jobID]
	cs.mu.RUnlock()
	if !ok {
		return time.Time{}
	}
	for _, e := range cs.cron.Entries() {
		if e.ID == entryID {
			return e.Next
		}
	}
	return time.Time{}
}

func (cs *CronScheduler) scheduleJob(job *CronJob) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	// Remove existing schedule if any
	if old, ok := cs.entries[job.ID]; ok {
		cs.cron.Remove(old)
	}

	jobID := job.ID
	entryID, err := cs.cron.AddFunc(job.CronExpr, func() {
		cs.executeJob(jobID)
	})
	if err != nil {
		return err
	}
	cs.entries[jobID] = entryID
	return nil
}

func (cs *CronScheduler) executeJob(jobID string) {
	job := cs.store.Get(jobID)
	if job == nil || !job.Enabled {
		return
	}

	cs.mu.RLock()
	engine, ok := cs.engines[job.Project]
	cs.mu.RUnlock()

	if !ok {
		slog.Error("cron: project not found", "job", jobID, "project", job.Project)
		cs.store.MarkRun(jobID, fmt.Errorf("project %q not found", job.Project))
		return
	}

	slog.Info("cron: executing job", "id", jobID, "project", job.Project, "prompt", truncateStr(job.Prompt, 60))

	jobCtx := context.Background()
	cancel := func() {}
	done := make(chan error, 1)
	timeout := job.ExecutionTimeout()
	if timeout > 0 {
		jobCtx, cancel = context.WithTimeout(context.Background(), timeout)
	}
	defer cancel()
	go func() {
		done <- engine.ExecuteCronJob(jobCtx, job)
	}()

	err := <-done
	if timeout > 0 && err == context.DeadlineExceeded {
		err = fmt.Errorf("job timed out after %v", timeout)
	}

	cs.store.MarkRun(jobID, err)

	if err != nil {
		slog.Error("cron: job failed", "id", jobID, "error", err)
	} else {
		slog.Info("cron: job completed", "id", jobID)
	}
}

func GenerateCronID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		slog.Warn("cron: failed to generate random id bytes", "error", err)
		return fmt.Sprintf("%08x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

var cronWeekdays = map[Language][7]string{
	LangEnglish:            {"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"},
	LangChinese:            {"周日", "周一", "周二", "周三", "周四", "周五", "周六"},
	LangTraditionalChinese: {"週日", "週一", "週二", "週三", "週四", "週五", "週六"},
	LangJapanese:           {"日曜", "月曜", "火曜", "水曜", "木曜", "金曜", "土曜"},
	LangSpanish:            {"domingo", "lunes", "martes", "miércoles", "jueves", "viernes", "sábado"},
}

var cronMonths = map[Language][13]string{
	LangEnglish:            {"", "Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"},
	LangChinese:            {"", "1月", "2月", "3月", "4月", "5月", "6月", "7月", "8月", "9月", "10月", "11月", "12月"},
	LangTraditionalChinese: {"", "1月", "2月", "3月", "4月", "5月", "6月", "7月", "8月", "9月", "10月", "11月", "12月"},
	LangJapanese:           {"", "1月", "2月", "3月", "4月", "5月", "6月", "7月", "8月", "9月", "10月", "11月", "12月"},
	LangSpanish:            {"", "ene", "feb", "mar", "abr", "may", "jun", "jul", "ago", "sep", "oct", "nov", "dic"},
}

func cronLangNames(lang Language) (weekdays [7]string, months [13]string) {
	if w, ok := cronWeekdays[lang]; ok {
		weekdays = w
	} else {
		weekdays = cronWeekdays[LangEnglish]
	}
	if m, ok := cronMonths[lang]; ok {
		months = m
	} else {
		months = cronMonths[LangEnglish]
	}
	return
}

func isZhLikeLang(lang Language) bool {
	return lang == LangChinese || lang == LangTraditionalChinese || lang == LangJapanese
}

// CronExprToHuman converts a standard 5-field cron expression to a human-readable string.
func CronExprToHuman(expr string, lang Language) string {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return expr
	}
	minute, hour, dom, month, dow := fields[0], fields[1], fields[2], fields[3], fields[4]
	weekdays, months := cronLangNames(lang)
	cjk := isZhLikeLang(lang)

	var parts []string

	// Weekday
	if dow != "*" {
		var n int
		if _, err := fmt.Sscanf(dow, "%d", &n); err == nil {
			if n >= 0 && n <= 6 {
				if cjk {
					parts = append(parts, weekdays[n])
				} else {
					parts = append(parts, "Every "+weekdays[n])
				}
			}
		} else {
			parts = append(parts, "weekday("+dow+")")
		}
	}

	// Month
	if month != "*" {
		var n int
		if _, err := fmt.Sscanf(month, "%d", &n); err == nil {
			if n >= 1 && n <= 12 {
				parts = append(parts, months[n])
			}
		}
	}

	// Day of month
	if dom != "*" {
		if cjk {
			parts = append(parts, dom+"日")
		} else {
			parts = append(parts, "day "+dom)
		}
	}

	// Time
	if hour != "*" && minute != "*" {
		parts = append(parts, fmt.Sprintf("%s:%s", padZero(hour), padZero(minute)))
	} else if hour != "*" {
		if cjk {
			parts = append(parts, hour+"時")
		} else {
			parts = append(parts, "hour "+hour)
		}
	} else if minute != "*" {
		switch lang {
		case LangChinese, LangTraditionalChinese:
			parts = append(parts, "每小时第"+minute+"分")
		case LangJapanese:
			parts = append(parts, "毎時"+minute+"分")
		default:
			parts = append(parts, "minute "+minute+" of every hour")
		}
	}

	// Frequency hint
	if dow == "*" && month == "*" && dom == "*" {
		switch lang {
		case LangChinese, LangTraditionalChinese:
			return "每天 " + strings.Join(parts, " ")
		case LangJapanese:
			return "毎日 " + strings.Join(parts, " ")
		case LangSpanish:
			return "Diario a las " + strings.Join(parts, " ")
		default:
			return "Daily at " + strings.Join(parts, " ")
		}
	}
	if dow != "*" && month == "*" && dom == "*" {
		switch lang {
		case LangChinese, LangTraditionalChinese:
			return "每" + strings.Join(parts, " ")
		case LangJapanese:
			return "毎" + strings.Join(parts, " ")
		default:
			return strings.Join(parts, " at ")
		}
	}
	if dom != "*" && month == "*" && dow == "*" {
		switch lang {
		case LangChinese, LangTraditionalChinese:
			return "每月" + strings.Join(parts, " ")
		case LangJapanese:
			return "毎月" + strings.Join(parts, " ")
		case LangSpanish:
			return "Mensual, " + strings.Join(parts, ", ")
		default:
			return "Monthly, " + strings.Join(parts, ", ")
		}
	}

	if cjk {
		return strings.Join(parts, " ")
	}
	return strings.Join(parts, ", ")
}

func padZero(s string) string {
	if len(s) == 1 {
		return "0" + s
	}
	return s
}
