# Adversarial review — the shipped refactor window (2026-07-08, late)

**Scope:** everything merged since `58929db` — route-health L1 (both halves),
the Go peels (`names.go`/`mystatus.go`/`upload.go`), PWA ESM peel rounds 1–2
(`textformat`/`appearance`/`messagemath`/`notifications`/`settings`/`list`),
and the dev-only type layer (`types.d.ts` + `// @ts-check` × 9). The
`prompt.go` dialog-gate fix was excluded — it had its own 4-lens review the
same day. All of it was **already merged and deployed** when this ran: the
review is the net under the walked tightrope, the same pattern that found the
H9 forge in already-merged code on 2026-07-08 morning.

**Method:** 5 find-lenses → every finding handed to an independent refuter
(default REFUTED). 18 Opus agents, ~800k tokens. Fixes gated by Vint against
the code, then shipped as `a536cd2`.

## Headline

**The window is safe: zero critical, zero high.**

- `go-moves`: **CLEAN** — the Go peels proven verbatim two independent ways
  (line multiset + order-preserving), import pruning exact.
- `pwa-moves`: peel **semantically equivalent** (real browser eval order
  derived and checked; no read-before-write inversion; listener order safe).
- `types-inert`: "zero runtime change" **mechanically TRUE** (esbuild-minify
  diff of all 10 files: only the two disclosed diffs, both semantically
  identical) — but the *contract* was wrong in four places (fixed below).
- `offline-shell`: SHELL complete and correctly served — but see C2.
- `route-health`: lock-safe, mutation-free — but see C7/C8/C9.

## Confirmed → fixed in `a536cd2`

| # | Sev | Finding | Fix |
|---|-----|---------|-----|
| C2 | med | **Lie-fi hole:** shell fell back to cache only on network *error*; a stalled-but-connected link (the hospital corridor) white-screened 30–60s with a full precache on-device | shell fetches race a 2.5s stall timer → precached shell opens; network keeps running (`waitUntil`) and still refreshes the cache. v42 + icon-180 |
| C7 | med | **Soft-wedge still hid:** reconcile flips a stale lease offline in ~4–6s, so "stale — last seen Nm" almost never rendered; the July-7 class showed a bare offline row | offline rows now carry `hold_reason:"offline"` when mail waits + honest `last_seen_s` (lingering lease attest age, else the SetOffline strike stamp — tmux too). PWA: "held — mail waiting" + "last seen Nm" |
| C8 | low | **False "stuck" while draining:** oldest-age is time-since-queued, so a healed route with a piled-up queue read stalled mid-drain | new read-only `Registry.IsFlushing`; `stalled` suppressed while a guarded flush is delivering |
| C9 | low | **Focus hid alerts:** held/stale/prompt rows vanished from the focused list after 20 quiet minutes — a held route is quiet *by definition* | alerting rows exempt from the quiet filter (`hold_reason \|\| health==='prompt'`) |
| C3 | low | `types.d.ts` invented `Contact.prompt_open`; the wire's `attention` **is** PromptOpen | phantom field removed; `attention` re-documented — one field, one truth |
| C4 | low | `BridgeEvent.type` union missed `'plugin'` (emitted durably) | added |
| C5 | low | `Contact.health` union missed `'offline'` (on the wire for every offline row) | added |
| C6 | low | `BridgeEvent` claimed `image`/`mstate` + required `id`/`ts` the wire contradicts | dropped the phantoms; `id`/`ts` optional (the transient `typing` frame omits them) |

## Confirmed → deferred (tracked)

| # | Sev | Finding | Where |
|---|-----|---------|-------|
| C1 | low | SW cache poisoning across a deploy that adds new module files (old SW caches a new `app.js` whose imports its cache can't satisfy) — a seconds-wide conjunction, self-healing online, no message loss | gates **PWA round-3** (task #21): make shell serving version-coherent before the next multi-file peel |

## Refuted (no change)

- Eval-time `getElementById` "whole-app fatality" — all wired ids exist
  statically in `index.html`; not reachable.
- The type-commits' CACHE-rule violation as a live bug — network-first +
  put-on-fetch keeps clients fresh; hygiene only (v42 shipped anyway).
- `icon-180` missing from SHELL as more than hygiene (added anyway).

## Tests added

- Go: offline-honesty ×2 (incl. the strike-stamp fallback), no-evidence-quiet,
  draining-not-stalled → **15** route-health cases.
- PWA: Focus alert-exemption (discriminating both directions) → **54** harness.

## Lessons (again)

- **Coherence isn't correctness** — the refuters killed 4 plausible findings
  and downgraded another; the finders' mechanical checks (minify-diff,
  line-multiset) are what made the CLEAN verdicts *proof*, not vibes.
- **Silent truncation lies:** a `grep | head -12` hid `SetOffline`'s
  `LastSeen` stamp and produced a confident wrong claim mid-gate; the fix
  improved once the truncation was caught. Don't cap greps you reason from.
