# Pregravity

A minimal, auditable, zero-dependency Go personal accountability & organization daemon designed to run 24/7 on your devbox and hand off deep engineering work cleanly to **Google Antigravity (`agy`)**.

## Why Pregravity?

* **Zero third-party dependencies:** 100% Go standard library (`CGO_ENABLED=0` static binary). Easy to read and audit top-to-bottom.
* **Strict least-privilege boundary:** Pregravity has **no shell execution** and **no access to your source repositories**. It can only read/write its own data directory (`~/.local/share/pregravity`) for tasks, memory, daily journals, and Markdown handoff briefs.
* **100% Antigravity ToS-compliant:** Scheduled check-ins and lightweight messaging run on your own **Gemini API key** (e.g., `gemini-3.8-flash`). When a thread needs deep coding or terminal work, Pregravity writes a clean Markdown handoff briefing (`handoffs/<id>.md`) and sends you a link to your first-party Antigravity Remote Control dashboard (`https://antigravity.google.com`) with a `/pickup <id>` command—never wrapping or proxying Antigravity OAuth tokens.

## Architecture

```
  [Cron Check-ins / Telegram / CLI]
                 │
                 ▼
       ┌───────────────────┐
       │    pregravity     │  (Gemini Flash API · NO shell · confined to data_dir)
       │ tasks · memory ·  │
       │ journal · handoff │
       └─────────┬─────────┘
                 │ writes handoffs/<id>.md
                 ▼
     ~/.../pregravity/handoffs/  ◄─── /pickup <id> ───  Antigravity (agy remote-control)
                 │                                       (full sub · native approvals)
                 └── reads <id>.done.md ◄─────────────── writes closing summary
```

## Quick Start

```bash
# 1. Build & install binary to ~/.local/bin/pregravity (zero C/make dependencies)
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o ~/.local/bin/pregravity ./cmd/pregravity

# 2. Initialize config, 0600 env template, data dir, and the /pickup Antigravity skill
pregravity init

# 3. Add your GEMINI_API_KEY (from https://aistudio.google.com/apikey)
$EDITOR ~/.config/pregravity/env

# 4. Verify setup
pregravity doctor

# 5. Try interactive CLI chat (or local zero-token commands)
pregravity tasks add --priority high --due tomorrow "Ship v0.1 of pregravity"
pregravity chat
```

## Commands

* `pregravity init` — creates `~/.config/pregravity/config.json`, `~/.config/pregravity/env` (`0600`), `~/.local/share/pregravity/`, and installs `~/.gemini/config/skills/pickup/SKILL.md`.
* `pregravity daemon` — runs the cron scheduler + optional Telegram long-poller, ntfy/webhook notifications, and loopback HTTP API.
* `pregravity chat [-s session] [-p "prompt"]` — interactive terminal chat or one-shot prompt.
* `pregravity tasks [list|add|done|snooze]` — manage tasks locally with zero API calls.
* `pregravity handoffs [list|create|show|done]` — inspect or stage Antigravity handoffs locally.
* `pregravity install-skill` — installs the `/pickup` Antigravity skill.
* `pregravity doctor` — checks config, secret file permissions, data dir, `/pickup` skill, and `agy remote-control` status.

## Running as a Hardened Systemd User Service

```bash
mkdir -p ~/.config/systemd/user
cp deploy/pregravity.service ~/.config/systemd/user/pregravity.service
systemctl --user daemon-reload
systemctl --user enable --now pregravity.service
journalctl --user -u pregravity.service -f
```

The included unit enforces `NoNewPrivileges=yes`, `ProtectSystem=strict`, `ProtectHome=read-only`, `ReadWritePaths=%h/.local/share/pregravity`, and `MemoryMax=128M`.

## Optional Transports

* **Telegram (recommended for mobile chat):** Long-polling (no inbound ports needed). Set `telegram.enabled: true` and your numeric chat ID in `telegram.allowed_chat_ids` in `~/.config/pregravity/config.json`, and put `PREGRAVITY_TELEGRAM_BOT_TOKEN=...` in `~/.config/pregravity/env`. Fails closed if `allowed_chat_ids` is empty.
* **ntfy.sh / Webhook:** Set `notify.ntfy_topic` (plus optional `notify.ntfy_url` / `PREGRAVITY_NTFY_TOKEN`) or `notify.webhook_url` to receive scheduled check-in push notifications.
* **Loopback HTTP API:** Set `http.enabled: true` (restricted strictly to `127.0.0.1` / `localhost` / `[::1]`) and `PREGRAVITY_HTTP_TOKEN=...` in `env`.
