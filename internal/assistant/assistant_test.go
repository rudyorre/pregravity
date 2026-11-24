package assistant

import (
	"context"
	"strings"
	"testing"

	"pregravity/internal/config"
	"pregravity/internal/gemini"
	"pregravity/internal/store"
)

type mockProvider struct {
	calls int
	reqs  []gemini.GenerateRequest
	resps []*gemini.GenerateResponse
}

func (m *mockProvider) Generate(_ context.Context, req gemini.GenerateRequest) (*gemini.GenerateResponse, error) {
	m.reqs = append(m.reqs, req)
	idx := m.calls
	m.calls++
	return m.resps[idx], nil
}

func TestAssistantToolLoopAndHandoff(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.Antigravity.InstanceName = "devbox-test"

	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	mock := &mockProvider{
		resps: []*gemini.GenerateResponse{
			{
				Candidates: []struct {
					Content      gemini.Content `json:"content"`
					FinishReason string         `json:"finishReason"`
				}{
					{
						Content: gemini.Content{
							Role: "model",
							Parts: []gemini.Part{
								{Text: "internal reasoning", Thought: true},
								{
									ThoughtSignature: "sig-abc-123",
									FunctionCall: &gemini.FunctionCall{
										Name: "add_task",
										Args: map[string]any{
											"title":    "Refactor OAuth handler",
											"priority": "high",
											"due":      "tomorrow",
										},
									},
								},
								{
									FunctionCall: &gemini.FunctionCall{
										Name: "create_handoff",
										Args: map[string]any{
											"title":   "Refactor OAuth handler",
											"goal":    "Audit and simplify OAuth refresh flow",
											"context": "User asked to escalate to Antigravity",
										},
									},
								},
							},
						},
					},
				},
				UsageMetadata: &gemini.UsageMetadata{TotalTokenCount: 100},
			},
			{
				Candidates: []struct {
					Content      gemini.Content `json:"content"`
					FinishReason string         `json:"finishReason"`
				}{
					{
						Content: gemini.Content{
							Role:  "model",
							Parts: []gemini.Part{{Text: "Added the task and staged your Antigravity handoff!"}},
						},
					},
				},
				UsageMetadata: &gemini.UsageMetadata{TotalTokenCount: 50},
			},
		},
	}

	asst := New(cfg, st, mock, nil)
	reply, err := asst.HandleMessage(context.Background(), "cli-test", "Track OAuth refactor and hand it off to agy")
	if err != nil {
		t.Fatalf("handle message: %v", err)
	}
	if !strings.Contains(reply.Text, "staged your Antigravity handoff") {
		t.Fatalf("unexpected reply text: %q", reply.Text)
	}
	if reply.Tokens != 150 {
		t.Fatalf("got tokens %d, want 150", reply.Tokens)
	}
	if len(reply.Handoffs) != 1 {
		t.Fatalf("expected 1 handoff, got %d", len(reply.Handoffs))
	}
	// Verify Gemini 3 thoughtSignature was preserved across tool-call rounds.
	if len(mock.reqs) < 2 || len(mock.reqs[1].Contents) < 2 || mock.reqs[1].Contents[1].Parts[1].ThoughtSignature != "sig-abc-123" {
		t.Fatalf("expected thoughtSignature to be preserved in round 2 request")
	}

	tasks, err := st.ListTasks("open")
	if err != nil || len(tasks) != 1 || tasks[0].Title != "Refactor OAuth handler" {
		t.Fatalf("expected persisted task, got %+v err=%v", tasks, err)
	}

	// Verify session history was persisted.
	hist, err := st.LoadSession("cli-test", 10)
	if err != nil || len(hist) != 2 {
		t.Fatalf("expected 2 session messages, got %+v err=%v", hist, err)
	}

	// Verify unknown/shell tool calls are rejected cleanly.
	res, _ := asst.executeTool("run_shell_command", map[string]any{"cmd": "rm -rf /"})
	if _, ok := res["error"]; !ok {
		t.Fatalf("expected disallowed tool to return error, got %+v", res)
	}
}
