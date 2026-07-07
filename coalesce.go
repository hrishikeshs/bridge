package main

// coalesce.go — inbound hold-and-batch. Phone texting is bursty: three short
// messages ten seconds apart are one thought. Instead of send-keys-ing each
// one into the agent's composer as it lands (starting a turn on a fragment),
// every inbound message is queued in the contact's persistent mailbox and a
// short per-contact timer is (re)armed; when the sender goes quiet the whole
// burst is delivered as ONE combined line (field request, 2026-07-06).
//
// The mailbox is the buffer on purpose: it is durable (a daemon crash mid-hold
// loses nothing — the revive flush delivers), it is ordered (a held text can
// never be overtaken by a later immediate delivery), and it already has a
// single-flusher guard. The hold also refuses to fire while a permission
// dialog is open: a delivered line ends in Enter, and Enter on an open dialog
// would select the highlighted option — so held messages wait out the prompt.

import (
	"os"
	"strconv"
	"sync"
	"time"
)

// coalesceWindow is how long the daemon waits for a follow-up message before
// delivering the batch. Each new message re-arms the timer (capped by
// coalesceMaxHold so a steady stream cannot starve delivery). BRIDGE_COALESCE_MS
// overrides it for tests; 0 delivers on the next tick with no hold.
var coalesceWindow = func() time.Duration {
	if ms, err := strconv.Atoi(os.Getenv("BRIDGE_COALESCE_MS")); err == nil && ms >= 0 {
		return time.Duration(ms) * time.Millisecond
	}
	return 10 * time.Second
}()

// coalesceMaxHold caps how long the first message of a burst can wait while
// follow-ups keep extending the window.
const coalesceMaxHold = 45 * time.Second

var (
	coalesceMu     sync.Mutex
	coalesceTimers = map[string]*time.Timer{}
	coalesceFirst  = map[string]time.Time{}
)

// holdInbound queues one inbound message durably and (re)arms the contact's
// delivery timer. This is the single entry point for live inbound delivery —
// phone sends, uploads and switchboard messages all ride it.
func holdInbound(c *Contact, m MailMessage) {
	registry.Queue(c.ID, m)
	if coalesceWindow == 0 {
		go coalesceDeliver(c.ID)
		return
	}
	coalesceMu.Lock()
	defer coalesceMu.Unlock()
	if t, ok := coalesceTimers[c.ID]; ok {
		if time.Since(coalesceFirst[c.ID]) < coalesceMaxHold {
			t.Reset(coalesceWindow)
		}
		return
	}
	coalesceFirst[c.ID] = time.Now()
	id := c.ID
	coalesceTimers[id] = time.AfterFunc(coalesceWindow, func() { coalesceDeliver(id) })
}

// holdInboundStaggered is holdInbound's fan-out sibling: it queues the message
// durably (identical, ordered) but arms the FRESH coalesce timer at `offset` — a
// per-recipient stagger delay — instead of coalesceWindow. It is the seam the
// thundering-herd fix hangs on: fanoutRoom and the plugin nudge storm route each
// LIVE recipient through it with a growing offset (fanoutOffset), so N agents'
// next Claude API calls spread across a window instead of bursting at one instant
// and tripping a server-side 429. Everything downstream (coalesceDeliver ->
// flushMailbox -> the pane-ready guard -> DropMailbox) is REUSED unchanged, so
// durability, ordering, the single-flusher, and the never-type-into-a-dialog
// guard all still hold — the stagger only delays the in-memory timer, never the
// durable mailbox. The 1:1 path stays on plain holdInbound and is untouched.
func holdInboundStaggered(c *Contact, m MailMessage, offset time.Duration) {
	if offset == 0 {
		// offset 0 means the feature is OFF (fanoutStaggerStep()==0 makes
		// fanoutOffset return 0 for every recipient). Fall straight through to the
		// 1:1 path so config step=0 restores today's behavior byte-for-byte — same
		// coalesceWindow, same test-mode next-tick deliver, same reset-on-follow-up.
		// fanoutOffset never returns 0 while the feature is ON (it floors to >=1),
		// so a live recipient can never accidentally land here.
		holdInbound(c, m)
		return
	}
	registry.Queue(c.ID, m)
	coalesceMu.Lock()
	defer coalesceMu.Unlock()
	if _, ok := coalesceTimers[c.ID]; ok {
		// A timer already exists (e.g. an in-flight 1:1 burst): LEAVE it. A room or
		// nudge message must never reset-collapse a running coalesce window, and
		// riding the existing schedule means no second wakeup — the message simply
		// delivers with the batch already pending. Ordering holds: Queue appended it.
		return
	}
	coalesceFirst[c.ID] = time.Now()
	id := c.ID
	coalesceTimers[id] = time.AfterFunc(offset, func() { coalesceDeliver(id) })
}

// fanoutOffset returns the stagger delay for the index-th LIVE recipient of a
// fan-out: min(index*step, max) + jitter. index 0 delivers promptly (~jitter);
// each later recipient waits one more step, capped at max so a big crew's tail
// isn't delayed unboundedly. The jitter — a small random fraction of step drawn
// with the house RNG (rand.go) — de-collides recipients pinned to the same value
// (the capped tail) so they never re-fire in lockstep. When step is 0 the feature
// is off and every recipient gets 0 (byte-for-byte the pre-fix herd).
func fanoutOffset(index int) time.Duration {
	step := fanoutStaggerStep()
	if step <= 0 {
		return 0 // feature off — holdInboundStaggered reads 0 as "no stagger"
	}
	base := time.Duration(index) * step
	if max := fanoutStaggerMax(); base > max {
		base = max
	}
	if span := int(step / 4); span > 0 {
		base += time.Duration(randInt(span))
	}
	if base <= 0 {
		// index 0 with a zero jitter draw: nudge to 1ns. 0 is reserved to mean
		// "feature off", so a live recipient's offset must never be exactly 0.
		base = 1
	}
	return base
}

// fanoutStaggerStep is the per-recipient stagger step as a Duration: config
// fanout_stagger_ms (default 2500), floored at 0 (0 disables staggering). Mirrors
// remoteTTL's clamp+default, plus a BRIDGE_FANOUT_STAGGER_MS env override so a
// test can set a tiny step and observe the spread fast (like BRIDGE_COALESCE_MS).
func fanoutStaggerStep() time.Duration {
	if ms, err := strconv.Atoi(os.Getenv("BRIDGE_FANOUT_STAGGER_MS")); err == nil && ms >= 0 {
		return time.Duration(ms) * time.Millisecond
	}
	step := 2500
	if authConfig.FanoutStaggerMs != nil {
		step = *authConfig.FanoutStaggerMs
	}
	if step < 0 {
		step = 0
	}
	return time.Duration(step) * time.Millisecond
}

// fanoutStaggerMax is the cap on total spread as a Duration: config
// fanout_stagger_max_ms (default 20000), floored at 0. k*step never exceeds this
// before jitter, so the last recipient of a large crew stays within one window.
func fanoutStaggerMax() time.Duration {
	max := 20000
	if authConfig.FanoutStaggerMaxMs != nil {
		max = *authConfig.FanoutStaggerMaxMs
	}
	if max < 0 {
		max = 0
	}
	return time.Duration(max) * time.Millisecond
}

// coalesceDeliver fires the held batch for one contact. Also called directly
// to force a flush (e.g. right after an interrupt: the agent just went idle
// and should hear everything that was waiting).
func coalesceDeliver(id string) {
	coalesceMu.Lock()
	if t, ok := coalesceTimers[id]; ok {
		t.Stop()
		delete(coalesceTimers, id)
	}
	delete(coalesceFirst, id)
	coalesceMu.Unlock()

	c := registry.Resolve(id)
	if c == nil || c.Status != "live" {
		return // stays queued; the revive flush delivers it
	}
	// The prompt guard now lives in flushMailbox (paneReadyForDelivery), which
	// reads PANE TRUTH rather than the hook-attested PromptOpen flag this used
	// to check — closing the race where the flag lagged the dialog by up to a
	// second (review H1/H2). flushMailbox re-arms via coalesceRearm if the pane
	// isn't ready, so a blocked burst still retries.
	flushMailbox(c)
}

// coalesceRearm schedules another delivery attempt one window from now,
// without extending the burst bookkeeping (used while a prompt blocks flush).
func coalesceRearm(id string) {
	delay := coalesceWindow
	if delay == 0 {
		delay = 2 * time.Second
	}
	coalesceMu.Lock()
	defer coalesceMu.Unlock()
	if _, ok := coalesceTimers[id]; ok {
		return // a newer message already re-armed it
	}
	coalesceFirst[id] = time.Now()
	coalesceTimers[id] = time.AfterFunc(delay, func() { coalesceDeliver(id) })
}
