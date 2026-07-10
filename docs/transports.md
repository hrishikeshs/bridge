# Transports: how words reach an agent, made pluggable

*Design doc — Phases 2–3 of the transport refactor (the 2026-07-06 "tmux
is just one mechanism" idea). Phase 1 shipped in `d6c9c93`: the
`Transport` interface exists, tmux implements it, every contact names its
transport. Phases 2a+2b shipped in `e0525b6` — the daemon-side remote
transport, with a curl loop proving itself a valid client in smoke.
Phase 3's daemon half shipped in `05af581` (attest-time permission cards
+ the delivery dialog belt) and its Emacs client as
`magnus-bridge-client.el` on magnus-bridge's `bridge-client` branch
(`059deaa`). This doc records the wire protocol and the settled
decisions; the protocol below is live.*

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

## Semantic protocol v2 (additive)

Clients backed by an agent API can negotiate `"protocol": 2` at hello (or use
the `/local/transport/v2/hello` alias). Version 1 remains the default whenever
the field is absent or unsupported, so existing Emacs and simulation clients
retain their byte-for-byte response and delivery shapes.

V2 replaces terminal text and keys with typed commands:

    GET /local/transport/v2/commands?lease=…&wait=25
    → { "commands": [
          { "id":"…", "contact":"…", "type":"input", "text":"…" },
          { "id":"…", "contact":"…", "type":"interrupt" },
          { "id":"…", "contact":"…", "type":"approval",
            "request_id":"…", "decision":"accept" } ] }
    POST /local/transport/v2/ack { "lease":"…", "ids":["…"] }

The same outbox, lease, timeout, and acknowledgement rules used by v1 remain
in force. A semantic client publishes normalized, user-visible output through:

    POST /local/transport/v2/events
    { "lease":"…", "events":[
        { "contact":"…", "type":"agent_message", "text":"…" },
        { "contact":"…", "type":"plan", "text":"…" },
        { "contact":"…", "type":"status", "status":"working" },
        { "contact":"…", "type":"approval_requested",
          "request_id":"…", "approval_kind":"command", "reason":"…" }
      ] }

There is intentionally no raw-reasoning or arbitrary-tool-output event. The
current phone UI remains compatible: its established numeric approval keys are
translated to semantic decisions only for a v2 lease (`1` accept, `2`
accept-for-session, `3` decline, `esc` cancel). V1 continues to receive those
values as literal whitelisted keystrokes.

`bridge codex` is the first v2 client. It starts or resumes a Codex App Server
thread and maps `input` to `turn/start` or `turn/steer`, `interrupt` to
`turn/interrupt`, and App Server messages, plan summaries, and approval requests
back to semantic events.

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

- **2a** — SHIPPED (`e0525b6`): daemon side — remote transport type,
  lease table, hello/attest/mail/ack handlers.
- **2b** — SHIPPED (folded into 2a's smoke): the curl client proves
  delivery/ack, guard-rides-attests, lease-death offline transition,
  durable redelivery with identity continuity, and ack-timeout
  redelivery. No hidden test verb needed.
- **3** — daemon half SHIPPED (`05af581`): attest-time permission cards
  and the delivery dialog belt (decisions 5–6 below). Emacs client
  SHIPPED as `magnus-bridge-client.el` (magnus-bridge `bridge-client`
  branch, `059deaa`): hello, attest timer, drain into vterm, generation
  guards, self-healing across daemon restarts — proven end-to-end by an
  integration script whose scratch daemon raised a card off the client's
  attested dialog tail. Remaining tail: wire connect into the magnus
  start ceremony, retire the legacy in-Emacs server, MELPA v0.8.
- **4**: the Magnus crew crosses; #crew gains quick-wolf.

## Decisions (2026-07-06 evening — lead dev; veto anytime)

1. **Lease per client.** One hello = one lease covering all its agents; a
   hung *single buffer* inside a live Emacs is the client's problem to
   attest (`ready:false` for that agent). A stale lease is dead forever —
   attest against it returns 410 and the client re-hellos.
2. **screen_tail: 4KB, tail end, strip nothing.** It transits localhost
   only and feeds the same card/dialog logic as capture-pane (same trust);
   the clamp keeps the *bottom* of the screen, where prompts live, and
   never splits a rune.
3. **The client launches remote agents.** Magnus already manages its own
   vterms; rehoming inside Emacs is the client's ceremony. The daemon only
   ever *reaches*, never *spawns*, remote agents — reconcile's revive
   branch is tmux-only, and a remote contact's revival IS the re-hello.
4. **The mechanism registers as `remote`** with a client-supplied flavor
   label ("emacs", "sim") stored on the contact and surfaced in
   `/api/status` — one implementation serves every future environment.

5. **One judge for prompts.** The daemon's `looksLikePrompt`, run on the
   attested tail, decides both the RAISE (attest-time, transition-only,
   the full hook-path ceremony) and the CLEAR (verifyPrompt's two-strike,
   which already rode the transport interface). The client's
   `prompt_open` field is advisory only — two judges would flap the card
   and ring the phone in a loop. Attest-time detection is the primary
   rung for remote agents: the user-global Notification hook fires the
   instant a dialog opens but judges the last attested tail, usually
   stale at that moment; the attest that carries the dialog is the
   reliable raise, and the transition guard makes the two paths compose.
6. **The client stays dumb and self-guarded.** It never detects dialogs
   in elisp — it ships the raw buffer tail and hands judgment to the
   daemon's one detector. Its half of the never-type-into-an-open-dialog
   guarantee: a text delivery for an attention-flagged agent is neither
   typed nor acked, so the daemon's ack timeout re-routes the line to
   the durable mailbox and the next attest raises a phone card instead.
   Approval keys are whitelisted on both sides of the wire.

Two implementation rulings the doc's first draft left implicit: `Deliver`
*blocks* on the client ack (bounded by `remote_ack_timeout_s`, default
10s) because flushMailbox's contract is synchronous — nil means typed,
and only a real ack may license DropMailbox (M6). And an ack timeout
marks the lease *suspect* — Ready reads false until the next attest — so
a half-broken client (attesting but not draining) costs one bounded
stall, not one per tick. Identity crosses environments: a hello matching
an OFFLINE contact by name+directory adopts it (thread, mailbox, id) and
flips its transport; a LIVE tmux contact is never hijacked — the hello
gets a suffixed fresh identity instead.

And three Phase 3 rulings from the elisp side, all bought with a live
bug: every string handed to url.el goes through one unibyte coercion at
the client's single HTTP seam — a `json-parse`d token anywhere in the
request re-poisons the UTF-8 body and url-http rejects the whole thing,
which killed the attest heartbeat the first time a screen tail carried a
real dialog's ❯. Any non-200 on attest or mail re-hellos with bounded
backoff (a 401 from a rebooted daemon must not strand the client or
hot-loop the long-poll). And every hello re-reads the daemon lockfile,
so a restart's fresh token and port heal without the user touching
Emacs.
