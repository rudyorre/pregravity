package assistant

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"pregravity/internal/config"
	"pregravity/internal/gemini"
	"pregravity/internal/store"
)

// Reply holds the assistant's text response and metadata for a single turn.
type Reply struct {
	Text      string
	ToolCalls []string
	Handoffs  []store.Handoff
	Tokens    int
}

// Assistant orchestrates Gemini API conversations with a strictly confined,
// shell-free accountability and Antigravity handoff toolset.
type Assistant struct {
	cfg      *config.Config
	store    *store.Store
	provider gemini.Provider
	logger   *slog.Logger

	// Per-session mutex to prevent interleaved turns on the same thread.
	sessMu sync.Mutex
	locks  map[string]*sync.Mutex
}

// New creates an Assistant.
func New(cfg *config.Config, st *store.Store, provider gemini.Provider, logger *slog.Logger) *Assistant {
	if logger == nil {
		logger = slog.Default()
	}
	return &Assistant{
		cfg:      cfg,
		store:    st,
		provider: provider,
		logger:   logger,
		locks:    make(map[string]*sync.Mutex),
	}
}

func (a *Assistant) lockSession(sessionID string) func() {
	a.sessMu.Lock()
	m, ok := a.locks[sessionID]
	if !ok {
		m = &sync.Mutex{}
		a.locks[sessionID] = m
	}
	a.sessMu.Unlock()
	m.Lock()
	return m.Unlock
}

// HandleMessage processes a user message within sessionID, executing any safe
// tool calls and persisting the turn.
func (a *Assistant) HandleMessage(ctx context.Context, sessionID, userText string) (Reply, error) {
	userText = strings.TrimSpace(userText)
	if userText == "" {
		return Reply{}, nil
	}

	unlock := a.lockSession(sessionID)
	defer unlock()

	history, err := a.store.LoadSession(sessionID, a.cfg.MaxHistoryTurns)
	if err != nil {
		return Reply{}, fmt.Errorf("load session: %w", err)
	}

	contents := make([]gemini.Content, 0, len(history)+4)
	for _, m := range history {
		contents = append(contents, gemini.Content{
			Role:  m.Role,
			Parts: []gemini.Part{{Text: m.Text}},
		})
	}
	contents = append(contents, gemini.Content{
		Role:  "user",
		Parts: []gemini.Part{{Text: userText}},
	})

	sysPrompt := a.buildSystemPrompt()
	tools := []gemini.Tool{{FunctionDeclarations: a.toolDeclarations()}}

	var reply Reply
	maxRounds := a.cfg.MaxToolRounds
	if maxRounds <= 0 {
		maxRounds = 6
	}

	for round := 0; round < maxRounds; round++ {
		resp, usedModel, err := a.generateWithFallback(ctx, gemini.GenerateRequest{
			Model: a.cfg.Model,
			SystemInstruction: &gemini.Content{
				Parts: []gemini.Part{{Text: sysPrompt}},
			},
			Contents: contents,
			Tools:    tools,
		})
		if err != nil {
			return reply, err
		}
		var pTok, oTok, tTok int
		if resp.UsageMetadata != nil {
			pTok = resp.UsageMetadata.PromptTokenCount
			oTok = resp.UsageMetadata.CandidatesTokenCount
			tTok = resp.UsageMetadata.TotalTokenCount
			reply.Tokens += tTok
		}
		_ = a.store.RecordAPIUsage(usedModel, pTok, oTok, tTok, resp.ServiceTier, false, 0)

		cand := resp.Candidates[0].Content
		if cand.Role == "" {
			cand.Role = "model"
		}
		contents = append(contents, cand)

		var calls []*gemini.FunctionCall
		var textParts []string
		for _, p := range cand.Parts {
			if p.FunctionCall != nil {
				calls = append(calls, p.FunctionCall)
			}
			if !p.Thought && strings.TrimSpace(p.Text) != "" {
				textParts = append(textParts, p.Text)
			}
		}

		if len(calls) == 0 {
			reply.Text = strings.TrimSpace(strings.Join(textParts, "\n"))
			break
		}

		// Execute strictly whitelisted, shell-free tools.
		respParts := make([]gemini.Part, 0, len(calls))
		for _, fc := range calls {
			reply.ToolCalls = append(reply.ToolCalls, fc.Name)
			a.logger.Info("executing tool", "session", sessionID, "tool", fc.Name)
			result, createdHandoff := a.executeTool(fc.Name, fc.Args)
			if createdHandoff != nil {
				reply.Handoffs = append(reply.Handoffs, *createdHandoff)
			}
			respParts = append(respParts, gemini.Part{
				FunctionResponse: &gemini.FunctionResponse{
					Name:     fc.Name,
					Response: result,
				},
			})
		}
		contents = append(contents, gemini.Content{
			Role:  "user",
			Parts: respParts,
		})
	}

	if reply.Text == "" {
		reply.Text = "Done. (No additional text returned.)"
	}

	now := time.Now().UTC()
	if err := a.store.AppendSession(sessionID,
		store.Message{Role: "user", Text: userText, Timestamp: now},
		store.Message{Role: "model", Text: reply.Text, Timestamp: time.Now().UTC()},
	); err != nil {
		a.logger.Warn("failed to persist session turn", "session", sessionID, "err", err)
	}
	return reply, nil
}

// RunScheduledCheckin executes a scheduled prompt in a dedicated schedule session.
func (a *Assistant) RunScheduledCheckin(ctx context.Context, scheduleName, prompt string) (Reply, error) {
	sessionID := "sched-" + sanitizeID(scheduleName)
	return a.HandleMessage(ctx, sessionID, prompt)
}

// generateWithFallback tries the primary model first and automatically falls back
// through cfg.FallbackModels on 429 RESOURCE_EXHAUSTED or 503 overload errors.
func (a *Assistant) generateWithFallback(ctx context.Context, req gemini.GenerateRequest) (*gemini.GenerateResponse, string, error) {
	models := make([]string, 0, 1+len(a.cfg.FallbackModels))
	models = append(models, a.cfg.Model)
	for _, fm := range a.cfg.FallbackModels {
		fm = strings.TrimSpace(fm)
		if fm != "" && fm != a.cfg.Model {
			models = append(models, fm)
		}
	}

	var lastErr error
	for i, model := range models {
		req.Model = model
		resp, err := a.provider.Generate(ctx, req)
		if err == nil {
			return resp, model, nil
		}
		lastErr = err
		var apiErr *gemini.APIError
		if errors.As(err, &apiErr) {
			if apiErr.Code == 429 {
				_ = a.store.RecordAPIUsage(model, 0, 0, 0, "", true, apiErr.QuotaLimit())
			}
			if (apiErr.Code == 429 || apiErr.Code == 503) && i+1 < len(models) {
				a.logger.Warn("model rate-limited or overloaded, trying fallback", "model", model, "fallback", models[i+1], "code", apiErr.Code)
				continue
			}
		}
		return nil, model, err
	}
	return nil, a.cfg.Model, lastErr
}

func (a *Assistant) buildSystemPrompt() string {
	loc, err := a.cfg.Location()
	if err != nil {
		loc = time.Local
	}
	now := time.Now().In(loc)

	var b strings.Builder
	b.WriteString(a.cfg.Persona)
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "Current date/time: %s (%s)\n", now.Format("Monday, 2006-01-02 15:04 MST"), loc.String())
	fmt.Fprintf(&b, "Antigravity Remote Control dashboard: %s (instance: %s, pickup command: %s <id>)\n",
		a.cfg.Antigravity.DashboardURL, a.cfg.Antigravity.InstanceName, a.cfg.Antigravity.PickupSkill)
	b.WriteString("\nSecurity & Handoff Rules:\n")
	b.WriteString("- You have NO shell execution and NO access to user source repositories.\n")
	b.WriteString("- Use your tools to manage tasks, durable memory, daily journal notes, and Antigravity handoffs.\n")
	b.WriteString("- Open tasks, pending reminders, and recent handoffs are ALREADY included below—do NOT call list_tasks, list_reminders, or list_handoffs unless you need completed/historical items not shown below (this conserves daily API quota).\n")
	b.WriteString("- When the user wants deep coding, debugging, terminal work, or multi-file changes, call `create_handoff` to stage a crisp briefing, then give them the Antigravity link and note that the `/pickup <id>` command will be sent right below.\n")

	if mem, err := a.store.ReadMemory(); err == nil && strings.TrimSpace(mem) != "" {
		b.WriteString("\n--- Durable Memory (MEMORY.md) ---\n")
		b.WriteString(mem)
		b.WriteString("\n")
	}

	if tasks, err := a.store.ListTasks("open"); err == nil && len(tasks) > 0 {
		b.WriteString("\n--- Current Open Tasks ---\n")
		for _, t := range tasks {
			due := ""
			if t.Due != "" {
				due = " [due: " + t.Due + "]"
			}
			fmt.Fprintf(&b, "- %s (%s): %s%s\n", t.ID, t.Priority, t.Title, due)
		}
	}

	if rems, err := a.store.ListReminders(false); err == nil && len(rems) > 0 {
		b.WriteString("\n--- Pending One-Shot Reminders ---\n")
		for _, r := range rems {
			fmt.Fprintf(&b, "- %s at %s: %s\n", r.ID, r.At, r.Message)
		}
	}

	if handoffs, err := a.store.ListHandoffs("all"); err == nil && len(handoffs) > 0 {
		b.WriteString("\n--- Recent Antigravity Handoffs ---\n")
		limit := len(handoffs)
		if limit > 6 {
			limit = 6
		}
		for _, h := range handoffs[:limit] {
			line := fmt.Sprintf("- %s [%s]: %s", h.ID, h.Status, h.Title)
			if h.Status == "done" && h.Outcome != "" {
				summary := strings.ReplaceAll(h.Outcome, "\n", " ")
				if len(summary) > 160 {
					summary = summary[:160] + "..."
				}
				line += " -> " + summary
			}
			b.WriteString(line + "\n")
		}
	}

	if journal, err := a.store.ReadTodayJournal(); err == nil && strings.TrimSpace(journal) != "" {
		b.WriteString("\n--- Today's Journal ---\n")
		b.WriteString(journal)
		b.WriteString("\n")
	}

	return b.String()
}

func (a *Assistant) toolDeclarations() []gemini.FunctionDeclaration {
	return []gemini.FunctionDeclaration{
		{
			Name:        "list_tasks",
			Description: "List tracked personal tasks and commitments.",
			Parameters: map[string]any{
				"type": "OBJECT",
				"properties": map[string]any{
					"status": map[string]any{
						"type":        "STRING",
						"description": "Filter by status: open, done, snoozed, or all. Defaults to open.",
					},
				},
			},
		},
		{
			Name:        "add_task",
			Description: "Add a new task or commitment to track and hold the user accountable for.",
			Parameters: map[string]any{
				"type": "OBJECT",
				"properties": map[string]any{
					"title":    map[string]any{"type": "STRING", "description": "Short action-oriented task title."},
					"notes":    map[string]any{"type": "STRING", "description": "Optional context or acceptance criteria."},
					"priority": map[string]any{"type": "STRING", "description": "low, medium, or high."},
					"due":      map[string]any{"type": "STRING", "description": "Optional due date/time. For exact timed reminder pings, format as 'YYYY-MM-DD HH:MM' in the user's local timezone."},
				},
				"required": []string{"title"},
			},
		},
		{
			Name:        "update_task",
			Description: "Update an existing task's status (open/done/snoozed), due date, priority, or notes.",
			Parameters: map[string]any{
				"type": "OBJECT",
				"properties": map[string]any{
					"id":       map[string]any{"type": "STRING", "description": "Task ID (e.g. t-a1b2c3)."},
					"status":   map[string]any{"type": "STRING", "description": "New status: open, done, or snoozed."},
					"due":      map[string]any{"type": "STRING", "description": "Updated due date/time."},
					"priority": map[string]any{"type": "STRING", "description": "Updated priority: low, medium, or high."},
					"notes":    map[string]any{"type": "STRING", "description": "Updated notes."},
				},
				"required": []string{"id"},
			},
		},
		{
			Name:        "set_reminder",
			Description: "Schedule a one-shot timed ping (zero-token notification) at a specific local date and time.",
			Parameters: map[string]any{
				"type": "OBJECT",
				"properties": map[string]any{
					"message": map[string]any{"type": "STRING", "description": "What to remind the user about."},
					"at":      map[string]any{"type": "STRING", "description": "Exact time formatted as 'YYYY-MM-DD HH:MM' in the user's local timezone."},
				},
				"required": []string{"message", "at"},
			},
		},
		{
			Name:        "list_reminders",
			Description: "List pending one-shot reminders.",
			Parameters:  map[string]any{"type": "OBJECT", "properties": map[string]any{}},
		},
		{
			Name:        "cancel_reminder",
			Description: "Cancel a pending one-shot reminder by ID.",
			Parameters: map[string]any{
				"type": "OBJECT",
				"properties": map[string]any{
					"id": map[string]any{"type": "STRING", "description": "Reminder ID (e.g. r-a1b2c3)."},
				},
				"required": []string{"id"},
			},
		},
		{
			Name:        "append_memory",
			Description: "Save a durable personal preference, recurring goal, or long-term fact to MEMORY.md.",
			Parameters: map[string]any{
				"type": "OBJECT",
				"properties": map[string]any{
					"entry": map[string]any{"type": "STRING", "description": "Concise durable fact to remember."},
				},
				"required": []string{"entry"},
			},
		},
		{
			Name:        "log_note",
			Description: "Append a timestamped note or accountability check-in to today's journal.",
			Parameters: map[string]any{
				"type": "OBJECT",
				"properties": map[string]any{
					"note": map[string]any{"type": "STRING", "description": "Note to record in today's journal."},
				},
				"required": []string{"note"},
			},
		},
		{
			Name:        "create_handoff",
			Description: "Stage a clean Markdown handoff briefing for Antigravity (agy) when a thread needs coding, terminal work, or deep reasoning.",
			Parameters: map[string]any{
				"type": "OBJECT",
				"properties": map[string]any{
					"title":         map[string]any{"type": "STRING", "description": "Short title for the handoff."},
					"goal":          map[string]any{"type": "STRING", "description": "Clear description of what Antigravity should accomplish."},
					"context":       map[string]any{"type": "STRING", "description": "Relevant background from the conversation, repos, or symptoms."},
					"constraints":   map[string]any{"type": "STRING", "description": "Optional guardrails, tests to run, or things to avoid."},
					"related_tasks": map[string]any{"type": "ARRAY", "items": map[string]any{"type": "STRING"}, "description": "Optional list of related task IDs."},
				},
				"required": []string{"title", "goal"},
			},
		},
		{
			Name:        "list_handoffs",
			Description: "List Antigravity handoffs and check whether any have completed (.done.md summaries).",
			Parameters: map[string]any{
				"type": "OBJECT",
				"properties": map[string]any{
					"status": map[string]any{"type": "STRING", "description": "Filter by open, done, or all."},
				},
			},
		},
		{
			Name:        "get_quota",
			Description: "Get Pregravity's locally tracked Gemini API daily request/token usage, remaining free-tier budget, and time until midnight PT reset.",
			Parameters:  map[string]any{"type": "OBJECT", "properties": map[string]any{}},
		},
	}
}

func (a *Assistant) executeTool(name string, args map[string]any) (map[string]any, *store.Handoff) {
	strArg := func(k string) string {
		if args == nil {
			return ""
		}
		if v, ok := args[k]; ok && v != nil {
			return fmt.Sprint(v)
		}
		return ""
	}
	strSliceArg := func(k string) []string {
		if args == nil {
			return nil
		}
		raw, ok := args[k]
		if !ok || raw == nil {
			return nil
		}
		list, ok := raw.([]any)
		if !ok {
			return nil
		}
		out := make([]string, 0, len(list))
		for _, item := range list {
			s := strings.TrimSpace(fmt.Sprint(item))
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	}

	switch name {
	case "list_tasks":
		status := strArg("status")
		if status == "" {
			status = "open"
		}
		tasks, err := a.store.ListTasks(status)
		if err != nil {
			return map[string]any{"error": err.Error()}, nil
		}
		return map[string]any{"tasks": tasks, "count": len(tasks)}, nil

	case "add_task":
		t, err := a.store.AddTask(strArg("title"), strArg("notes"), strArg("priority"), strArg("due"))
		if err != nil {
			return map[string]any{"error": err.Error()}, nil
		}
		return map[string]any{"task": t}, nil

	case "update_task":
		t, err := a.store.UpdateTask(strArg("id"), strArg("status"), strArg("due"), strArg("notes"), strArg("priority"))
		if err != nil {
			return map[string]any{"error": err.Error()}, nil
		}
		return map[string]any{"task": t}, nil

	case "set_reminder":
		loc, err := a.cfg.Location()
		if err != nil {
			loc = time.Local
		}
		r, err := a.store.AddReminder(strArg("message"), strArg("at"), loc)
		if err != nil {
			return map[string]any{"error": err.Error()}, nil
		}
		return map[string]any{"reminder": r}, nil

	case "list_reminders":
		rems, err := a.store.ListReminders(false)
		if err != nil {
			return map[string]any{"error": err.Error()}, nil
		}
		return map[string]any{"reminders": rems, "count": len(rems)}, nil

	case "cancel_reminder":
		if err := a.store.CancelReminder(strArg("id")); err != nil {
			return map[string]any{"error": err.Error()}, nil
		}
		return map[string]any{"ok": true}, nil

	case "append_memory":
		if err := a.store.AppendMemory(strArg("entry")); err != nil {
			return map[string]any{"error": err.Error()}, nil
		}
		return map[string]any{"ok": true}, nil

	case "log_note":
		p, err := a.store.AppendJournal(strArg("note"))
		if err != nil {
			return map[string]any{"error": err.Error()}, nil
		}
		return map[string]any{"ok": true, "path": p}, nil

	case "create_handoff":
		h, err := a.store.CreateHandoff(
			strArg("title"),
			strArg("goal"),
			strArg("context"),
			strArg("constraints"),
			strSliceArg("related_tasks"),
		)
		if err != nil {
			return map[string]any{"error": err.Error()}, nil
		}
		return map[string]any{
			"handoff":        h,
			"dashboard_url":  a.cfg.Antigravity.DashboardURL,
			"instance_name":  a.cfg.Antigravity.InstanceName,
			"pickup_command": fmt.Sprintf("%s %s", a.cfg.Antigravity.PickupSkill, h.ID),
		}, &h

	case "list_handoffs":
		hs, err := a.store.ListHandoffs(strArg("status"))
		if err != nil {
			return map[string]any{"error": err.Error()}, nil
		}
		return map[string]any{"handoffs": hs, "count": len(hs)}, nil

	case "get_quota":
		summary, err := a.store.GetUsageSummary(time.Now(), a.cfg.Model, a.cfg.Quota.LimitForModel(a.cfg.Model))
		if err != nil {
			return map[string]any{"error": err.Error()}, nil
		}
		return map[string]any{"quota": summary}, nil

	default:
		return map[string]any{"error": fmt.Sprintf("unknown or disallowed tool %q", name)}, nil
	}
}

func sanitizeID(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	res := strings.Trim(b.String(), "-")
	if res == "" {
		return "default"
	}
	return res
}
