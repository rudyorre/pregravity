package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStoreTasksMemoryJournalAndHandoffs(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// Tasks CRUD
	task, err := st.AddTask("Finish pregravity MVP", "Keep it zero-dep", "high", "2026-11-24")
	if err != nil {
		t.Fatalf("add task: %v", err)
	}
	openTasks, err := st.ListTasks("open")
	if err != nil || len(openTasks) != 1 || openTasks[0].ID != task.ID {
		t.Fatalf("unexpected open tasks: %+v err=%v", openTasks, err)
	}
	mdRaw, err := os.ReadFile(filepath.Join(dir, "TASKS.md"))
	if err != nil || !strings.Contains(string(mdRaw), "Finish pregravity MVP") {
		t.Fatalf("expected TASKS.md snapshot to contain task, got %q err=%v", string(mdRaw), err)
	}

	updated, err := st.UpdateTask(task.ID, "done", "", "", "")
	if err != nil || updated.Status != "done" || updated.CompletedAt.IsZero() {
		t.Fatalf("update task failed: %+v err=%v", updated, err)
	}

	// Memory & Journal
	if err := st.AppendMemory("Prefers concise bullet points"); err != nil {
		t.Fatalf("append memory: %v", err)
	}
	mem, err := st.ReadMemory()
	if err != nil || !strings.Contains(mem, "Prefers concise bullet points") {
		t.Fatalf("read memory mismatch: %q err=%v", mem, err)
	}
	if _, err := st.AppendJournal("Checked in on devbox setup"); err != nil {
		t.Fatalf("append journal: %v", err)
	}
	journal, err := st.ReadTodayJournal()
	if err != nil || !strings.Contains(journal, "Checked in on devbox setup") {
		t.Fatalf("read journal mismatch: %q err=%v", journal, err)
	}

	// Handoffs & automatic .done.md detection (simulating Antigravity /pickup closing the loop)
	h, err := st.CreateHandoff("Fix Auth Race", "Resolve token refresh race condition", "Occurs on resume", "Do not break CLI login", []string{task.ID})
	if err != nil {
		t.Fatalf("create handoff: %v", err)
	}
	if _, err := os.Stat(h.MarkdownPath); err != nil {
		t.Fatalf("expected handoff markdown file: %v", err)
	}

	// Simulate Antigravity /pickup writing <id>.done.md directly.
	doneFile := filepath.Join(st.HandoffsDir(), h.ID+".done.md")
	if err := os.WriteFile(doneFile, []byte("Fixed race with mutex and added regression test."), 0o600); err != nil {
		t.Fatalf("write done file: %v", err)
	}
	got, err := st.GetHandoff(h.ID)
	if err != nil {
		t.Fatalf("get handoff: %v", err)
	}
	if got.Status != "done" || !strings.Contains(got.Outcome, "Fixed race") {
		t.Fatalf("expected handoff to auto-detect .done.md completion: %+v", got)
	}
}

func TestStoreRejectsPathTraversal(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := st.LoadSession("../escape", 10); err == nil {
		t.Fatal("expected traversal session id to be rejected")
	}
	if _, err := st.GetHandoff("../../etc/passwd"); err == nil {
		t.Fatal("expected traversal handoff id to be rejected")
	}
	if _, err := st.UpdateTask("../bad", "done", "", "", ""); err == nil {
		t.Fatal("expected traversal task id to be rejected")
	}
}

func TestClaimDueNotificationsLeadDueRescheduleAndReminders(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	loc := time.FixedZone("PST", -8*3600)

	// Task due at 17:30 local, plus a fuzzy-due task that should be safely ignored.
	task, err := st.AddTask("Drop off TV in SF", "", "medium", "2026-11-23 17:30:00")
	if err != nil {
		t.Fatalf("add task: %v", err)
	}
	if _, err := st.AddTask("Someday task", "", "low", "next friday"); err != nil {
		t.Fatalf("add fuzzy task: %v", err)
	}
	rem, err := st.AddReminder("Grab keys", "2026-11-23 17:20", loc)
	if err != nil {
		t.Fatalf("add reminder: %v", err)
	}

	lead := 15 * time.Minute

	// 17:10 -> nothing due yet
	got, err := st.ClaimDueNotifications(time.Date(2026, 11, 23, 17, 10, 0, 0, loc), loc, lead)
	if err != nil || len(got) != 0 {
		t.Fatalf("expected 0 notifications at 17:10, got %+v err=%v", got, err)
	}

	// 17:15 -> task lead-time fires once
	got, err = st.ClaimDueNotifications(time.Date(2026, 11, 23, 17, 15, 0, 0, loc), loc, lead)
	if err != nil || len(got) != 1 || got[0].Kind != "task_lead" || got[0].ID != task.ID {
		t.Fatalf("expected task_lead at 17:15, got %+v err=%v", got, err)
	}
	// 17:16 -> no duplicate lead ping
	got, err = st.ClaimDueNotifications(time.Date(2026, 11, 23, 17, 16, 0, 0, loc), loc, lead)
	if err != nil || len(got) != 0 {
		t.Fatalf("expected no duplicate at 17:16, got %+v err=%v", got, err)
	}

	// 17:20 -> standalone reminder fires once
	got, err = st.ClaimDueNotifications(time.Date(2026, 11, 23, 17, 20, 0, 0, loc), loc, lead)
	if err != nil || len(got) != 1 || got[0].Kind != "reminder" || got[0].ID != rem.ID {
		t.Fatalf("expected reminder at 17:20, got %+v err=%v", got, err)
	}

	// 17:30 -> exact due-time fires once
	got, err = st.ClaimDueNotifications(time.Date(2026, 11, 23, 17, 30, 0, 0, loc), loc, lead)
	if err != nil || len(got) != 1 || got[0].Kind != "task_due" || got[0].ID != task.ID {
		t.Fatalf("expected task_due at 17:30, got %+v err=%v", got, err)
	}
	// 17:31 -> no duplicate due ping
	got, err = st.ClaimDueNotifications(time.Date(2026, 11, 23, 17, 31, 0, 0, loc), loc, lead)
	if err != nil || len(got) != 0 {
		t.Fatalf("expected no duplicate at 17:31, got %+v err=%v", got, err)
	}

	// Rescheduling due date resets notification flags so it pings again at the new time.
	if _, err := st.UpdateTask(task.ID, "", "2026-11-23 18:00", "", ""); err != nil {
		t.Fatalf("reschedule: %v", err)
	}
	got, err = st.ClaimDueNotifications(time.Date(2026, 11, 23, 18, 0, 0, 0, loc), loc, lead)
	if err != nil || len(got) != 1 || got[0].Kind != "task_due" {
		t.Fatalf("expected rescheduled task_due at 18:00, got %+v err=%v", got, err)
	}
}
