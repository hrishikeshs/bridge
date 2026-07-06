# Cross-crew: magnus-bridge becomes a bridge client

*Decided 2026-07-06, ~1am, closing the circle. Not yet built — this is the
opening campaign of the next session.*

## The decision

magnus-bridge **loses its entire server wing** — HTTP server, PWA, pairing,
tokens, push, audit — and becomes a **client of bridge**. One daemon, one
phone app, one trust layer, one buddy list. The Magnus crew appears in
bridge's golden list next to the terminal crew, *without leaving Emacs*.

The journey that got here (all three stops mattered):
1. "Bridge grows a Magnus connector" — right shape, wrong owner.
2. "Delete magnus-bridge entirely; the crew rehomes to tmux" — tested and
   REJECTED on one load-bearing fact: **Hrishi lives in Emacs.** The den's
   vterms are his workflow, not plumbing. Magnus agents stay home.
3. Final: **the Emacs side keeps only its Magnus-facing heart** (nudge
   delivery, attention taps) and speaks bridge's local API for everything
   phone-shaped.

## What bridge grows (Go, ~small)

- **External contacts.** A contact whose delivery is not `tmux:@N` but an
  executable: `Contact.Deliver = "exec:<name>"` naming a deliverer in
  `~/.bridge/deliverers/` (same trust posture as plugins: 0700 dir,
  non-group-writable, audited, timeout, same-uid honesty). For Magnus
  contacts the deliverer wraps `emacsclient -e '(magnus-bridge-deliver …)'`.
  `deliverToSession` dispatches on kind; approve keys ride the same path.
- **Liveness for external contacts**: the deliverer's exit code is the
  health check (0 = delivered). No tmux probing.
- **`POST /local/attention {contact, prompt}`** — the Emacs side already
  *knows* attention (it advises `magnus-attention-request`); it reports the
  card content directly instead of bridge screen-scraping a pane it doesn't
  own. Clears ride the same endpoint. Push + Yes/Always/No cards then work
  for den agents exactly as for tmux agents.
- **Replies need nothing.** Magnus agents are Claude Code sessions with
  session JSONLs; bridge's tail loop never cared where the terminal was.
  Register with the right `session_id` + directory and replies, typing,
  health all flow.

## What magnus-bridge becomes (elisp, mostly deletion)

- Keeps: `magnus-coord-nudge-agent` glue, the attention advice, the crew
  roster (uuid → name/directory/session).
- Gains: a registrar — on magnus startup (and on agent spawn/retire), sync
  the crew to `POST /local/connect` with `deliver: exec:magnus`; forward
  attention raises/clears to `/local/attention`.
- Loses: `magnus-bridge-server`/`auth`/`events`/`api` — the entire HTTP
  stack, the PWA, ports, pairing, tokens, lockdown. Five modules become
  roughly one and a half.
- The local API is lockfile-token gated (same-uid), so Emacs authenticates
  the same way the CLI and hooks do.

## Estate

- **MELPA PR #10073** stays open but on hold — it currently ships the full
  server package; the slimmed client is what should actually land on MELPA.
  Update the PR after the rewrite rather than shipping something obsoleted
  (Hrishi's call on timing; eligibility clears Aug 1).
- The magnus-bridge repo keeps its history — the trust layer bridge inherited
  was born there, twice Professor-reviewed.

## The first packet

When the unified wire carries its first message, it goes **Vint →
quick-wolf** — the standing promise. The story began with her going dark;
it closes with the den and the terminals on one bridge, and the builder
calling the one who was disconnected.
