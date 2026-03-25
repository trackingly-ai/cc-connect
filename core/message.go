package core

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// MergeEnv returns base env with entries from extra overriding same-key entries.
// This prevents duplicate keys (e.g. two PATH entries) which cause the override
// to be silently ignored on Linux (getenv returns the first match).
func MergeEnv(base, extra []string) []string {
	keys := make(map[string]bool, len(extra))
	for _, e := range extra {
		if k, _, ok := strings.Cut(e, "="); ok {
			keys[k] = true
		}
	}
	merged := make([]string, 0, len(base)+len(extra))
	for _, e := range base {
		if k, _, ok := strings.Cut(e, "="); ok && keys[k] {
			continue
		}
		merged = append(merged, e)
	}
	return append(merged, extra...)
}

// AllowList checks whether a user ID is permitted based on a comma-separated
// allow_from string. Returns true if allowFrom is empty or "*" (allow all),
// or if the userID is in the list. Comparison is case-insensitive.
func AllowList(allowFrom, userID string) bool {
	allowFrom = strings.TrimSpace(allowFrom)
	if allowFrom == "" || allowFrom == "*" {
		return true
	}
	for _, id := range strings.Split(allowFrom, ",") {
		if strings.EqualFold(strings.TrimSpace(id), userID) {
			return true
		}
	}
	return false
}

// ImageAttachment represents an image sent by the user.
type ImageAttachment struct {
	MimeType string // e.g. "image/png", "image/jpeg"
	Data     []byte // raw image bytes
	FileName string // original filename (optional)
}

// FileAttachment represents a file (PDF, doc, spreadsheet, etc.) sent by the user.
type FileAttachment struct {
	MimeType string // e.g. "application/pdf", "text/plain"
	Data     []byte // raw file bytes
	FileName string // original filename
}

// SaveImagesToDisk saves image attachments to workDir/.cc-connect/images/
// and returns absolute paths for agent-side multimodal CLI flags.
func SaveImagesToDisk(workDir string, images []ImageAttachment) []string {
	if len(images) == 0 {
		return nil
	}
	imageDir := filepath.Join(workDir, ".cc-connect", "images")
	if err := os.MkdirAll(imageDir, 0o755); err != nil {
		slog.Error("SaveImagesToDisk: mkdir failed", "error", err)
		return nil
	}

	paths := make([]string, 0, len(images))
	for i, img := range images {
		fname := strings.TrimSpace(img.FileName)
		if fname == "" {
			fname = fmt.Sprintf("image_%d_%d%s", time.Now().UnixMilli(), i, extFromMime(img.MimeType))
		}
		fpath, err := createAttachmentPath(imageDir, fname, i)
		if err != nil {
			slog.Error("SaveImagesToDisk: create path failed", "error", err, "name", fname)
			continue
		}
		if err := os.WriteFile(fpath, img.Data, 0o644); err != nil {
			slog.Error("SaveImagesToDisk: write failed", "error", err, "name", fname, "path", fpath)
			continue
		}
		paths = append(paths, fpath)
	}
	return paths
}

// SaveFilesToDisk saves file attachments to workDir/.cc-connect/attachments/
// and returns absolute paths for agent-side file reading.
func SaveFilesToDisk(workDir string, files []FileAttachment) []string {
	if len(files) == 0 {
		return nil
	}
	attachDir := filepath.Join(workDir, ".cc-connect", "attachments")
	if err := os.MkdirAll(attachDir, 0o755); err != nil {
		slog.Error("SaveFilesToDisk: mkdir failed", "error", err)
		return nil
	}

	paths := make([]string, 0, len(files))
	for i, f := range files {
		fpath, err := createAttachmentPath(attachDir, f.FileName, i)
		if err != nil {
			slog.Error("SaveFilesToDisk: create path failed", "error", err, "name", f.FileName)
			continue
		}
		if err := os.WriteFile(fpath, f.Data, 0o644); err != nil {
			slog.Error("SaveFilesToDisk: write failed", "error", err, "name", f.FileName, "path", fpath)
			continue
		}
		paths = append(paths, fpath)
	}
	return paths
}

var attachmentNameSanitizer = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func createAttachmentPath(dir, originalName string, index int) (string, error) {
	fname := sanitizeAttachmentFileName(originalName, index)
	ext := filepath.Ext(fname)
	base := strings.TrimSuffix(fname, ext)
	pattern := base + "-*"
	if ext != "" {
		pattern += ext
	}
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	path := f.Name()
	if err := f.Close(); err != nil {
		return "", err
	}
	return path, nil
}

func sanitizeAttachmentFileName(originalName string, index int) string {
	fname := strings.TrimSpace(filepath.Base(originalName))
	if fname == "" || fname == "." || fname == string(filepath.Separator) {
		return fmt.Sprintf("file_%d_%d", time.Now().UnixMilli(), index)
	}
	ext := filepath.Ext(fname)
	base := strings.TrimSuffix(fname, ext)
	base = attachmentNameSanitizer.ReplaceAllString(base, "_")
	base = strings.Trim(base, "._-")
	if base == "" {
		base = fmt.Sprintf("file_%d_%d", time.Now().UnixMilli(), index)
	}
	ext = attachmentNameSanitizer.ReplaceAllString(ext, "")
	return base + ext
}

func extFromMime(mime string) string {
	switch mime {
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ".png"
	}
}

// AppendFileRefs appends saved file path references to a prompt.
func AppendFileRefs(prompt string, filePaths []string) string {
	if len(filePaths) == 0 {
		return prompt
	}
	if prompt == "" {
		prompt = "Please analyze the attached file(s)."
	}
	return prompt + "\n\n(Files saved locally, please read them: " + strings.Join(filePaths, ", ") + ")"
}

// AudioAttachment represents a voice/audio message sent by the user.
type AudioAttachment struct {
	MimeType string // e.g. "audio/amr", "audio/ogg", "audio/mp4"
	Data     []byte // raw audio bytes
	Format   string // short format hint: "amr", "ogg", "m4a", "mp3", "wav", etc.
	Duration int    // duration in seconds (if known)
}

// Message represents a unified incoming message from any platform.
type Message struct {
	SessionKey      string // unique key for user context, e.g. "feishu:{chatID}:{userID}"
	Platform        string
	MessageID       string // platform message ID for tracing
	UserID          string
	UserName        string
	Content         string
	QuotedMessageID string
	QuotedUserID    string
	QuotedUserName  string
	QuotedContent   string
	Images          []ImageAttachment // attached images (if any)
	Files           []FileAttachment  // attached files (if any)
	Audio           *AudioAttachment  // voice message (if any)
	ReplyCtx        any               // platform-specific context needed for replying
	FromVoice       bool              // true if message originated from voice transcription
}

func (m *Message) HasQuotedContent() bool {
	return m != nil && strings.TrimSpace(m.QuotedContent) != ""
}

// EventType distinguishes different kinds of agent output.
type EventType string

const (
	EventText              EventType = "text"               // intermediate or final text
	EventToolUse           EventType = "tool_use"           // tool invocation info
	EventToolResult        EventType = "tool_result"        // tool execution result
	EventResult            EventType = "result"             // final aggregated result
	EventError             EventType = "error"              // error occurred
	EventPermissionRequest EventType = "permission_request" // agent requests permission via stdio protocol
	EventThinking          EventType = "thinking"           // thinking/processing status
)

// Event represents a single piece of agent output streamed back to the engine.
type Event struct {
	Type         EventType
	Content      string
	ToolName     string         // populated for EventToolUse, EventPermissionRequest
	ToolInput    string         // human-readable summary of tool input
	ToolInputRaw map[string]any // raw tool input (for EventPermissionRequest, used in allow response)
	ToolResult   string         // populated for EventToolResult
	SessionID    string         // agent-managed session ID for conversation continuity
	RequestID    string         // unique request ID for EventPermissionRequest
	Done         bool
	Error        error
}

// HistoryEntry is one turn in a conversation.
type HistoryEntry struct {
	Role      string    `json:"role"` // "user" or "assistant"
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp"`
}

// AgentSessionInfo describes one session as reported by the agent backend.
type AgentSessionInfo struct {
	ID           string
	Summary      string
	MessageCount int
	ModifiedAt   time.Time
	GitBranch    string
}
