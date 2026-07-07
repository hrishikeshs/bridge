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
sessions (claude — in daemon-owned tmux, or hosted by a connected transport client)
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
  10-minute, single-use, printed only by `bridge pair` on the machine.
- `GET  /api/status` → `{contacts: [{id, name, directory, status, health,
  attention}], version}`. `status`: `live | offline`. `health`:
  `ok | working | prompt | offline`. Non-tmux contacts additionally carry
  `transport` (`"remote"`) and `transport_flavor` (`"emacs"`), so the buddy
  list can tell a vterm from a tmux window (§7).
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
  (single line; literal mode) — the tmux transport. For a **remote** contact
  the same composed line is instead *parked* for its hosting client to drain,
  type, and ack (§7); the frame is daemon-authored either way. Prefixed
  `[From <user> (phone)]:`. Works
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
- **Remote contacts**: the same dialog detector runs daemon-side at *attest
  time* on the client-attested screen tail — one judge for both the raise and
  the clear, transition-only so the hook path composes instead of double-
  ringing. Approval keys ride the client drain with Deliver's ack discipline.

## 6. Switchboard (agent ⇄ agent)

`bridge send --to <name> "<text>"` routes agent-to-agent through the same
mailboxes, delivered as `[From <sender> (bridge)]:` via send-keys. Same
offline semantics. The phone sees the traffic in the relevant threads.

## 7. Transports — how words reach an agent (pluggable)

Only five operations were ever tmux-shaped, and they are the `Transport`
interface: **Alive** (host exists), **Ready** (safe to type THIS instant),
**Deliver** (one prepared line), **Capture** (screen for dialog detection),
**SendKey** (one whitelisted approval key). A contact names its transport
(empty = `tmux`, so every pre-transport roster row migrates by doing
nothing); an unknown name fails safe — never ready, mail waits durably.

The **remote transport** inverts the reach: the daemon never touches the
client's environment; the client (an Emacs package first — a curl loop in
smoke, which is the point) registers agents and continuously attests their
state over the local API (lockfile token, same trust as every CLI verb):

- `POST /local/transport/hello {transport, agents:[{name, directory,
  session_id}]}` → `{agents:[{id, name}], lease, ttl_s}` — registers or
  adopts contacts. A LIVE tmux contact is never hijacked (the hello gets a
  suffixed fresh identity); an offline same-name+directory contact is
  adopted — same id, same thread, same mailbox.
- `POST /local/transport/attest {lease, states:[{id, ready, prompt_open,
  screen_tail}]}` — the heartbeat. Alive = lease fresh; Ready = fresh ∧
  attested ready ∧ no dialog visible in the attested tail (the delivery
  belt); Capture = the attested tail. A stale lease fails every answer safe;
  attesting against one returns 410 and the client re-hellos.
- `GET /local/transport/mail?lease=…&wait=25` — long-poll; returns ALL
  currently-parked deliveries every time (the client dedups by id).
- `POST /local/transport/ack {lease, ids}` — `Deliver`/`SendKey` BLOCK until
  this ack (bounded by `remote_ack_timeout_s`); only a real ack releases the
  durable mailbox. At-least-once: a timeout redelivers under a fresh id.

A disconnected client is byte-for-byte an offline contact: its lease goes
stale, its agents read dead, mail queues durably, a re-hello revives them —
no new failure modes. Full protocol, settled decisions, and the recorded
design rulings: [transports.md](transports.md).

## 8. Security model (inherited, adapted)

1. Daemon binds **127.0.0.1**; the tailnet (via `tailscale serve`) is the
   phone's only road in. `bridge expose` wraps the serve setup.
2. Tailscale identity allowlist (`~/.bridge/config.json`) + per-device
   pairing tokens (0600), single-use 10-minute codes printed only on-machine.
3. Local CLI↔daemon calls authenticate with the lockfile token (0600) —
   same local-trust posture as magnus-bridge, documented.
4. Approve endpoint: whitelisted keys, hook-attested prompt state only.
5. Every request audited (`~/.bridge/audit.log`); bodies size-capped;
   `bridge lockdown` kills the server and revokes every device.
6. Agents still face Claude Code's own permission system. For an **honest**
   agent, the bridge *extends* the human loop — you approve from your phone —
   it doesn't replace it.
7. **What the bridge does NOT do: contain a compromised agent.** Managed
   agents run as the *same UNIX user* as the daemon, so a prompt-injected
   agent can read its own device token from `~/.bridge/` and `POST
   /api/approve` to answer its own permission prompt — no human. The 0600
   file perms stop *other users*, not the very process they host. So the
   human-in-the-loop guarantee holds exactly as far as the agent is
   trustworthy. Real containment (running agents under a separate uid, or an
   out-of-band approve confirmation) is out of scope for v1 — **don't rely on
   the bridge to sandbox an agent you don't already trust.**

## 9. Explicitly out (v1)

Windows (POSIX+tmux first) ·
Channels transport (optional adapter if/when it exits preview and the
allowlist politics resolve) · multi-machine federation (one daemon per box;
the tailnet already reaches them all — the PWA can point at several).
(Web push graduated from this list: shipped, RFC 8291 E2E-encrypted.)
