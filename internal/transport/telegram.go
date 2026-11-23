package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"pregravity/internal/assistant"
	"pregravity/internal/config"
	"pregravity/internal/store"
)

// TelegramBot runs a zero-inbound-port long-polling Telegram bot restricted
// strictly to an allowlist of chat IDs.
type TelegramBot struct {
	token      string
	apiBase    string
	allowed    map[int64]bool
	allowedIDs []int64
	pollSec    int
	cfg        *config.Config
	st         *store.Store
	asst       *assistant.Assistant
	httpClient *http.Client
	logger     *slog.Logger
}

// NewTelegramBot constructs a fail-closed TelegramBot.
func NewTelegramBot(cfg *config.Config, st *store.Store, asst *assistant.Assistant, logger *slog.Logger) (*TelegramBot, error) {
	if strings.TrimSpace(cfg.TelegramBotToken) == "" {
		return nil, errors.New("PREGRAVITY_TELEGRAM_BOT_TOKEN is not set")
	}
	if len(cfg.Telegram.AllowedChatIDs) == 0 {
		return nil, errors.New("telegram.allowed_chat_ids must contain at least one chat ID")
	}
	if logger == nil {
		logger = slog.Default()
	}
	allowed := make(map[int64]bool, len(cfg.Telegram.AllowedChatIDs))
	for _, id := range cfg.Telegram.AllowedChatIDs {
		allowed[id] = true
	}
	pollSec := cfg.Telegram.PollTimeoutSec
	if pollSec <= 0 {
		pollSec = 30
	}
	return &TelegramBot{
		token:      cfg.TelegramBotToken,
		apiBase:    "https://api.telegram.org",
		allowed:    allowed,
		allowedIDs: append([]int64(nil), cfg.Telegram.AllowedChatIDs...),
		pollSec:    pollSec,
		cfg:        cfg,
		st:         st,
		asst:       asst,
		httpClient: &http.Client{Timeout: time.Duration(pollSec+15) * time.Second},
		logger:     logger,
	}, nil
}

type tgUpdatesResp struct {
	OK     bool       `json:"ok"`
	Result []tgUpdate `json:"result"`
}

type tgUpdate struct {
	UpdateID int64      `json:"update_id"`
	Message  *tgMessage `json:"message"`
}

type tgMessage struct {
	MessageID int64  `json:"message_id"`
	Chat      tgChat `json:"chat"`
	Text      string `json:"text"`
}

type tgChat struct {
	ID int64 `json:"id"`
}

// Run polls Telegram until ctx is canceled.
func (b *TelegramBot) Run(ctx context.Context) {
	var offset int64
	b.logger.Info("telegram long-poller started", "allowed_chats", len(b.allowedIDs))

	for {
		if ctx.Err() != nil {
			return
		}
		updates, err := b.getUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			b.logger.Warn("telegram getUpdates failed", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
			continue
		}
		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			if u.Message == nil || strings.TrimSpace(u.Message.Text) == "" {
				continue
			}
			chatID := u.Message.Chat.ID
			if !b.allowed[chatID] {
				b.logger.Warn("dropped message from unauthorized telegram chat", "chat_id", chatID)
				continue
			}
			go b.handleMessage(ctx, chatID, u.Message.Text)
		}
	}
}

func (b *TelegramBot) handleMessage(ctx context.Context, chatID int64, text string) {
	text = strings.TrimSpace(text)

	// Fast local slash commands that don't burn API tokens.
	switch {
	case text == "/help" || text == "/start":
		_ = b.SendMessage(ctx, chatID, strings.Join([]string{
			"Pregravity — Personal Accountability & Antigravity Handoff Assistant",
			"",
			"Commands:",
			"  /tasks — list open tasks (no API tokens)",
			"  /handoffs — list open Antigravity handoffs",
			"  /handoff <title> | <goal> — immediately stage a handoff for agy",
			"  Or just chat normally to track commitments, journal notes, or escalate work.",
		}, "\n"))
		return

	case text == "/tasks":
		tasks, err := b.st.ListTasks("open")
		if err != nil {
			_ = b.SendMessage(ctx, chatID, "Error reading tasks: "+err.Error())
			return
		}
		if len(tasks) == 0 {
			_ = b.SendMessage(ctx, chatID, "No open tasks.")
			return
		}
		var sb strings.Builder
		sb.WriteString("Open tasks:\n")
		for _, t := range tasks {
			due := ""
			if t.Due != "" {
				due = " (due " + t.Due + ")"
			}
			fmt.Fprintf(&sb, "• %s [%s] %s%s\n", t.ID, t.Priority, t.Title, due)
		}
		_ = b.SendMessage(ctx, chatID, sb.String())
		return

	case text == "/handoffs":
		hs, err := b.st.ListHandoffs("open")
		if err != nil {
			_ = b.SendMessage(ctx, chatID, "Error reading handoffs: "+err.Error())
			return
		}
		if len(hs) == 0 {
			_ = b.SendMessage(ctx, chatID, "No open Antigravity handoffs.")
			return
		}
		var sb strings.Builder
		sb.WriteString("Open Antigravity handoffs:\n")
		for _, h := range hs {
			fmt.Fprintf(&sb, "• %s — %s\n  Run: %s %s\n", h.ID, h.Title, b.cfg.Antigravity.PickupSkill, h.ID)
		}
		fmt.Fprintf(&sb, "\nOpen: %s (%s)", b.cfg.Antigravity.DashboardURL, b.cfg.Antigravity.InstanceName)
		_ = b.SendMessage(ctx, chatID, sb.String())
		return

	case strings.HasPrefix(text, "/handoff "):
		payload := strings.TrimSpace(strings.TrimPrefix(text, "/handoff "))
		title, goal, ok := strings.Cut(payload, "|")
		if !ok {
			title = payload
			goal = payload
		}
		h, err := b.st.CreateHandoff(strings.TrimSpace(title), strings.TrimSpace(goal), "Created manually via Telegram /handoff.", "", nil)
		if err != nil {
			_ = b.SendMessage(ctx, chatID, "Could not create handoff: "+err.Error())
			return
		}
		msg := fmt.Sprintf("Handoff staged: %s\nOpen %s (%s) and run:\n%s %s",
			h.Title, b.cfg.Antigravity.DashboardURL, b.cfg.Antigravity.InstanceName, b.cfg.Antigravity.PickupSkill, h.ID)
		_ = b.SendMessage(ctx, chatID, msg)
		return
	}

	sessionID := fmt.Sprintf("tg-%d", chatID)
	reply, err := b.asst.HandleMessage(ctx, sessionID, text)
	if err != nil {
		b.logger.Error("assistant error on telegram message", "chat_id", chatID, "err", err)
		_ = b.SendMessage(ctx, chatID, "Error: "+err.Error())
		return
	}
	_ = b.SendMessage(ctx, chatID, reply.Text)
}

// Notify implements Notifier by broadcasting scheduled briefings to all allowed chats.
func (b *TelegramBot) Notify(ctx context.Context, title, body string) error {
	msg := body
	if title != "" {
		msg = fmt.Sprintf("[%s]\n\n%s", title, body)
	}
	var firstErr error
	for _, id := range b.allowedIDs {
		if err := b.SendMessage(ctx, id, msg); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// SendMessage sends text to chatID, automatically splitting long messages at 3900 chars.
func (b *TelegramBot) SendMessage(ctx context.Context, chatID int64, text string) error {
	chunks := splitChunks(text, 3900)
	for _, chunk := range chunks {
		payload := map[string]any{
			"chat_id":                  chatID,
			"text":                     chunk,
			"disable_web_page_preview": true,
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		endpoint := fmt.Sprintf("%s/bot%s/sendMessage", b.apiBase, b.token)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := b.httpClient.Do(req)
		if err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			return fmt.Errorf("telegram sendMessage returned HTTP %d", resp.StatusCode)
		}
	}
	return nil
}

func (b *TelegramBot) getUpdates(ctx context.Context, offset int64) ([]tgUpdate, error) {
	endpoint := fmt.Sprintf("%s/bot%s/getUpdates?timeout=%d&offset=%d", b.apiBase, b.token, b.pollSec, offset)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("telegram getUpdates returned HTTP %d", resp.StatusCode)
	}
	var parsed tgUpdatesResp
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&parsed); err != nil {
		return nil, err
	}
	if !parsed.OK {
		return nil, errors.New("telegram getUpdates returned ok=false")
	}
	return parsed.Result, nil
}

func splitChunks(s string, maxLen int) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if len(s) <= maxLen {
		return []string{s}
	}
	var out []string
	for len(s) > maxLen {
		cut := strings.LastIndex(s[:maxLen], "\n")
		if cut < maxLen/2 {
			cut = maxLen
		}
		out = append(out, strings.TrimSpace(s[:cut]))
		s = strings.TrimSpace(s[cut:])
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}
