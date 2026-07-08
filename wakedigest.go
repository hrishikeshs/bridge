package main

// wakedigest.go — the "since you woke" welcome-back digest: when a contact wakes
// after being genuinely away, the daemon leads its held backlog with ONE
// daemon-authored line summarizing what elapsed while it was dark — giving the
// waking agent a sense of time (docs/route-health.md calls this the first limb of
// the route-health family, the "artifacts of the dark" it delivers).
//
// The wake DETECTION lives in registry.go (noteSeenAndWakeLocked, run atomically
// as a contact transitions to live); this file owns the COMPOSITION and the
// DELIVERY seam. Two invariants hold the whole feature together:
//
//   - Daemon-authored only. Every value in the line is daemon-computed — a
//     humanized duration and two integer counts — interpolated into a
//     compile-time template. NO phone/client text is ever placed here, so there
//     is zero injection surface, exactly like /compact and the approve keys.
//   - It rides the EXISTING guarded path. The line is prepended to the durable
//     mailbox (QueueFront) and delivered by the ordinary flushMailbox: same
//     never-type-into-an-open-dialog guard, same neutralizeFrame framing, same
//     durable ack discipline. There is no second, unguarded typing path.

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// wakeDigestMinAwaySeconds is the away threshold in whole seconds for the
// since-you-woke digest: config wake_digest_min_away_s (default 300 = 5m), 0 =
// feature OFF (no digest is ever composed), negatives floored to 0. Mirrors
// remoteTTL's clamp+default shape. BRIDGE_WAKE_DIGEST_MIN_AWAY_S overrides it so a
// smoke test can set a tiny threshold and observe a wake fast (the same env-knob
// pattern as BRIDGE_COALESCE_MS / BRIDGE_FANOUT_STAGGER_MS).
func wakeDigestMinAwaySeconds() int64 {
	s := 300
	if authConfig.WakeDigestMinAwayS != nil {
		s = *authConfig.WakeDigestMinAwayS
	}
	if v := os.Getenv("BRIDGE_WAKE_DIGEST_MIN_AWAY_S"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			s = n
		}
	}
	if s < 0 {
		s = 0
	}
	return int64(s)
}

// humanizeAway renders an away duration for the wake digest: the compact humanDur
// form ("45s", "12m", "2h13m") for anything under a day, extended with a day unit
// above it ("3d", "3d4h") so a long absence reads naturally. Kept separate from
// humanDur (resilience.go) so the existing wake-gap / frozen-prompt call sites
// stay byte-for-byte unchanged — additive only.
func humanizeAway(seconds int64) string {
	if seconds < 0 {
		seconds = 0
	}
	if d := time.Duration(seconds) * time.Second; d < 24*time.Hour {
		return humanDur(d)
	}
	days := seconds / 86400
	hours := (seconds % 86400) / 3600
	if hours == 0 {
		return fmt.Sprintf("%dd", days)
	}
	return fmt.Sprintf("%dd%dh", days, hours)
}

// wakeDigestLine composes the one daemon-authored welcome-back line a waking
// contact reads before its held backlog. EVERY value is daemon-derived — the
// humanized away duration and the two integer counts — interpolated into a
// compile-time template; NO phone/client text is ever placed here (H9 / the
// /compact-style fixed vocabulary). The ⏱ prefix is its daemon voice; "held
// above" names the backlog this line leads into. When the gap was quiet it says
// so gracefully (the clock itself is the value, so it is still sent).
func wakeDigestLine(awaySeconds int64, crew, direct int) string {
	dur := humanizeAway(awaySeconds)
	if crew <= 0 && direct <= 0 {
		return fmt.Sprintf("⏱ back after %s — all quiet while you were gone.", dur)
	}
	parts := make([]string, 0, 2)
	if crew > 0 {
		parts = append(parts, fmt.Sprintf("%d in %s", crew, roomCrewName))
	}
	if direct > 0 {
		parts = append(parts, fmt.Sprintf("%d direct", direct))
	}
	return fmt.Sprintf("⏱ back after %s — while you were away: %s (held above).",
		dur, strings.Join(parts, ", "))
}

// prependWakeDigest leads a waking contact's held backlog with ONE daemon-authored
// since-you-woke line, when Connect/ConnectRemote flagged this connect as a
// genuine wake (c.wakeAwaySeconds > 0; the detector is noteSeenAndWakeLocked in
// registry.go). It reads the counts straight from the durable mailbox — the
// faithful "held above" backlog, split by the daemon-set Room field into #crew vs
// direct — composes the line from a compile-time template, and QueueFronts it so
// it sorts ahead of the real backlog without disturbing that backlog's order.
// Delivery is then the ordinary guarded flush: the caller invokes flushMailbox
// immediately after, so the digest inherits every safety rule above the transport
// line (the pane-ready guard, neutralizeFrame, the durable ack discipline).
//
// Called at each wake site right before flushMailbox. A no-op when this connect
// was not a wake (wakeAwaySeconds <= 0), which is the common case — so the three
// call sites pay nothing on an ordinary connect.
func prependWakeDigest(c *Contact) {
	if c.wakeAwaySeconds <= 0 {
		return
	}
	crew, direct := registry.MailboxBacklogCounts(c.ID)
	line := wakeDigestLine(c.wakeAwaySeconds, crew, direct)
	// From is empty ON PURPOSE. flushMailbox delivers a From-less group as its raw
	// (neutralized) text with NO "[From …]" head — so the line carries no forgeable
	// provenance frame at all (the strongest answer to the no-injection invariant),
	// and it is its own group by construction: no real mailbox message is ever
	// From-less, so the digest can never merge into the backlog it leads. Emitted:true
	// so the flush raises no phone bubble — this is the agent's terminal lead-in, not
	// a thread message. neutralizeFrame still runs over it in flushMailbox as
	// defense-in-depth, though a compile-time template can carry no framing alphabet.
	registry.QueueFront(c.ID, MailMessage{Text: line, TS: nowUTC(), Emitted: true})
}
