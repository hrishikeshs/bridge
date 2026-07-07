# Transports: how words reach an agent, made pluggable

*Design doc — Phase 2 of the transport refactor (the 2026-07-06 "tmux is
just one mechanism" idea). Phase 1 shipped in `d6c9c93`: the `Transport`
interface exists, tmux implements it, every contact names its transport.
This doc specifies the **remote transport** — how an external environment
(Emacs first) becomes a client of the bridge and hosts agents the daemon
can reach. Review before building; nothing here is load-bearing yet.*

## The idea

The daemon's core never cared about tmux. Mailboxes, coalescing, rooms,
the cooldown, away messages, the Herald, the guarded flush, the JSONL
tail and the whole identity ladder are transport-agnostic today. Only five
questions were ever tmux-shaped, and they are now the `Transport`
interface (transport.go):

    Alive(c)    — does the agent's host exist right now?
    Ready(c)    — is it SAFE to type this instant?
    Deliver(c)  — type one prepared line, submitted
    Capture(c)  — screen snapshot for dialog detection / cards
    SendKey(c)  — one whitelisted approval keystroke

tmux answers these by shelling out. A **remote transport** answers them
from state that a connected client continuously attests. The daemon never
reaches into Emacs; Emacs reaches into the daemon — *"Emacs is just
another client of the bridge."*

## The remote-client protocol (local API, lockfile token)

A client (an Emacs package, or anything that can speak HTTP on this
machine) manages one or more agents. All endpoints are under `/local/`
and authenticate with the lockfile token, same as every CLI verb.

### 1. Register

    POST /local/transport/hello
    { "transport": "emacs", "agents": [
        { "name": "quick-wolf", "directory": "/Users/x/proj",
          "session_id": "<claude session uuid>" } ] }
    → { "agents": [ { "id": "<contact uuid>", "name": "quick-wolf" } ],
        "lease": "<opaque lease token>", "ttl_s": 30 }

Registers (or revives) each agent as a contact with
`Transport:"emacs"`. Name/identity rules are exactly `handleLocalConnect`'s
(H9 sanitization, uniqueness suffixes). The **lease** is the liveness
primitive: everything below carries it.

### 2. Attest (the heartbeat that answers Alive/Ready)

    POST /local/transport/attest
    { "lease": "…", "states": [
        { "id": "<contact>", "ready": true, "prompt_open": false,
          "screen_tail": "<last ~18 lines, optional>" } ] }
    → { "ok": true, "ttl_s": 30 }

Sent every ~10s and on any state change. Daemon-side, the remote
transport's implementation of the interface is then pure bookkeeping:

- `Alive(c)`   = lease fresh (attested within TTL)
- `Ready(c)`   = lease fresh **and** last attested `ready` — stale ⇒ false
  (fail-safe: the guarded flush defers, mail waits durably)
- `Capture(c)` = last attested `screen_tail` ("" when stale ⇒ callers
  assume the worst, exactly like an unreadable pane today)

A dead client (crash, Emacs quit, laptop sleep) simply stops attesting:
its agents go offline through the normal two-strike path, mail queues
durably, and a re-`hello` revives them. **No new failure modes — a
disconnected client is byte-for-byte an offline contact.**

### 3. Drain (delivery)

    GET /local/transport/mail?lease=…&wait=25
    → { "deliveries": [ { "id": "<delivery id>", "contact": "<id>",
                           "text": "[From Hrishi (phone)]: …" } ] }
    POST /local/transport/ack   { "lease": "…", "ids": ["<delivery id>"] }

Long-poll (25s, then re-poll; SSE later if it earns its keep). The
daemon-side `Deliver(c, text)` **parks the composed line** in a
per-transport outbox and returns success only after the client acks —
until then flushMailbox treats it as in-flight, exactly like the
BeginFlush slot today. The frame text is composed daemon-side as always
(provenance is never client-authored). The client types it into the
agent's vterm/buffer and acks. Un-acked deliveries return to the mailbox
when the lease dies — group-granular redelivery, the M6 guarantee kept.

### 4. Keys

Approvals ride the same drain as typed deliveries with a `key` field
instead of `text`; the client maps them to its environment (vterm
send-key). `SendKey` shares Deliver's ack discipline.

## What does NOT change

- **Outbound is untouched.** Remote agents are Claude Code sessions; the
  JSONL tail, identity ladder, pins and chains work on them today.
- **Every safety rule holds by construction**: the guarded flush calls
  `Ready`; stale attestation fails safe; provenance frames and
  neutralization stay daemon-side; the cooldown, rooms, coalescing and
  client_id dedup live above the transport line and never knew tmux
  existed.
- **The tmux path is not modified.** Remote transports are additive files
  plus `/local/transport/*` routes. Stop-anywhere safe (baby rule).

## Phase plan

- **2a**: daemon side — remote transport type, lease table,
  hello/attest/mail/ack handlers, smoke coverage with a scripted fake
  client (a curl loop is a valid transport client; that is the point).
- **2b**: `bridge transport-sim` hidden test verb OR pure smoke curls —
  prove delivery/ack/redelivery and lease-death offline transitions.
- **3**: the Emacs client (elisp): hello on magnus start, attest timer,
  drain into vterm, prompt detection from buffer tail. magnus-bridge
  becomes this package — one daemon, one PWA, N environments.
- **4**: the Magnus crew crosses; #crew gains quick-wolf.

## Open questions (for review)

1. **Lease per client or per agent?** Per client (one Emacs = one lease)
   is simpler and matches reality; a hung *single buffer* inside a live
   Emacs is the client's problem to attest (`ready:false` per agent).
2. **screen_tail privacy**: it transits localhost only and feeds the same
   card/dialog logic as capture-pane; cap at ~4KB, strip nothing (same
   trust as tmux capture today).
3. **Who launches remote agents?** v1: the client does (Magnus already
   manages its own vterms); `bridge connect`-style rehoming inside Emacs
   is the client's ceremony. The daemon only ever *reaches*, never
   *spawns*, remote agents.
4. **Name**: "emacs" the transport vs "remote" the mechanism — propose
   registering the *mechanism* as `remote` with a client-supplied flavor
   label surfaced in `/api/status`, so one implementation serves every
   future environment.
