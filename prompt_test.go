package main

// prompt_test.go — the pane reader. looksLikePrompt (raises the attention
// card) and paneShowsDialog (the stricter delivery gate) are the guards the
// 2026-07-06 criticals converged on: a false negative types into an open
// dialog and blind-selects its highlighted option. They parse only text, so
// they test directly.

import "testing"

// A real Claude Code permission dialog, as capture-pane renders it: the
// option list is line-anchored (no box border ahead of the ❯/number) and the ❯
// selection cursor sits on the option line — exactly what dialogFrameRe requires.
const realPromptPane = `
⏺ Bash(git push origin master)
  ⎿  Running…

Do you want to proceed?
 ❯ 1. Yes
   2. Yes, and don't ask again this session
   3. No, and tell Claude what to do differently

   esc to cancel`

// The folder-trust dialog — a different frame the strict vocabulary set
// missed until review H7 broadened it.
const trustPane = `
Do you trust the files in this folder?
 ❯ 1. Yes, proceed
   2. No, exit
   Enter to confirm · Esc to exit`

func TestLooksLikePrompt(t *testing.T) {
	yes := map[string]string{
		"real permission dialog": realPromptPane,
		"folder-trust dialog":    trustPane,
	}
	for name, pane := range yes {
		if !looksLikePrompt(pane) {
			t.Errorf("%s: looksLikePrompt = false, want true", name)
		}
	}

	no := map[string]string{
		"empty pane": "",
		// The exact false-positive class the frame test exists to reject:
		// an agent writing PROSE about a prompt. Vocabulary is present, the
		// dialog frame (❯ + line-anchored option + proceed) is not.
		"agent prose about a prompt": "I'll show a menu with 1. Yes and 2. No, " +
			"then you press esc to cancel if you don't want to proceed.",
		"selector but no option": "❯ type your message here (do you want to continue?)",
		"option but no selector": "Here are steps:\n1. Yes we start\n2. No we stop",
		"plain reply":            "Done — pushed to master. Anything else?",
	}
	for name, pane := range no {
		if looksLikePrompt(pane) {
			t.Errorf("%s: looksLikePrompt = true, want false", name)
		}
	}
}

func TestPaneShowsDialogIsStricterGate(t *testing.T) {
	// Delivery refuses on the FRAME alone (❯ + option), without demanding the
	// proceed vocabulary looksLikePrompt wants — a text-input or plan dialog
	// with a highlighted option would still eat a blind Enter.
	framedNoVocab := `
Choose a plan
 ❯ 1. Conservative
   2. Aggressive`
	if !paneShowsDialog(framedNoVocab) {
		t.Error("paneShowsDialog = false on a framed dialog lacking proceed vocab; delivery would type into it")
	}
	if looksLikePrompt(framedNoVocab) {
		t.Error("looksLikePrompt should stay strict (no proceed vocab) — that's why delivery has its own gate")
	}
	// Anything the card gate accepts, the delivery gate must also stop.
	for _, pane := range []string{realPromptPane, trustPane} {
		if looksLikePrompt(pane) && !paneShowsDialog(pane) {
			t.Error("a pane raises the card but delivery would not refuse it — the guard has a hole")
		}
	}
}

// quickWolfIdlePane reproduces quick-wolf's real buffer on 2026-07-08: an agent
// idle at the bare ❯ input caret, with a numbered list earlier in its scrollback
// — her OWN debug prose, not a dialog. The old gate saw "❯ somewhere" AND "a N.
// line somewhere" and wrongly held 8 messages for hours with health reading ok
// and no card raised. The ❯ sits on its own line; no ❯ is adjacent to a number,
// and there is no proceed vocabulary anywhere.
const quickWolfIdlePane = `  [response]
  It's the fix for your "generator exited abnormally with code 1" — two files:

  1. magnus--headless-command was calling claude --bare --print (exit 1).
  2. Even with that fixed, replies come wrapped in [thinking]/[response] markers.

  Both files byte-compile clean, smoke-tested. Say the word and I'll push. 🐺
  [end-response]

✻ Sautéed for 18s
─────────────────────────────────────────────────────────
❯
─────────────────────────────────────────────────────────
  ⏵⏵ accept edits on (shift+tab to cycle) · ← for agents`

// TestPaneShowsDialogRejectsIdleCaretWithNumberedProse is the 2026-07-08
// quick-wolf regression: an idle ❯ caret plus a numbered list in the scrollback
// is NOT a dialog, and delivery must not be held as if it were one.
func TestPaneShowsDialogRejectsIdleCaretWithNumberedProse(t *testing.T) {
	if paneShowsDialog(quickWolfIdlePane) {
		t.Error("paneShowsDialog = true on an idle ❯ caret + numbered prose; delivery would be held with no dialog present (the quick-wolf false positive)")
	}
	// And it must not raise a card either — no proceed vocabulary.
	if looksLikePrompt(quickWolfIdlePane) {
		t.Error("looksLikePrompt = true on idle prose lacking proceed vocabulary")
	}
}

// TestPaneShowsDialogCatchesSelectionPastThree guards the \d+ (not [123]) choice:
// a real dialog whose cursor is on option 4+ (unselected options carry no ❯) must
// still block delivery. A narrower [123] would MISS this and type into the dialog.
func TestPaneShowsDialogCatchesSelectionPastThree(t *testing.T) {
	pane := `Pick a target
   1. staging
   2. canary
   3. one box
 ❯ 4. production`
	if !paneShowsDialog(pane) {
		t.Error("paneShowsDialog = false with the cursor on option 4; delivery would type into an open dialog")
	}
}

// TestGatesRejectIdlePlanWithProceedVocab closes the vocab-BEARING half of the
// quick-wolf class (2026-07-08 review, lens 3). An idle agent whose screenful is a
// numbered PLAN ending in "Would you like me to proceed?" — with the bare ❯ input
// caret always on screen — is NOT a dialog: the ❯ is not on a numbered option. The
// first fix caught only the vocab-LESS variant (quick-wolf's); this asserts BOTH
// gates now reject the vocab-bearing one too, so the agent's mail is delivered
// (not held) and no false attention card is raised.
func TestGatesRejectIdlePlanWithProceedVocab(t *testing.T) {
	pane := `Here's my plan:
  1. Refactor the parser
  2. Add tests
  3. Update docs

Would you like me to proceed?
──────────────────────────────
❯
──────────────────────────────
  ⏵⏵ accept edits on (shift+tab to cycle)`
	if paneShowsDialog(pane) {
		t.Error("paneShowsDialog = true on an idle numbered plan + 'proceed?' prose; the agent's mail would be wrongly held")
	}
	if looksLikePrompt(pane) {
		t.Error("looksLikePrompt = true on an idle plan; a false attention card would be raised")
	}
}

// framelessModalPane is a hypothetical FUTURE dialog shape whose frame is NOT a
// numbered ❯ option — the version-fragility hedge (2026-07-09). The ❯N. gate
// misses it, but its modal "Esc to cancel" footer (with no REPL footer present)
// is caught by the dialogFooterRe fallback, so delivery still refuses.
const framelessModalPane = `
 Approve this action?
   Approve
   Deny

 Esc to cancel`

// framelessTrustPane exercises the "Enter to confirm" arm of the footer fallback
// on a frame the ❯N. test would miss.
const framelessTrustPane = `
 Do you trust the files in this directory?
   Yes, proceed
   No, exit

 Enter to confirm`

// proseWithCancelIdlePane is THE deadlock guard (2026-07-09). An idle agent whose
// message text mentions "esc to cancel" — but whose bottom rows are the input
// caret block and REPL footer — must NOT be read as a dialog: the footer fallback
// matches cancel/confirm vocab only on the pane's last few non-blank lines, and
// the prose sits ABOVE the caret block, out of that window. So mail is delivered,
// not held forever. This is the quick-wolf class the bottom-row anchor closes.
const proseWithCancelIdlePane = `  I'll run the migration now. To abort it mid-run, hit esc to cancel at the
  confirm step and nothing gets written.

  Ready when you are.
────────────────────────────────────────────────────────────
❯
────────────────────────────────────────────────────────────
  ? for shortcuts · ← for agents`

// framelessModalReplWordInBody and framelessModalPlanModeBody are the adversarial
// review's Finding 1 (2026-07-09): a frameless modal whose BODY text happens to
// read like a REPL-footer phrase must still hold. An earlier design vetoed the
// hold when "accept edits on" / "plan mode on" appeared anywhere → blind-select.
// The bottom-row anchor has no veto: the modal's own "Enter to confirm" / "Esc to
// cancel" footer is on the last line and triggers the hold regardless. Both hold.
const framelessModalReplWordInBody = ` Proceed with this command?
   git commit -m 'accept edits on main'

 Enter to confirm`

const framelessModalPlanModeBody = ` Turn plan mode on for this session?
   Yes
   No

 Esc to cancel`

// TestPaneShowsDialogFooterFallback: a modal whose frame is not a numbered option
// still blocks delivery via its confirm/cancel footer, so a CC dialog-UI change
// cannot silently reopen the blind-select hole.
func TestPaneShowsDialogFooterFallback(t *testing.T) {
	for name, pane := range map[string]string{
		"frameless approve/deny modal":   framelessModalPane,
		"frameless trust modal":          framelessTrustPane,
		"repl word in modal body":        framelessModalReplWordInBody,
		"plan-mode phrase in modal body": framelessModalPlanModeBody,
	} {
		if dialogFrameRe.MatchString(pane) {
			t.Fatalf("%s: fixture accidentally has a ❯N. frame; it must exercise the FOOTER path", name)
		}
		if !paneShowsDialog(pane) {
			t.Errorf("%s: paneShowsDialog = false; a frameless modal would take a blind delivery", name)
		}
	}
}

// TestPaneShowsDialogIgnoresCancelVocabAboveFooter is the deadlock guard: footer
// vocab in agent prose (above the input caret block) must not hold mail on an idle
// screen. The bottom-row anchor closes this — the vocab is outside the last-few-
// non-blank-lines window the fallback reads.
func TestPaneShowsDialogIgnoresCancelVocabAboveFooter(t *testing.T) {
	if !dialogFooterRe.MatchString(proseWithCancelIdlePane) {
		t.Fatal("fixture should carry footer vocab up-tail, so the bottom-row anchor is what makes the difference")
	}
	if paneShowsDialog(proseWithCancelIdlePane) {
		t.Error("paneShowsDialog = true on idle prose mentioning 'esc to cancel'; delivery would be held forever (the deadlock the anchor prevents)")
	}
	if looksLikePrompt(proseWithCancelIdlePane) {
		t.Error("looksLikePrompt = true on idle prose; a false attention card would be raised")
	}
	// Every idle/working footer must leave an idle screen deliverable — INCLUDING a
	// wrap-split one (Finding D: the daemon captures without -J, so a narrow pane
	// records the footer as separate physical rows, shredding any single token).
	// The anchor doesn't care: none of these bottom rows carry cancel/confirm vocab.
	for name, footer := range map[string]string{
		"accept-edits": "  ⏵⏵ accept edits on (shift+tab to cycle) · ← for agents",
		"plan-mode":    "  ⏸ plan mode on (shift+tab to cycle) · ← for agents",
		"working":      "  esc to interrupt · ← for agents",
		"shortcuts":    "  ? for shortcuts · ← for agents",
		"wrap-split":   "  ⏵⏵ accept edits on (sh\nift+tab to cycle) · ← fo\nr agents",
	} {
		pane := "  earlier I noted you can press esc to cancel the job.\n────\n❯\n────\n" + footer
		if paneShowsDialog(pane) {
			t.Errorf("%s footer: delivery held on an idle screen (false-positive deadlock)", name)
		}
	}
}

// statuslineBelowFooterPane, autoCompactHintBelowFooterPane, and
// bgTaskBelowFooterPane are adversarial-review round 2 (2026-07-09, Findings A–C):
// Claude Code renders a custom /statusline, the "Context left until auto-compact"
// hint, or a background-task row BELOW its "← for agents" footer. An idle agent
// there — even with "esc to cancel" in its scrollback prose — must still be
// delivered to. The bottom-row anchor keeps these deliverable: the prose vocab is
// far up-tail, and the actual bottom rows (footer + the extra row) carry no
// cancel/confirm vocab. A discarded bottom-LINE veto read only the extra row,
// missed the footer, and held the mail forever.
const statuslineBelowFooterPane = `  Sure — I'll drop the staging table. At the confirm prompt press esc to cancel.
────────────────────────────────────────────────────────────
❯
────────────────────────────────────────────────────────────
  ? for shortcuts · ← for agents
  ⎇ main ✚2 · claude-opus-4-8 · 34% context`

const autoCompactHintBelowFooterPane = `  To abort the migration, hit esc to cancel at the confirmation step.
────────────────────────────────────────────────────────────
❯
────────────────────────────────────────────────────────────
  ⏵⏵ accept edits on (shift+tab to cycle) · ← for agents
  Context left until auto-compact: 8%`

const bgTaskBelowFooterPane = `  You can press esc to cancel that job once it starts running.
────────────────────────────────────────────────────────────
❯
────────────────────────────────────────────────────────────
  ? for shortcuts · ← for agents
  ⏵ 1 background task · ctrl+b to manage`

// TestPaneShowsDialogDeliversWithRowsBelowFooter locks Findings A–C: an idle screen
// with an extra CC-drawn row beneath its footer must stay deliverable — the anchor
// reads the bottom rows (footer + extra row), neither of which carries cancel vocab.
func TestPaneShowsDialogDeliversWithRowsBelowFooter(t *testing.T) {
	for name, pane := range map[string]string{
		"custom /statusline below footer":  statuslineBelowFooterPane,
		"auto-compact hint below footer":   autoCompactHintBelowFooterPane,
		"background-task row below footer": bgTaskBelowFooterPane,
	} {
		if !dialogFooterRe.MatchString(pane) {
			t.Fatalf("%s: fixture must carry footer vocab up-tail so the bottom-row anchor is what decides", name)
		}
		if paneShowsDialog(pane) {
			t.Errorf("%s: paneShowsDialog = true; idle mail would be held forever (the deadlock the bottom-row anchor prevents)", name)
		}
	}
}

func TestFirstPromptLine(t *testing.T) {
	if got := firstPromptLine(realPromptPane); got != "Bash(git push origin master)" {
		t.Errorf("firstPromptLine = %q, want the tool call", got)
	}
	dontAsk := "Some preamble\ndon't ask again for: git push *\n❯ 1. Yes"
	if got := firstPromptLine(dontAsk); got != "git push" {
		t.Errorf("firstPromptLine = %q, want the command family 'git push'", got)
	}
	if got := firstPromptLine("just a plain screen"); got != "wants your approval — tap to review" {
		t.Errorf("firstPromptLine fallback = %q", got)
	}
}

// TestMarkPromptRaiseRefreshNoFlap is the stale-permission-card fix (#15): the
// card's caption is the COMMAND, and MarkPrompt decides atomically whether a
// detected dialog raises a fresh card, re-captions an open one for a new
// command, or (the anti-flap case) changes nothing because it is the same
// command re-detected — the guarantee that an approval never lands on a command
// the human never read, and that a dialog re-attested every ~10s never re-rings.
func TestMarkPromptRaiseRefreshNoFlap(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // MarkPrompt/SetPrompt save() under $HOME/.bridge
	r := &Registry{
		contacts: map[string]*Contact{"c1": {ID: "c1", Name: "wolf", Status: "live", Health: "ok"}},
		mailbox:  map[string][]MailMessage{},
		flushing: map[string]bool{},
	}
	c := r.contacts["c1"]

	// First detection RAISES a fresh card.
	if got := r.MarkPrompt("c1", "Bash(git push)"); got != promptRaised {
		t.Fatalf("first MarkPrompt = %v, want promptRaised", got)
	}
	if !c.PromptOpen || c.PromptSig != "Bash(git push)" || c.PromptSince == 0 {
		t.Fatalf("after raise: open=%v sig=%q since=%d", c.PromptOpen, c.PromptSig, c.PromptSince)
	}

	// Same command re-detected: NO CHANGE — the anti-flap guarantee (no re-ring).
	if got := r.MarkPrompt("c1", "Bash(git push)"); got != promptNoChange {
		t.Fatalf("re-detect same command = %v, want promptNoChange", got)
	}

	// Pin an old open-time, then a DIFFERENT command: REFRESH, and the
	// frozen-prompt clock must survive untouched (still one continuous wait).
	c.PromptSince = 12345
	if got := r.MarkPrompt("c1", "Bash(rm -rf build)"); got != promptRefreshed {
		t.Fatalf("new command = %v, want promptRefreshed", got)
	}
	if c.PromptSig != "Bash(rm -rf build)" {
		t.Fatalf("after refresh sig=%q, want the new command", c.PromptSig)
	}
	if c.PromptSince != 12345 {
		t.Fatalf("refresh restamped PromptSince to %d: the frozen-prompt clock must not reset on a re-caption", c.PromptSince)
	}
	if !c.PromptOpen {
		t.Fatal("refresh dropped PromptOpen: the approve gate must stay enabled across a re-caption")
	}

	// Clearing drops the caption so a later revival can't inherit a stale card.
	r.SetPrompt("c1", false)
	if c.PromptOpen || c.PromptSig != "" {
		t.Fatalf("after clear: open=%v sig=%q, want closed with an empty caption", c.PromptOpen, c.PromptSig)
	}

	// MarkPrompt on an unknown contact is a safe no-op.
	if got := r.MarkPrompt("ghost", "Bash(x)"); got != promptNoChange {
		t.Fatalf("MarkPrompt(unknown) = %v, want promptNoChange", got)
	}
}
