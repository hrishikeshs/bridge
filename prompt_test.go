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
