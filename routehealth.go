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
//	"offline"     mail is queued for a contact that is gone.
func deriveRouteHealth(c *Contact) (holdReason string, lastSeenS int) {
	// An offline contact is already surfaced as offline, but route-health still
	// owes it honesty (2026-07-08 refactor review, C7): reconcile two-strikes a
	// stale-lease remote contact to offline within ~4–6s, so the live-path
	// "stale" state below is a seconds-wide window in practice — the July-7
	// soft-wedge class lands HERE, on the offline row. Two truthful clocks
	// survive the flip, tried in evidence order: (a) the stale lease itself
	// lingers (the reap is lazy and 10× the TTL — and if the wedged client is
	// the only one, nothing attests, so nothing reaps), so its lastSeen is the
	// tightest "last attested" evidence; (b) Contact.LastSeen, which SetOffline
	// stamps at the strike ("last seen alive ≈ the strike", registry.go) and
	// connect/revive refresh — coarser, but honest, and it covers tmux rows and
	// reaped leases. And queued mail is queued mail, on any transport.
	if c.Status == "offline" {
		if c.Transport == "remote" {
			if r := remoteRouteInfo(c.ID); r.hasLease {
				lastSeenS = r.lastSeenAgo
			}
		}
		if lastSeenS == 0 && c.LastSeen > 0 {
			if v := int(timeNowUnix() - c.LastSeen); v > 0 {
				lastSeenS = v
			}
		}
		if registry.HasMail(c.ID) {
			return "offline", lastSeenS // mail is waiting for someone who is gone
		}
		return "", lastSeenS
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
			// A route actively mid-drain is not stalled (review C8): oldest-age
			// measures time-since-QUEUED, so a backlog that legitimately piled up
			// while the agent was busy/offline reads old the moment the route
			// heals. flushing[id] is the single-flusher flag — while a guarded
			// flush is delivering, the backlog is draining, not stuck. (The ~2s
			// reconcile gaps between flushes can still sample "stalled" on a deep
			// queue; rare at the phone's 30s poll, accepted.)
			case registry.MailboxOldestAgeSeconds(c.ID) > routeStallSeconds && !registry.IsFlushing(c.ID):
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
	// mail-stall catch. Same mid-drain exemption as the remote branch (review C8).
	if registry.HasMail(c.ID) && registry.MailboxOldestAgeSeconds(c.ID) > routeStallSeconds &&
		!registry.IsFlushing(c.ID) {
		return "stalled", 0
	}
	return "", 0
}
