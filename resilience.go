package main

// resilience.go — the daemon knows when it lost time, and says so.
//
// The 2026-07-06 field failure: the Mac slept 3h with the lid closed, the
// daemon froze, an agent sat blocked on a permission prompt the whole time,
// and the phone showed a generic "unreachable" with no hint that the Mac had
// simply gone to sleep — or when it came back. This file gives the daemon three
// truths it was missing: (1) it notices a wall-clock gap (= it was asleep) and
// tells the phone; (2) /api/status carries the clocks the phone needs to say
// "asleep since X" instead of "unreachable"; (3) a permission prompt left open
// while the user is away re-rings the phone at escalating ages (pushes ride
// APNs, so they arrive even when the phone can't reach the tailnet).

import (
	"fmt"
	"sync"
	"time"
)

const (
	// sleepGapThreshold: a reconcile pass starting this long after the previous
	// one means the process was frozen (asleep) — normal spacing is ~2s.
	sleepGapThreshold = 90 * time.Second
	// wakePushThreshold: only ring the phone for a gap at least this long, so a
	// brief hiccup doesn't buzz the lock screen; shorter gaps are still audited
	// and exposed on /api/status.
	wakePushThreshold = 5 * time.Minute
	// frozenFirstAge / frozenRepeatEvery: a prompt open past frozenFirstAge with
	// the user apparently away gets a re-ring, then at most one every
	// frozenRepeatEvery after.
	frozenFirstAge    = 5 * time.Minute
	frozenRepeatEvery = 30 * time.Minute
)

var (
	daemonStartUnix int64 // set in runServe; exposed on /api/status as "started"

	resilienceMu sync.Mutex
	lastWakeFrom int64 // unix seconds; 0 = no sleep gap observed since start
	lastWakeTo   int64

	// frozenPushedAt tracks the last escalation-push time per contact so a
	// long-open prompt re-rings on a schedule rather than every tick. Reconcile
	// goroutine only.
	frozenPushedAt = map[string]int64{}
)

// noteWakeGap records a detected sleep gap, audits it, and (for a long enough
// gap) rings the phone. It also clears the rollover-chain throttle so the very
// next poll re-resolves every contact's session file — mtimes are now hours
// stale and the identity ladder must re-check rather than trust them.
func noteWakeGap(from, to time.Time) {
	resilienceMu.Lock()
	lastWakeFrom, lastWakeTo = from.Unix(), to.Unix()
	resilienceMu.Unlock()

	gap := to.Sub(from)
	window := fmt.Sprintf("%s–%s (%s)", from.Format("15:04"), to.Format("15:04"), humanDur(gap))
	audit("wake", "slept "+window, "daemon")
	for id := range chainChecked { // force an immediate identity re-resolve
		delete(chainChecked, id)
	}
	if gap >= wakePushThreshold {
		notifyPush("bridge is back", "Mac was asleep "+window, "wake", "")
	}
}

// wakeGap returns the most recently observed sleep window (unix seconds), or
// (0,0) if none since the daemon started. Read by handleStatus.
func wakeGap() (from, to int64) {
	resilienceMu.Lock()
	defer resilienceMu.Unlock()
	return lastWakeFrom, lastWakeTo
}

// escalateFrozenPrompt re-rings the phone for a permission prompt that has sat
// open too long — the "agent frozen while I was away" case. It reuses the
// attn-<id> push tag, so the lock-screen notification is UPDATED in place
// ("still waiting 15m") rather than stacking. Called once per live contact per
// reconcile pass.
func escalateFrozenPrompt(c *Contact) {
	if !c.PromptOpen || c.PromptSince == 0 {
		delete(frozenPushedAt, c.ID)
		return
	}
	age := time.Duration(timeNowUnix()-c.PromptSince) * time.Second
	if age < frozenFirstAge {
		return // the original attention push still stands
	}
	now := timeNowUnix()
	if last := frozenPushedAt[c.ID]; last != 0 && now-last < int64(frozenRepeatEvery/time.Second) {
		return
	}
	frozenPushedAt[c.ID] = now
	notifyPush(c.Name+" still needs you",
		fmt.Sprintf("permission prompt open %s", humanDur(age)),
		"attn-"+c.ID, c.ID)
	markAttnPushed(c.ID)
	audit("frozen-escalate", fmt.Sprintf("%s prompt open %s", c.Name, humanDur(age)), "daemon")
}

// humanDur renders a duration compactly: "45s", "12m", "1h3m", "3h".
func humanDur(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}
