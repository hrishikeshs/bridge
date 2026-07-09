package main

// routehealth.go — Route Health Layer 1, daemon half (docs/route-health.md).
//
// Today /api/status renders only Contact.Health, which the tmux tail and hooks
// set and which is ORTHOGONAL to remote lease state: a remote agent whose
// delivery is gated reads health:"ok" while its mail sits, invisibly. That is
// exactly how quick-wolf had 8 messages held with zero trace on 2026-07-08 — a
// 40-minute shell investigation for what should be a glance. deriveRouteHealth
// closes that blind spot by DERIVING an honest delivery-hold state from evidence
// the daemon already has (the lease, the mailbox), so a stuck route becomes
// visible.
//
// This is the surfacing-only layer. It READS lease + mailbox state and mutates
// NOTHING — never a delivery/typing/park/ack path, and crucially never
// Contact.Health (whose every existing consumer must stay byte-identical; the PWA
// composes the dot color from these derived fields in a later, deploy-gated
// change). It is modeled on the contextPct precedent (context.go): a pure
// derive-at-/api/status helper returning empty values that omitempty hides.

// routeStallSeconds is how long queued mail may sit before a route is called
// "stalled". It MUST exceed both windows in which held mail is entirely normal,
// or a healthy batch mid-hold would false-positive: coalesceMaxHold (45s, the
// burst-coalescing hold) and fanoutStaggerMax (20s, the #crew fan-out spread).
// 90s clears both with margin. A plain const for now — config-tunability is a
// later route-health step.
const routeStallSeconds = 90

// deriveRouteHealth returns an honest delivery-hold reason (or "" when nothing is
// actually held) plus the seconds since a remote route was last seen. It is PURE
// and read-only. Called once per contact per /api/status poll, so it stays cheap:
// a short lease read (remoteRouteInfo), and only when mail is actually queued, one
// in-memory mailbox-age scan. It performs NO capture-pane exec.
//
// Load-bearing invariant: NEVER return a hold reason unless mail is genuinely
// queued (the sole exception is "stale", which is a pure-liveness signal — a
// client that stopped attesting is worth greying whether or not mail is waiting).
//
// Hold reasons (most-diagnostic first within the mail-held cascade):
//
//	"stale"       remote lease aged past the TTL; the client stopped attesting.
//	"unconfirmed" a Deliver timed out (lease suspect); mail may not have landed.
//	"at-prompt"   the attested screen shows a dialog; the guard is holding mail.
//	"busy"        the remote agent attested not-ready; mail waits for it to idle.
//	"stalled"     mail has sat past routeStallSeconds with no benign explanation.
func deriveRouteHealth(c *Contact) (holdReason string, lastSeenS int) {
	// An offline contact is already surfaced as offline; route-health adds nothing.
	if c.Status == "offline" {
		return "", 0
	}

	// Remote contacts: the transport with liveness but no honesty. Transport is
	// "remote" for the emacs client (empty means the tmux default). Read the RAW
	// lease so staleness surfaces instead of collapsing to "no lease".
	if c.Transport == "remote" {
		r := remoteRouteInfo(c.ID)
		lastSeenS = r.lastSeenAgo
		if !r.hasLease {
			// No lease at all is reconcile's job to flip offline; don't invent a hold.
			return "", lastSeenS
		}
		if r.stale {
			return "stale", lastSeenS
		}
		// A fresh lease with no queued mail is a healthy live route — say nothing.
		if registry.HasMail(c.ID) {
			switch {
			case r.suspect:
				return "unconfirmed", lastSeenS
			case r.dialog:
				return "at-prompt", lastSeenS
			case !r.ready:
				return "busy", lastSeenS
			case registry.MailboxOldestAgeSeconds(c.ID) > routeStallSeconds:
				return "stalled", lastSeenS
			}
		}
		// Fresh, ready, mail (if any) still inside the benign window: in flight, not
		// stuck.
		return "", lastSeenS
	}

	// tmux / default transport: no lease to read, and deliberately NO capture-pane
	// exec here (that would make /api/status shell out once per contact per poll).
	// The honest, cheap tmux signal is a backlog that has sat too long — the
	// mail-stall catch.
	if registry.HasMail(c.ID) && registry.MailboxOldestAgeSeconds(c.ID) > routeStallSeconds {
		return "stalled", 0
	}
	return "", 0
}
