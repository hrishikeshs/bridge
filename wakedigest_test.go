package main

// wakedigest_test.go — the since-you-woke digest's pure logic that smoke.sh can't
// reach cheaply: the duration humanizer, the composer (counts + quiet case), the
// no-injection invariant, the wake detector (fire/no-fire/debounce/disable), and
// the mailbox-backed count + guarded-path prepend. Run with `go test ./...`.

import (
	"strings"
	"testing"
)

func TestHumanizeAway(t *testing.T) {
	cases := []struct {
		seconds int64
		want    string
	}{
		{0, "0s"},
		{45, "45s"},
		{47 * 60, "47m"},
		{2*3600 + 13*60, "2h13m"}, // the spec's own example
		{3 * 3600, "3h"},
		{3 * 86400, "3d"},          // days above the hour ceiling humanDur stops at
		{3*86400 + 4*3600, "3d4h"}, // day + hour remainder
		{-5, "0s"},                 // defensive: a negative never underflows
	}
	for _, c := range cases {
		if got := humanizeAway(c.seconds); got != c.want {
			t.Errorf("humanizeAway(%d) = %q, want %q", c.seconds, got, c.want)
		}
	}
}

func TestWakeDigestLine(t *testing.T) {
	// The canonical both-present case: humanized duration + both counts + the
	// held-above annotation (mirrors the spec's example line).
	got := wakeDigestLine(47*60, 3, 1)
	for _, want := range []string{"back after 47m", "3 in #crew", "1 direct", "held above"} {
		if !strings.Contains(got, want) {
			t.Errorf("wakeDigestLine(47m,3,1) = %q, missing %q", got, want)
		}
	}
	// Only one channel present -> only that clause appears.
	if got := wakeDigestLine(3600, 2, 0); !strings.Contains(got, "2 in #crew") || strings.Contains(got, "direct") {
		t.Errorf("crew-only line wrong: %q", got)
	}
	if got := wakeDigestLine(3600, 0, 5); !strings.Contains(got, "5 direct") || strings.Contains(got, "#crew") {
		t.Errorf("direct-only line wrong: %q", got)
	}
	// The quiet case: nothing happened, but the clock is still the value.
	quiet := wakeDigestLine(90, 0, 0)
	if !strings.Contains(quiet, "all quiet") || !strings.Contains(quiet, "1m") {
		t.Errorf("quiet line wrong: %q", quiet)
	}
}

// TestWakeDigestLineNoInjection is the security assertion: the composed line is
// daemon-authored from a compile-time template + daemon integers, so it can never
// carry client-derived text or the daemon's forgeable framing alphabet — no
// matter the counts. The away duration and the two counts are the ONLY inputs,
// and all are daemon-computed, never phone-supplied.
func TestWakeDigestLineNoInjection(t *testing.T) {
	for _, tc := range []struct{ crew, direct int }{{0, 0}, {3, 1}, {99, 0}, {0, 7}} {
		line := wakeDigestLine(1234, tc.crew, tc.direct)
		if strings.Contains(line, "[From ") {
			t.Errorf("digest %q carries a forgeable frame head", line)
		}
		if strings.Contains(line, "⏎") {
			t.Errorf("digest %q carries the batch-boundary alphabet", line)
		}
		// It survives neutralizeFrame byte-for-byte — proof it never carried the
		// framing alphabet to begin with (a body-derived line would be rewritten).
		if neutralizeFrame(line) != line {
			t.Errorf("digest %q was altered by neutralizeFrame — it contained framing alphabet", line)
		}
	}
}

// TestWakeDetection exercises noteSeenAndWakeLocked — the transport-agnostic wake
// decision Connect/ConnectRemote share: fire only on a genuine, trustworthy,
// long-enough away, and never twice within one wake.
func TestWakeDetection(t *testing.T) {
	t.Setenv("BRIDGE_WAKE_DIGEST_MIN_AWAY_S", "60") // 60s threshold, deterministic
	// daemonStartUnix anchors the "trustworthy last-seen" guard; keep it at 0 so
	// every positive prevLastSeen is trusted, then flip it for the restart case.
	savedStart := daemonStartUnix
	daemonStartUnix = 0
	defer func() { daemonStartUnix = savedStart }()

	r := &Registry{contacts: map[string]*Contact{}, mailbox: map[string][]MailMessage{}, flushing: map[string]bool{}}
	now := timeNowUnix()

	// (1) Genuine wake: offline, trusted, away >= threshold -> fires.
	c1 := &Contact{ID: "wake", Status: "live"}
	if away := r.noteSeenAndWakeLocked(c1, "offline", now-120); away < 120 {
		t.Errorf("genuine wake: away = %d, want >= 120 (a fire)", away)
	}
	if c1.LastSeen < now {
		t.Errorf("LastSeen not advanced to now (got %d, now %d)", c1.LastSeen, now)
	}
	if c1.LastDigestAt == 0 {
		t.Error("LastDigestAt not stamped on a fire")
	}

	// (2) Brief blip: away < threshold -> no fire (but last-seen still advances).
	c2 := &Contact{ID: "blip", Status: "live"}
	if away := r.noteSeenAndWakeLocked(c2, "offline", now-10); away != 0 {
		t.Errorf("brief blip (10s < 60s): away = %d, want 0 (no fire)", away)
	}
	if c2.LastSeen < now {
		t.Error("LastSeen must advance even when no digest fires")
	}

	// (3) Routine re-hello of a still-LIVE contact -> no fire, even with a huge gap.
	c3 := &Contact{ID: "relive", Status: "live"}
	if away := r.noteSeenAndWakeLocked(c3, "live", now-99999); away != 0 {
		t.Errorf("live re-hello: away = %d, want 0 (not a wake)", away)
	}

	// (4) Untrusted last-seen (predates this daemon's boot) -> no fire: a restart's
	// blank slate must never invent a giant gap.
	daemonStartUnix = now // pretend the daemon booted 'now'
	c4 := &Contact{ID: "restart", Status: "live"}
	if away := r.noteSeenAndWakeLocked(c4, "offline", now-3600); away != 0 {
		t.Errorf("pre-boot last-seen: away = %d, want 0 (untrusted gap)", away)
	}
	daemonStartUnix = 0

	// (5) Debounce: a fire, then a second attempt for the SAME wake (LastDigestAt
	// just stamped) is refused even though away >= threshold.
	c5 := &Contact{ID: "debounce", Status: "live"}
	if away := r.noteSeenAndWakeLocked(c5, "offline", now-120); away < 120 {
		t.Fatalf("debounce setup: first call should fire, got %d", away)
	}
	if away := r.noteSeenAndWakeLocked(c5, "offline", now-120); away != 0 {
		t.Errorf("debounce: second call within one wake fired (away = %d), want 0", away)
	}
}

// TestWakeDigestDisabled: wake_digest_min_away_s = 0 turns the feature off — no
// digest ever fires, though last-seen bookkeeping still advances (cheap substrate
// route-health can use, and so the knob is instantly reversible).
func TestWakeDigestDisabled(t *testing.T) {
	t.Setenv("BRIDGE_WAKE_DIGEST_MIN_AWAY_S", "0")
	savedStart := daemonStartUnix
	daemonStartUnix = 0
	defer func() { daemonStartUnix = savedStart }()

	r := &Registry{contacts: map[string]*Contact{}, mailbox: map[string][]MailMessage{}, flushing: map[string]bool{}}
	now := timeNowUnix()
	c := &Contact{ID: "off", Status: "live"}
	if away := r.noteSeenAndWakeLocked(c, "offline", now-99999); away != 0 {
		t.Errorf("feature disabled (threshold 0): away = %d, want 0", away)
	}
	if c.LastSeen < now {
		t.Error("LastSeen must still advance when the feature is disabled")
	}
	if c.LastDigestAt != 0 {
		t.Error("LastDigestAt must stay 0 when nothing fired")
	}
}

// TestPrependWakeDigest locks prependWakeDigest's contract at the unit level: it
// counts the durable backlog (split by the daemon-set Room field), composes the
// line, and QueueFronts it as a From-less (no forgeable "[From …]" head), Emitted
// (no phone bubble) lead-in that sorts ahead of the untouched backlog. The full
// prepend→guarded-flush→deliver path is smoke's job.
func TestPrependWakeDigest(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // QueueFront/save write under $HOME/.bridge
	savedReg := registry
	registry = &Registry{contacts: map[string]*Contact{}, mailbox: map[string][]MailMessage{}, flushing: map[string]bool{}}
	defer func() { registry = savedReg }()

	const id = "wake-1"
	registry.contacts[id] = &Contact{ID: id, Name: "otter", Status: "live"}
	// Backlog: two #crew messages (Room set) and one direct, in send order.
	registry.mailbox[id] = []MailMessage{
		{From: "Hrishi", Via: "phone", Text: "direct one"},
		{From: "marvin", Via: "bridge", Room: roomCrewName, Text: "crew one"},
		{From: "ludwig", Via: "bridge", Room: roomCrewName, Text: "crew two"},
	}

	crew, direct := registry.MailboxBacklogCounts(id)
	if crew != 2 || direct != 1 {
		t.Fatalf("MailboxBacklogCounts = (crew %d, direct %d), want (2, 1)", crew, direct)
	}

	c := (&Contact{ID: id}).copy()
	c.wakeAwaySeconds = 47 * 60
	prependWakeDigest(c)

	q := registry.mailbox[id]
	if len(q) != 4 {
		t.Fatalf("mailbox length = %d, want 4 (digest + 3 backlog)", len(q))
	}
	head := q[0]
	if head.From != "" {
		t.Errorf("digest From = %q, want empty (no forgeable frame head)", head.From)
	}
	if !head.Emitted {
		t.Error("digest must be Emitted:true (no phone thread bubble)")
	}
	for _, want := range []string{"back after 47m", "2 in #crew", "1 direct"} {
		if !strings.Contains(head.Text, want) {
			t.Errorf("digest head %q missing %q", head.Text, want)
		}
	}
	// The real backlog kept its order behind the lead-in.
	if q[1].Text != "direct one" || q[2].Text != "crew one" || q[3].Text != "crew two" {
		t.Errorf("backlog reordered by the prepend: %q, %q, %q", q[1].Text, q[2].Text, q[3].Text)
	}
	// The digest is From-less, so a re-count never counts it (no self-inflation).
	if crew2, direct2 := registry.MailboxBacklogCounts(id); crew2 != 2 || direct2 != 1 {
		t.Errorf("post-prepend counts = (%d,%d), want (2,1) — the digest self-counted", crew2, direct2)
	}

	// A no-op when this connect was not a wake (wakeAwaySeconds <= 0).
	c2 := (&Contact{ID: id}).copy() // wakeAwaySeconds defaults to 0
	prependWakeDigest(c2)
	if len(registry.mailbox[id]) != 4 {
		t.Errorf("prependWakeDigest fired on a non-wake (len now %d)", len(registry.mailbox[id]))
	}
}
