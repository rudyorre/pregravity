package gtasks

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGoogleTasksClientAndStdioMCPServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-access-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/users/@me/lists"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []any{map[string]any{"id": "list-1", "title": "My Tasks"}},
			})
		case strings.Contains(r.URL.Path, "/lists/list-1/tasks"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []any{map[string]any{"id": "gt-1", "title": "Buy coffee", "status": "needsAction"}},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	t.Setenv("PREGRAVITY_GTASKS_ACCESS_TOKEN", "test-access-token")
	cli := NewClient("", "", srv.URL)

	// Drive the stdio MCP server with initialize + tools/list + tools/call.
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_tasks","arguments":{"tasklist_id":"list-1"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"delete_task","arguments":{}}}`,
	}, "\n") + "\n"

	var out bytes.Buffer
	if err := ServeStdioMCP(context.Background(), cli, strings.NewReader(input), &out); err != nil {
		t.Fatalf("serve stdio mcp: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected 4 JSON-RPC responses, got %d:\n%s", len(lines), out.String())
	}
	if !strings.Contains(lines[1], `"readOnlyHint":true`) {
		t.Fatalf("expected readOnlyHint on tools/list: %s", lines[1])
	}
	if !strings.Contains(lines[2], "Buy coffee") {
		t.Fatalf("expected task payload in tools/call response: %s", lines[2])
	}
	if !strings.Contains(lines[3], `"error"`) {
		t.Fatalf("expected error on disallowed delete_task call: %s", lines[3])
	}
}
