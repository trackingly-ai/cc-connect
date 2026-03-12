package core

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestGeminiSTTInterpretAuthErrorForVertexAPIKeyMismatch(t *testing.T) {
	g := &GeminiSTT{ProjectID: "demo-project"}
	body := []byte(`{"error":{"status":"UNAUTHENTICATED","details":[{"reason":"API_KEY_SERVICE_BLOCKED"}]}}`)

	msg := g.interpretAuthError(http.StatusUnauthorized, body)
	if msg == "" {
		t.Fatal("expected actionable auth error message")
	}
	if !strings.Contains(msg, "Vertex AI endpoint") {
		t.Fatalf("unexpected message: %q", msg)
	}
}

func TestGeminiSTTUsesVertexAPIKey(t *testing.T) {
	t.Run("vertex api key", func(t *testing.T) {
		g := &GeminiSTT{ProjectID: "demo-project", APIKey: "AQ.test"}
		if !g.usesVertexAPIKey() {
			t.Fatal("expected Vertex API key detection")
		}
	})

	t.Run("oauth token", func(t *testing.T) {
		g := &GeminiSTT{ProjectID: "demo-project", APIKey: "ya29.test"}
		if g.usesVertexAPIKey() {
			t.Fatal("did not expect OAuth token to be treated as API key")
		}
	})

	t.Run("no vertex project", func(t *testing.T) {
		g := &GeminiSTT{APIKey: "AQ.test"}
		if g.usesVertexAPIKey() {
			t.Fatal("did not expect API key mode without project_id")
		}
	})
}

func TestGeminiSTTTranscribeUsesQueryAPIKeyForVertexAPIKeys(t *testing.T) {
	var capturedReq *http.Request
	g := &GeminiSTT{
		APIKey:    "AQ.test-key",
		Model:     "gemini-2.5-flash",
		ProjectID: "demo-project",
		Location:  "us-central1",
		Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			capturedReq = req.Clone(req.Context())
			return jsonResponse(`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`), nil
		})},
	}

	text, err := g.Transcribe(context.Background(), []byte("audio"), "mp3", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "ok" {
		t.Fatalf("unexpected text: %q", text)
	}
	if capturedReq == nil {
		t.Fatal("expected request to be captured")
	}
	if got := capturedReq.URL.Query().Get("key"); got != "AQ.test-key" {
		t.Fatalf("expected query api key, got %q", got)
	}
	if got := capturedReq.Header.Get("Authorization"); got != "" {
		t.Fatalf("did not expect bearer auth header, got %q", got)
	}
}

func TestGeminiSTTTranscribeUsesBearerForVertexOAuthTokens(t *testing.T) {
	var capturedReq *http.Request
	g := &GeminiSTT{
		APIKey:    "ya29.test-token",
		Model:     "gemini-2.5-flash",
		ProjectID: "demo-project",
		Location:  "us-central1",
		Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			capturedReq = req.Clone(req.Context())
			return jsonResponse(`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`), nil
		})},
	}

	text, err := g.Transcribe(context.Background(), []byte("audio"), "mp3", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "ok" {
		t.Fatalf("unexpected text: %q", text)
	}
	if capturedReq == nil {
		t.Fatal("expected request to be captured")
	}
	if got := capturedReq.URL.Query().Get("key"); got != "" {
		t.Fatalf("did not expect query api key, got %q", got)
	}
	if got := capturedReq.Header.Get("Authorization"); got != "Bearer ya29.test-token" {
		t.Fatalf("unexpected bearer auth header: %q", got)
	}
}

type roundTripFunc func(req *http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func jsonResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewBufferString(body)),
	}
}
