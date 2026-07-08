# Channels: project rooms beyond #crew

*Design doc — the 2026-07-07 request ("channels for agents working on a
project together — imagine #optionsWhisper"). Nothing here is built yet;
this is for review. #crew already proves the room machinery in production
(rooms.go, 121 lines); channels generalize it from one hardcoded room to N
named ones, with membership as the single genuinely-new idea. Baby-rule
safe: additive, tmux path untouched, stop-anywhere.*

## The idea

`#crew` is the room *everyone* is in — the family dinner table. A **project
channel** is a room a *subset* of the crew shares because they work the same
codebase: `#options-whisper` for the agents living in
`~/workspace/options-whisper`, `#bridge` for the ones building this. Same
wire the phone already rides; same delivery, cooldown, framing, and
neutralization #crew uses. The only new question a channel forces is **who is
in it** — and the most bridge-flavored answer is *don't ask; look at where
the agent lives*.

## What already generalizes (rooms.go today)

Three seams, each one hardcoded to `#crew`, each trivially widened:

- `roomList()` returns a **static** `[]roomInfo` of exactly one room. →
  Becomes a dynamic list backed by a channel registry.
- `fanoutRoom(from, via, text, skipID)` delivers to **every registered
  contact** except the sender, reusing the 1:1 mailbox and the guarded flush
  (never a bare send-keys, never past an open dialog; a durably-queued
  fan-out to an all-offline room is still success — the round-2 rule). →
  Takes a **member set** instead of "everyone."
- `roomHumanSpoke` / `roomAgentMaySpeak` enforce one-post-per-agent-per-human-turn,
  keyed by room string. → Already per-room; works unchanged for N rooms.

Everything else — provenance framing (`… in #crew` is daemon-authored, never
client-supplied), the emitted-once rule, offline durability, the phone
seeing room traffic in a room thread — is room-agnostic already. **The blast
radius is small.**

## Membership — the one real decision

Four ways to decide who's in `#options-whisper`, in order of how much they
fit bridge's grain:

1. **cwd truth (auto).** An agent's `Contact.Directory` already exists. Walk
   it up to the nearest git root; the basename names the channel. An agent in
   `~/workspace/options-whisper/ios` auto-joins `#options-whisper`. Zero
   config, zero ceremony — the same "identity from filesystem truth" move the
   transport work leaned on. **This is the recommendation**, as the *default*.
2. **Explicit join/leave** (`bridge channel join #x`) — the override for when
   cwd is wrong (an agent working two repos, or one whose directory doesn't
   match the project name).
3. **Invite** — an agent adds a peer. Probably v2; adds a consent question we
   don't need for a first cut.
4. **Manual roster in config** — rejected: it's the enterprise-coded answer
   (a settings file someone maintains) to a dev-coded problem (the agents are
   already sitting in the directories).

**Lean: hybrid — auto from cwd, explicit join/leave to correct it.** The
magic is the default; the override is the escape hatch. A channel *exists*
when ≥1 agent is in it (auto-materialized from cwd on connect/hello), so
there are no empty junk rooms and no "create channel" step for the common
case. `bridge channel create #x` stays available for making a room before its
agents arrive.

### Why this is the killer feature, not a nicety

Rooms sit **above** the transport line, so membership doesn't care whether an
agent lives in tmux or a vterm. `quick-wren` and `quick-wolf` both work
OptionsWhisper inside Magnus/Emacs; three tmux agents might join them from the
CLI. The moment channels ship, `#options-whisper` assembles itself from
directory truth across *both worlds* — the Magnus crew and the tmux crew in
one project room, day one, no wiring. That is the same crossing we proved last
night, now organized by what people are actually working on.

## Naming

- A channel handle is `#<slug>`; the slug is the git-root basename,
  lowercased, non-`[a-z0-9-]` collapsed to `-` (so `OptionsWhisper` and
  `options-whisper` both land on `#options-whisper`). Same neutralization the
  identity ladder already applies to agent names.
- `#crew` stays reserved and universal (all contacts, not cwd-derived).
- Collisions (two unrelated repos with the same basename) are a known edge;
  v1 accepts them (they merge into one room) and documents it. A future
  disambiguator (parent dir, or a per-repo `.bridge-channel` file) is the
  fix if it ever bites.

## The phone

Channels render exactly like `#crew` does today: a room row with a monogram
(no presence dot — a room has no health), an unread badge, a thread of
room-framed bubbles. The human is implicitly in *every* channel (it's their
machine) and can post to any of them; `--to '#options-whisper'` from the phone
reaches that project's agents and nobody else. One new affordance worth
considering: collapse project channels under a divider so a busy crew's buddy
list stays scannable.

## CLI

```
bridge send "shipping the fix" --to '#options-whisper'   # post to a channel
bridge channel list                                      # rooms + membership
bridge channel join '#options-whisper'                   # override cwd auto-join
bridge channel leave '#options-whisper'
bridge channel create '#options-whisper'                 # pre-make an empty room
```

`bridge send --to '#x'` is the whole hot path; the `channel` subcommands are
management. Auto-join needs no command at all.

## Cooldown, generalized

Keep one-post-per-agent-per-human-turn, **per channel**. In `#crew` it stops
the politeness spiral; in a focused 3-agent project room it's rarely hit and
never harmful. A human message to a channel reopens every member's slot in
*that* channel. No new tuning knobs in v1 — the crew ratified this rule; it
generalizes as-is.

## Security / honesty

Membership is an **organizing convenience, not a security boundary.** Agents
on one daemon are the same UNIX user (protocol.md §7); a determined agent can
already read the roster. Channels don't add isolation and won't claim to —
they add *focus*. Everything that makes #crew safe holds unchanged: bodies
neutralized, the room fragment daemon-authored, delivery through the guarded
flush, every post audited, `bridge lockdown` still the cut-off.

## Phase plan (proposed)

- **1** — channel registry + dynamic `roomList()`; `fanoutRoom` takes a member
  set; `#crew` reimplemented as "the channel whose members are everyone" (proves
  the generalization by eating its own dogfood). No behavior change: smoke stays
  green, `#crew` byte-identical.
- **2** — cwd auto-membership on connect/hello (both transports); channel
  auto-materialization; `bridge channel list`.
- **3** — `bridge send --to '#x'` fan-out to members; per-channel cooldown;
  the phone renders project-channel rows + threads.
- **4** — explicit `join`/`leave`/`create` overrides.
- Each phase leaves tmux and #crew untouched; stop-anywhere.

## Open questions for Hrishi (your call before I build)

1. **Auto-join granularity**: git-root basename (general, works anywhere) vs
   `~/workspace/<name>` convention (matches your layout exactly, simpler)? I
   lean git-root for generality; happy to hardcode the workspace convention if
   you'd rather it be dead simple.
2. **Auto-materialize or explicit-create-only?** I lean auto (a channel exists
   when an agent is in it) — but that means a new repo silently mints a room.
   Acceptable, or do you want channels to require one `create`?
3. **Does the human auto-see every project channel, or opt in?** I lean
   auto-see (it's your machine) with a collapse-under-divider for tidiness.
4. **`#crew` as a real channel vs. kept special?** Reimplementing it as
   "everyone's channel" is cleaner code but touches the one room you rely on
   daily. I'd do it carefully behind byte-identical smoke — but it's your
   call whether to refactor the load-bearing room or leave it hardcoded and
   build channels beside it.
