package gtasks

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// ReadonlyScope grants read-only access to Google Tasks.
	ReadonlyScope = "https://www.googleapis.com/auth/tasks.readonly"
	// ReadWriteScope grants read+write access to Google Tasks.
	// Note: Pregravity intentionally NEVER implements DELETE or clear endpoints—
	// only reversible operations (list, create, complete, reopen, move).
	ReadWriteScope = "https://www.googleapis.com/auth/tasks"
)

// SafeWriteToolNames is the exact allowlist of reversible MCP tools exposed in write mode.
var SafeWriteToolNames = []string{
	"list_tasklists",
	"list_tasks",
	"create_task",
	"complete_task",
	"reopen_task",
	"move_task",
}

// Credentials matches Google Cloud Desktop OAuth client JSON (installed or web).
type Credentials struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	AuthURI      string `json:"auth_uri"`
	TokenURI     string `json:"token_uri"`
	RedirectURI  string `json:"redirect_uri"`
}

// Token stores the persisted OAuth2 access + refresh token (chmod 0600).
type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	TokenType    string    `json:"token_type"`
	Expiry       time.Time `json:"expiry"`
	Scope        string    `json:"scope"`
}

// TaskList is a Google Tasks list.
type TaskList struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Updated string `json:"updated,omitempty"`
}

// TaskItem is a single task returned by Google Tasks API v1.
type TaskItem struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Notes     string `json:"notes,omitempty"`
	Status    string `json:"status"` // "needsAction" or "completed"
	Due       string `json:"due,omitempty"`
	Parent    string `json:"parent,omitempty"`
	Position  string `json:"position,omitempty"`
	Completed string `json:"completed,omitempty"`
	Updated   string `json:"updated,omitempty"`
}

// BackupSnapshot captures every list and task (open + completed) before bulk triage.
type BackupSnapshot struct {
	CreatedAt time.Time        `json:"created_at"`
	Lists     []BackupListData `json:"lists"`
}

// BackupListData pairs a TaskList with all of its tasks.
type BackupListData struct {
	List  TaskList   `json:"list"`
	Tasks []TaskItem `json:"tasks"`
}

// LoadCredentials loads Google Desktop OAuth JSON (accepting standard {"installed":{...}} or flat JSON).
func LoadCredentials(path string) (Credentials, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Credentials{}, fmt.Errorf("read google tasks credentials %s: %w", path, err)
	}
	var wrapped struct {
		Installed *Credentials `json:"installed"`
		Web       *Credentials `json:"web"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil {
		if wrapped.Installed != nil && wrapped.Installed.ClientID != "" {
			return normalizeCreds(*wrapped.Installed), nil
		}
		if wrapped.Web != nil && wrapped.Web.ClientID != "" {
			return normalizeCreds(*wrapped.Web), nil
		}
	}
	var flat Credentials
	if err := json.Unmarshal(raw, &flat); err != nil {
		return Credentials{}, fmt.Errorf("parse credentials: %w", err)
	}
	if flat.ClientID == "" {
		return Credentials{}, errors.New("credentials JSON missing client_id")
	}
	return normalizeCreds(flat), nil
}

func normalizeCreds(c Credentials) Credentials {
	if c.AuthURI == "" {
		c.AuthURI = "https://accounts.google.com/o/oauth2/v2/auth"
	}
	if c.TokenURI == "" {
		c.TokenURI = "https://oauth2.googleapis.com/token"
	}
	if c.RedirectURI == "" {
		c.RedirectURI = "http://127.0.0.1:8789"
	}
	return c
}

// AuthURL builds the OAuth consent URL for `pregravity gtasks auth`.
func AuthURL(c Credentials, redirectURI, state, scope string) string {
	if redirectURI == "" {
		redirectURI = c.RedirectURI
	}
	if scope == "" {
		scope = ReadWriteScope
	}
	v := url.Values{
		"client_id":     {c.ClientID},
		"redirect_uri":  {redirectURI},
		"response_type": {"code"},
		"scope":         {scope},
		"access_type":   {"offline"},
		"prompt":        {"consent"},
		"state":         {state},
	}
	return c.AuthURI + "?" + v.Encode()
}

// RandomState generates a CSRF state token for OAuth.
func RandomState() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ExchangeCode exchanges an OAuth authorization code for a Token.
func ExchangeCode(ctx context.Context, c Credentials, code, redirectURI string, hc *http.Client) (Token, error) {
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	form := url.Values{
		"code":          {code},
		"client_id":     {c.ClientID},
		"client_secret": {c.ClientSecret},
		"redirect_uri":  {redirectURI},
		"grant_type":    {"authorization_code"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.TokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := hc.Do(req)
	if err != nil {
		return Token{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return Token{}, fmt.Errorf("oauth token exchange HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var parsed struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return Token{}, err
	}
	return Token{
		AccessToken:  parsed.AccessToken,
		RefreshToken: parsed.RefreshToken,
		TokenType:    parsed.TokenType,
		Expiry:       time.Now().UTC().Add(time.Duration(parsed.ExpiresIn) * time.Second),
		Scope:        parsed.Scope,
	}, nil
}

// SaveToken writes tok atomically with 0600 permissions.
func SaveToken(path string, tok Token) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(tok, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadToken reads a saved OAuth Token from path.
func LoadToken(path string) (Token, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Token{}, err
	}
	var tok Token
	if err := json.Unmarshal(raw, &tok); err != nil {
		return Token{}, err
	}
	return tok, nil
}

// Client is a non-destructive Google Tasks v1 REST client (supports only read + reversible writes;
// DELETE and clear are intentionally unimplemented).
type Client struct {
	credsPath  string
	tokenPath  string
	apiBase    string
	httpClient *http.Client
}

// NewClient constructs a Google Tasks client.
func NewClient(credsPath, tokenPath, apiBase string) *Client {
	if apiBase == "" {
		apiBase = "https://tasks.googleapis.com/tasks/v1"
	}
	return &Client{
		credsPath:  credsPath,
		tokenPath:  tokenPath,
		apiBase:    strings.TrimRight(apiBase, "/"),
		httpClient: &http.Client{Timeout: 20 * time.Second},
	}
}

func (c *Client) validAccessToken(ctx context.Context) (string, error) {
	if envTok := strings.TrimSpace(os.Getenv("PREGRAVITY_GTASKS_ACCESS_TOKEN")); envTok != "" {
		return envTok, nil
	}
	tok, err := LoadToken(c.tokenPath)
	if err != nil {
		return "", fmt.Errorf("google tasks not authenticated yet (run `pregravity gtasks auth`): %w", err)
	}
	if tok.AccessToken != "" && time.Now().UTC().Before(tok.Expiry.Add(-2*time.Minute)) {
		return tok.AccessToken, nil
	}
	if tok.RefreshToken == "" {
		return "", errors.New("google tasks token expired and has no refresh_token; re-run `pregravity gtasks auth`")
	}
	creds, err := LoadCredentials(c.credsPath)
	if err != nil {
		return "", err
	}
	form := url.Values{
		"client_id":     {creds.ClientID},
		"client_secret": {creds.ClientSecret},
		"refresh_token": {tok.RefreshToken},
		"grant_type":    {"refresh_token"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, creds.TokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("refresh google tasks token HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var refreshed struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &refreshed); err != nil {
		return "", err
	}
	tok.AccessToken = refreshed.AccessToken
	tok.Expiry = time.Now().UTC().Add(time.Duration(refreshed.ExpiresIn) * time.Second)
	_ = SaveToken(c.tokenPath, tok)
	return tok.AccessToken, nil
}

func (c *Client) doJSON(ctx context.Context, method, endpoint string, body any, out any) error {
	// Hard guardrail: never allow HTTP DELETE in this client.
	if method == http.MethodDelete {
		return errors.New("DELETE operations are intentionally disabled in pregravity gtasks")
	}
	tok, err := c.validAccessToken(ctx)
	if err != nil {
		return err
	}
	var reader io.Reader
	if body != nil {
		rawBody, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(rawBody)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("google tasks API HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// ListTaskLists returns all of the user's Google Task lists (paginated).
func (c *Client) ListTaskLists(ctx context.Context) ([]TaskList, error) {
	var all []TaskList
	pageToken := ""
	for {
		q := url.Values{"maxResults": {"100"}}
		if pageToken != "" {
			q.Set("pageToken", pageToken)
		}
		var parsed struct {
			Items         []TaskList `json:"items"`
			NextPageToken string     `json:"nextPageToken"`
		}
		if err := c.doJSON(ctx, http.MethodGet, c.apiBase+"/users/@me/lists?"+q.Encode(), nil, &parsed); err != nil {
			return nil, err
		}
		all = append(all, parsed.Items...)
		if parsed.NextPageToken == "" {
			break
		}
		pageToken = parsed.NextPageToken
	}
	return all, nil
}

// ListTasks returns all tasks inside tasklistID (defaults to "@default", paginated).
func (c *Client) ListTasks(ctx context.Context, tasklistID string, showCompleted bool) ([]TaskItem, error) {
	if strings.TrimSpace(tasklistID) == "" {
		tasklistID = "@default"
	}
	var all []TaskItem
	pageToken := ""
	for {
		q := url.Values{
			"maxResults":    {"100"},
			"showCompleted": {fmt.Sprintf("%t", showCompleted)},
			"showHidden":    {fmt.Sprintf("%t", showCompleted)},
		}
		if pageToken != "" {
			q.Set("pageToken", pageToken)
		}
		endpoint := fmt.Sprintf("%s/lists/%s/tasks?%s", c.apiBase, url.PathEscape(tasklistID), q.Encode())
		var parsed struct {
			Items         []TaskItem `json:"items"`
			NextPageToken string     `json:"nextPageToken"`
		}
		if err := c.doJSON(ctx, http.MethodGet, endpoint, nil, &parsed); err != nil {
			return nil, err
		}
		all = append(all, parsed.Items...)
		if parsed.NextPageToken == "" {
			break
		}
		pageToken = parsed.NextPageToken
	}
	return all, nil
}

// CreateTaskList creates a new Google Tasks list (e.g. "Someday" or "Archive").
func (c *Client) CreateTaskList(ctx context.Context, title string) (TaskList, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return TaskList{}, errors.New("task list title cannot be empty")
	}
	var created TaskList
	err := c.doJSON(ctx, http.MethodPost, c.apiBase+"/users/@me/lists", map[string]string{"title": title}, &created)
	return created, err
}

// CreateTask creates a new task in tasklistID (defaults to "@default").
func (c *Client) CreateTask(ctx context.Context, tasklistID, title, notes, due string) (TaskItem, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return TaskItem{}, errors.New("task title cannot be empty")
	}
	if strings.TrimSpace(tasklistID) == "" {
		tasklistID = "@default"
	}
	payload := map[string]any{"title": title}
	if n := strings.TrimSpace(notes); n != "" {
		payload["notes"] = n
	}
	if d := normalizeGoogleDue(due); d != "" {
		payload["due"] = d
	}
	endpoint := fmt.Sprintf("%s/lists/%s/tasks", c.apiBase, url.PathEscape(tasklistID))
	var created TaskItem
	err := c.doJSON(ctx, http.MethodPost, endpoint, payload, &created)
	return created, err
}

// CompleteTask marks an existing task as completed (reversible in Google Tasks).
func (c *Client) CompleteTask(ctx context.Context, tasklistID, taskID string) (TaskItem, error) {
	if strings.TrimSpace(tasklistID) == "" {
		tasklistID = "@default"
	}
	if strings.TrimSpace(taskID) == "" {
		return TaskItem{}, errors.New("task_id is required")
	}
	endpoint := fmt.Sprintf("%s/lists/%s/tasks/%s", c.apiBase, url.PathEscape(tasklistID), url.PathEscape(taskID))
	var updated TaskItem
	err := c.doJSON(ctx, http.MethodPatch, endpoint, map[string]any{"status": "completed"}, &updated)
	return updated, err
}

// ReopenTask restores a completed task back to needsAction.
func (c *Client) ReopenTask(ctx context.Context, tasklistID, taskID string) (TaskItem, error) {
	if strings.TrimSpace(tasklistID) == "" {
		tasklistID = "@default"
	}
	if strings.TrimSpace(taskID) == "" {
		return TaskItem{}, errors.New("task_id is required")
	}
	endpoint := fmt.Sprintf("%s/lists/%s/tasks/%s", c.apiBase, url.PathEscape(tasklistID), url.PathEscape(taskID))
	var updated TaskItem
	err := c.doJSON(ctx, http.MethodPatch, endpoint, map[string]any{
		"status":    "needsAction",
		"completed": nil,
	}, &updated)
	return updated, err
}

// MoveTask atomically moves taskID from fromTasklistID to destTasklistID using
// Google Tasks v1 `tasks.move` with `destinationTasklist` (non-destructive, zero DELETE).
func (c *Client) MoveTask(ctx context.Context, fromTasklistID, taskID, destTasklistID string) (TaskItem, error) {
	if strings.TrimSpace(fromTasklistID) == "" {
		fromTasklistID = "@default"
	}
	if strings.TrimSpace(taskID) == "" || strings.TrimSpace(destTasklistID) == "" {
		return TaskItem{}, errors.New("task_id and destination_tasklist_id are required")
	}
	q := url.Values{"destinationTasklist": {destTasklistID}}
	endpoint := fmt.Sprintf("%s/lists/%s/tasks/%s/move?%s",
		c.apiBase, url.PathEscape(fromTasklistID), url.PathEscape(taskID), q.Encode())
	var moved TaskItem
	err := c.doJSON(ctx, http.MethodPost, endpoint, nil, &moved)
	return moved, err
}

// BackupAll downloads every task list and all tasks (open + completed) into a local 0600 JSON snapshot.
func (c *Client) BackupAll(ctx context.Context, backupDir string) (string, BackupSnapshot, error) {
	lists, err := c.ListTaskLists(ctx)
	if err != nil {
		return "", BackupSnapshot{}, err
	}
	snap := BackupSnapshot{
		CreatedAt: time.Now().UTC(),
		Lists:     make([]BackupListData, 0, len(lists)),
	}
	for _, l := range lists {
		tasks, err := c.ListTasks(ctx, l.ID, true)
		if err != nil {
			return "", BackupSnapshot{}, fmt.Errorf("backup list %q (%s): %w", l.Title, l.ID, err)
		}
		snap.Lists = append(snap.Lists, BackupListData{List: l, Tasks: tasks})
	}
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return "", snap, err
	}
	path := filepath.Join(backupDir, fmt.Sprintf("gtasks-%s.json", snap.CreatedAt.Format("20060102-150405")))
	raw, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return "", snap, err
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		return "", snap, err
	}
	return path, snap, nil
}

func normalizeGoogleDue(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if _, err := time.Parse(time.RFC3339, s); err == nil {
		return s
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC().Format(time.RFC3339)
	}
	if t, err := time.Parse("2006-01-02 15:04", s); err == nil {
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	}
	return ""
}

// ServeStdioMCP runs the zero-dependency Google Tasks MCP stdio server.
// When allowSafeWrites is false, only read-only tools are exposed.
// When allowSafeWrites is true, reversible non-destructive tools (create, complete, reopen, move) are also exposed.
// DELETE and clear are NEVER implemented or exposed.
func ServeStdioMCP(ctx context.Context, client *Client, allowSafeWrites bool, in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	enc := json.NewEncoder(out)

	strArg := func(args map[string]any, k string) string {
		if args == nil {
			return ""
		}
		if v, ok := args[k]; ok && v != nil {
			return strings.TrimSpace(fmt.Sprint(v))
		}
		return ""
	}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id,omitempty"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params,omitempty"`
		}
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}
		if len(req.ID) == 0 || string(req.ID) == "null" {
			continue
		}

		writeResult := func(res any) {
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": res})
		}
		writeErr := func(code int, msg string) {
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": code, "message": msg}})
		}
		writeJSONContent := func(v any, err error) {
			if err != nil {
				writeResult(map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": err.Error()}}})
				return
			}
			payload, _ := json.Marshal(v)
			writeResult(map[string]any{"content": []any{map[string]any{"type": "text", "text": string(payload)}}})
		}

		switch req.Method {
		case "initialize":
			name := "pregravity-gtasks-readonly"
			if allowSafeWrites {
				name = "pregravity-gtasks-safewrite"
			}
			writeResult(map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": name, "version": "1.1"},
			})

		case "tools/list":
			tools := []map[string]any{
				{
					"name":        "list_tasklists",
					"description": "List the user's Google Tasks lists (read-only).",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
					"annotations": map[string]any{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true},
				},
				{
					"name":        "list_tasks",
					"description": "List tasks in a Google Tasks list (read-only). Defaults to '@default'.",
					"inputSchema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"tasklist_id":    map[string]any{"type": "string", "description": "Google Tasks list ID, or '@default'."},
							"show_completed": map[string]any{"type": "boolean", "description": "Whether to include completed tasks (default false)."},
						},
					},
					"annotations": map[string]any{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true},
				},
			}
			if allowSafeWrites {
				tools = append(tools,
					map[string]any{
						"name":        "create_task",
						"description": "Create a new task in a Google Tasks list (non-destructive).",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"tasklist_id": map[string]any{"type": "string", "description": "Target list ID (defaults to '@default')."},
								"title":       map[string]any{"type": "string", "description": "Task title."},
								"notes":       map[string]any{"type": "string", "description": "Optional notes."},
								"due":         map[string]any{"type": "string", "description": "Optional due date (YYYY-MM-DD or RFC3339)."},
							},
							"required": []string{"title"},
						},
						"annotations": map[string]any{"readOnlyHint": false, "destructiveHint": false},
					},
					map[string]any{
						"name":        "complete_task",
						"description": "Mark an existing Google Task as completed (reversible).",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"tasklist_id": map[string]any{"type": "string", "description": "List ID containing the task (defaults to '@default')."},
								"task_id":     map[string]any{"type": "string", "description": "Google Task ID to complete."},
							},
							"required": []string{"task_id"},
						},
						"annotations": map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": true},
					},
					map[string]any{
						"name":        "reopen_task",
						"description": "Reopen a completed Google Task back to needsAction.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"tasklist_id": map[string]any{"type": "string", "description": "List ID containing the task (defaults to '@default')."},
								"task_id":     map[string]any{"type": "string", "description": "Google Task ID to reopen."},
							},
							"required": []string{"task_id"},
						},
						"annotations": map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": true},
					},
					map[string]any{
						"name":        "move_task",
						"description": "Move a Google Task from one list to another (non-destructive, atomic).",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"from_tasklist_id": map[string]any{"type": "string", "description": "Current list ID (defaults to '@default')."},
								"task_id":          map[string]any{"type": "string", "description": "Google Task ID to move."},
								"to_tasklist_id":   map[string]any{"type": "string", "description": "Destination list ID."},
							},
							"required": []string{"task_id", "to_tasklist_id"},
						},
						"annotations": map[string]any{"readOnlyHint": false, "destructiveHint": false},
					},
				)
			}
			writeResult(map[string]any{"tools": tools})

		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &p); err != nil {
				writeErr(-32602, "invalid params")
				continue
			}
			switch p.Name {
			case "list_tasklists":
				lists, err := client.ListTaskLists(ctx)
				writeJSONContent(map[string]any{"tasklists": lists, "count": len(lists)}, err)

			case "list_tasks":
				listID := strArg(p.Arguments, "tasklist_id")
				if listID == "" {
					listID = "@default"
				}
				showDone, _ := p.Arguments["show_completed"].(bool)
				tasks, err := client.ListTasks(ctx, listID, showDone)
				writeJSONContent(map[string]any{"tasklist_id": listID, "tasks": tasks, "count": len(tasks)}, err)

			case "create_task":
				if !allowSafeWrites {
					writeErr(-32601, "create_task is disabled in read-only mode")
					continue
				}
				t, err := client.CreateTask(ctx, strArg(p.Arguments, "tasklist_id"), strArg(p.Arguments, "title"), strArg(p.Arguments, "notes"), strArg(p.Arguments, "due"))
				writeJSONContent(map[string]any{"task": t}, err)

			case "complete_task":
				if !allowSafeWrites {
					writeErr(-32601, "complete_task is disabled in read-only mode")
					continue
				}
				t, err := client.CompleteTask(ctx, strArg(p.Arguments, "tasklist_id"), strArg(p.Arguments, "task_id"))
				writeJSONContent(map[string]any{"task": t}, err)

			case "reopen_task":
				if !allowSafeWrites {
					writeErr(-32601, "reopen_task is disabled in read-only mode")
					continue
				}
				t, err := client.ReopenTask(ctx, strArg(p.Arguments, "tasklist_id"), strArg(p.Arguments, "task_id"))
				writeJSONContent(map[string]any{"task": t}, err)

			case "move_task":
				if !allowSafeWrites {
					writeErr(-32601, "move_task is disabled in read-only mode")
					continue
				}
				t, err := client.MoveTask(ctx, strArg(p.Arguments, "from_tasklist_id"), strArg(p.Arguments, "task_id"), strArg(p.Arguments, "to_tasklist_id"))
				writeJSONContent(map[string]any{"task": t}, err)

			default:
				writeErr(-32601, fmt.Sprintf("unknown or disallowed tool %q", p.Name))
			}

		default:
			writeErr(-32601, fmt.Sprintf("unsupported method %q", req.Method))
		}
	}
	return scanner.Err()
}
