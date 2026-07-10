# Delivery-gate version-robustness (task #27) — investigation & staged fix

**Date:** 2026-07-09 · **Author:** Vint · **Status:** staged on branch, NOT deployed
(prod = `544186a`). Finish/deploy post-baby.

## TL;DR

The delivery gate (`paneShowsDialog`) refuses to type queued phone mail into a
pane that is showing an interactive dialog — otherwise a trailing Enter would
blind-select the dialog's highlighted option. Task #27 worried about a dialog
with a `❯` input field but **no numbered option** ("tell Claude what to do
differently") slipping past the gate.

**Finding: that gap is not reachable in current Claude Code.** CC v2.1.197 renders
`❯ N.` on *every* interactive dialog state, and the existing `dialogFrameRe`
already catches all of them. So the live gate is correct as-is.

This branch adds a **version-robustness hedge** (`dialogFooterRe`) against a
*hypothetical future* frameless dialog — but it is **held off prod** because it
fixes no live bug and the hedge proved subtle (three adversarial passes, three
bugs). The frame gate stands on its own for current CC.

## Ground truth (throwaway `claude` + `tmux` probes)

CC v2.1.197 Bash permission dialog:

```
Do you want to proceed?
❯ 1. Yes
  2. No
Esc to cancel · Tab to amend · ctrl+e to explain
```

- **Tab (amend)** renders the free-text INLINE into option 1: `❯ 1. Yes, <text>` —
  still a `❯ N.` line, still caught by `dialogFrameRe`.
- **ctrl+e (explain)** keeps the `❯ N.` frame.
- **"2. No"** returns to the idle caret.
- Sending approve-key **"3"** to a 2-option dialog is out-of-range: the trailing
  Enter accepts the highlighted option; no text box opens.

So the old "option 3 → bare-`❯` text box" shape #27 was filed against does not
exist in this CC. The dialog UI **changed shape** since #27 was filed (the frame
was 3-option before) — which is the real lesson: the render is version-fragile.

## Can we read dialog state from the JSONL instead of the render? No.

CC writes **no event while a permission dialog is open**. In a probe that sat on a
dialog for ~2s, the `tool_use` and its `tool_result` were **473 ms apart and both
written only AFTER approval** — the entire dialog-open window is a blank in the
transcript. There is no "awaiting permission" record. And the daemon has no access
to a *remote* (Magnus) agent's JSONL anyway — `remote.go` gates on the attested
`ScreenTail`. **The rendered pane is the correct and only uniform signal.**

## The hedge, and why it took three passes

`dialogFooterRe` treats a modal's confirm/cancel **footer** ("esc to cancel/exit",
"enter to confirm") as a dialog even when the frame isn't `❯ N.`. The hard part is
never firing on a genuine idle screen (that deadlocks mail forever — the
2026-07-08 quick-wolf incident). Every veto-based design broke:

| Pass | Design | Adversarial finding |
|---|---|---|
| 1 | whole-tail REPL-footer veto, generic tokens | A modal whose **body** read "accept edits on" vetoed its own hold → blind-select (frameless-gated). |
| 2 | veto on the **last line** only | CC draws a `/statusline`, auto-compact hint, or background-task row **below** its footer → last-line veto reads that row, misses the footer → **idle deadlock**. |
| 3 | whole-tail veto, REPL-only tokens | Daemon captures with `capture-pane -p` (no `-J`, `tmux.go`); a **narrow pane wraps** the footer into split rows, shredding "shift+tab to cycle" and "← for agents" → veto misses → **idle deadlock** (verified at width 24; crew runs 170, so latent, not live). |

**Final design (this branch): drop the veto entirely.** Match `dialogFooterRe`
only against the pane's **last few non-blank lines** (`lastNonBlankLines`), where a
real modal's footer sits. A genuine REPL footer carries no cancel/confirm vocab, so
an idle screen never trips it — wrapped, statusline'd, or otherwise — and agent
prose mentioning "esc to cancel" is above the input caret block, outside the
window. All three prior bugs were *veto* bugs; removing the veto removes the class.
`dialogFrameRe` is untouched, so the change is monotonic (only ever adds a hold).

## Why not deployed

- The frame gate already covers 100% of current CC dialogs; the hedge fixes no
  live bug.
- It touches the hospital-line delivery gate, 6 days before the baby.
- A frameless dialog (the only thing the hedge guards against) would also break the
  approval-**card** gate (`looksLikePrompt` still needs `❯ N.`) — so it would be
  *noticed*, not silent — and route-health L1 surfaces held mail regardless.

## Residual / follow-ups (post-baby)

- **Finding 2:** `remote.go` `remoteTransport.Ready` has no `paneSessionID`
  process check (the tmux gate has one). A misbehaving remote client attesting
  `ready:true` with a foreign TUI could deadlock held mail (safe direction). Fold
  into route-health.
- **Finding 3:** the 18-line tail truncation could drop a `❯ N.` frame scrolled
  far up on a tall footer-less picker (not reachable in current CC).
- **Card gate:** a frameless modal caught by `dialogFooterRe` holds mail but does
  not raise a push card (`looksLikePrompt` unchanged). Align post-baby if wanted.
- Pairs naturally with route-health **L2 self-heal** (auto-`esc` + flush after a
  generous timeout, audited) — the higher-value robustness layer.
