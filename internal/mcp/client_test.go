package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReadOnlyFirewall(t *testing.T) {
	allowed := []Tool{
		{Name: "list_tasks", Annotations: &ToolAnnotations{ReadOnlyHint: true}},
		{Name: "get_calendar_events"},
		{Name: "search_notes", Annotations: &ToolAnnotations{ReadOnlyHint: true}},
	}
	for _, tool := range allowed {
		if !IsToolAllowedReadOnly(tool, nil) {
			t.Errorf("expected %q to be allowed read-only", tool.Name)
		}
	}

	blocked := []Tool{
		{Name: "delete_task", Annotations: &ToolAnnotations{ReadOnlyHint: true}}, // even if server lies!
		{Name: "create_task"},
		{Name: "update_task"},
		{Name: "run_shell"},
		{Name: "list_secret", Annotations: &ToolAnnotations{DestructiveHint: true}},
		{Name: "mysterious_tool"},
	}
	for _, tool := range blocked {
		if IsToolAllowedReadOnly(tool, nil) {
			t.Errorf("expected %q to be blocked by read-only firewall", tool.Name)
		}
	}
}

func TestHTTPMCPClientDiscoveryAndCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.ID == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2024-11-05"}
		case "tools/list":
			result = map[string]any{
				"tools": []any{
					map[string]any{
						"name":        "list_tasks",
						"description": "Read tasks",
						"annotations": map[string]any{"readOnlyHint": true},
					},
					map[string]any{
						"name":        "delete_task",
						"description": "Dangerous mutation",
					},
				},
			}
		case "tools/call":
			result = map[string]any{
				"content": []any{map[string]any{"type": "text", "text": `{"count":1}`}},
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result":  result,
		})
	}))
	defer srv.Close()

	mgr := NewManager(context.Background(), []ServerConfig{
		{Name: "gtasks", Enabled: true, Transport: "http", URL: srv.URL},
	}, nil)
	defer mgr.Close()

	tools := mgr.Tools()
	if len(tools) != 1 || tools[0].ExposedName != "mcp__gtasks__list_tasks" {
		t.Fatalf("expected only read-only tool exposed, got %+v", tools)
	}
	res, err := mgr.Call(context.Background(), "mcp__gtasks__list_tasks", nil)
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if _, ok := res["content"]; !ok {
		t.Fatalf("unexpected tool result: %+v", res)
	}
}
