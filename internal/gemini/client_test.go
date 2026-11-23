package gemini

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClientGenerateHeaderAuthAndParsing(t *testing.T) {
	var gotKey string
	var gotPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-goog-api-key")
		gotPath = r.URL.Path
		if r.URL.RawQuery != "" {
			t.Errorf("expected empty query string (no key in URL), got %q", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"candidates": []any{
				map[string]any{
					"content": map[string]any{
						"role":  "model",
						"parts": []any{map[string]any{"text": "All systems nominal."}},
					},
					"finishReason": "STOP",
				},
			},
			"usageMetadata": map[string]any{"totalTokenCount": 42},
		})
	}))
	defer srv.Close()

	client := NewClient("secret-key-123", "gemini-3.8-flash", srv.URL, 5*time.Second)
	resp, err := client.Generate(context.Background(), GenerateRequest{
		Contents: []Content{{Role: "user", Parts: []Part{{Text: "ping"}}}},
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if gotKey != "secret-key-123" {
		t.Fatalf("got header key %q, want secret-key-123", gotKey)
	}
	if !strings.HasSuffix(gotPath, "/models/gemini-3.8-flash:generateContent") {
		t.Fatalf("unexpected endpoint path %q", gotPath)
	}
	if resp.Candidates[0].Content.Parts[0].Text != "All systems nominal." {
		t.Fatalf("unexpected text: %+v", resp.Candidates[0])
	}
	if resp.UsageMetadata == nil || resp.UsageMetadata.TotalTokenCount != 42 {
		t.Fatalf("unexpected usage: %+v", resp.UsageMetadata)
	}
}

func TestClientRequiresAPIKey(t *testing.T) {
	client := NewClient("", "gemini-3.8-flash", "", time.Second)
	if _, err := client.Generate(context.Background(), GenerateRequest{}); err == nil {
		t.Fatal("expected error when API key is missing")
	}
}
