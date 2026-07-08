package main

// reconcile.go — the daemon's heartbeat: one goroutine that keeps every
// contact's state honest. Liveness (two-strike, flap-proof), revival after a
// daemon restart, tail driving, deferred-mail retry, prompt re-verification,
// frozen-prompt escalation, and the sleep watchdog all live in this loop.

import (
	"strings"
	"time"
)

// startSessionManager launches the reconcile loop that keeps a JSONL tail
// running for every live contact and retires contacts whose tmux session has
// ended. Called once from runServe.
func startSessionManager() {
	go func() {
		lastTick := time.Now()
		for {
			// Wall-clock watchdog: a pass starting far later than the ~2s
			// cadence means the process was frozen — the Mac slept. Tell the
			// phone and force an identity re-resolve (mtimes are now stale).
			now := time.Now()
			if now.Sub(lastTick) > sleepGapThreshold {
				noteWakeGap(lastTick, now)
			}
			lastTick = now
			drainPendingHooks()
			// The morning edition: once past the configured hour, the first
			// tick of the day goes to press (paper.go). Cheap check, runs on
			// this goroutine like everything else that reads the roster.
			if paperDue(now) {
				publishPaper(now)
			}
			roster := registry.Roster()
			for _, c := range roster {
				alive := transportFor(c).Alive(c)
				if alive {
					delete(aliveStrikes, c.ID)
				}
				switch {
				case c.Status == "live" && !alive:
					// A failed tmux exec reads exactly like a dead window, so
					// one bad tick must not retire the contact: a flap used to
					// destroy the tail state and skip the relay to EOF, eating
					// every in-gap reply (H6). Two consecutive dead ticks
					// (~4s) make it real. The tail entry is KEPT either way —
					// a revival resumes the relay mid-stream.
					aliveStrikes[c.ID]++
					if aliveStrikes[c.ID] < 2 {
						break
					}
					delete(aliveStrikes, c.ID)
					registry.SetOffline(c.ID)
					Emit("attention-clear", c.ID, c.Name, "")
					clearAttnPush(c.ID, c.Name)
				case c.Status != "live" && alive:
					// Remote revival is hello's job, not the reconcile loop's:
					// this branch is tmux-shaped (it migrates a legacy
					// name-based target to a window id) and cannot legitimately
					// fire for a remote contact — a stale lease is never alive.
					// Guard anyway, belt-and-braces, so a remote row can never be
					// sent through Connect() and blank-slated back onto tmux.
					if c.Transport != "" && c.Transport != defaultTransport {
						break
					}
					// Its window outlived a daemon restart: revive it so a
					// restart never orphans a running agent. Migrate a legacy
					// name-based target to the grammar-immune window id here.
					target := c.TmuxTarget
					if !strings.HasPrefix(target, "@") {
						if id := tmuxWindowID(c.Name); id != "" {
							target = id
						}
					}
					revived := registry.Connect(c.Name, c.Directory, c.SessionID, target)
					Emit("connected", revived.ID, revived.Name, "")
					// A window can outlive a daemon restart with a permission
					// dialog still open; Connect just blindly reset
					// PromptOpen=false. Re-detect it from the pane so a frozen
					// prompt is re-surfaced (card + push) instead of silently
					// cleared — and so flushMailbox refuses to type into it
					// (review 2026-07-06, criticals C2/C4).
					raiseOrRefreshPrompt(revived, transportFor(revived).Capture(revived))
					// A genuine wake (away >= threshold) leads the backlog with a
					// since-you-woke line. Across a daemon restart this is normally a
					// no-op: the pre-restart LastSeen predates daemonStartUnix, which
					// the detector refuses to trust (no invented "back after 40d" gap).
					prependWakeDigest(revived)
					flushMailbox(revived)
				case c.Status == "live" && alive:
					pollReplies(c)
					verifyPrompt(c)
					// Universal, self-healing delivery retry: any contact
					// holding mail gets a flush attempt every tick. flushMailbox
					// self-guards on pane readiness, so this both delivers
					// deferred/connect-time mail once Claude is up and retries
					// after a delivery error — closing the "no re-arm" strand
					// the coalescer alone left open (review L1/L2, finding 6).
					if registry.HasMail(c.ID) {
						flushMailbox(c)
					}
					// Re-ring the phone for a prompt left open too long while
					// the user is away (round 4 — the frozen-agent case).
					escalateFrozenPrompt(c)
				}
			}
			// Tail state now outlives an offline transition (H6), so prune it
			// only when the contact left the roster entirely (retired).
			if len(tails) > 0 {
				known := make(map[string]bool, len(roster))
				for _, c := range roster {
					known[c.ID] = true
				}
				for id := range tails {
					if !known[id] {
						delete(tails, id)
						tailsDirty = true
					}
				}
			}
			if tailsDirty { // persist advanced offsets once per pass (4b)
				saveTails()
				tailsDirty = false
			}
			time.Sleep(2 * time.Second)
		}
	}()
}

// aliveStrikes counts consecutive reconcile ticks where a live contact's tmux
// check failed — retirement needs two, so a transient exec error can't flap a
// contact offline and back (H6). Reconcile goroutine only.
var aliveStrikes = map[string]int{}

// promptStrikes counts consecutive reconcile ticks where a hook-attested open
// prompt was NOT visible on the contact's screen. Only the reconcile goroutine
// touches it.
var promptStrikes = map[string]int{}

// verifyPrompt re-checks an open permission prompt against the actual pane. A
// prompt answered at the desk leaves no hook event behind, so without this the
// "needs your approval" state goes stale and false urgency spreads across the
// phone (found live: a night of desk-answered prompts left banners on every
// thread). Two consecutive misses (~4s) clear it — one miss could just be the
// dialog still painting.
func verifyPrompt(c *Contact) {
	if !c.PromptOpen {
		delete(promptStrikes, c.ID)
		return
	}
	if snap := transportFor(c).Capture(c); looksLikePrompt(snap) {
		delete(promptStrikes, c.ID)
		// The dialog is still up — but it may be a DIFFERENT command than the one
		// the card is captioned with (Claude answered one prompt and opened the
		// next without a clean frame between them). raiseOrRefreshPrompt re-captions
		// the card when the command changed and is a no-op when it didn't, so this
		// per-tick re-check is also the tmux stale-caption fix.
		raiseOrRefreshPrompt(c, snap)
		return
	}
	promptStrikes[c.ID]++
	if promptStrikes[c.ID] >= 2 {
		delete(promptStrikes, c.ID)
		registry.SetPrompt(c.ID, false)
		Emit("attention-clear", c.ID, c.Name, "")
		clearAttnPush(c.ID, c.Name)
	}
}
