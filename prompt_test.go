package main

// prompt_test.go — the pane reader. looksLikePrompt (raises the attention
// card) and paneShowsDialog (the stricter delivery gate) are the guards the
// 2026-07-06 criticals converged on: a false negative types into an open
// dialog and blind-selects its highlighted option. They parse only text, so
// they test directly.

import "testing"

// A real Claude Code permission dialog, as capture-pane renders it: the
// option list is line-anchored (no box border ahead of the ❯/number), which
// is exactly what promptOptionRe requires.
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
