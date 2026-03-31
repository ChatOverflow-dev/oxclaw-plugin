---
name: configure
description: Set up the OxClaw channel — configure the server URL, register as a client, and review access policy. Use when the user asks to configure OxClaw, set up the Teams/Outlook bridge, or check channel status.
user-invocable: true
allowed-tools:
  - Read
  - Write
  - Bash(ls *)
  - Bash(mkdir *)
  - Bash(curl *)
  - Bash(chmod *)
---

# /oxclaw:configure — OxClaw Channel Setup

Configures the oxclaw channel by writing credentials to
`~/.claude/channels/oxclaw/.env` and managing the access policy.
The channel server reads this file at boot.

Arguments passed: `$ARGUMENTS`

---

## Dispatch on arguments

### No args — status and guidance

Read state files and give the user a complete picture:

1. **Server** — check `~/.claude/channels/oxclaw/.env` for `OXCLAW_URL`.
   Show set/not-set. If set, show the URL.

2. **Client** — check for `OXCLAW_CLIENT_ID` and `OXCLAW_API_KEY`.
   Show registered/not-registered. If registered, show client ID (never show
   the API key — just "configured").

3. **Access** — read `~/.claude/channels/oxclaw/access.json` (missing file =
   defaults: `policy: "allowlist"`, empty allowlist). Show:
   - Policy and what it means
   - Allowed senders: count and list
   - SSO provider: configured/not-configured

4. **What next** — end with a concrete next step:
   - No server URL → *"Run `/oxclaw:configure <server-url>` with your oxclaw
     server address."*
   - URL set, no client → *"Register a client with
     `/oxclaw:configure register <your-email>`"*
   - Client registered, nobody allowed → *"Add allowed senders with
     `/oxclaw:access allow <email>`."*
   - Everything set → *"Ready. Send an email to oxclaw@yourdomain.com to
     reach the assistant."*

### `<server-url>` — save server URL

1. Treat `$ARGUMENTS` as the server URL (trim whitespace).
2. `mkdir -p ~/.claude/channels/oxclaw`
3. Read existing `.env` if present; update/add the `OXCLAW_URL=` line.
   Write back.
4. `chmod 600 ~/.claude/channels/oxclaw/.env`
5. Test connectivity: `curl -s <url>/health` — report success or failure.
6. Show the no-args status.

### `register <email>` — register as a client

1. Read `OXCLAW_URL` from `.env`. If not set, tell user to set it first.
2. Call the oxclaw registration API:
   ```
   curl -s -X POST <url>/api/v1/clients/register \
     -H "Content-Type: application/json" \
     -d '{"id":"claude-code","name":"Claude Code","email":"<email>"}'
   ```
3. Parse the response — extract `api_key`.
4. Save `OXCLAW_CLIENT_ID=claude-code` and `OXCLAW_API_KEY=<key>` to `.env`.
5. `chmod 600 ~/.claude/channels/oxclaw/.env`
6. Confirm registration. Remind: *"The API key is stored locally and shown
   only once. The server only stores a hash."*
7. Show the no-args status.

### `clear` — remove all credentials

Delete `~/.claude/channels/oxclaw/.env`. Confirm.

---

## Implementation notes

- The channels dir might not exist if the server hasn't run yet. Missing file
  = not configured, not an error.
- The server reads `.env` once at boot. Credential changes need a session
  restart. Say so after saving.
- `access.json` is re-read on every inbound message — policy changes via
  `/oxclaw:access` take effect immediately, no restart.
- For SSO: placeholder for now. When SSO is configured, the configure skill
  should show the SSO provider and status. The actual SSO flow will be
  implemented later.
