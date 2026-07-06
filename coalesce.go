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
