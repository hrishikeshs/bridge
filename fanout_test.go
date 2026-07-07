package main

// fanout_test.go — unit tests for the fan-out stagger (the thundering-herd fix,
// coalesce.go). fanoutOffset is a pure function of config, so it is tested
// exhaustively here; holdInboundStaggered's timer scheduling is tested for the
// observable map invariants (arm once, ride-don't-reset, queue-always) with a
// throwaway id and full cleanup. End-to-end timing lives in test/smoke.sh, which
// sets BRIDGE_FANOUT_STAGGER_MS small and watches a real #crew fan-out spread.

import (
	"testing"
	"time"
)

func TestFanoutOffset(t *testing.T) {
	// Drive the tunables through authConfig; neutralize any ambient env override
	// so the numbers below are the ones under test.
	t.Setenv("BRIDGE_FANOUT_STAGGER_MS", "")
	saved := authConfig
	defer func() { authConfig = saved }()

	step := 2500
	max := 20000
	authConfig.FanoutStaggerMs = &step
	authConfig.FanoutStaggerMaxMs = &max
	stepD := time.Duration(step) * time.Millisecond
	maxD := time.Duration(max) * time.Millisecond
	jitterSpan := stepD / 4 // fanoutOffset draws jitter in [0, step/4)

	// (1) Spreads: offset(0) < offset(3) < offset(7). The indices are far enough
	// apart that jitter (< step/4) can never invert the ordering, whatever it draws.
	o0, o3, o7 := fanoutOffset(0), fanoutOffset(3), fanoutOffset(7)
	if !(o0 < o3 && o3 < o7) {
		t.Errorf("offsets not increasing: offset(0)=%v offset(3)=%v offset(7)=%v", o0, o3, o7)
	}

	// (2) Each index lands in its [base, base+jitterSpan) bucket, base = k*step
	// clamped to max. (index 0 floors to >=1ns but that is still within [0, span).)
	for _, idx := range []int{0, 1, 2, 3, 5, 7} {
		base := time.Duration(idx) * stepD
		if base > maxD {
			base = maxD
		}
		got := fanoutOffset(idx)
		if got < base || got >= base+jitterSpan {
			t.Errorf("offset(%d)=%v outside expected bucket [%v, %v)", idx, got, base, base+jitterSpan)
		}
	}

	// (3) Respects the max cap: a huge index is clamped to max (+ jitter), never
	// index*step.
	big := fanoutOffset(1000)
	if big < maxD || big >= maxD+jitterSpan {
		t.Errorf("offset(1000)=%v not clamped into [%v, %v)", big, maxD, maxD+jitterSpan)
	}
	if uncapped := time.Duration(1000) * stepD; big >= uncapped {
		t.Errorf("offset(1000)=%v was not capped (>= index*step = %v)", big, uncapped)
	}

	// (4) The cap engages at a realistic index with a smaller ceiling: with
	// max=5000 and step=2500, index 3 (7500ms raw) clamps to 5000ms.
	smallMax := 5000
	authConfig.FanoutStaggerMaxMs = &smallMax
	smallMaxD := time.Duration(smallMax) * time.Millisecond
	capped := fanoutOffset(3)
	if capped < smallMaxD || capped >= smallMaxD+jitterSpan {
		t.Errorf("offset(3) with max=5000 = %v, want clamped into [%v, %v)", capped, smallMaxD, smallMaxD+jitterSpan)
	}
	authConfig.FanoutStaggerMaxMs = &max // restore for (5)

	// (5) Never exactly 0 while the feature is ON, even at index 0 — 0 is the
	// reserved "feature off" sentinel the delivery path reads, so a live
	// recipient's offset must never leak it (catch the zero-jitter draw).
	for i := 0; i < 1000; i++ {
		if fanoutOffset(0) == 0 {
			t.Fatal("offset(0) returned 0 with the feature on: the 'off' sentinel leaked")
		}
	}

	// (6) step==0 disables staggering: every index returns exactly 0 (the pre-fix
	// herd — reversible by config).
	zero := 0
	authConfig.FanoutStaggerMs = &zero
	for _, idx := range []int{0, 1, 7, 1000} {
		if got := fanoutOffset(idx); got != 0 {
			t.Errorf("offset(%d)=%v with step=0, want 0 (feature off)", idx, got)
		}
	}
}

func TestFanoutStaggerConfigDefaults(t *testing.T) {
	t.Setenv("BRIDGE_FANOUT_STAGGER_MS", "")
	saved := authConfig
	defer func() { authConfig = saved }()

	// Unset config falls back to the shipped defaults.
	authConfig.FanoutStaggerMs = nil
	authConfig.FanoutStaggerMaxMs = nil
	if got := fanoutStaggerStep(); got != 2500*time.Millisecond {
		t.Errorf("default fanoutStaggerStep() = %v, want 2.5s", got)
	}
	if got := fanoutStaggerMax(); got != 20000*time.Millisecond {
		t.Errorf("default fanoutStaggerMax() = %v, want 20s", got)
	}

	// A negative step/max is floored to 0 (never negative), not left to underflow.
	neg := -1
	authConfig.FanoutStaggerMs = &neg
	authConfig.FanoutStaggerMaxMs = &neg
	if got := fanoutStaggerStep(); got != 0 {
		t.Errorf("fanoutStaggerStep() with -1 = %v, want 0 (floored)", got)
	}
	if got := fanoutStaggerMax(); got != 0 {
		t.Errorf("fanoutStaggerMax() with -1 = %v, want 0 (floored)", got)
	}

	// The env override wins over config (the smoke-test hook).
	authConfig.FanoutStaggerMs = &neg
	t.Setenv("BRIDGE_FANOUT_STAGGER_MS", "150")
	if got := fanoutStaggerStep(); got != 150*time.Millisecond {
		t.Errorf("fanoutStaggerStep() with env=150 = %v, want 150ms", got)
	}
}

func TestHoldInboundStaggeredRidesExistingTimer(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // registry.Queue -> save() writes under $HOME/.bridge

	// A throwaway id that is NOT a registered contact: if a timer ever fired,
	// coalesceDeliver's Resolve would return nil and no-op. We use a 30s offset and
	// stop the timer in cleanup, so it never actually fires during the test.
	const id = "ffffffff-0000-4000-8000-00000000fa11"
	c := &Contact{ID: id, Name: "stagger-probe", Status: "live"}
	const big = 30 * time.Second

	clear := func() {
		coalesceMu.Lock()
		if tmr, ok := coalesceTimers[id]; ok {
			tmr.Stop()
			delete(coalesceTimers, id)
		}
		delete(coalesceFirst, id)
		coalesceMu.Unlock()
		registry.mu.Lock()
		delete(registry.mailbox, id)
		registry.mu.Unlock()
	}
	clear()
	defer clear()

	// First fan-out message: queues durably and arms exactly one timer.
	holdInboundStaggered(c, MailMessage{From: "a", Via: "bridge", Text: "first"}, big)
	coalesceMu.Lock()
	tmr1, ok1 := coalesceTimers[id]
	first1 := coalesceFirst[id]
	coalesceMu.Unlock()
	if !ok1 || tmr1 == nil {
		t.Fatal("first staggered call armed no timer")
	}
	if !registry.HasMail(id) {
		t.Fatal("first staggered call did not queue the message durably")
	}

	// A second fan-out message for the SAME contact must RIDE the existing timer:
	// no replacement timer, and the burst-start stamp is left untouched (a room or
	// nudge message must never reset-collapse an in-flight coalesce window).
	time.Sleep(2 * time.Millisecond)
	holdInboundStaggered(c, MailMessage{From: "b", Via: "bridge", Text: "second"}, big)
	coalesceMu.Lock()
	tmr2 := coalesceTimers[id]
	first2 := coalesceFirst[id]
	coalesceMu.Unlock()
	if tmr2 != tmr1 {
		t.Error("second staggered call replaced the timer instead of riding it")
	}
	if !first2.Equal(first1) {
		t.Errorf("second staggered call reset the burst stamp %v -> %v (must ride, not reset)", first1, first2)
	}

	// Both messages are durably queued, in the order they arrived (Queue appends;
	// the stagger never lets a fan-out overtake already-queued mail).
	registry.mu.Lock()
	q := append([]MailMessage(nil), registry.mailbox[id]...)
	registry.mu.Unlock()
	if len(q) != 2 {
		t.Fatalf("mailbox holds %d messages, want 2 (both queued)", len(q))
	}
	if q[0].Text != "first" || q[1].Text != "second" {
		t.Errorf("mailbox order = [%q, %q], want [first, second]", q[0].Text, q[1].Text)
	}
}
