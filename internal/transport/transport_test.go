package transport

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pregravity/internal/assistant"
	"pregravity/internal/config"
	"pregravity/internal/gemini"
	"pregravity/internal/store"
)

type staticProvider struct{ reply string }

func (s staticProvider) Generate(_ context.Context, _ gemini.GenerateRequest) (*gemini.GenerateResponse, error) {
	return &gemini.GenerateResponse{
		Candidates: []struct {
			Content      gemini.Content `json:"content"`
			FinishReason string         `json:"finishReason"`
		}{
			{Content: gemini.Content{Role: "model", Parts: []gemini.Part{{Text: s.reply}}}},
		},
	}, nil
}

func TestHTTPAPIAuthAndEndpoints(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	asst := assistant.New(cfg, st, staticProvider{reply: "roger that"}, nil)
	srv, err := NewHTTPServer("127.0.0.1:0", "topsecret", cfg, st, asst)
	if err != nil {
		t.Fatalf("new http server: %v", err)
	}

	// /health is unauthenticated
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("health status = %d", w.Code)
	}

	// /tasks requires bearer token
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/tasks", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", w.Code)
	}

	// /chat with valid token works
	req := httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(`{"message":"hello"}`))
	req.Header.Set("Authorization", "Bearer topsecret")
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "roger that") {
		t.Fatalf("chat failed: %d %s", w.Code, w.Body.String())
	}
}

func TestSplitChunks(t *testing.T) {
	text := strings.Repeat("line\n", 100)
	chunks := splitChunks(text, 80)
	if len(chunks) < 5 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if len(c) > 80 {
			t.Errorf("chunk %d exceeded maxLen: %d", i, len(c))
		}
	}
}

func TestNotifiers(t *testing.T) {
	var ntfyTitle, ntfyBody, hookBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if strings.HasPrefix(r.URL.Path, "/mytopic") {
			ntfyTitle = r.Header.Get("Title")
			ntfyBody = string(raw)
		} else {
			hookBody = string(raw)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	multi := &MultiNotifier{
		Targets: []Notifier{
			&NtfyNotifier{ServerURL: srv.URL, Topic: "mytopic"},
			&WebhookNotifier{URL: srv.URL + "/hook"},
		},
	}
	if err := multi.Notify(context.Background(), "Morning", "Top 3 tasks"); err != nil {
		t.Fatalf("notify: %v", err)
	}
	if ntfyTitle != "Morning" || ntfyBody != "Top 3 tasks" {
		t.Fatalf("ntfy mismatch: title=%q body=%q", ntfyTitle, ntfyBody)
	}
	if !strings.Contains(hookBody, "Top 3 tasks") {
		t.Fatalf("webhook mismatch: %q", hookBody)
	}
}

func TestTelegramSendReplyHTMLFallbackAndReplyParameters(t *testing.T) {
	var requests []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		requests = append(requests, payload)
		// Force HTTP 400 on first HTML call when text contains TRIGGER_400 to test plain-text fallback.
		if txt, _ := payload["text"].(string); strings.Contains(txt, "TRIGGER_400") && payload["parse_mode"] == "HTML" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"ok":false,"description":"Bad Request: can't parse entities"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	bot := &TelegramBot{
		token:      "test-token",
		apiBase:    srv.URL,
		httpClient: srv.Client(),
	}

	if err := bot.SendReply(context.Background(), 42, 777, "**Bold** and *italic* `code`"); err != nil {
		t.Fatalf("send reply: %v", err)
	}
	if len(requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(requests))
	}
	if requests[0]["parse_mode"] != "HTML" {
		t.Fatalf("expected parse_mode HTML, got %v", requests[0]["parse_mode"])
	}
	if requests[0]["text"] != "<b>Bold</b> and <i>italic</i> <code>code</code>" {
		t.Fatalf("unexpected rendered HTML: %q", requests[0]["text"])
	}
	rp, ok := requests[0]["reply_parameters"].(map[string]any)
	if !ok || int64(rp["message_id"].(float64)) != 777 {
		t.Fatalf("expected reply_parameters.message_id=777, got %#v", requests[0]["reply_parameters"])
	}

	// Verify HTTP 400 triggers automatic plain-text fallback.
	if err := bot.SendReply(context.Background(), 42, 888, "**TRIGGER_400**"); err != nil {
		t.Fatalf("fallback send reply failed: %v", err)
	}
	if len(requests) != 3 {
		t.Fatalf("expected 2 additional requests (HTML + plain fallback), total=%d", len(requests))
	}
	if _, hasParseMode := requests[2]["parse_mode"]; hasParseMode {
		t.Fatalf("expected fallback retry to omit parse_mode, got %v", requests[2]["parse_mode"])
	}
}
