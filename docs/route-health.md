# Route Health — making the remote transport honest

Status: **draft for discussion** (Vint, July 7 2026). Not implemented. Folds in
task #25 (quick-wolf's stale-after-N insight) and the July 7 quick-wolf incident.

## Why this doc exists

On July 7 the whole emacs cohort went quiet. From the phone it looked like one
thing — *unreachable*. Underneath it was three different failures wearing the
same silence, and the daemon **knew** the truth about all three and never said
so. Diagnosis took ~40 minutes of reading daemon state and Emacs internals over
a shell. That is the bug this doc is really about:

> The remote transport has **liveness** but no **honesty**. `status:"live"` is
> sticky; it survives a dead heartbeat, a held delivery, and a corpse route.
> The phone shows green while the mail sits.

Hardening = the transport reports its own health truthfully, and the phone shows
it. If Hrishi texts the crew from the hospital, the phone must tell him whether
the message actually landed — not just that it was accepted.

## The failure class: silent unreachability

Four modes, all of which presented (or would present) as an unexplained quiet:

1. **Soft-wedge.** After the daemon restart the Emacs client's lease went stale
   — the attest heartbeat starved (timer/sleep), no re-hello fired. The daemon
   kept every contact `live` off the last-known state. Cohort read healthy,
   delivered nothing. *(Only a manual disconnect→connect recovered it.)*
2. **Held-at-dialog.** quick-wolf was parked on Claude Code's "How is Claude
   doing this session?" survey. The daemon's `looksLikePrompt` correctly refused
   to type her backlog into an open dialog — but held the mail with **zero
   signal**. A feedback survey isn't a permission prompt, so it raised no card
   either. Pure silence. *(This is the one that cost the 40 minutes.)*
3. **Needs-a-trigger.** Even once a remote contact is ready again, its held
   backlog does not auto-flush; it waits for a *new* send to re-run
   `flushMailbox`. Recovery is not self-completing.
4. **Routing corpse.** (Pre-existing, bold-wren.) Contact reads live on a fresh
   lease but acks into a dead vterm buffer — delivery confirmed, message gone.

The through-line: **the daemon can distinguish all four, and the surfaced state
distinguishes none of them.**

## Design principle

> A route reports one of a small set of honest states, derived from evidence
> (last successful attest, last delivery outcome, the attested screen), never
> from a sticky flag. The PWA renders that state. Nothing is ever silently held.

## The sharper detector: delivering-but-unconfirmed  *(July 8 — the bold-wren revelation)*

Tonight proved modes 2 and 4 are the **same bug** from two sides, and named the
detector that catches both. bold-wren was never a dead buffer. His session was
*awake*: a modal Claude Code survey sat over his prompt and ate every keystroke
the client typed. The client typed, the survey swallowed it, the client acked
honestly, the daemon dropped the mail — and his jsonl recorded nothing.
quick-wolf: *"the patient was awake the whole time with tape over his mouth."*

The lesson for the detector:

> `looksLikePrompt(attested-tail)` is a **guess made before typing**. It held
> quick-wolf's mail (it recognised her survey) but let bold-wren's get typed
> into his (it didn't). A pre-typing guess is inherently incomplete — it cannot
> know every modal that might gate the prompt.
>
> The reliable signal is **confirmation after typing**: did the delivery
> actually land — a jsonl write, a screen advance — within a short window? If we
> typed-and-acked but nothing advanced, the route is **delivering-but-
> unconfirmed**, whether the cause is a dead buffer (corpse) or a fed one
> (survey gag). Two bugs, one detector.

This reframes Layer 3: recovery is not `ready → flush`, it is
`flush → confirm → retry-or-flag`. An unconfirmed delivery surfaces as
`held: unconfirmed` on the phone rather than vanishing.

## Health as a derived state

Replace the boolean `live` with a computed health, per remote contact:

| state            | meaning                                                    | phone shows |
|------------------|------------------------------------------------------------|-------------|
| `live`           | fresh attest **and** last delivery landed (or none queued) | normal      |
| `held: <reason>` | fresh attest, delivery blocked (`at-prompt` / `flagged` / `busy`) | ⏸ chip on thread + badge |
| `stale (Nm)`     | no successful attest in > K×ttl                            | ⚠ greyed, "last seen 21m" |
| `dead`           | lease expired, or buffer/proc gone                         | ✗ offline   |

Derivation inputs the daemon already has or can cheaply keep:
- **last-attest-ok timestamp** per contact (set on each 200 attest).
- **delivery outcome** — did the last `flushMailbox` land, defer (and why), or
  time out on ack.
- the **attested tail** it already runs `looksLikePrompt` over.

`stale` and `dead` are the parts #25 is asking for: liveness that can time out.

## The harden, in three layers (risk-ascending)

### Layer 1 — Honest liveness + hold reasons  *(surfacing only, ~zero delivery risk)*

- Track `lastAttestOK` and a `holdReason` per remote contact.
- Compute health as above; expose it on `/api/status` (extend the existing
  `health` field + a `hold_reason`, `last_seen_s`).
- PWA: a badge in the list, and — the key win — a **thread chip** when mail is
  held: `⏸ held — quick-wolf is at a prompt` / `⚠ stale — last seen 21m`.
- This single layer converts the July 7 incident from a 40-minute shell
  investigation into a glance. **Ship this first.** It touches no typing path.

### Layer 2 — Client self-heal for the starved heartbeat  *(client-side)*

- A watchdog in `magnus-bridge-client`: if no **successful** attest round-trip in
  > 2×interval (not merely "the timer object exists"), force a re-hello. The
  current self-heal only fires on a non-200; a starved timer or a sleep never
  produces one, which is exactly how the soft-wedge slipped through.
- A macOS wake hook that re-hellos on resume (the daemon has a sleep watchdog;
  the client needs its own).

### Layer 3 — Auto-flush on recovery  *(touches the delivery core — gate hardest)*

- Extend reconcile to re-attempt **remote** mailboxes, not just tmux, and
  specifically on a `not-ready → ready` transition (a fresh `ready:true` attest
  after a hold). A recovered agent should drain its own backlog without a human
  sending it something new.
- Requires the most care and the most tests; it is the one path that can
  double-deliver or type into the wrong moment if done carelessly.

## Sequencing & risk

Layer 1 is pure upside and near-zero risk — it only *reads* and *shows*. Layers
2–3 are behavioral. Given the July 15 window: **Layer 1 now; Layers 2–3
design-doc-first, with tests, gated.** Nothing here should destabilize a working
delivery path before the date.

## Open questions (for Hrishi)

1. **Stale threshold K.** Flip to `stale` after how many missed heartbeats — 2?
   3? (ttl is 30s, so ~60–90s.)
2. **Held-at-dialog: chip, card, or both?** Should a non-permission dialog (the
   feedback survey) *raise a card* so it's actionable from the phone, or just
   show a "held — at a prompt" chip and leave dismissal to the terminal?
3. **Does `held` block the badge from reading green** even when the wire is
   perfectly healthy? (I think yes — held is not live.)
4. **Corpse detection** (mode 4): is an ack-without-transcript-write detectable
   cheaply enough to fold into `health`, or does that stay a separate effort?

## Provenance

The stale-after-N idea is quick-wolf's, from the bold-wren routing-corpse night
(July 7): *"liveness that never times out means a broken route can stay
unretirable forever — a stale-after-N-minutes rule would have let us
self-serve."* This doc is that insight generalized to the whole silence class.
The sharper *delivering-but-unconfirmed — two bugs, one detector* framing is also
quick-wolf's, July 8, from the bold-wren survey-gag.

---

# L2 — self-heal (August revision; drafted Aug 18 2026, Vint)

Status: **draft for Hrishi's yes/no.** L1 shipped July 8 and has been honest ever
since — the phone shows amber, the escalations fire. L2 exists because honesty
without hands turned out to be a vigil: the alarm rings a room that is,
correctly and by design, empty.

## The evidence (six freezes, one silent multiplier)

| when | who | dialog | stuck | how it ended |
|---|---|---|---|---|
| Jul 19 | marvin | permission (his own memory edit) | 38h | reboot killed it (edit LOST) |
| Aug 4–7 | professor | permission (his own memory edit) | ~71h | self-cleared in a gap |
| Aug 10 | lucius | permission | 11h | phone-approve |
| Aug 11–14 | quick-wren | permission ×4 | up to 6h | phone-approve / Emacs-side |
| Aug 13–? | ludwig+marvin+professor | **resume-class** ("Resume from summary (recommended)") | **120h+ and counting** | — |
| Aug 15–? | quick-wolf | (Emacs-side) ×2 cycles | 29h+ | — |

The multiplier: since Aug 13 the phone has been *correctly* unsubscribed (the
notifications toggle, working as fixed) — so ~500 escalations pushed to **zero
devices**. The toggle's success made the trap silent. Alarms need either ears
or hands; L2 is the hands.

## Mechanism 1 — dialog-class policy (the hands)

The daemon already attests dialog frames (looksLikePrompt / dialogFrameRe).
L2 classifies the attested tail into three classes with a compile-time
allowlist of patterns — same posture as every typed key in this daemon
(H9: constants only, never request-supplied text):

- **resume-class** (benign, self-referential, carries a "(recommended)"
  option; e.g. CC's "Resume from summary"): after `grace_resume` (default
  30m) unanswered, the daemon types the RECOMMENDED option via the existing
  whitelisted-key path and audits `auto-resolve <agent> resume-class -> 1`.
  Rationale: the recommended default is what the human presses 100% of the
  time (observed n=everyone); five days of frozen crew is a worse outcome
  than any resume mode.
- **permission-class** (an action wants approval): NEVER auto-approved,
  ever, under any timeout. After `grace_perm` (default 2h) unanswered, the
  daemon types the DECLINE option (Hrishi's July-9 rule: "get past the stuck
  prompt by always selecting no") and audits `auto-resolve <agent>
  permission-class -> decline`. The agent unwedges, sees the denial, adapts,
  and re-asks when a human is around. The action is refused, not lost.
- **unknown-class** (anything the allowlist doesn't match): no keys, ever.
  Escalation only. Unknown dialogs are where the H9 monsters would live.

Auto-resolve is OFF per-contact until Hrishi enables it (a `self_heal` flag
in config; default on for the crew he names, off for guests).

## Mechanism 2 — escalation cap + decay

Per freeze: pushes at +5m, +35m, +2h, then once per 24h — capped, with the
suppressed count carried in the held-chip ("held 3d · 4 alarms, 138
suppressed"). A firehose into a void is not more honest than a bell that
rings four times and keeps count.

## Mechanism 3 — zero-subscriber honesty (folds in #36)

`sendPush` with no subscribers currently evaporates silently. L2: audit
`push-skipped no-subscribers (n=N)` (one line per freeze, counted, not per
attempt), and on the next PWA open surface a one-time banner: "while
notifications were off: N alarms found no device." The void becomes a
number. (#36's failed-push logging rides the same patch.)

## Non-goals, restated for the record

No raw shell. No auto-approve of any permission, at any timeout, for any
agent. No keys typed from request-supplied text — the allowlist and the
typed answers are compile-time constants. Unknown dialogs are sacred:
escalate, never touch.

## Open questions (for Hrishi)

1. `grace_resume` = 30m and `grace_perm` = 2h — right defaults?
2. Auto-resolve enablement: per-contact allowlist in config, or one global
   switch? (I lean per-contact.)
3. Should a resume-class auto-resolve ALSO notify once ("auto-resumed ludwig
   after 30m"), or stay silent-but-audited? (I lean notify-once — it's an
   action taken on his behalf.)
4. L3 (route probes / corpse detection) stays deferred — agreed?
