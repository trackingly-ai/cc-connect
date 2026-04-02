package claudecode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/draw"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// claudeSession manages a long-running Claude Code process using
// --input-format stream-json and --permission-prompt-tool stdio.
//
// In "auto" mode, permission requests are auto-approved internally
// (avoiding --dangerously-skip-permissions which fails under root).
type claudeSession struct {
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	stdinMu     sync.Mutex
	events      chan core.Event
	recentMu    sync.Mutex
	recentTools []string
	sessionID   atomic.Value // stores string
	autoApprove bool         // auto mode: approve all permission requests
	workDir     string
	ctx         context.Context
	cancel      context.CancelFunc
	done        chan struct{}
	alive       atomic.Bool
}

var heicImageConverter = convertHEICImage

func newClaudeSession(ctx context.Context, workDir, model, sessionID, mode string, allowedTools []string, extraEnv []string) (*claudeSession, error) {
	sessionCtx, cancel := context.WithCancel(ctx)

	args := buildClaudeSessionArgs(model, sessionID, mode, allowedTools)

	slog.Debug("claudeSession: starting", "args", core.RedactArgs(args), "dir", workDir, "mode", mode)

	cmd := exec.CommandContext(sessionCtx, "claude", args...)
	cmd.Dir = workDir
	// Filter out CLAUDECODE env var to prevent "nested session" detection,
	// since cc-connect is a bridge, not a nested Claude Code session.
	env := filterEnv(os.Environ(), "CLAUDECODE")
	if len(extraEnv) > 0 {
		env = core.MergeEnv(env, extraEnv)
	}
	cmd.Env = env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("claudeSession: stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("claudeSession: stdout pipe: %w", err)
	}

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("claudeSession: start: %w", err)
	}

	cs := &claudeSession{
		cmd:         cmd,
		stdin:       stdin,
		events:      make(chan core.Event, 64),
		autoApprove: mode == "bypassPermissions",
		workDir:     workDir,
		ctx:         sessionCtx,
		cancel:      cancel,
		done:        make(chan struct{}),
	}
	cs.sessionID.Store(sessionID)
	cs.alive.Store(true)

	go cs.readLoop(stdout, &stderrBuf)

	return cs, nil
}

func buildClaudeSessionArgs(model, sessionID, mode string, allowedTools []string) []string {
	args := []string{
		"--output-format", "stream-json",
		"--verbose",
		"--input-format", "stream-json",
		"--permission-prompt-tool", "stdio",
	}

	if mode != "" && mode != "default" {
		args = append(args, "--permission-mode", mode)
	}
	if isClaudeSessionID(sessionID) {
		args = append(args, "--resume", sessionID)
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	if len(allowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(allowedTools, ","))
	}
	if sysPrompt := core.AgentSystemPrompt(); sysPrompt != "" {
		args = append(args, "--append-system-prompt", sysPrompt)
	}

	return args
}

func isClaudeSessionID(sessionID string) bool {
	if len(sessionID) != 36 {
		return false
	}
	for idx, ch := range sessionID {
		switch idx {
		case 8, 13, 18, 23:
			if ch != '-' {
				return false
			}
		default:
			if (ch < '0' || ch > '9') &&
				(ch < 'a' || ch > 'f') &&
				(ch < 'A' || ch > 'F') {
				return false
			}
		}
	}
	return true
}

func (cs *claudeSession) readLoop(stdout io.ReadCloser, stderrBuf *bytes.Buffer) {
	defer func() {
		cs.alive.Store(false)
		if err := cs.cmd.Wait(); err != nil {
			stderrMsg := strings.TrimSpace(stderrBuf.String())
			if stderrMsg != "" {
				slog.Error("claudeSession: process failed", "error", err, "stderr", stderrMsg)
				evt := core.Event{Type: core.EventError, Error: fmt.Errorf("%s", stderrMsg)}
				select {
				case cs.events <- evt:
				case <-cs.ctx.Done():
					return
				}
			}
		}
		close(cs.events)
		close(cs.done)
	}()

	if err := readJSONLines(stdout, func(line []byte) error {
		lineText := string(line)
		if lineText == "" {
			return nil
		}

		var raw map[string]any
		if err := json.Unmarshal(line, &raw); err != nil {
			slog.Debug("claudeSession: non-JSON line", "line", lineText)
			return nil
		}

		eventType, _ := raw["type"].(string)
		slog.Debug("claudeSession: event", "type", eventType)

		switch eventType {
		case "system":
			cs.handleSystem(raw)
		case "assistant":
			cs.handleAssistant(raw)
		case "user":
			cs.handleUser(raw)
		case "result":
			cs.handleResult(raw)
		case "control_request":
			cs.handleControlRequest(raw)
		case "control_cancel_request":
			requestID, _ := raw["request_id"].(string)
			slog.Debug("claudeSession: permission cancelled", "request_id", requestID)
		}
		return nil
	}); err != nil {
		slog.Error("claudeSession: scanner error", "error", err)
		evt := core.Event{Type: core.EventError, Error: fmt.Errorf("read stdout: %w", err)}
		select {
		case cs.events <- evt:
		case <-cs.ctx.Done():
			return
		}
	}
}

func readJSONLines(r io.Reader, handle func([]byte) error) error {
	reader := bufio.NewReader(r)

	for {
		line, err := reader.ReadBytes('\n')
		if errors.Is(err, io.EOF) && len(line) == 0 {
			return nil
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}

		line = bytes.TrimRight(line, "\r\n")
		if len(line) > 0 {
			if err := handle(line); err != nil {
				return err
			}
		}

		if errors.Is(err, io.EOF) {
			return nil
		}
	}
}

func (cs *claudeSession) handleSystem(raw map[string]any) {
	if sid, ok := raw["session_id"].(string); ok && sid != "" {
		cs.sessionID.Store(sid)
		evt := core.Event{Type: core.EventText, SessionID: sid}
		select {
		case cs.events <- evt:
		case <-cs.ctx.Done():
			return
		}
	}
}

func (cs *claudeSession) handleAssistant(raw map[string]any) {
	msg, ok := raw["message"].(map[string]any)
	if !ok {
		return
	}
	contentArr, ok := msg["content"].([]any)
	if !ok {
		return
	}
	for _, contentItem := range contentArr {
		item, ok := contentItem.(map[string]any)
		if !ok {
			continue
		}
		contentType, _ := item["type"].(string)
		switch contentType {
		case "tool_use":
			toolName, _ := item["name"].(string)
			inputSummary := summarizeInput(toolName, item["input"])
			cs.recordToolUse(toolName, inputSummary)
			evt := core.Event{Type: core.EventToolUse, ToolName: toolName, ToolInput: inputSummary}
			select {
			case cs.events <- evt:
			case <-cs.ctx.Done():
				return
			}
		case "thinking":
			if thinking, ok := item["thinking"].(string); ok && thinking != "" {
				evt := core.Event{Type: core.EventThinking, Content: thinking}
				select {
				case cs.events <- evt:
				case <-cs.ctx.Done():
					return
				}
			}
		case "text":
			if text, ok := item["text"].(string); ok && text != "" {
				evt := core.Event{Type: core.EventText, Content: text}
				select {
				case cs.events <- evt:
				case <-cs.ctx.Done():
					return
				}
			}
		}
	}
}

func (cs *claudeSession) handleUser(raw map[string]any) {
	msg, ok := raw["message"].(map[string]any)
	if !ok {
		return
	}
	contentArr, ok := msg["content"].([]any)
	if !ok {
		return
	}
	for _, contentItem := range contentArr {
		item, ok := contentItem.(map[string]any)
		if !ok {
			continue
		}
		contentType, _ := item["type"].(string)
		if contentType == "tool_result" {
			result, _ := item["content"].(string)
			if strings.TrimSpace(result) != "" {
				evt := core.Event{Type: core.EventToolResult, Content: result}
				select {
				case cs.events <- evt:
				case <-cs.ctx.Done():
					return
				}
			}
			isError, _ := item["is_error"].(bool)
			if isError {
				if strings.Contains(strings.ToLower(result), "could not process image") {
					slog.Warn("claudeSession: image tool error", "content", result)
				} else {
					slog.Debug("claudeSession: tool error", "content", result)
				}
			}
		}
	}
}

func (cs *claudeSession) handleResult(raw map[string]any) {
	var content string
	if result, ok := raw["result"].(string); ok {
		content = result
	}
	if containsClaudeImageError(content) {
		slog.Error("claudeSession: image failure result", "content", content, "recent_tools", strings.Join(cs.snapshotRecentTools(), " | "))
	}
	if sid, ok := raw["session_id"].(string); ok && sid != "" {
		cs.sessionID.Store(sid)
	}
	evt := core.Event{Type: core.EventResult, Content: content, SessionID: cs.CurrentSessionID(), Done: true}
	select {
	case cs.events <- evt:
	case <-cs.ctx.Done():
		return
	}
}

func (cs *claudeSession) handleControlRequest(raw map[string]any) {
	requestID, _ := raw["request_id"].(string)
	request, _ := raw["request"].(map[string]any)
	if request == nil {
		return
	}
	subtype, _ := request["subtype"].(string)
	if subtype != "can_use_tool" {
		slog.Debug("claudeSession: unknown control request subtype", "subtype", subtype)
		return
	}

	toolName, _ := request["tool_name"].(string)
	input, _ := request["input"].(map[string]any)

	// Auto mode: approve immediately without asking the user
	if cs.autoApprove {
		slog.Debug("claudeSession: auto-approving", "request_id", requestID, "tool", toolName)
		_ = cs.RespondPermission(requestID, core.PermissionResult{
			Behavior:     "allow",
			UpdatedInput: input,
		})
		return
	}

	slog.Info("claudeSession: permission request", "request_id", requestID, "tool", toolName)
	evt := core.Event{
		Type:         core.EventPermissionRequest,
		RequestID:    requestID,
		ToolName:     toolName,
		ToolInput:    summarizeInput(toolName, input),
		ToolInputRaw: input,
	}
	select {
	case cs.events <- evt:
	case <-cs.ctx.Done():
		return
	}
}

func normalizeClaudeImage(img core.ImageAttachment) core.ImageAttachment {
	if len(img.Data) == 0 {
		return img
	}

	decoded, format, err := image.Decode(bytes.NewReader(img.Data))
	if err != nil {
		if looksLikeHEIC(img) {
			slog.Info("claudeSession: attempting HEIC conversion", "file", img.FileName, "mime", img.MimeType, "bytes", len(img.Data))
			converted, convErr := heicImageConverter(img)
			if convErr == nil {
				slog.Info("claudeSession: HEIC conversion succeeded", "file", img.FileName, "out_file", converted.FileName, "out_mime", converted.MimeType, "out_bytes", len(converted.Data))
				return converted
			}
			slog.Warn("claudeSession: HEIC conversion failed, using original image", "error", convErr, "file", img.FileName)
		} else {
			slog.Warn("claudeSession: image decode failed, using original image", "error", err, "file", img.FileName, "mime", img.MimeType, "bytes", len(img.Data))
		}
		return img
	}

	opaque := image.NewRGBA(decoded.Bounds())
	draw.Draw(opaque, opaque.Bounds(), image.NewUniform(image.White), image.Point{}, draw.Src)
	draw.Draw(opaque, opaque.Bounds(), decoded, decoded.Bounds().Min, draw.Over)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, opaque, &jpeg.Options{Quality: 90}); err != nil {
		return img
	}

	normalized := img
	normalized.Data = buf.Bytes()
	normalized.MimeType = "image/jpeg"
	if normalized.FileName != "" {
		base := strings.TrimSuffix(normalized.FileName, filepath.Ext(normalized.FileName))
		normalized.FileName = base + ".jpg"
	}
	slog.Debug("claudeSession: image normalized", "file", img.FileName, "mime", img.MimeType, "bytes", len(img.Data), "decoded_format", format, "out_file", normalized.FileName, "out_mime", normalized.MimeType, "out_bytes", len(normalized.Data))
	return normalized
}

func containsClaudeImageError(content string) bool {
	lower := strings.ToLower(content)
	return strings.Contains(lower, "could not process image") ||
		(strings.Contains(lower, "api error: 400") && strings.Contains(lower, "image"))
}

func (cs *claudeSession) recordToolUse(toolName, inputSummary string) {
	entry := toolName
	if strings.TrimSpace(inputSummary) != "" {
		entry += ": " + inputSummary
	}
	cs.recentMu.Lock()
	defer cs.recentMu.Unlock()
	cs.recentTools = append(cs.recentTools, entry)
	if len(cs.recentTools) > 12 {
		cs.recentTools = append([]string(nil), cs.recentTools[len(cs.recentTools)-12:]...)
	}
}

func (cs *claudeSession) snapshotRecentTools() []string {
	cs.recentMu.Lock()
	defer cs.recentMu.Unlock()
	if len(cs.recentTools) == 0 {
		return nil
	}
	return append([]string(nil), cs.recentTools...)
}

func looksLikeHEIC(img core.ImageAttachment) bool {
	name := strings.ToLower(strings.TrimSpace(img.FileName))
	if strings.HasSuffix(name, ".heic") || strings.HasSuffix(name, ".heif") {
		return true
	}

	mimeType := strings.ToLower(strings.TrimSpace(img.MimeType))
	if strings.Contains(mimeType, "heic") || strings.Contains(mimeType, "heif") {
		return true
	}

	if len(img.Data) < 12 {
		return false
	}
	if string(img.Data[4:8]) != "ftyp" {
		return false
	}
	brand := string(img.Data[8:12])
	switch brand {
	case "heic", "heix", "hevc", "hevx", "heim", "heis", "hevm", "hevs", "mif1", "msf1":
		return true
	default:
		return false
	}
}

func convertHEICImage(img core.ImageAttachment) (core.ImageAttachment, error) {
	tmpDir, err := os.MkdirTemp("", "cc-connect-heic-*")
	if err != nil {
		return img, err
	}
	defer os.RemoveAll(tmpDir)

	name := strings.TrimSpace(img.FileName)
	if name == "" {
		name = "image.heic"
	}
	if ext := strings.ToLower(filepath.Ext(name)); ext == "" || (ext != ".heic" && ext != ".heif") {
		name = strings.TrimSuffix(name, filepath.Ext(name)) + ".heic"
	}

	inputPath := filepath.Join(tmpDir, filepath.Base(name))
	outputPath := filepath.Join(tmpDir, strings.TrimSuffix(filepath.Base(name), filepath.Ext(name))+".jpg")
	if err := os.WriteFile(inputPath, img.Data, 0o644); err != nil {
		return img, err
	}

	var cmd *exec.Cmd
	if runtime.GOOS == "darwin" {
		if _, err := exec.LookPath("sips"); err == nil {
			cmd = exec.Command("sips", "-s", "format", "jpeg", inputPath, "--out", outputPath)
		}
	}
	if cmd == nil {
		if _, err := exec.LookPath("ffmpeg"); err == nil {
			cmd = exec.Command("ffmpeg", "-y", "-i", inputPath, outputPath)
		} else if _, err := exec.LookPath("magick"); err == nil {
			cmd = exec.Command("magick", inputPath, outputPath)
		}
	}
	if cmd == nil {
		return img, fmt.Errorf("no HEIC converter available (tried sips, ffmpeg, magick)")
	}

	if output, err := cmd.CombinedOutput(); err != nil {
		return img, fmt.Errorf("%s: %w: %s", filepath.Base(cmd.Path), err, strings.TrimSpace(string(output)))
	}

	convertedData, err := os.ReadFile(outputPath)
	if err != nil {
		return img, err
	}
	converted := img
	converted.Data = convertedData
	converted.MimeType = "image/jpeg"
	converted.FileName = strings.TrimSuffix(filepath.Base(name), filepath.Ext(name)) + ".jpg"
	return converted, nil
}

// Send writes a user message (with optional images) to the Claude process stdin.
// Images are normalized to a canonical JPEG encoding first because some valid
// image files still trigger "Could not process image" errors from Claude's API.
// The normalized images are then saved locally and only the file paths are
// referenced in the text prompt so Claude Code can inspect them via local tools.
func (cs *claudeSession) Send(prompt string, images []core.ImageAttachment, files []core.FileAttachment) error {
	if !cs.alive.Load() {
		return fmt.Errorf("session process is not running")
	}
	filePaths := core.SaveFilesToDisk(cs.workDir, files)
	prompt = core.AppendFileRefs(prompt, filePaths)

	if len(images) == 0 {
		return cs.writeJSON(map[string]any{
			"type":    "user",
			"message": map[string]any{"role": "user", "content": prompt},
		})
	}

	normalizedImages := make([]core.ImageAttachment, 0, len(images))
	for _, img := range images {
		normalized := normalizeClaudeImage(img)
		slog.Info("claudeSession: prepared image", "file", img.FileName, "mime", img.MimeType, "bytes", len(img.Data), "normalized_file", normalized.FileName, "normalized_mime", normalized.MimeType, "normalized_bytes", len(normalized.Data), "heic", looksLikeHEIC(img))
		normalizedImages = append(normalizedImages, normalized)
	}
	savedPaths := core.SaveImagesToDisk(cs.workDir, normalizedImages)
	for i, img := range normalizedImages {
		if i < len(savedPaths) {
			slog.Debug("claudeSession: image saved", "path", savedPaths[i], "size", len(img.Data))
		}
	}

	textPart := prompt
	if textPart == "" {
		textPart = "Please analyze the attached local image file(s)."
	}
	if len(savedPaths) > 0 || len(filePaths) > 0 {
		refs := append(append([]string{}, savedPaths...), filePaths...)
		textPart += "\n\n(Local files: " + strings.Join(refs, ", ") + ")"
	}

	return cs.writeJSON(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": textPart},
	})
}

// RespondPermission writes a control_response to the Claude process stdin.
func (cs *claudeSession) RespondPermission(requestID string, result core.PermissionResult) error {
	if !cs.alive.Load() {
		return fmt.Errorf("session process is not running")
	}

	var permResponse map[string]any
	if result.Behavior == "allow" {
		updatedInput := result.UpdatedInput
		if updatedInput == nil {
			updatedInput = make(map[string]any)
		}
		permResponse = map[string]any{
			"behavior":     "allow",
			"updatedInput": updatedInput,
		}
	} else {
		msg := result.Message
		if msg == "" {
			msg = "The user denied this tool use. Stop and wait for the user's instructions."
		}
		permResponse = map[string]any{
			"behavior": "deny",
			"message":  msg,
		}
	}

	controlResponse := map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": requestID,
			"response":   permResponse,
		},
	}

	slog.Debug("claudeSession: permission response", "request_id", requestID, "behavior", result.Behavior)
	return cs.writeJSON(controlResponse)
}

func (cs *claudeSession) writeJSON(v any) error {
	cs.stdinMu.Lock()
	defer cs.stdinMu.Unlock()

	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if _, err := cs.stdin.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write stdin: %w", err)
	}
	return nil
}

func (cs *claudeSession) Events() <-chan core.Event {
	return cs.events
}

func (cs *claudeSession) CurrentSessionID() string {
	v, _ := cs.sessionID.Load().(string)
	return v
}

func (cs *claudeSession) Alive() bool {
	return cs.alive.Load()
}

func (cs *claudeSession) Close() error {
	cs.cancel()

	select {
	case <-cs.done:
		return nil
	case <-time.After(8 * time.Second):
		slog.Warn("claudeSession: graceful close timed out, killing process")
		if cs.cmd != nil && cs.cmd.Process != nil {
			_ = cs.cmd.Process.Kill()
		}
		<-cs.done
		return nil
	}
}

// filterEnv returns a copy of env with entries matching the given key removed.
func filterEnv(env []string, key string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env))
	for _, e := range env {
		if !strings.HasPrefix(e, prefix) {
			out = append(out, e)
		}
	}
	return out
}
