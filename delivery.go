package main

// delivery.go — how words reach an agent: send dedup by client_id, the
// provenance frame (and its H9 neutralizer), and flushMailbox — the one path
// every message takes, guarded so nothing types into a bare shell or an open
// permission dialog. The physical typing itself goes through the contact's
// Transport (transport.go); tmux is simply the first one.

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode"
)

// errSessionAdapter is returned when a contact's transport is unknown or not
// loaded (nullTransport) — mail waits durably rather than guessing.
var errSessionAdapter = errors.New("session adapter not loaded")

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
// stripping control bytes (stripControl, L4). A room message carries " in #crew"
// inside the head — daemon-authored from MailMessage.Room, never from body text
// — so a party-line delivery reads `[From marvin (bridge) in #crew]: text`.
func formatInbound(from, via, room, text string) string {
	in := ""
	if room != "" {
		in = " in " + room
	}
	return fmt.Sprintf("[From %s (%s)%s]: %s", from, via, in, strings.TrimSpace(stripControl(text)))
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

// Rune bounds for the body-derived snippets a quote or reaction carries inline
// to a pane: a reply-quote's author and excerpt, and the echo of a reacted line.
const (
	quoteMaxNameRunes    = 40
	quoteMaxExcerptRunes = 80
	reactExcerptRunes    = 60
	reactionMaxRunes     = 12 // a reaction emoji — even a family ZWJ sequence — is well under this
)

// sanitizeExcerpt makes body-derived text safe to ride inline into a terminal
// and bounded for storage. It strips control bytes to a single line
// (stripControl) and defangs the daemon's framing alphabet (neutralizeFrame), so
// a quoted excerpt can't forge a "[From …]" head or a "⏎" batch boundary (H9),
// then truncates to maxRunes (never mid-character). stripControl runs first, so a
// control-obfuscated frame (a NUL buried inside "[From ") can't slip past
// neutralizeFrame and re-form once the pane drops the NUL.
func sanitizeExcerpt(s string, maxRunes int) string {
	s = strings.TrimSpace(neutralizeFrame(stripControl(s)))
	if r := []rune(s); len(r) > maxRunes {
		s = strings.TrimSpace(string(r[:maxRunes]))
	}
	return s
}

// decorateQuote prefixes a quoted reply the way it rides inline in the mailbox —
// `(re <name>: "<excerpt>") <text>` — so the agent sees what is being replied to
// without any mailbox/coalescer schema change. name and excerpt are already
// sanitized+clamped by the caller (sanitizeExcerpt); a quote that clamped to
// nothing leaves the text undecorated.
func decorateQuote(name, excerpt, text string) string {
	if name == "" && excerpt == "" {
		return text
	}
	return fmt.Sprintf(`(re %s: "%s") %s`, name, excerpt, text)
}

// reactionDelivery is the line an emoji reaction rides into the agent's terminal
// — `reacted <emoji> to "<excerpt>"` — where excerpt is a sanitized, bounded cut
// of the agent's own reacted message (agent-authored text going back into a
// pane, so stripControl + neutralizeFrame via sanitizeExcerpt, H9).
func reactionDelivery(emoji, targetText string) string {
	// BOTH body-derived fields are sanitized before they ride into the agent's
	// terminal: the emoji (handleReact already validated it via reactionSafe;
	// re-sanitize here as defense-in-depth) and the excerpt. Neither can carry a
	// control byte or forge the daemon's framing alphabet (H9).
	return fmt.Sprintf(`reacted %s to "%s"`,
		sanitizeExcerpt(emoji, reactionMaxRunes),
		sanitizeExcerpt(targetText, reactExcerptRunes))
}

// reactionSafe sanitizes a phone-supplied reaction emoji and reports whether it
// is acceptable. The phone may now send ANY emoji (the old fixed 6-emoji
// whitelist is gone), so THIS is the guard that keeps an arbitrary field out of
// an agent's terminal (reactionDelivery types it via send-keys): it strips
// control bytes and defangs the framing alphabet exactly like every other
// body-derived snippet (sanitizeExcerpt), bounds the length, then requires the
// result be emoji-ish — non-empty with at least one non-ASCII rune — so plain
// text or ASCII control-art can't ride in as a "reaction". Never trust the phone.
func reactionSafe(s string) (string, bool) {
	s = sanitizeExcerpt(s, reactionMaxRunes)
	if s == "" {
		return "", false
	}
	for _, r := range s {
		if r > unicode.MaxASCII {
			return s, true
		}
	}
	return "", false
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
		if !transportFor(c).Ready(c) {
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
			// The frame is authored once from group[0]; Room is part of the group
			// key (PeekMailboxGroup), so every message in this run shares it.
			text = formatInbound(group[0].From, group[0].Via, group[0].Room, text)
		}
		if err := transportFor(c).Deliver(c, text); err != nil {
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
