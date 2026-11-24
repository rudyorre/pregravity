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
			go b.handleMessage(ctx, chatID, u.Message.MessageID, u.Message.Text)
		}
	}
}

func (b *TelegramBot) handleMessage(ctx context.Context, chatID, replyToMsgID int64, text string) {
	text = strings.TrimSpace(text)

	// Fast local slash commands that don't burn API tokens.
	switch {
	case text == "/help" || text == "/start":
		_ = b.SendReply(ctx, chatID, replyToMsgID, strings.Join([]string{
			"**Pregravity** — Personal Accountability & Antigravity Handoff Assistant",
			"",
			"Commands:",
			"- `/tasks` — list open tasks (no API tokens)",
			"- `/handoffs` — list open Antigravity handoffs",
			"- `/handoff <title> | <goal>` — immediately stage a handoff for agy",
			"- `/quota` — show Gemini API daily usage & remaining freebie budget (no API tokens)",
			"- Or just chat normally to track commitments, journal notes, or escalate work.",
		}, "\n"))
		return

	case text == "/quota" || text == "/usage":
		summary, err := b.st.GetUsageSummary(time.Now(), b.cfg.Model, b.cfg.Quota.LimitForModel(b.cfg.Model))
		if err != nil {
			_ = b.SendReply(ctx, chatID, replyToMsgID, "Error reading usage: "+err.Error())
			return
		}
		msg := store.FormatQuotaReport(summary) + "\n\n_(For Antigravity subscription quota, run `pregravity quota` on devbox.)_"
		_ = b.SendReply(ctx, chatID, replyToMsgID, msg)
		return

	case text == "/tasks":
		tasks, err := b.st.ListTasks("open")
		if err != nil {
			_ = b.SendReply(ctx, chatID, replyToMsgID, "Error reading tasks: "+err.Error())
			return
		}
		if len(tasks) == 0 {
			_ = b.SendReply(ctx, chatID, replyToMsgID, "No open tasks.")
			return
		}
		var sb strings.Builder
		sb.WriteString("**Open tasks:**\n")
		for _, t := range tasks {
			due := ""
			if t.Due != "" {
				due = " *(due " + t.Due + ")*"
			}
			fmt.Fprintf(&sb, "- `%s` **[%s]** %s%s\n", t.ID, t.Priority, t.Title, due)
		}
		_ = b.SendReply(ctx, chatID, replyToMsgID, sb.String())
		return

	case text == "/handoffs":
		hs, err := b.st.ListHandoffs("open")
		if err != nil {
			_ = b.SendReply(ctx, chatID, replyToMsgID, "Error reading handoffs: "+err.Error())
			return
		}
		if len(hs) == 0 {
			_ = b.SendReply(ctx, chatID, replyToMsgID, "No open Antigravity handoffs.")
			return
		}
		var sb strings.Builder
		sb.WriteString("**Open Antigravity handoffs:**\n")
		for _, h := range hs {
			fmt.Fprintf(&sb, "- `%s` — %s\n  `%s %s`\n", h.ID, h.Title, b.cfg.Antigravity.PickupSkill, h.ID)
		}
		fmt.Fprintf(&sb, "\nOpen: %s (`%s`)", b.cfg.Antigravity.DashboardURL, b.cfg.Antigravity.InstanceName)
		_ = b.SendReply(ctx, chatID, replyToMsgID, sb.String())
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
			_ = b.SendReply(ctx, chatID, replyToMsgID, "Could not create handoff: "+err.Error())
			return
		}
		msg := fmt.Sprintf("**Handoff staged:** %s\nOpen %s (`%s`) and paste the pickup command below:",
			h.Title, b.cfg.Antigravity.DashboardURL, b.cfg.Antigravity.InstanceName)
		_ = b.SendReply(ctx, chatID, replyToMsgID, msg)
		// Mobile accessibility: send the pickup command as a standalone tap-to-copy message.
		_ = b.SendMessage(ctx, chatID, fmt.Sprintf("`%s %s`", b.cfg.Antigravity.PickupSkill, h.ID))
		return
	}

	sessionID := fmt.Sprintf("tg-%d", chatID)
	reply, err := b.asst.HandleMessage(ctx, sessionID, text)
	if err != nil {
		b.logger.Error("assistant error on telegram message", "chat_id", chatID, "err", err)
		_ = b.SendReply(ctx, chatID, replyToMsgID, "Error: "+err.Error())
		return
	}
	_ = b.SendReply(ctx, chatID, replyToMsgID, reply.Text)

	// Mobile accessibility: whenever Grav stages one or more Antigravity handoffs,
	// follow up with a standalone copy-ready `/pickup <id>` message (one-tap copy on Telegram mobile).
	for _, h := range reply.Handoffs {
		_ = b.SendMessage(ctx, chatID, fmt.Sprintf("`%s %s`", b.cfg.Antigravity.PickupSkill, h.ID))
	}
}

// Notify implements Notifier by broadcasting scheduled briefings to all allowed chats.
func (b *TelegramBot) Notify(ctx context.Context, title, body string) error {
	msg := body
	if title != "" {
		msg = fmt.Sprintf("**%s**\n\n%s", title, body)
	}
	var firstErr error
	for _, id := range b.allowedIDs {
		if err := b.SendMessage(ctx, id, msg); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// SendMessage sends Markdown-formatted text as Telegram HTML (with plain-text fallback).
func (b *TelegramBot) SendMessage(ctx context.Context, chatID int64, text string) error {
	return b.SendReply(ctx, chatID, 0, text)
}

// SendReply sends Markdown-formatted text as Telegram HTML, optionally threading
// a reply to replyToMsgID on the first chunk (and falling back to plain text on HTTP 400).
func (b *TelegramBot) SendReply(ctx context.Context, chatID, replyToMsgID int64, text string) error {
	chunks := splitChunks(text, 3500)
	for i, chunk := range chunks {
		var replyID int64
		if i == 0 {
			replyID = replyToMsgID
		}
		htmlChunk := MarkdownToTelegramHTML(chunk)
		status, err := b.postMessage(ctx, chatID, replyID, htmlChunk, "HTML")
		if err != nil && status == http.StatusBadRequest {
			// Safe fallback: if Telegram rejects HTML markup for any edge case, deliver plain text.
			if b.logger != nil {
				b.logger.Warn("telegram rejected HTML formatting, retrying plain text", "chat_id", chatID, "err", err)
			}
			_, err = b.postMessage(ctx, chatID, replyID, chunk, "")
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (b *TelegramBot) postMessage(ctx context.Context, chatID, replyToMsgID int64, text, parseMode string) (int, error) {
	payload := map[string]any{
		"chat_id":                  chatID,
		"text":                     text,
		"disable_web_page_preview": true,
	}
	if parseMode != "" {
		payload["parse_mode"] = parseMode
	}
	if replyToMsgID > 0 {
		payload["reply_parameters"] = map[string]any{
			"message_id":                replyToMsgID,
			"allow_sending_without_reply": true,
		}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	endpoint := fmt.Sprintf("%s/bot%s/sendMessage", b.apiBase, b.token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("telegram sendMessage returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}
	return resp.StatusCode, nil
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
