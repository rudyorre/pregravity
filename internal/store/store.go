package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var safeIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

// Task represents a personal commitment or todo tracked by Pregravity.
type Task struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	Notes        string    `json:"notes,omitempty"`
	Priority     string    `json:"priority"` // low, medium, high
	Status       string    `json:"status"`   // open, done, snoozed
	Due          string    `json:"due,omitempty"`
	NotifiedLead bool      `json:"notified_lead,omitempty"`
	NotifiedDue  bool      `json:"notified_due,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	CompletedAt  time.Time `json:"completed_at,omitempty"`
}

// Reminder represents a standalone one-shot timed ping.
type Reminder struct {
	ID        string    `json:"id"`
	Message   string    `json:"message"`
	At        string    `json:"at"` // YYYY-MM-DD HH:MM (local) or RFC3339
	Fired     bool      `json:"fired"`
	CreatedAt time.Time `json:"created_at"`
	FiredAt   time.Time `json:"fired_at,omitempty"`
}

// DueNotification is an actionable ping ready to send (zero LLM tokens required).
type DueNotification struct {
	Kind    string // "task_lead", "task_due", "reminder"
	ID      string
	Title   string
	DueText string
	LeadMin int
}

// Handoff represents a structured escalation packet handed off to Antigravity (agy).
type Handoff struct {
	ID            string    `json:"id"`
	Title         string    `json:"title"`
	Goal          string    `json:"goal"`
	Context       string    `json:"context"`
	Constraints   string    `json:"constraints,omitempty"`
	RelatedTasks  []string  `json:"related_tasks,omitempty"`
	Status        string    `json:"status"` // open, done
	CreatedAt     time.Time `json:"created_at"`
	CompletedAt   time.Time `json:"completed_at,omitempty"`
	Outcome       string    `json:"outcome,omitempty"`
	MarkdownPath  string    `json:"markdown_path"`
	DoneNotePath  string    `json:"done_note_path,omitempty"`
}

// Message is a persisted conversational turn for a given thread/chat ID.
type Message struct {
	Role      string    `json:"role"` // user, model
	Text      string    `json:"text"`
	Timestamp time.Time `json:"timestamp"`
}

// Store manages all Pregravity state strictly confined to rootDir.
type Store struct {
	rootDir string
	mu      sync.Mutex
}

// Open initializes the directory layout under rootDir and returns a Store.
func Open(rootDir string) (*Store, error) {
	absRoot, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, fmt.Errorf("resolve data dir: %w", err)
	}
	dirs := []string{
		absRoot,
		filepath.Join(absRoot, "handoffs"),
		filepath.Join(absRoot, "journal"),
		filepath.Join(absRoot, "sessions"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("create dir %s: %w", d, err)
		}
	}

	s := &Store{rootDir: absRoot}

	// Seed MEMORY.md if missing.
	memPath := s.MemoryPath()
	if _, err := os.Stat(memPath); errors.Is(err, os.ErrNotExist) {
		seed := "# Pregravity Memory\n\nDurable preferences, recurring goals, and personal context live here.\n"
		if err := atomicWriteFile(memPath, []byte(seed), 0o600); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// RootDir returns the confined root data directory.
func (s *Store) RootDir() string { return s.rootDir }

// MemoryPath returns the path to MEMORY.md.
func (s *Store) MemoryPath() string { return filepath.Join(s.rootDir, "MEMORY.md") }

// HandoffsDir returns the directory containing Antigravity handoff briefs.
func (s *Store) HandoffsDir() string { return filepath.Join(s.rootDir, "handoffs") }

func (s *Store) tasksJSONPath() string     { return filepath.Join(s.rootDir, "tasks.json") }
func (s *Store) tasksMDPath() string       { return filepath.Join(s.rootDir, "TASKS.md") }
func (s *Store) remindersJSONPath() string { return filepath.Join(s.rootDir, "reminders.json") }

// safePath ensures rel stays strictly within s.rootDir (prevents path traversal).
func (s *Store) safePath(elems ...string) (string, error) {
	joined := filepath.Join(append([]string{s.rootDir}, elems...)...)
	cleaned := filepath.Clean(joined)
	if cleaned != s.rootDir && !strings.HasPrefix(cleaned, s.rootDir+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes data directory", cleaned)
	}
	return cleaned, nil
}

// --- Tasks ---

func (s *Store) loadTasksLocked() ([]Task, error) {
	raw, err := os.ReadFile(s.tasksJSONPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []Task{}, nil
		}
		return nil, err
	}
	var tasks []Task
	if err := json.Unmarshal(raw, &tasks); err != nil {
		return nil, fmt.Errorf("parse tasks.json: %w", err)
	}
	return tasks, nil
}

func (s *Store) saveTasksLocked(tasks []Task) error {
	raw, err := json.MarshalIndent(tasks, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if err := atomicWriteFile(s.tasksJSONPath(), raw, 0o600); err != nil {
		return err
	}
	// Also render a clean human-readable TASKS.md snapshot.
	return atomicWriteFile(s.tasksMDPath(), []byte(renderTasksMarkdown(tasks)), 0o600)
}

// ListTasks returns tasks filtered by status ("", "all", "open", "done", "snoozed").
func (s *Store) ListTasks(status string) ([]Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tasks, err := s.loadTasksLocked()
	if err != nil {
		return nil, err
	}
	status = strings.ToLower(strings.TrimSpace(status))
	if status == "" || status == "all" {
		return tasks, nil
	}
	out := make([]Task, 0, len(tasks))
	for _, t := range tasks {
		if t.Status == status {
			out = append(out, t)
		}
	}
	return out, nil
}

// AddTask creates and persists a new task.
func (s *Store) AddTask(title, notes, priority, due string) (Task, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return Task{}, errors.New("task title cannot be empty")
	}
	priority = normalizePriority(priority)

	s.mu.Lock()
	defer s.mu.Unlock()

	tasks, err := s.loadTasksLocked()
	if err != nil {
		return Task{}, err
	}
	now := time.Now().UTC()
	task := Task{
		ID:        "t-" + shortRandHex(3),
		Title:     title,
		Notes:     strings.TrimSpace(notes),
		Priority:  priority,
		Status:    "open",
		Due:       strings.TrimSpace(due),
		CreatedAt: now,
		UpdatedAt: now,
	}
	tasks = append(tasks, task)
	if err := s.saveTasksLocked(tasks); err != nil {
		return Task{}, err
	}
	return task, nil
}

// UpdateTask updates fields on an existing task by ID.
func (s *Store) UpdateTask(id, status, due, notes, priority string) (Task, error) {
	id = strings.TrimSpace(id)
	if !safeIDPattern.MatchString(id) {
		return Task{}, fmt.Errorf("invalid task id %q", id)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tasks, err := s.loadTasksLocked()
	if err != nil {
		return Task{}, err
	}
	for i := range tasks {
		if tasks[i].ID != id {
			continue
		}
		now := time.Now().UTC()
		if status != "" {
			st := strings.ToLower(strings.TrimSpace(status))
			if st != "open" && st != "done" && st != "snoozed" {
				return Task{}, fmt.Errorf("invalid status %q (expected open, done, snoozed)", status)
			}
			tasks[i].Status = st
			if st == "done" {
				tasks[i].CompletedAt = now
			}
		}
		if due != "" {
			newDue := strings.TrimSpace(due)
			if newDue != tasks[i].Due {
				tasks[i].Due = newDue
				tasks[i].NotifiedLead = false
				tasks[i].NotifiedDue = false
			}
		}
		if notes != "" {
			tasks[i].Notes = strings.TrimSpace(notes)
		}
		if priority != "" {
			tasks[i].Priority = normalizePriority(priority)
		}
		tasks[i].UpdatedAt = now
		if err := s.saveTasksLocked(tasks); err != nil {
			return Task{}, err
		}
		return tasks[i], nil
	}
	return Task{}, fmt.Errorf("task %q not found", id)
}

// --- Reminders & Due-Time Notification Engine ---

func (s *Store) loadRemindersLocked() ([]Reminder, error) {
	raw, err := os.ReadFile(s.remindersJSONPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []Reminder{}, nil
		}
		return nil, err
	}
	var out []Reminder
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse reminders.json: %w", err)
	}
	return out, nil
}

func (s *Store) saveRemindersLocked(rems []Reminder) error {
	raw, err := json.MarshalIndent(rems, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(s.remindersJSONPath(), append(raw, '\n'), 0o600)
}

// ParseLocalTime parses a user/LLM timestamp string in loc (supports RFC3339 and YYYY-MM-DD HH:MM[:SS]).
func ParseLocalTime(raw string, loc *time.Location) (time.Time, bool) {
	if loc == nil {
		loc = time.Local
	}
	s := strings.TrimSpace(raw)
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.In(loc), true
	}
	layouts := []string{
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
	}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// AddReminder creates a standalone one-shot reminder at a parseable local timestamp.
func (s *Store) AddReminder(message, at string, loc *time.Location) (Reminder, error) {
	message = strings.TrimSpace(message)
	at = strings.TrimSpace(at)
	if message == "" {
		return Reminder{}, errors.New("reminder message cannot be empty")
	}
	parsed, ok := ParseLocalTime(at, loc)
	if !ok {
		return Reminder{}, fmt.Errorf("invalid reminder time %q (use YYYY-MM-DD HH:MM in local time or RFC3339)", at)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	rems, err := s.loadRemindersLocked()
	if err != nil {
		return Reminder{}, err
	}
	r := Reminder{
		ID:        "r-" + shortRandHex(3),
		Message:   message,
		At:        parsed.Format("2006-01-02 15:04"),
		Fired:     false,
		CreatedAt: time.Now().UTC(),
	}
	rems = append(rems, r)
	if err := s.saveRemindersLocked(rems); err != nil {
		return Reminder{}, err
	}
	return r, nil
}

// ListReminders returns pending (or all) reminders.
func (s *Store) ListReminders(includeFired bool) ([]Reminder, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rems, err := s.loadRemindersLocked()
	if err != nil {
		return nil, err
	}
	if includeFired {
		return rems, nil
	}
	out := make([]Reminder, 0, len(rems))
	for _, r := range rems {
		if !r.Fired {
			out = append(out, r)
		}
	}
	return out, nil
}

// CancelReminder deletes a reminder by ID.
func (s *Store) CancelReminder(id string) error {
	id = strings.TrimSpace(id)
	if !safeIDPattern.MatchString(id) {
		return fmt.Errorf("invalid reminder id %q", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	rems, err := s.loadRemindersLocked()
	if err != nil {
		return err
	}
	out := make([]Reminder, 0, len(rems))
	found := false
	for _, r := range rems {
		if r.ID == id {
			found = true
			continue
		}
		out = append(out, r)
	}
	if !found {
		return fmt.Errorf("reminder %q not found", id)
	}
	return s.saveRemindersLocked(out)
}

// ClaimDueNotifications atomically finds any open tasks or unfired reminders that
// have entered their lead-time or due window at `now`, marks them notified so they
// never fire twice, and returns only newly claimed pings. On idle ticks nothing is written.
func (s *Store) ClaimDueNotifications(now time.Time, loc *time.Location, lead time.Duration) ([]DueNotification, error) {
	if loc == nil {
		loc = time.Local
	}
	nowLocal := now.In(loc)

	s.mu.Lock()
	defer s.mu.Unlock()

	var claimed []DueNotification

	tasks, err := s.loadTasksLocked()
	if err != nil {
		return nil, err
	}
	tasksDirty := false
	for i := range tasks {
		if tasks[i].Status != "open" || tasks[i].Due == "" {
			continue
		}
		dueAt, ok := ParseLocalTime(tasks[i].Due, loc)
		if !ok {
			continue
		}
		// Exact due time (or overdue and not yet pinged for due).
		if !nowLocal.Before(dueAt) {
			if !tasks[i].NotifiedDue {
				tasks[i].NotifiedDue = true
				tasks[i].NotifiedLead = true
				tasksDirty = true
				claimed = append(claimed, DueNotification{
					Kind:    "task_due",
					ID:      tasks[i].ID,
					Title:   tasks[i].Title,
					DueText: dueAt.Format("15:04 MST"),
				})
			}
			continue
		}
		// Lead-time heads-up window (e.g. 15m before due).
		if lead > 0 && !tasks[i].NotifiedLead && !nowLocal.Before(dueAt.Add(-lead)) {
			tasks[i].NotifiedLead = true
			tasksDirty = true
			mins := int(dueAt.Sub(nowLocal).Round(time.Minute) / time.Minute)
			if mins < 1 {
				mins = 1
			}
			claimed = append(claimed, DueNotification{
				Kind:    "task_lead",
				ID:      tasks[i].ID,
				Title:   tasks[i].Title,
				DueText: dueAt.Format("15:04 MST"),
				LeadMin: mins,
			})
		}
	}
	if tasksDirty {
		if err := s.saveTasksLocked(tasks); err != nil {
			return nil, err
		}
	}

	rems, err := s.loadRemindersLocked()
	if err != nil {
		return nil, err
	}
	remsDirty := false
	for i := range rems {
		if rems[i].Fired {
			continue
		}
		at, ok := ParseLocalTime(rems[i].At, loc)
		if !ok {
			continue
		}
		if !nowLocal.Before(at) {
			rems[i].Fired = true
			rems[i].FiredAt = now.UTC()
			remsDirty = true
			claimed = append(claimed, DueNotification{
				Kind:    "reminder",
				ID:      rems[i].ID,
				Title:   rems[i].Message,
				DueText: at.Format("15:04 MST"),
			})
		}
	}
	if remsDirty {
		if err := s.saveRemindersLocked(rems); err != nil {
			return nil, err
		}
	}

	return claimed, nil
}

// --- Memory & Journal ---

// ReadMemory returns the contents of MEMORY.md (capped at 16 KiB for prompt hygiene).
func (s *Store) ReadMemory() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, err := os.ReadFile(s.MemoryPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	const maxBytes = 16 * 1024
	if len(raw) > maxBytes {
		raw = raw[len(raw)-maxBytes:]
	}
	return string(raw), nil
}

// AppendMemory appends a durable bullet entry to MEMORY.md.
func (s *Store) AppendMemory(entry string) error {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return errors.New("memory entry cannot be empty")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	existing, err := os.ReadFile(s.MemoryPath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var b strings.Builder
	b.Write(existing)
	if len(existing) > 0 && existing[len(existing)-1] != '\n' {
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "- [%s] %s\n", time.Now().Format("2006-01-02"), entry)
	return atomicWriteFile(s.MemoryPath(), []byte(b.String()), 0o600)
}

// AppendJournal appends a timestamped note to journal/YYYY-MM-DD.md.
func (s *Store) AppendJournal(note string) (string, error) {
	note = strings.TrimSpace(note)
	if note == "" {
		return "", errors.New("journal note cannot be empty")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	rel := filepath.Join("journal", now.Format("2006-01-02")+".md")
	p, err := s.safePath(rel)
	if err != nil {
		return "", err
	}
	existing, err := os.ReadFile(p)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	var b strings.Builder
	if len(existing) == 0 {
		fmt.Fprintf(&b, "# Journal — %s\n\n", now.Format("2006-01-02"))
	} else {
		b.Write(existing)
		if existing[len(existing)-1] != '\n' {
			b.WriteByte('\n')
		}
	}
	fmt.Fprintf(&b, "- **%s** %s\n", now.Format("15:04"), note)
	if err := atomicWriteFile(p, []byte(b.String()), 0o600); err != nil {
		return "", err
	}
	return p, nil
}

// ReadTodayJournal returns today's journal notes if any exist.
func (s *Store) ReadTodayJournal() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rel := filepath.Join("journal", time.Now().Format("2006-01-02")+".md")
	p, err := s.safePath(rel)
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	return string(raw), nil
}

// --- Sessions ---

// LoadSession loads up to maxMessages recent messages for sessionID.
func (s *Store) LoadSession(sessionID string, maxMessages int) ([]Message, error) {
	if !safeIDPattern.MatchString(sessionID) {
		return nil, fmt.Errorf("invalid session id %q", sessionID)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	p, err := s.safePath("sessions", sessionID+".jsonl")
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []Message{}, nil
		}
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	msgs := make([]Message, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var m Message
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		msgs = append(msgs, m)
	}
	if maxMessages > 0 && len(msgs) > maxMessages {
		msgs = msgs[len(msgs)-maxMessages:]
	}
	return msgs, nil
}

// AppendSession appends one or more messages to sessionID.jsonl.
func (s *Store) AppendSession(sessionID string, msgs ...Message) error {
	if !safeIDPattern.MatchString(sessionID) {
		return fmt.Errorf("invalid session id %q", sessionID)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	p, err := s.safePath("sessions", sessionID+".jsonl")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	for _, m := range msgs {
		if m.Timestamp.IsZero() {
			m.Timestamp = time.Now().UTC()
		}
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		if _, err := f.Write(append(b, '\n')); err != nil {
			return err
		}
	}
	return nil
}

// --- Handoffs (Antigravity /pickup bridge) ---

// CreateHandoff writes both <id>.json metadata and <id>.md human/agent briefing.
func (s *Store) CreateHandoff(title, goal, ctxSummary, constraints string, relatedTasks []string) (Handoff, error) {
	title = strings.TrimSpace(title)
	goal = strings.TrimSpace(goal)
	if title == "" || goal == "" {
		return Handoff{}, errors.New("handoff title and goal are required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	slug := slugify(title)
	if len(slug) > 24 {
		slug = strings.TrimRight(slug[:24], "-")
	}
	id := fmt.Sprintf("%s-%s-%s", now.Format("20060102"), slug, shortRandHex(2))

	mdPath, err := s.safePath("handoffs", id+".md")
	if err != nil {
		return Handoff{}, err
	}
	jsonPath, err := s.safePath("handoffs", id+".json")
	if err != nil {
		return Handoff{}, err
	}

	h := Handoff{
		ID:           id,
		Title:        title,
		Goal:         goal,
		Context:      strings.TrimSpace(ctxSummary),
		Constraints:  strings.TrimSpace(constraints),
		RelatedTasks: relatedTasks,
		Status:       "open",
		CreatedAt:    now,
		MarkdownPath: mdPath,
	}

	md := renderHandoffMarkdown(h)
	if err := atomicWriteFile(mdPath, []byte(md), 0o600); err != nil {
		return Handoff{}, err
	}
	raw, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return Handoff{}, err
	}
	if err := atomicWriteFile(jsonPath, append(raw, '\n'), 0o600); err != nil {
		return Handoff{}, err
	}
	return h, nil
}

// ListHandoffs returns handoffs filtered by status ("", "all", "open", "done"),
// automatically detecting <id>.done.md files written back by Antigravity /pickup.
func (s *Store) ListHandoffs(status string) ([]Handoff, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.HandoffsDir())
	if err != nil {
		return nil, err
	}
	status = strings.ToLower(strings.TrimSpace(status))
	var out []Handoff
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p, err := s.safePath("handoffs", e.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var h Handoff
		if err := json.Unmarshal(raw, &h); err != nil {
			continue
		}
		// Sync completion if Antigravity /pickup dropped a <id>.done.md file.
		donePath, err := s.safePath("handoffs", h.ID+".done.md")
		if err == nil {
			if doneRaw, err := os.ReadFile(donePath); err == nil {
				h.Status = "done"
				h.DoneNotePath = donePath
				h.Outcome = strings.TrimSpace(string(doneRaw))
				if h.CompletedAt.IsZero() {
					if fi, statErr := os.Stat(donePath); statErr == nil {
						h.CompletedAt = fi.ModTime().UTC()
					}
				}
			}
		}
		if status != "" && status != "all" && h.Status != status {
			continue
		}
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

// GetHandoff reads a single handoff by ID.
func (s *Store) GetHandoff(id string) (Handoff, error) {
	if !safeIDPattern.MatchString(id) {
		return Handoff{}, fmt.Errorf("invalid handoff id %q", id)
	}
	all, err := s.ListHandoffs("all")
	if err != nil {
		return Handoff{}, err
	}
	for _, h := range all {
		if h.ID == id {
			return h, nil
		}
	}
	return Handoff{}, fmt.Errorf("handoff %q not found", id)
}

// CompleteHandoff marks a handoff done and writes <id>.done.md.
func (s *Store) CompleteHandoff(id, outcome string) (Handoff, error) {
	if !safeIDPattern.MatchString(id) {
		return Handoff{}, fmt.Errorf("invalid handoff id %q", id)
	}
	h, err := s.GetHandoff(id)
	if err != nil {
		return Handoff{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	donePath, err := s.safePath("handoffs", id+".done.md")
	if err != nil {
		return Handoff{}, err
	}
	now := time.Now().UTC()
	body := fmt.Sprintf("# Outcome — %s\n\nCompleted: %s\n\n%s\n", h.Title, now.Format(time.RFC3339), strings.TrimSpace(outcome))
	if err := atomicWriteFile(donePath, []byte(body), 0o600); err != nil {
		return Handoff{}, err
	}
	h.Status = "done"
	h.CompletedAt = now
	h.Outcome = strings.TrimSpace(outcome)
	h.DoneNotePath = donePath

	jsonPath, err := s.safePath("handoffs", id+".json")
	if err != nil {
		return Handoff{}, err
	}
	raw, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return Handoff{}, err
	}
	if err := atomicWriteFile(jsonPath, append(raw, '\n'), 0o600); err != nil {
		return Handoff{}, err
	}
	return h, nil
}

// --- Helpers ---

func renderTasksMarkdown(tasks []Task) string {
	var b strings.Builder
	b.WriteString("# Pregravity Tasks\n\n")
	writeSection := func(heading, status string) {
		fmt.Fprintf(&b, "## %s\n\n", heading)
		count := 0
		for _, t := range tasks {
			if t.Status != status {
				continue
			}
			count++
			box := "[ ]"
			if status == "done" {
				box = "[x]"
			}
			due := ""
			if t.Due != "" {
				due = " (due: " + t.Due + ")"
			}
			fmt.Fprintf(&b, "- %s `%s` **[%s]** %s%s\n", box, t.ID, t.Priority, t.Title, due)
			if t.Notes != "" {
				fmt.Fprintf(&b, "  - %s\n", t.Notes)
			}
		}
		if count == 0 {
			b.WriteString("_None._\n")
		}
		b.WriteByte('\n')
	}
	writeSection("Open", "open")
	writeSection("Snoozed", "snoozed")
	writeSection("Completed", "done")
	return b.String()
}

func renderHandoffMarkdown(h Handoff) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Antigravity Handoff: %s\n\n", h.Title)
	fmt.Fprintf(&b, "- **Handoff ID:** `%s`\n", h.ID)
	fmt.Fprintf(&b, "- **Created:** %s\n", h.CreatedAt.Format(time.RFC3339))
	if len(h.RelatedTasks) > 0 {
		fmt.Fprintf(&b, "- **Related Tasks:** %s\n", strings.Join(h.RelatedTasks, ", "))
	}
	b.WriteString("\n## Goal\n\n")
	b.WriteString(h.Goal + "\n")
	if h.Context != "" {
		b.WriteString("\n## Context\n\n")
		b.WriteString(h.Context + "\n")
	}
	if h.Constraints != "" {
		b.WriteString("\n## Constraints\n\n")
		b.WriteString(h.Constraints + "\n")
	}
	b.WriteString("\n## Closing the Loop\n\n")
	fmt.Fprintf(&b, "When finished in Antigravity, write a short summary to `%s` (or run `pregravity handoffs done %s \"<summary>\"`) so Pregravity knows this is resolved.\n",
		filepath.Join(filepath.Dir(h.MarkdownPath), h.ID+".done.md"), h.ID)
	return b.String()
}

func normalizePriority(p string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "high", "h", "p0", "p1":
		return "high"
	case "low", "l", "p3":
		return "low"
	default:
		return "medium"
	}
}

func shortRandHex(nBytes int) string {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano()&0xffffff)
	}
	return hex.EncodeToString(b)
}

func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	res := strings.Trim(b.String(), "-")
	if res == "" {
		return "task"
	}
	return res
}

func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := f.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		cleanup()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		cleanup()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}
