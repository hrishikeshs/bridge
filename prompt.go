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
	// off the pane. A real dialog has the ❯ selector plus a line-anchored
	// numbered option in its final lines.
	lines := strings.Split(strings.TrimRight(pane, "\n"), "\n")
	if len(lines) > 18 {
		lines = lines[len(lines)-18:]
	}
	tail := strings.Join(lines, "\n")
	low := strings.ToLower(tail)
	hasSelector := strings.Contains(tail, "❯")
	hasOption := promptOptionRe.MatchString(tail)
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
	return hasSelector && hasOption && hasProceed
}

// paneShowsDialog is the conservative DELIVERY gate: does the pane's last
// screenful show the FRAME of an interactive dialog — the ❯ selector plus a
// line-anchored numbered option — regardless of the specific proceed
// vocabulary? looksLikePrompt (which raises the attention card) additionally
// demands proceed vocabulary to avoid false cards from agent prose; delivery
// refuses on the frame alone, because a trailing Enter would select whatever
// option is highlighted — trust / text-input / plan dialogs included, even the
// shapes looksLikePrompt is deliberately too strict to name.
func paneShowsDialog(pane string) bool {
	if pane == "" {
		return false
	}
	lines := strings.Split(strings.TrimRight(pane, "\n"), "\n")
	if len(lines) > 18 {
		lines = lines[len(lines)-18:]
	}
	tail := strings.Join(lines, "\n")
	return strings.Contains(tail, "❯") && promptOptionRe.MatchString(tail)
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

// promptOptionRe matches a dialog option at the start of a line ("❯ 1. Yes",
// "  2. No…") — prose mentions of "1." mid-sentence do not anchor.
var promptOptionRe = regexp.MustCompile(`(?m)^\s*(?:❯\s*)?[123]\.\s`)

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
