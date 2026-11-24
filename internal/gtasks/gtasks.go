package gtasks

import (
	"bufio"
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

// ReadonlyScope is the ONLY OAuth scope Pregravity ever requests for Google Tasks.
// The Google API server itself rejects any write/delete calls made with this token.
const ReadonlyScope = "https://www.googleapis.com/auth/tasks.readonly"

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
	Completed string `json:"completed,omitempty"`
	Updated   string `json:"updated,omitempty"`
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
		c.RedirectURI = "http://127.0.0.1:8789/callback"
	}
	return c
}

// AuthURL builds the read-only OAuth consent URL for `pregravity gtasks auth`.
func AuthURL(c Credentials, redirectURI, state string) string {
	if redirectURI == "" {
		redirectURI = c.RedirectURI
	}
	v := url.Values{
		"client_id":     {c.ClientID},
		"redirect_uri":  {redirectURI},
		"response_type": {"code"},
		"scope":         {ReadonlyScope},
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

// Client is a strictly read-only Google Tasks v1 REST client.
type Client struct {
	credsPath  string
	tokenPath  string
	apiBase    string
	httpClient *http.Client
}

// NewClient constructs a read-only Google Tasks client.
func NewClient(credsPath, tokenPath, apiBase string) *Client {
	if apiBase == "" {
		apiBase = "https://tasks.googleapis.com/tasks/v1"
	}
	return &Client{
		credsPath:  credsPath,
		tokenPath:  tokenPath,
		apiBase:    strings.TrimRight(apiBase, "/"),
		httpClient: &http.Client{Timeout: 15 * time.Second},
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

func (c *Client) getJSON(ctx context.Context, endpoint string, out any) error {
	tok, err := c.validAccessToken(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("google tasks API HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return json.Unmarshal(raw, out)
}

// ListTaskLists returns the user's Google Task lists (read-only).
func (c *Client) ListTaskLists(ctx context.Context) ([]TaskList, error) {
	var parsed struct {
		Items []TaskList `json:"items"`
	}
	if err := c.getJSON(ctx, c.apiBase+"/users/@me/lists?maxResults=50", &parsed); err != nil {
		return nil, err
	}
	return parsed.Items, nil
}

// ListTasks returns tasks inside tasklistID (defaults to "@default", read-only).
func (c *Client) ListTasks(ctx context.Context, tasklistID string, showCompleted bool) ([]TaskItem, error) {
	if strings.TrimSpace(tasklistID) == "" {
		tasklistID = "@default"
	}
	q := url.Values{
		"maxResults":    {"100"},
		"showCompleted": {fmt.Sprintf("%t", showCompleted)},
		"showHidden":    {"false"},
	}
	endpoint := fmt.Sprintf("%s/lists/%s/tasks?%s", c.apiBase, url.PathEscape(tasklistID), q.Encode())
	var parsed struct {
		Items []TaskItem `json:"items"`
	}
	if err := c.getJSON(ctx, endpoint, &parsed); err != nil {
		return nil, err
	}
	return parsed.Items, nil
}

// ServeStdioMCP runs a zero-dependency, strictly read-only MCP stdio server
// exposing ONLY `list_tasklists` and `list_tasks` with `readOnlyHint: true`.
func ServeStdioMCP(ctx context.Context, client *Client, in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	enc := json.NewEncoder(out)

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
		// Notifications have no ID; do not send a response.
		if len(req.ID) == 0 || string(req.ID) == "null" {
			continue
		}

		writeResult := func(res any) {
			_ = enc.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result":  res,
			})
		}
		writeErr := func(code int, msg string) {
			_ = enc.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"error":   map[string]any{"code": code, "message": msg},
			})
		}

		switch req.Method {
		case "initialize":
			writeResult(map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "pregravity-gtasks-readonly", "version": "1.0"},
			})

		case "tools/list":
			writeResult(map[string]any{
				"tools": []map[string]any{
					{
						"name":        "list_tasklists",
						"description": "List the user's Google Tasks lists (strictly read-only).",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
						"annotations": map[string]any{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true},
					},
					{
						"name":        "list_tasks",
						"description": "List tasks in a Google Tasks list (strictly read-only). Defaults to '@default'.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"tasklist_id":    map[string]any{"type": "string", "description": "Google Tasks list ID, or '@default'."},
								"show_completed": map[string]any{"type": "boolean", "description": "Whether to include completed tasks (default false)."},
							},
						},
						"annotations": map[string]any{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true},
					},
				},
			})

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
				if err != nil {
					writeResult(map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": err.Error()}}})
					continue
				}
				payload, _ := json.Marshal(map[string]any{"tasklists": lists, "count": len(lists)})
				writeResult(map[string]any{"content": []any{map[string]any{"type": "text", "text": string(payload)}}})

			case "list_tasks":
				listID := "@default"
				if v, ok := p.Arguments["tasklist_id"].(string); ok && strings.TrimSpace(v) != "" {
					listID = v
				}
				showDone, _ := p.Arguments["show_completed"].(bool)
				tasks, err := client.ListTasks(ctx, listID, showDone)
				if err != nil {
					writeResult(map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": err.Error()}}})
					continue
				}
				payload, _ := json.Marshal(map[string]any{"tasklist_id": listID, "tasks": tasks, "count": len(tasks)})
				writeResult(map[string]any{"content": []any{map[string]any{"type": "text", "text": string(payload)}}})

			default:
				writeErr(-32601, fmt.Sprintf("unknown or disallowed tool %q", p.Name))
			}

		default:
			writeErr(-32601, fmt.Sprintf("unsupported method %q", req.Method))
		}
	}
	return scanner.Err()
}
