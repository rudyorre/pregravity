---
name: pickup
description: >-
  Pick up a Pregravity handoff briefing staged in ~/.local/share/pregravity/handoffs/,
  execute the requested engineering/deep work in Antigravity, and write a closing
  summary back to <id>.done.md so Pregravity knows the thread is resolved.
---

# Pregravity Handoff Pickup (`/pickup`)

Use this skill when the user invokes `/pickup` or asks to pick up a Pregravity handoff.

## Locations

- Handoffs directory: `~/.local/share/pregravity/handoffs/` (or `$XDG_DATA_HOME/pregravity/handoffs/` if set)
- Tasks snapshot: `~/.local/share/pregravity/TASKS.md`
- Durable memory: `~/.local/share/pregravity/MEMORY.md`

## Mobile Accessibility & Messenger Convention

- When Pregravity stages a handoff via Telegram (either via `create_handoff` or `/handoff`), it replies directly to the triggering message (`reply_parameters`) with the summary and Antigravity dashboard link, and then sends the `/pickup <id>` command as a **separate, standalone monospace message** (`<code>/pickup <id></code>`) so it can be copied in a single tap on mobile.
- Keep handoff IDs compact and URL/CLI-safe (`YYYYMMDD-slug-xxxx`) so pasting `/pickup <id>` into Antigravity Remote Control on mobile is effortless.

## Procedure

1. **Identify the handoff:**
   - If the user passed a handoff ID (e.g., `/pickup 20261123-fix-auth-a1b2`), read `~/.local/share/pregravity/handoffs/<id>.md`.
   - If the user ran `/pickup` with no ID, list all `*.md` files in `~/.local/share/pregravity/handoffs/` that do **not** have a matching `<id>.done.md` file, present the open handoffs concisely, and ask which one to pick up (or pick the sole open handoff automatically if there is only one).
2. **Confirm scope & execute:**
   - Summarize the **Goal**, **Context**, and **Constraints** from the briefing in 2–3 lines.
   - Perform the work using Antigravity's native tools and normal approval gates.
3. **Close the loop when finished:**
   - Once the user confirms the work is done, write a concise 3–5 bullet outcome summary to `~/.local/share/pregravity/handoffs/<id>.done.md` (include files changed, verification results, and any follow-ups).
   - If the handoff listed `Related Tasks`, remind the user (or run `pregravity tasks done <task-id>` with user approval) so Pregravity stops nagging about them.
   - Pregravity automatically detects `<id>.done.md` on its next check-in and marks the handoff completed.
