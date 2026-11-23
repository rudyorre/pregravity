package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Notifier delivers scheduled check-in output or alerts to the user.
type Notifier interface {
	Notify(ctx context.Context, title, body string) error
}

// MultiNotifier fans out notifications to multiple targets.
type MultiNotifier struct {
	Targets []Notifier
}

// Notify delivers to every configured target and returns the first error, if any.
func (m *MultiNotifier) Notify(ctx context.Context, title, body string) error {
	var firstErr error
	for _, t := range m.Targets {
		if t == nil {
			continue
		}
		if err := t.Notify(ctx, title, body); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// NtfyNotifier pushes notifications to ntfy.sh or a self-hosted ntfy server.
type NtfyNotifier struct {
	ServerURL string
	Topic     string
	Token     string
	Client    *http.Client
}

// Notify publishes a message to ntfy.
func (n *NtfyNotifier) Notify(ctx context.Context, title, body string) error {
	server := strings.TrimRight(n.ServerURL, "/")
	if server == "" {
		server = "https://ntfy.sh"
	}
	if strings.TrimSpace(n.Topic) == "" {
		return nil
	}
	client := n.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server+"/"+strings.TrimLeft(n.Topic, "/"), strings.NewReader(body))
	if err != nil {
		return err
	}
	if title != "" {
		req.Header.Set("Title", title)
	}
	if n.Token != "" {
		req.Header.Set("Authorization", "Bearer "+n.Token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 16<<10))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("ntfy returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// WebhookNotifier POSTs a JSON payload {"title": ..., "text": ..., "content": ...}
// (compatible with Discord/Slack/custom webhooks).
type WebhookNotifier struct {
	URL    string
	Client *http.Client
}

// Notify sends the JSON payload to the webhook URL.
func (w *WebhookNotifier) Notify(ctx context.Context, title, body string) error {
	if strings.TrimSpace(w.URL) == "" {
		return nil
	}
	client := w.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	content := body
	if title != "" {
		content = fmt.Sprintf("**%s**\n%s", title, body)
	}
	raw, err := json.Marshal(map[string]string{
		"title":   title,
		"text":    content,
		"content": content,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 16<<10))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned HTTP %d", resp.StatusCode)
	}
	return nil
}
