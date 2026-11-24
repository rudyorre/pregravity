package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ServerConfig configures a single external or built-in MCP server.
// Pregravity enforces strict read-only filtering on all MCP servers.
type ServerConfig struct {
	Name          string            `json:"name"`
	Enabled       bool              `json:"enabled"`
	Transport     string            `json:"transport"` // "stdio" or "http"
	Command       string            `json:"command,omitempty"`
	Args          []string          `json:"args,omitempty"`
	URL           string            `json:"url,omitempty"`
	BearerTokenEv string            `json:"bearer_token_env,omitempty"`
	ReadOnly      *bool             `json:"read_only,omitempty"` // defaults to true (cannot be disabled in hardened mode)
	AllowedTools  []string          `json:"allowed_tools,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
}

// ToolAnnotations follows the MCP tool annotations specification.
type ToolAnnotations struct {
	ReadOnlyHint    bool `json:"readOnlyHint,omitempty"`
	DestructiveHint bool `json:"destructiveHint,omitempty"`
	IdempotentHint  bool `json:"idempotentHint,omitempty"`
}

// Tool describes an MCP tool returned by tools/list.
type Tool struct {
	Server      string           `json:"-"`
	ExposedName string           `json:"-"` // mcp__<server>__<name>
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	InputSchema map[string]any   `json:"inputSchema,omitempty"`
	Annotations *ToolAnnotations `json:"annotations,omitempty"`
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("mcp rpc error (%d): %s", e.Code, e.Message)
}

// IsToolAllowedReadOnly enforces Pregravity's strict read-only firewall on MCP tools.
// A tool is rejected if it has any mutating/destructive keyword or destructiveHint,
// and must either have readOnlyHint=true, appear in allowedList, or use a read-only verb prefix.
func IsToolAllowedReadOnly(tool Tool, allowedList []string) bool {
	name := strings.ToLower(strings.TrimSpace(tool.Name))
	if name == "" {
		return false
	}

	// Unconditionally reject mutating/execution verbs regardless of hints.
	forbiddenSubstrings := []string{
		"create", "add_", "insert", "update", "edit", "patch", "put_", "post_",
		"delete", "remove", "drop", "destroy", "clear", "purge", "write",
		"move", "complete", "mark_", "set_", "exec", "shell", "bash", "cmd", "run_",
	}
	for _, bad := range forbiddenSubstrings {
		if strings.Contains(name, bad) {
			return false
		}
	}
	if tool.Annotations != nil && tool.Annotations.DestructiveHint {
		return false
	}

	// If explicit allowlist is provided, require membership.
	if len(allowedList) > 0 {
		for _, a := range allowedList {
			if strings.EqualFold(strings.TrimSpace(a), tool.Name) {
				return true
			}
		}
		return false
	}

	// Otherwise require explicit readOnlyHint or safe read-only verb prefix.
	if tool.Annotations != nil && tool.Annotations.ReadOnlyHint {
		return true
	}
	safePrefixes := []string{"list_", "get_", "read_", "search_", "query_", "find_", "fetch_", "describe_", "show_", "check_"}
	for _, p := range safePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// Client talks JSON-RPC 2.0 to a single MCP server over stdio or HTTP.
type Client struct {
	cfg        ServerConfig
	httpClient *http.Client
	reqID      atomic.Int64

	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	reader *bufio.Reader
	inited bool
}

// NewClient constructs a Client for cfg.
func NewClient(cfg ServerConfig) (*Client, error) {
	if strings.TrimSpace(cfg.Name) == "" {
		return nil, errors.New("mcp server name is required")
	}
	t := strings.ToLower(strings.TrimSpace(cfg.Transport))
	if t == "" {
		if cfg.URL != "" {
			t = "http"
		} else {
			t = "stdio"
		}
	}
	cfg.Transport = t
	switch t {
	case "http":
		if strings.TrimSpace(cfg.URL) == "" {
			return nil, fmt.Errorf("mcp server %q: url is required for http transport", cfg.Name)
		}
	case "stdio":
		if strings.TrimSpace(cfg.Command) == "" {
			return nil, fmt.Errorf("mcp server %q: command is required for stdio transport", cfg.Name)
		}
	default:
		return nil, fmt.Errorf("mcp server %q: unsupported transport %q", cfg.Name, t)
	}
	return &Client{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 20 * time.Second},
	}, nil
}

// Close shuts down any running stdio subprocess.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stdin != nil {
		_ = c.stdin.Close()
		c.stdin = nil
	}
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
		_ = c.cmd.Wait()
		c.cmd = nil
	}
	c.inited = false
	return nil
}

func (c *Client) ensureInitialized(ctx context.Context) error {
	if c.inited {
		return nil
	}
	if c.cfg.Transport == "stdio" && c.cmd == nil {
		cmd := exec.Command(c.cfg.Command, c.cfg.Args...)
		cmd.Env = os.Environ()
		for k, v := range c.cfg.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return err
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return err
		}
		cmd.Stderr = io.Discard
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("start mcp stdio %q: %w", c.cfg.Name, err)
		}
		c.cmd = cmd
		c.stdin = stdin
		c.reader = bufio.NewReaderSize(stdout, 1<<20)
	}

	_, err := c.callLocked(ctx, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    "pregravity",
			"version": "1.0",
		},
	})
	if err != nil {
		return fmt.Errorf("mcp initialize %q: %w", c.cfg.Name, err)
	}
	// Send initialized notification (best effort).
	_ = c.notifyLocked(ctx, "notifications/initialized", map[string]any{})
	c.inited = true
	return nil
}

func (c *Client) callLocked(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.reqID.Add(1)
	req := rpcRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	if c.cfg.Transport == "http" {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL, bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "application/json")
		if c.cfg.BearerTokenEv != "" {
			if tok := strings.TrimSpace(os.Getenv(c.cfg.BearerTokenEv)); tok != "" {
				httpReq.Header.Set("Authorization", "Bearer "+tok)
			}
		}
		resp, err := c.httpClient.Do(httpReq)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if err != nil {
			return nil, err
		}
		if resp.StatusCode >= 300 {
			return nil, fmt.Errorf("mcp http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		var rpcResp rpcResponse
		if err := json.Unmarshal(body, &rpcResp); err != nil {
			return nil, fmt.Errorf("decode mcp http response: %w", err)
		}
		if rpcResp.Error != nil {
			return nil, rpcResp.Error
		}
		return rpcResp.Result, nil
	}

	// Stdio JSON-RPC (newline-delimited JSON).
	if _, err := c.stdin.Write(append(raw, '\n')); err != nil {
		return nil, err
	}
	for {
		line, err := c.reader.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var rpcResp rpcResponse
		if err := json.Unmarshal(line, &rpcResp); err != nil {
			continue
		}
		// Skip server notifications (id == 0).
		if rpcResp.ID != id {
			continue
		}
		if rpcResp.Error != nil {
			return nil, rpcResp.Error
		}
		return rpcResp.Result, nil
	}
}

func (c *Client) notifyLocked(ctx context.Context, method string, params any) error {
	raw, err := json.Marshal(rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	if c.cfg.Transport == "http" {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL, bytes.NewReader(raw))
		if err != nil {
			return err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		resp, err := c.httpClient.Do(httpReq)
		if err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 16<<10))
		resp.Body.Close()
		return nil
	}
	if c.stdin == nil {
		return nil
	}
	_, err = c.stdin.Write(append(raw, '\n'))
	return err
}

// ListReadOnlyTools queries tools/list and returns only tools that pass the strict read-only firewall.
func (c *Client) ListReadOnlyTools(ctx context.Context) ([]Tool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.ensureInitialized(ctx); err != nil {
		return nil, err
	}
	raw, err := c.callLocked(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Tools []Tool `json:"tools"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}

	var allowed []Tool
	for _, t := range parsed.Tools {
		if !IsToolAllowedReadOnly(t, c.cfg.AllowedTools) {
			continue
		}
		t.Server = c.cfg.Name
		t.ExposedName = "mcp__" + sanitizeName(c.cfg.Name) + "__" + sanitizeName(t.Name)
		allowed = append(allowed, t)
	}
	return allowed, nil
}

// CallTool invokes an already-vetted read-only tool by its native MCP name.
func (c *Client) CallTool(ctx context.Context, toolName string, args map[string]any) (map[string]any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.ensureInitialized(ctx); err != nil {
		return nil, err
	}
	if args == nil {
		args = map[string]any{}
	}
	raw, err := c.callLocked(ctx, "tools/call", map[string]any{
		"name":      toolName,
		"arguments": args,
	})
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		return map[string]any{"raw": string(raw)}, nil
	}
	return result, nil
}

// Manager coordinates multiple configured read-only MCP servers for Assistant.
type Manager struct {
	clients map[string]*Client
	tools   map[string]Tool
	ordered []Tool
	logger  *slog.Logger
}

// NewManager initializes all enabled MCP servers and discovers their strictly read-only tools.
func NewManager(ctx context.Context, configs []ServerConfig, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	m := &Manager{
		clients: map[string]*Client{},
		tools:   map[string]Tool{},
		logger:  logger,
	}
	for _, sc := range configs {
		if !sc.Enabled {
			continue
		}
		cli, err := NewClient(sc)
		if err != nil {
			logger.Warn("skipping invalid mcp server config", "server", sc.Name, "err", err)
			continue
		}
		discoverCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		tools, err := cli.ListReadOnlyTools(discoverCtx)
		cancel()
		if err != nil {
			logger.Warn("failed to discover read-only tools from mcp server", "server", sc.Name, "err", err)
			_ = cli.Close()
			continue
		}
		m.clients[sc.Name] = cli
		for _, t := range tools {
			m.tools[t.ExposedName] = t
			m.ordered = append(m.ordered, t)
		}
		logger.Info("connected read-only mcp server", "server", sc.Name, "tools", len(tools))
	}
	return m
}

// Tools returns all vetted read-only tools across connected MCP servers.
func (m *Manager) Tools() []Tool {
	if m == nil {
		return nil
	}
	return m.ordered
}

// HasTool reports whether exposedName belongs to a connected read-only MCP server.
func (m *Manager) HasTool(exposedName string) bool {
	if m == nil {
		return false
	}
	_, ok := m.tools[exposedName]
	return ok
}

// Call executes an approved read-only MCP tool by its exposed name.
func (m *Manager) Call(ctx context.Context, exposedName string, args map[string]any) (map[string]any, error) {
	if m == nil {
		return nil, fmt.Errorf("mcp tool %q unavailable", exposedName)
	}
	tool, ok := m.tools[exposedName]
	if !ok {
		return nil, fmt.Errorf("unknown or disallowed mcp tool %q", exposedName)
	}
	cli := m.clients[tool.Server]
	if cli == nil {
		return nil, fmt.Errorf("mcp server %q not connected", tool.Server)
	}
	return cli.CallTool(ctx, tool.Name, args)
}

// Close closes all connected MCP server clients.
func (m *Manager) Close() {
	if m == nil {
		return
	}
	for _, c := range m.clients {
		_ = c.Close()
	}
}

func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return strings.Trim(b.String(), "_")
}
