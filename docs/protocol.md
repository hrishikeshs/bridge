# bridge protocol

The contract between the three parties: your **phone** (PWA), the **daemon**
(`bridge serve`, one per machine), and your **sessions** (Claude Code
processes the daemon manages). Everything below runs on hardware the user
owns; no third party carries a byte of it.

```
phone (PWA over tailnet HTTPS)
   ⇅  REST + SSE, paired-device tokens
daemon (127.0.0.1, Go, one process)
   ⇅  tmux send-keys (in) · session JSONL tail (out) · Notification hook (prompts)
sessions (claude, rehomed into daemon-owned tmux)
```

Design lineage: the phone⇄daemon half is ported from
[magnus-bridge](https://github.com/hrishikeshs/magnus-bridge) v0.7.0
(reviewed at production grade); the daemon⇄session half replaces Emacs/vterm
with tmux + files + hooks. Deliberately **no** dependence on gated platform
features (Channels allowlists, Remote Control): only `--resume`, session
JSONL files, and settings.json hooks — the parts nobody can revoke.

---

## 1. Identity model

| Identifier | What | Lifetime |
|---|---|---|
| `name` | the address ("wolf", self-chosen at connect) | stable, unique among the living |
| `contact id` | daemon-minted uuid for the *agent* | survives restarts/resurrections of the same managed session |
| `session-id` | Claude Code's conversation file id | churns on every `--resume`; tracked per contact |

Threads belong to the **contact id**; names address whoever currently
holds them. (Ontology per the den: a new contact is a new agent, even
under an old name. Sessions are the mortal layer.)

## 2. Phone ⇄ daemon (ported API, unchanged semantics)

All endpoints require the Tailscale identity header (when configured) and a
paired-device token, except `POST /api/pair` and static assets. Requests are
audited; bodies capped; history durable (JSONL on disk).

- `POST /api/pair {code, device}` → Set-Cookie token. Codes: 6 digits,
  2-minute, single-use, printed only by `bridge pair` on the machine.
- `GET  /api/status` → `{contacts: [{id, name, directory, status, health,
  attention}], version}`. `status`: `live | offline`. `health`:
  `ok | working | prompt | offline`.
- `POST /api/send {agent, text, client_id}` → 200 | 200 duplicate | 409 offline.
  `agent` = contact id or name.
- `POST /api/approve {agent, key}` — key ∈ `1 2 3 y n esc` and only while a
  prompt is open (hook-attested, see §5).
- `POST /api/upload {agent, text, image, client_id}` — photo saved locally
  (daemon-named, 0600), path handed to the agent to Read.
- `GET  /api/history?since=N`, `GET /api/events` (SSE, resume by id;
  transient `typing` events carry no id).

Idempotency: `client_id` dedup (200-entry ring) — retries never
double-deliver.

## 3. Onboarding — consent-first rehome

The agent does the work; the human types two commands and one code.

1. User to a running agent: **"use bridge so we can text"**
2. Agent: installs (`brew install hrishikeshs/tap/bridge` | `go install
   github.com/hrishikeshs/bridge@latest`), reads `bridge --help` (written
   for agents), runs **`bridge connect --name <self-chosen>`**
3. `connect`: starts the daemon if absent (lockfile `~/.bridge/daemon.json`
   with port+local token); registers the contact; identifies the calling
   session's id + cwd (from the newest JSONL under `~/.claude/projects/`
   matching cwd, confirmed by the agent); **rehomes**: spawns
   `tmux new-session -d -s bridge-<name> claude --resume <session-id>` in
   the original cwd; installs the Notification hook (§5) if absent.
4. The *original* terminal copy signs off: *"I've moved. Quit this session,
   then `bridge attach <name>` to keep a terminal on me, `bridge pair` for
   your phone."* The user quits it — one fork, zero divergence, their PTY
   never touched.
5. `bridge attach <name>` = `tmux attach -t bridge-<name>` (the terminal
   experience returns, now with a phone line). `bridge pair` prints the code.

## 4. Message flow

- **Phone → agent**: daemon `send-keys -t bridge-<name> -l <text>` + Enter
  (single line; literal mode). Prefixed `[From <user> (phone)]:`. Works
  mid-task (queued by CC input) and while idle (wakes the read loop).
  Offline contact → mailbox (server-side outbox) + 409 to phone; delivered
  on next `connect`/revive of that contact.
- **Agent → phone**: daemon tails the contact's *current* session JSONL
  (re-resolved after every resume), relays **assistant `text` blocks only**
  — thinking and tool internals never leave the machine (magnus-bridge
  semantics, smoke-tested there). Growth without text ⇒ transient `typing`.
- **Agent-initiated**: agents may run `bridge send "<text>"` themselves at
  any time (the daemon knows the caller from the session env var
  `BRIDGE_CONTACT` set inside the rehomed tmux) — composed messages, not
  scraped ones.

## 5. Permission prompts — hook-triggered, capture-enriched

- `bridge connect` ensures a **Notification hook** in `~/.claude/settings.json`
  (hooks hot-reload; no restart): matcher for permission/idle notifications,
  command `bridge hook` — which POSTs `{session_id, message, kind}` to the
  daemon's local endpoint.
- On a permission event for a managed contact, the daemon takes **one**
  `tmux capture-pane -p -t bridge-<name>` snapshot for the card body
  (options text), emits an `attention` event, and marks the contact
  `prompt`. The phone renders labeled buttons parsed from the snapshot
  (fallback: 1 / 3 / esc).
- `POST /api/approve` is honored only while the contact is hook-attested as
  prompting (no blind key injection). Keys go in via send-keys. An
  `idle_prompt` notification clears the prompt state.
- **No polling loop; no continuous scraping.** Worst case if CC repaints its
  prompt UI: an uglier card whose buttons still work.

## 6. Switchboard (agent ⇄ agent)

`bridge send --to <name> "<text>"` routes agent-to-agent through the same
mailboxes, delivered as `[From <sender> (bridge)]:` via send-keys. Same
offline semantics. The phone sees the traffic in the relevant threads.

## 7. Security model (inherited, adapted)

1. Daemon binds **127.0.0.1**; the tailnet (via `tailscale serve`) is the
   phone's only road in. `bridge expose` wraps the serve setup.
2. Tailscale identity allowlist (`~/.bridge/config.json`) + per-device
   pairing tokens (0600), single-use 2-minute codes printed only on-machine.
3. Local CLI↔daemon calls authenticate with the lockfile token (0600) —
   same local-trust posture as magnus-bridge, documented.
4. Approve endpoint: whitelisted keys, hook-attested prompt state only.
5. Every request audited (`~/.bridge/audit.log`); bodies size-capped;
   `bridge lockdown` kills the server and revokes every device.
6. Agents still face Claude Code's own permission system — the bridge
   extends the human loop, never removes it.

## 8. Explicitly out (v1)

Windows (POSIX+tmux first) · web push (RFC 8291 later — payloads E2E) ·
Channels transport (optional adapter if/when it exits preview and the
allowlist politics resolve) · multi-machine federation (one daemon per box;
the tailnet already reaches them all — the PWA can point at several).
