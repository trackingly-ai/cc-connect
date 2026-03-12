package core

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"
)

// SpeechToText transcribes audio to text.
type SpeechToText interface {
	Transcribe(ctx context.Context, audio []byte, format string, lang string) (string, error)
}

// SpeechConfig holds STT configuration for the engine.
type SpeechCfg struct {
	Enabled  bool
	Provider string
	Language string
	STT      SpeechToText
}

// OpenAIWhisper implements SpeechToText using the OpenAI-compatible Whisper API.
// Works with OpenAI, Groq, and any endpoint that implements the same multipart API.
type OpenAIWhisper struct {
	APIKey  string
	BaseURL string
	Model   string
	Client  *http.Client
}

func NewOpenAIWhisper(apiKey, baseURL, model string) *OpenAIWhisper {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	if model == "" {
		model = "whisper-1"
	}
	return &OpenAIWhisper{
		APIKey:  apiKey,
		BaseURL: strings.TrimRight(baseURL, "/"),
		Model:   model,
		Client:  &http.Client{Timeout: 60 * time.Second},
	}
}

func (w *OpenAIWhisper) Transcribe(ctx context.Context, audio []byte, format string, lang string) (string, error) {
	ext := formatToExt(format)

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	part, err := writer.CreateFormFile("file", "audio."+ext)
	if err != nil {
		return "", fmt.Errorf("create form file: %w", err)
	}
	if _, err := part.Write(audio); err != nil {
		return "", fmt.Errorf("write audio: %w", err)
	}
	_ = writer.WriteField("model", w.Model)
	_ = writer.WriteField("response_format", "text")
	if lang != "" {
		_ = writer.WriteField("language", lang)
	}
	writer.Close()

	url := w.BaseURL + "/audio/transcriptions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &buf)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+w.APIKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := w.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("whisper request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("whisper API %d: %s", resp.StatusCode, string(body))
	}

	// response_format=text returns plain text; try to handle JSON fallback
	text := strings.TrimSpace(string(body))
	if strings.HasPrefix(text, "{") {
		var jr struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(body, &jr) == nil {
			text = jr.Text
		}
	}
	return text, nil
}

// QwenASR implements SpeechToText using the Qwen ASR model via DashScope's
// OpenAI-compatible chat completions API. Unlike Whisper, audio is sent as a
// base64 data URI inside the messages array.
type QwenASR struct {
	APIKey  string
	BaseURL string
	Model   string
	Client  *http.Client
}

func NewQwenASR(apiKey, baseURL, model string) *QwenASR {
	if baseURL == "" {
		baseURL = "https://dashscope.aliyuncs.com/compatible-mode/v1"
	}
	if model == "" {
		model = "qwen3-asr-flash"
	}
	return &QwenASR{
		APIKey:  apiKey,
		BaseURL: strings.TrimRight(baseURL, "/"),
		Model:   model,
		Client:  &http.Client{Timeout: 60 * time.Second},
	}
}

func (q *QwenASR) Transcribe(ctx context.Context, audio []byte, format string, lang string) (string, error) {
	b64 := base64.StdEncoding.EncodeToString(audio)
	dataURI := fmt.Sprintf("data:%s;base64,%s", formatToAudioMIME(format), b64)

	reqBody := map[string]any{
		"model": q.Model,
		"messages": []map[string]any{
			{
				"role": "user",
				"content": []map[string]any{
					{
						"type": "input_audio",
						"input_audio": map[string]any{
							"data": dataURI,
						},
					},
				},
			},
		},
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	url := q.BaseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(jsonData))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+q.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := q.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("qwen asr request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("qwen asr API %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}
	if len(result.Choices) == 0 {
		return "", fmt.Errorf("qwen asr: empty choices in response")
	}

	return strings.TrimSpace(result.Choices[0].Message.Content), nil
}

// GeminiSTT implements SpeechToText using the Gemini API.
// Instead of raw transcription, it sends the audio to Gemini with a prompt
// that asks it to understand the user's intent and output a clear, actionable
// requirement — ready to be passed directly to Claude Code.
type GeminiSTT struct {
	APIKey    string
	Model     string
	ProjectID string
	Location  string
	Client    *http.Client
}

func NewGeminiSTT(apiKey, model, projectID, location string) *GeminiSTT {
	if model == "" {
		model = "gemini-2.5-flash"
	}
	if location == "" {
		location = "us-central1"
	}
	return &GeminiSTT{
		APIKey:    apiKey,
		Model:     model,
		ProjectID: projectID,
		Location:  location,
		Client:    &http.Client{Timeout: 120 * time.Second},
	}
}

func (g *GeminiSTT) Transcribe(ctx context.Context, audio []byte, format string, lang string) (string, error) {
	b64 := base64.StdEncoding.EncodeToString(audio)
	mime := formatToAudioMIME(format)

	systemPrompt := "You are a voice-to-task interpreter. Listen to the audio and extract the user's intent. " +
		"Output ONLY the clear, actionable requirement or instruction — as if the user had typed it. " +
		"Do not include filler words, hesitations, or conversational artifacts. " +
		"If the user speaks in a non-English language, output the requirement in that same language. " +
		"Do not add any explanation or commentary."

	reqBody := map[string]any{
		"system_instruction": map[string]any{
			"parts": []map[string]any{
				{"text": systemPrompt},
			},
		},
		"contents": []map[string]any{
			{
				"parts": []any{
					map[string]any{
						"inline_data": map[string]any{
							"mime_type": mime,
							"data":      b64,
						},
					},
					map[string]any{
						"text": "Listen to this audio and output the user's requirement.",
					},
				},
			},
		},
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("gemini stt: marshal request: %w", err)
	}

	var apiURL string
	if g.ProjectID != "" {
		// Vertex AI endpoint
		apiURL = fmt.Sprintf("https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/google/models/%s:generateContent",
			g.Location, g.ProjectID, g.Location, g.Model)
		if g.usesVertexAPIKey() {
			apiURL += "?key=" + url.QueryEscape(g.APIKey)
		}
	} else {
		// Gemini API endpoint
		apiURL = fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s", g.Model, g.APIKey)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(jsonData))
	if err != nil {
		return "", fmt.Errorf("gemini stt: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if g.ProjectID != "" && !g.usesVertexAPIKey() {
		req.Header.Set("Authorization", "Bearer "+g.APIKey)
	}

	resp, err := g.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("gemini stt: request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("gemini stt: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		if msg := g.interpretAuthError(resp.StatusCode, body); msg != "" {
			return "", fmt.Errorf("%s", msg)
		}
		return "", fmt.Errorf("gemini stt API %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("gemini stt: parse response: %w", err)
	}
	if len(result.Candidates) == 0 || len(result.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("gemini stt: empty response")
	}

	return strings.TrimSpace(result.Candidates[0].Content.Parts[0].Text), nil
}

func (g *GeminiSTT) interpretAuthError(statusCode int, body []byte) string {
	if statusCode != http.StatusUnauthorized {
		return ""
	}

	bodyText := string(body)
	if g.ProjectID != "" && strings.Contains(bodyText, "API_KEY_SERVICE_BLOCKED") {
		return "gemini stt auth failed: cc-connect is calling the Vertex AI endpoint, but the configured speech.gemini.api_key was rejected. If this value is a Vertex API key, it must be sent as an API key instead of an Authorization bearer token. If this value is an OAuth access token, make sure it is valid for the target project and region"
	}

	return ""
}

func (g *GeminiSTT) usesVertexAPIKey() bool {
	if g.ProjectID == "" {
		return false
	}
	return strings.HasPrefix(g.APIKey, "AIza") || strings.HasPrefix(g.APIKey, "AQ.")
}

// ConvertAudioToMP3 uses ffmpeg to convert audio from unsupported formats to mp3.
// Returns the mp3 bytes. If ffmpeg is not installed, returns an error.
func ConvertAudioToMP3(audio []byte, srcFormat string) ([]byte, error) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, fmt.Errorf("ffmpeg not found in PATH: install ffmpeg to enable voice message support")
	}

	var cmd *exec.Cmd
	if srcFormat == "amr" || srcFormat == "silk" {
		cmd = exec.Command(ffmpegPath,
			"-f", srcFormat,
			"-i", "pipe:0",
			"-f", "mp3",
			"-ac", "1",
			"-ar", "16000",
			"-y",
			"pipe:1",
		)
	} else {
		cmd = exec.Command(ffmpegPath,
			"-i", "pipe:0",
			"-f", "mp3",
			"-ac", "1",
			"-ar", "16000",
			"-y",
			"pipe:1",
		)
	}

	cmd.Stdin = bytes.NewReader(audio)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg conversion failed: %w (stderr: %s)", err, stderr.String())
	}
	return stdout.Bytes(), nil
}

// ConvertAudioToOpus uses ffmpeg to convert audio to opus format (ogg container).
// Returns the opus bytes. If ffmpeg is not installed, returns an error.
func ConvertAudioToOpus(ctx context.Context, audio []byte, srcFormat string) ([]byte, error) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, fmt.Errorf("ffmpeg not found in PATH: install ffmpeg to enable audio conversion")
	}

	args := []string{"-i", "pipe:0", "-c:a", "libopus", "-f", "opus", "-y", "pipe:1"}
	if srcFormat == "amr" || srcFormat == "silk" {
		args = append([]string{"-f", srcFormat}, args...)
	}
	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	cmd.Stdin = bytes.NewReader(audio)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg opus conversion failed: %w (stderr: %s)", err, stderr.String())
	}
	return stdout.Bytes(), nil
}

// NeedsConversion returns true if the audio format is not directly supported by Whisper API.
func NeedsConversion(format string) bool {
	switch strings.ToLower(format) {
	case "mp3", "mp4", "mpeg", "mpga", "m4a", "wav", "webm":
		return false
	default:
		return true
	}
}

// HasFFmpeg checks if ffmpeg is available.
func HasFFmpeg() bool {
	_, err := exec.LookPath("ffmpeg")
	return err == nil
}

func formatToExt(format string) string {
	switch strings.ToLower(format) {
	case "amr":
		return "amr"
	case "ogg", "oga", "opus":
		return "ogg"
	case "m4a", "mp4", "aac":
		return "m4a"
	case "mp3":
		return "mp3"
	case "wav":
		return "wav"
	case "webm":
		return "webm"
	case "silk":
		return "silk"
	default:
		return format
	}
}

func formatToAudioMIME(format string) string {
	switch strings.ToLower(format) {
	case "mp3", "mpeg", "mpga":
		return "audio/mpeg"
	case "wav":
		return "audio/wav"
	case "ogg", "oga", "opus":
		return "audio/ogg"
	case "m4a", "mp4", "aac":
		return "audio/mp4"
	case "webm":
		return "audio/webm"
	default:
		return "audio/octet-stream"
	}
}

// TranscribeAudio is a convenience function used by the Engine.
// It handles format conversion (if needed) and calls the STT provider.
func TranscribeAudio(ctx context.Context, stt SpeechToText, audio *AudioAttachment, lang string) (string, error) {
	data := audio.Data
	format := strings.ToLower(audio.Format)

	if NeedsConversion(format) {
		slog.Debug("speech: converting audio", "from", format, "to", "mp3")
		converted, err := ConvertAudioToMP3(data, format)
		if err != nil {
			return "", err
		}
		data = converted
		format = "mp3"
	}

	slog.Debug("speech: transcribing", "format", format, "size", len(data))
	return stt.Transcribe(ctx, data, format, lang)
}
