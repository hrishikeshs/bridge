package main

// prompt.go — reading a permission dialog off a captured pane: is one open
// (looksLikePrompt), is the pane safe to type into (paneReadyForDelivery),
// and what one line should the push notification carry (firstPromptLine).

import (
	"regexp"
	"strings"
	"time"
)

// looksLikePrompt reports whether a captured pane is showing a Claude Code
// permission dialog right now — the gate that keeps routine idle/waiting
// notifications from raising a false attention card.
func looksLikePrompt(pane string) bool {
	if pane == "" {
		return false
	}
	// Judge only the pane's last screenful and demand the dialog's actual
	// FRAME, not its vocabulary: agents constantly write prose ABOUT prompts
	// ("1. Yes", "esc to cancel"), and the old substring test minted fake
	// attention cards out of agent chatter — including message text captured
	// off the pane. A real dialog has the ❯ SELECTION CURSOR sitting ON a
	// numbered option (dialogFrameRe) plus proceed vocabulary. Keying on the
	// cursor-on-the-option-line rather than a ❯ anywhere is what stops an idle
	// agent — whose bare ❯ input caret is ALWAYS on screen — from minting a false
	// card the moment it prints a numbered plan and asks "would you like to
	// proceed?" (the vocab-bearing half of the quick-wolf class; see
	// paneShowsDialog for the full story).
	lines := strings.Split(strings.TrimRight(pane, "\n"), "\n")
	if len(lines) > 18 {
		lines = lines[len(lines)-18:]
	}
	tail := strings.Join(lines, "\n")
	low := strings.ToLower(tail)
	hasProceed := strings.Contains(low, "do you want") ||
		strings.Contains(low, "esc to cancel") ||
		strings.Contains(low, "no, and tell") ||
		strings.Contains(low, "yes, and") ||
		// Real CC dialogs the strict set missed (review H7): the folder-trust
		// prompt ("Do you trust the files…" / "Enter to confirm · Esc to exit")
		// and other confirm framings. Broadened so PromptOpen is set — and so
		// the delivery guard refuses — for the dialogs an agent actually hits.
		strings.Contains(low, "do you trust") ||
		strings.Contains(low, "enter to confirm") ||
		strings.Contains(low, "would you like")
	return dialogFrameRe.MatchString(tail) && hasProceed
}

// paneShowsDialog is the conservative DELIVERY gate: does the pane's last
// screenful show the FRAME of an interactive dialog — the ❯ SELECTION CURSOR
// sitting on a numbered option line ("❯ 1. Yes")? Delivery refuses on the frame
// alone, without the proceed vocabulary looksLikePrompt (the card gate) also
// wants, because a trailing Enter would blind-select whatever option is
// highlighted — numbered trust and plan dialogs included. looksLikePrompt is a
// strict SUBSET (it ANDs this same frame with proceed vocabulary), so anything
// that raises an attention card also blocks a blind delivery.
//
// The ❯ glyph is OVERLOADED: Claude Code's IDLE input caret is a bare ❯, while a
// dialog's selection cursor is ❯ immediately before a numbered option. The old
// test (❯ ANYWHERE in the tail AND a numbered line ANYWHERE, checked
// independently) conflated the two — an idle agent with a numbered list anywhere
// in its scrollback tripped the gate and had its mail HELD as if a dialog were
// open, with zero trace (health stayed ok, no card, no audit line). That silently
// held quick-wolf's 8 messages on 2026-07-08. Requiring the cursor ON the option
// line disambiguates them and loses no real dialog: across 356 real production
// captures on BOTH transports the selected option always renders " ❯ N." with a
// plain leading space (the emacs/vterm client's buffer-substring-no-properties and
// tmux's capture-pane -p both strip ANSI/borders), and dialogFrameRe uses \d+ (not
// [123]) so a selection past option 3 is still caught.
//
// 2026-07-09 UPDATE (Vint, ground-truth probes in throwaway claude+tmux
// sessions): CC v2.1.197 collapsed the old 3-option Bash dialog into "❯ 1. Yes /
// 2. No / Tab to amend". The amend free-text renders INLINE into option 1
// ("❯ 1. Yes, <typed text>"), so every interactive Bash-dialog state STILL
// carries the ❯N. frame and is caught above; the old "option 3 → bare-❯ text
// box" (#27) is no longer reachable there. What remains is VERSION FRAGILITY —
// the frame glyph changed once and could change again under us. The dialogFooterRe
// fallback hedges it: a modal confirm/cancel FOOTER ("esc to cancel/exit", "enter
// to confirm") on the pane's BOTTOM ROWS (lastNonBlankLines) counts as a dialog
// too. Anchoring to the bottom rows — where a real modal's footer sits — is what
// keeps this off the idle caret without a separate REPL-footer veto: a genuine
// REPL footer carries none of this vocab, and agent prose that merely mentions it
// sits above the input caret block, out of the window. (Three adversarial passes,
// 2026-07-09, each broke a veto-based design — whole-tail, bottom-line, and
// wrap-split; the bottom-row anchor drops the veto entirely, docs/dialog-gate-
// 2026-07-09.md.) Monotonic: the ❯N. gate is untouched; this only ever ADDS a
// hold, for a shape CC does not render today. Route-health L1 ("stalled") and a
// future L2 auto-esc self-heal are the backstops for whatever both layers miss.
//
// SHIP STATUS: staged on a branch, NOT deployed to prod (Hrishi's call, 6 days
// pre-baby: the frame gate already covers 100% of current CC, so this hedge is not
// worth prod risk near release). Finish/deploy post-baby.
func paneShowsDialog(pane string) bool {
	if pane == "" {
		return false
	}
	lines := strings.Split(strings.TrimRight(pane, "\n"), "\n")
	if len(lines) > 18 {
		lines = lines[len(lines)-18:]
	}
	tail := strings.Join(lines, "\n")
	// The proven numbered-option frame — every current CC dialog renders it.
	if dialogFrameRe.MatchString(tail) {
		return true
	}
	// Version-robust fallback for a hypothetical FRAMELESS future dialog: a modal's
	// confirm/cancel footer on the pane's bottom rows. Anchoring to the last few
	// non-blank lines — where a real modal's footer sits — is the whole trick, and
	// it replaces the REPL-footer veto that three adversarial passes (2026-07-09)
	// each broke (a whole-tail veto let a modal's own body text suppress it; a
	// bottom-line veto and a wrap-split footer both deadlocked idle mail). No veto
	// is needed: a genuine REPL footer ("? for shortcuts", "← for agents", "shift+
	// tab to cycle", …) carries NO cancel/confirm vocab, so an idle screen never
	// trips this — even wrapped, even with a /statusline or auto-compact hint drawn
	// below it. And ordinary agent prose that merely mentions "esc to cancel" sits
	// ABOVE the input caret block, out of this bottom window. Monotonic: the ❯N.
	// gate is untouched and this only ever ADDS a hold, for a shape CC does not
	// render today. (Deliberately conservative near release; see docs.)
	if dialogFooterRe.MatchString(lastNonBlankLines(lines, 3)) {
		return true
	}
	return false
}

// lastNonBlankLines joins the final n lines that carry non-whitespace content
// (skipping blank/padding rows). The delivery gate reads a modal's confirm/cancel
// FOOTER here: it sits on the pane's bottom rows, whereas agent prose that happens
// to mention those words sits above the input caret block, outside this window.
func lastNonBlankLines(lines []string, n int) string {
	out := make([]string, 0, n)
	for i := len(lines) - 1; i >= 0 && len(out) < n; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			out = append(out, lines[i])
		}
	}
	return strings.Join(out, "\n")
}

// paneReadyForDelivery reports whether it is safe to send-keys into c's pane
// right now. Two conditions, both learned from the 2026-07-06 review's four
// critical findings: (1) the pane must be running Claude Code, not the bare
// shell a fresh `bridge connect` briefly leaves before launchClaude fires —
// paneSessionID is "" for a shell (no `--resume` in its args); (2) the pane
// must not be showing a dialog, whose highlighted option a trailing Enter would
// blind-select. FAIL-SAFE: an unreadable pane (capture/ps hiccup) returns
// false, so the durable mail waits for the next reconcile tick rather than
// risking a blind delivery — waiting never loses anything.
func paneReadyForDelivery(c *Contact) bool {
	if paneSessionID(c) == "" {
		return false
	}
	return !paneShowsDialog(tmuxCapturePane(c))
}

// dialogFrameRe matches a dialog's SELECTED option — the ❯ cursor on the SAME
// line as its numbered option ("❯ 1. Yes", " ❯ 2. No"). The ❯ is REQUIRED and
// adjacent to the number, so a bare idle ❯ input caret paired with a distant
// numbered list (agent prose) no longer reads as a dialog. Both the delivery gate
// (paneShowsDialog) and the card gate (looksLikePrompt, which additionally
// requires proceed vocabulary) key off this one frame test; \d+ (not [123]) so a
// selection on option 4+ is still caught.
var dialogFrameRe = regexp.MustCompile(`(?m)^\s*❯\s*\d+\.\s`)

// dialogFooterRe matches the confirm/cancel FOOTER a modal permission dialog
// prints in place of the REPL input line — "Esc to cancel", "Esc to exit",
// "Enter to confirm". It is the version-robust half of the delivery gate: it can
// recognize a dialog whose frame is NOT a numbered ❯ option (a future/other CC
// shape). paneShowsDialog matches it only against the pane's bottom rows
// (lastNonBlankLines), where a real modal's footer sits — so a genuine REPL
// footer (which carries none of this vocab) and agent prose above the input caret
// both fall outside it. It deliberately does NOT list "esc to interrupt" (the
// WORKING-state footer) — that is a REPL line, not a modal.
var dialogFooterRe = regexp.MustCompile(`(?i)\b(esc|escape) to (cancel|exit)\b|\benter to confirm\b`)

// timeNowUnix returns the current unix time in seconds.
func timeNowUnix() int64 { return time.Now().Unix() }

// firstPromptLine pulls the most informative line out of a permission-dialog
// capture for a push body: the tool call (Bash(...)), the "don't ask again
// for: X" command family, or the command line just above the approval ask.
func firstPromptLine(pane string) string {
	lines := strings.Split(pane, "\n")
	// 1. a tool invocation like Bash(git push) / Write(file.go)
	for _, ln := range lines {
		t := strings.TrimSpace(strings.TrimLeft(ln, "⏺ "))
		if toolCallRe.MatchString(t) {
			return t
		}
	}
	// 2. the "don't ask again for: <cmd>" hint names the command family
	for _, ln := range lines {
		if i := strings.Index(strings.ToLower(ln), "don't ask again for:"); i >= 0 {
			if cmd := strings.TrimSpace(ln[i+len("don't ask again for:"):]); cmd != "" {
				return strings.TrimSuffix(cmd, " *")
			}
		}
	}
	// 3. the first meaningful line above the approval ask is the command
	for i, ln := range lines {
		low := strings.ToLower(ln)
		if strings.Contains(low, "requires approval") || strings.Contains(low, "do you want to proceed") {
			for j := i - 1; j >= 0; j-- {
				if t := strings.TrimSpace(lines[j]); t != "" {
					return t
				}
			}
		}
	}
	return "wants your approval — tap to review"
}

// raiseOrRefreshPrompt is the single entry point for surfacing a permission
// dialog to the phone — the one judge for RAISE and REFRESH (verifyPrompt owns
// CLEAR). It is called wherever a dialog is detected on a contact's screen: the
// tmux hook (local.go), the tmux re-check and revive (reconcile.go), and the
// remote attest (remote.go). firstPromptLine is the card's caption AND its
// identity: MarkPrompt compares it against the caption currently up and decides
// atomically whether this is a new card, the same card re-captioned for a
// different command (the stale-text bug this fixes: an approval must never land
// on a command the human never read), or the same command re-detected — in
// which case nothing fires, so re-attesting an unchanged dialog every ~10s can
// never flap the card or re-ring the phone. The ceremony on a raise or refresh
// is identical, and identical to what a tmux card always ran, so a refreshed
// card is indistinguishable downstream from a freshly raised one.
//
// Caveat by construction: two DIFFERENT dialogs that both fall to
// firstPromptLine's generic fallback share a caption and so won't refresh
// between each other. Real dialogs are tool calls (captured verbatim); the
// fallback is the rare unrecognized shape, and it fails safe toward the
// existing behavior (no worse than today), never toward a wrong caption.
func raiseOrRefreshPrompt(c *Contact, snapshot string) {
	if !looksLikePrompt(snapshot) {
		return
	}
	sig := firstPromptLine(snapshot)
	switch registry.MarkPrompt(c.ID, sig) {
	case promptRaised, promptRefreshed:
		Emit("attention", c.ID, c.Name, snapshot)
		notifyPush(c.Name+" needs you", sig, "attn-"+c.ID, c.ID)
		markAttnPushed(c.ID)
		dispatchPluginEvent("permission.prompt", c, map[string]any{"prompt": sig})
	}
}

var toolCallRe = regexp.MustCompile(`^[A-Z][A-Za-z]*\(.+\)`)
