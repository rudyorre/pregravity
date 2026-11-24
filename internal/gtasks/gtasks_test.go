package gtasks

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestGoogleTasksClientSafeWritesBackupAndStdioMCP(t *testing.T) {
	var movedDest string
	var completedID string
	var createdTitle string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-access-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodDelete {
			t.Errorf("DELETE must never be called by gtasks client")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/users/@me/lists"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []any{map[string]any{"id": "list-1", "title": "My Tasks"}},
			})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/lists/list-1/tasks"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []any{map[string]any{"id": "gt-1", "title": "Buy coffee", "status": "needsAction"}},
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/lists/list-1/tasks"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			createdTitle, _ = body["title"].(string)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "gt-2", "title": createdTitle, "status": "needsAction"})
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/lists/list-1/tasks/gt-1"):
			completedID = "gt-1"
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "gt-1", "title": "Buy coffee", "status": "completed"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/lists/list-1/tasks/gt-1/move"):
			movedDest = r.URL.Query().Get("destinationTasklist")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "gt-1", "title": "Buy coffee", "status": "needsAction"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	t.Setenv("PREGRAVITY_GTASKS_ACCESS_TOKEN", "test-access-token")
	cli := NewClient("", "", srv.URL)

	// 1. Test BackupAll
	backupPath, snap, err := cli.BackupAll(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if len(snap.Lists) != 1 || len(snap.Lists[0].Tasks) != 1 {
		t.Fatalf("unexpected backup snapshot: %+v", snap)
	}
	if _, err := os.Stat(backupPath); err != nil {
		t.Fatalf("expected backup file at %s: %v", backupPath, err)
	}

	// 2. Test stdio MCP server in safe-write mode (create, complete, move allowed; delete rejected).
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"create_task","arguments":{"tasklist_id":"list-1","title":"Ship triage CLI"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"complete_task","arguments":{"tasklist_id":"list-1","task_id":"gt-1"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"move_task","arguments":{"from_tasklist_id":"list-1","task_id":"gt-1","to_tasklist_id":"list-archive"}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"delete_task","arguments":{}}}`,
	}, "\n") + "\n"

	var out bytes.Buffer
	if err := ServeStdioMCP(context.Background(), cli, true, strings.NewReader(input), &out); err != nil {
		t.Fatalf("serve stdio mcp: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 6 {
		t.Fatalf("expected 6 JSON-RPC responses, got %d:\n%s", len(lines), out.String())
	}
	if createdTitle != "Ship triage CLI" {
		t.Fatalf("expected created task title, got %q", createdTitle)
	}
	if completedID != "gt-1" {
		t.Fatalf("expected gt-1 completed, got %q", completedID)
	}
	if movedDest != "list-archive" {
		t.Fatalf("expected move destination list-archive, got %q", movedDest)
	}
	if !strings.Contains(lines[5], `"error"`) {
		t.Fatalf("expected error on disallowed delete_task call: %s", lines[5])
	}
}
