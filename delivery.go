package main

// delivery.go — how words reach a pane: the session-adapter seams tmux.go
// fills in, send dedup by client_id, the provenance frame (and its H9
// neutralizer), and flushMailbox — the one path every message takes, guarded
// so nothing types into a bare shell or an open permission dialog.

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode"
)

// errSessionAdapter is returned by the session-adapter stubs until tmux.go
// wires the real tmux implementation.
var errSessionAdapter = errors.New("session adapter not loaded")

// deliverToSession injects a message into a live contact's managed tmux session
// (send-keys, literal, followed by Enter). tmux.go assigns the real
// implementation at startup; until then the stub reports no adapter is loaded.
var deliverToSession = func(c *Contact, text string) error { return errSessionAdapter }

// capturePrompt returns a single tmux capture-pane snapshot of a contact's
// terminal, used to render a permission card. tmux.go assigns the real
// implementation; the stub returns the empty string.
var capturePrompt = func(c *Contact) string { return "" }

// sendKey delivers one whitelisted approval keystroke to a live contact:
// "1"/"2"/"3"/"y"/"n" answered with Enter, or "esc" as a bare Escape. It is
// separate from deliverToSession because approvals carry no sender prefix and
// "esc" takes no Enter. tmux.go assigns the real implementation; the stub
// reports no adapter is loaded.
var sendKey = func(c *Contact, key string) error { return errSessionAdapter }

// approveKeys is the closed set of keystrokes the approve endpoint will deliver.
var approveKeys = map[string]bool{"1": true, "2": true, "3": true, "y": true, "n": true, "esc": true}

var (
	seenMu   sync.Mutex
	seenIDs  []string
	inFlight = map[string]bool{}
)

// claimClientID reserves a client_id for delivery under a single lock, closing
// the TOCTOU window the H1 dedup split reintroduced (#3). It returns true only
// when id is neither already committed (a completed send) nor currently
// in-flight, and in that case marks it in-flight: the caller then owns the claim
// and MUST resolve it once via releaseClientID. A false return is a real
// duplicate — the first attempt already succeeded, or a racing retry's original
// delivery is still running — so the caller acks it without delivering, and two
// racing retries can never both send-keys. A blank id can't be deduped, so it is
// always claimed and never tracked (its release is a no-op).
func claimClientID(id string) bool {
	if id == "" {
		return true
	}
	seenMu.Lock()
	defer seenMu.Unlock()
	for _, s := range seenIDs {
		if s == id {
			return false
		}
	}
	if inFlight[id] {
		return false
	}
	inFlight[id] = true
	return true
}

// releaseClientID resolves a claim taken by claimClientID. It clears the
// in-flight mark either way; when ok it also records id as durably handled
// (delivered to a live session or queued to a mailbox) so a later retry is
// acknowledged without redelivery. A failed attempt (ok=false) leaves no trace,
// so its retry runs for real instead of being swallowed as a "duplicate" —
// preserving H1's guarantee that an id is committed only after a durable accept.
func releaseClientID(id string, ok bool) {
	if id == "" {
		return
	}
	seenMu.Lock()
	defer seenMu.Unlock()
	delete(inFlight, id)
	if !ok {
		return
	}
	for _, s := range seenIDs {
		if s == id {
			return // already recorded
		}
	}
	seenIDs = append([]string{id}, seenIDs...)
	if len(seenIDs) > clientIDRing {
		seenIDs = seenIDs[:clientIDRing]
	}
}

// stripControl flattens text to a single terminal-safe line: newline/CR/tab
// become one space, and every other non-printable rune (ESC, arrow/cursor
// sequences, other C0/C1 controls, format characters) is dropped so untrusted
// text can't drive a TUI when it reaches a pane — or an agent's stdout (L4).
// Printable characters, including non-ASCII graphics, pass through. Shared by
// formatInbound (peer/phone mail) and runSend (the human's my-status echoed
// into an agent's transcript).
func stripControl(text string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t':
			return ' '
		}
		if !unicode.IsPrint(r) {
			return -1 // drop
		}
		return r
	}, text)
}

// formatInbound builds the prefix a delivered message wears in the agent's
// terminal, collapsing whitespace so it lands as a single send-keys line and
// stripping control bytes (stripControl, L4).
func formatInbound(from, via, text string) string {
	return fmt.Sprintf("[From %s (%s)]: %s", from, via, strings.TrimSpace(stripControl(text)))
}

// neutralizeFrame rewrites the daemon's framing alphabet out of a message
// body: "[From " heads become "['From " and the "⏎" batch separator becomes
// "↵". After this, the one line an agent receives can only carry
// daemon-authored provenance and message boundaries — a body cannot forge a
// second sender or splice a fake message into a coalesced batch (H9). The
// rewrite is visible but gentle: quoted frames stay readable, they just stop
// matching the daemon's exact format.
func neutralizeFrame(s string) string {
	s = strings.ReplaceAll(s, "[From ", "['From ")
	return strings.ReplaceAll(s, "⏎", "↵")
}

// flushMailbox delivers a contact's queued messages in order, combining each
// run of consecutive messages from one sender+channel into a single delivery
// line ("msg1 ⏎ msg2 ⏎ msg3") — a burst of phone texts lands as one thought,
// not three turn-starting fragments (the coalescing UX, coalesce.go). Groups
// are removed from disk only after their delivery succeeds, so a crash
// mid-drain redelivers at most one group instead of losing the batch (M6,
// group-granular). Concurrent flushes of one mailbox coalesce to one.
func flushMailbox(c *Contact) {
	if !registry.BeginFlush(c.ID) {
		return
	}
	defer registry.EndFlush(c.ID)
	for {
		group := registry.PeekMailboxGroup(c.ID)
		if len(group) == 0 {
			return
		}
		// THE guard — the one the four criticals converge on (review
		// 2026-07-06). Never send-keys into a pane that is the pre-Claude bare
		// shell or is showing a permission dialog (a trailing Enter would
		// blind-select its highlighted option and the mail would be swallowed).
		// Leave everything queued and re-arm; the reconcile loop retries every
		// tick and coalesceRearm covers the fast path — the mailbox is durable,
		// so a deferral is never a loss.
		if !paneReadyForDelivery(c) {
			coalesceRearm(c.ID)
			return
		}
		parts := make([]string, 0, len(group))
		for _, m := range group {
			// Neutralize per part, before the join: only the daemon may write
			// a "[From …]" head or a "⏎" boundary into the delivered line (H9).
			parts = append(parts, neutralizeFrame(m.Text))
		}
		text := strings.Join(parts, " ⏎ ")
		if group[0].From != "" {
			text = formatInbound(group[0].From, group[0].Via, text)
		}
		if err := deliverToSession(c, text); err != nil {
			return // leave the group (and the rest) queued for the next flush
		}
		for _, m := range group {
			if m.Emitted {
				continue
			}
			// Offline-queued mail emits on delivery: peer messages wear their
			// author (see handleLocalSend), phone messages remain "sent".
			if m.Via == "bridge" {
				Emit("peer", c.ID, m.From, m.Text)
			} else {
				Emit("sent", c.ID, c.Name, m.Text)
			}
		}
		registry.DropMailbox(c.ID, group)
	}
}
