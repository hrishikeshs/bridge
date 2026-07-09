package main

// routehealth_test.go — deriveRouteHealth's derivation, exercised against the
// exact silence classes from docs/route-health.md (July 7–8): the held-at-dialog
// mail (quick-wolf's 40-minute mystery), a delivering-but-unconfirmed route, a
// soft-wedged stale lease, a busy agent, and a stalled tmux backlog — plus the
// no-false-positive cases that keep a healthy route quiet. Leases are injected
// into the globals under remoteMu exactly like TestRemoteReadyDialogBelt; the
// process registry is swapped for a scratch one exactly like TestPrependWakeDigest.

import (
	"testing"
	"time"
)

func TestDeriveRouteHealth(t *testing.T) {
	// Swap in a scratch registry; restore the process one after (TestPrependWakeDigest
	// pattern). deriveRouteHealth only READS, and mail is seeded by direct map
	// assignment (not Queue, which would save() to disk), so no HOME is needed.
	savedReg := registry
	registry = &Registry{
		contacts: map[string]*Contact{},
		mailbox:  map[string][]MailMessage{},
		flushing: map[string]bool{},
	}
	defer func() { registry = savedReg }()

	// mail builds a queued message whose RFC3339 TS is agoS seconds old — the same
	// stamp every real queue path writes via nowUTC.
	mail := func(agoS int) MailMessage {
		return MailMessage{
			From: "Hrishi", Via: "phone", Text: "hi",
			TS: time.Now().Add(-time.Duration(agoS) * time.Second).UTC().Format(time.RFC3339),
		}
	}

	// addLease injects a scratch remote lease into the globals (remoteMu) and
	// returns its cleanup, mirroring TestRemoteReadyDialogBelt.
	addLease := func(cid string, lastSeen time.Time, suspect bool, st remoteState) func() {
		token := "rh-token-" + cid
		remoteMu.Lock()
		remoteLeases[token] = &remoteLease{
			token:    token,
			lastSeen: lastSeen,
			suspect:  suspect,
			agents:   map[string]bool{cid: true},
			states:   map[string]remoteState{cid: st},
		}
		remoteLeaseByContact[cid] = token
		remoteMu.Unlock()
		return func() {
			remoteMu.Lock()
			remoteDeleteLeaseLocked(token)
			remoteMu.Unlock()
		}
	}
	noCleanup := func() {}

	cases := []struct {
		name     string
		setup    func() (*Contact, func())
		wantHold string
		verify   func(t *testing.T, lastSeenS int) // optional lastSeenS assertion
	}{
		{
			// (a) held-at-dialog: the quick-wolf incident. A fresh, non-suspect lease
			// that even attests ready:true, but the attested tail shows a real dialog
			// and mail is queued — the guard is holding it. dialog is judged before
			// ready, so the stale ready:true never wins.
			name: "remote at-prompt: fresh ready lease, dialog on tail, mail held",
			setup: func() (*Contact, func()) {
				cid := "rh-at-prompt"
				clean := addLease(cid, time.Now(), false, remoteState{Ready: true, ScreenTail: rtDialogTail})
				registry.mailbox[cid] = []MailMessage{mail(5)}
				return &Contact{ID: cid, Name: "wolf", Status: "live", Transport: "remote"}, clean
			},
			wantHold: "at-prompt",
		},
		{
			// (b) delivering-but-unconfirmed: a Deliver timed out (lease suspect) and
			// mail is queued — it may not have landed. suspect outranks every other
			// mail-held reason.
			name: "remote unconfirmed: suspect lease, mail held",
			setup: func() (*Contact, func()) {
				cid := "rh-unconfirmed"
				clean := addLease(cid, time.Now(), true, remoteState{Ready: true, ScreenTail: ""})
				registry.mailbox[cid] = []MailMessage{mail(5)}
				return &Contact{ID: cid, Name: "otter", Status: "live", Transport: "remote"}, clean
			},
			wantHold: "unconfirmed",
		},
		{
			// (c) soft-wedge: the client stopped attesting, the lease aged past the
			// TTL, and new mail can't be delivered. "stale" is a pure-liveness signal
			// checked BEFORE the mail cascade, so held mail can't mask it — and
			// lastSeenS carries the real "last seen" age the phone shows.
			name: "remote stale: aged lease with mail held reads stale, not a mail reason",
			setup: func() (*Contact, func()) {
				cid := "rh-stale"
				clean := addLease(cid, time.Now().Add(-time.Hour), false, remoteState{Ready: true})
				registry.mailbox[cid] = []MailMessage{mail(5)}
				return &Contact{ID: cid, Name: "wren", Status: "live", Transport: "remote"}, clean
			},
			wantHold: "stale",
			verify: func(t *testing.T, ls int) {
				if ls < 60 { // lastSeen was an hour ago — must surface a real age, not ~0
					t.Errorf("stale lastSeenS = %d, want a large age (client last seen ~1h ago)", ls)
				}
			},
		},
		{
			// (d) busy: a fresh, clean, non-suspect lease that simply attested
			// not-ready, with mail queued — it waits for the agent to idle.
			name: "remote busy: fresh clean lease attests not-ready, mail held",
			setup: func() (*Contact, func()) {
				cid := "rh-busy"
				clean := addLease(cid, time.Now(), false, remoteState{Ready: false, ScreenTail: ""})
				registry.mailbox[cid] = []MailMessage{mail(5)}
				return &Contact{ID: cid, Name: "crow", Status: "live", Transport: "remote"}, clean
			},
			wantHold: "busy",
		},
		{
			// (e) tmux stalled: no lease; the honest tmux signal is a backlog that has
			// sat past routeStallSeconds. The OLD message is deliberately NOT at the
			// head, so a head-only age scan would wrongly read 5s and miss it — this
			// pins the oldest-message semantics. lastSeenS is 0 for tmux.
			name: "tmux stalled: oldest queued message older than the stall window",
			setup: func() (*Contact, func()) {
				cid := "rh-stalled-tmux"
				registry.mailbox[cid] = []MailMessage{mail(5), mail(routeStallSeconds + 30)}
				return &Contact{ID: cid, Name: "marvin", Status: "live", Transport: ""}, noCleanup
			},
			wantHold: "stalled",
			verify: func(t *testing.T, ls int) {
				if ls != 0 {
					t.Errorf("tmux lastSeenS = %d, want 0 (no lease to age)", ls)
				}
			},
		},
		{
			// (f1) no false positive: fresh, ready, non-suspect lease with mail still
			// well inside the benign window — in flight, not stuck.
			name: "no-fp remote: fresh ready lease, mail within the window",
			setup: func() (*Contact, func()) {
				cid := "rh-fresh-mail"
				clean := addLease(cid, time.Now(), false, remoteState{Ready: true, ScreenTail: ""})
				registry.mailbox[cid] = []MailMessage{mail(5)}
				return &Contact{ID: cid, Name: "fox", Status: "live", Transport: "remote"}, clean
			},
			wantHold: "",
		},
		{
			// (f2) no false positive: fresh, ready lease with an EMPTY mailbox — the
			// common healthy live route.
			name: "no-fp remote: fresh ready lease, no mail",
			setup: func() (*Contact, func()) {
				cid := "rh-fresh-nomail"
				clean := addLease(cid, time.Now(), false, remoteState{Ready: true, ScreenTail: ""})
				return &Contact{ID: cid, Name: "hare", Status: "live", Transport: "remote"}, clean
			},
			wantHold: "",
		},
		{
			// (f3) no false positive: a remote contact with NO lease and mail queued —
			// reconcile owns the offline flip; route-health must not invent a hold.
			name: "no-fp remote: no lease, mail queued (reconcile owns the flip)",
			setup: func() (*Contact, func()) {
				cid := "rh-no-lease"
				registry.mailbox[cid] = []MailMessage{mail(200)} // even old mail: still no hold
				return &Contact{ID: cid, Name: "lynx", Status: "live", Transport: "remote"}, noCleanup
			},
			wantHold: "",
		},
		{
			// (f4) no false positive: tmux with mail still inside the window.
			name: "no-fp tmux: mail within the window",
			setup: func() (*Contact, func()) {
				cid := "rh-tmux-fresh"
				registry.mailbox[cid] = []MailMessage{mail(5)}
				return &Contact{ID: cid, Name: "ludwig", Status: "live", Transport: ""}, noCleanup
			},
			wantHold: "",
		},
		{
			// (f5) no false positive: tmux with an empty mailbox.
			name: "no-fp tmux: no mail",
			setup: func() (*Contact, func()) {
				return &Contact{ID: "rh-tmux-nomail", Name: "simon", Status: "live", Transport: ""}, noCleanup
			},
			wantHold: "",
		},
		{
			// (g1) offline honesty (2026-07-08 review, C7): mail queued for a gone
			// contact reads "offline" — the July-7 soft-wedge lands on the offline
			// row, and its waiting mail must not be silent there.
			name: "offline honesty: held mail on an offline row reads offline",
			setup: func() (*Contact, func()) {
				cid := "rh-offline"
				registry.mailbox[cid] = []MailMessage{mail(routeStallSeconds + 100)}
				return &Contact{ID: cid, Name: "ghost", Status: "offline", Transport: "remote"}, noCleanup
			},
			wantHold: "offline",
		},
		{
			// (g2) offline honesty: the lingering stale lease supplies the TRUE
			// last-attested age for the offline row (Contact.LastSeen is hello-time
			// and deliberately unused). The lease is 2h stale — well past the TTL,
			// short of the 10×TTL reap.
			name: "offline honesty: lingering stale lease supplies last seen",
			setup: func() (*Contact, func()) {
				cid := "rh-offline-lease"
				clean := addLease(cid, time.Now().Add(-2*time.Hour), false, remoteState{Ready: true})
				registry.mailbox[cid] = []MailMessage{mail(30)}
				return &Contact{ID: cid, Name: "wedge", Status: "offline", Transport: "remote"}, clean
			},
			wantHold: "offline",
			verify: func(t *testing.T, ls int) {
				if ls < 7000 {
					t.Errorf("offline lastSeenS = %d, want the lease's ~2h attest age", ls)
				}
			},
		},
		{
			// (g3) no false positive: an offline contact with NO mail and NO
			// evidence (zero LastSeen, no lease) stays fully quiet.
			name: "no-fp offline: no mail, no evidence stays quiet",
			setup: func() (*Contact, func()) {
				return &Contact{ID: "rh-offline-quiet", Name: "gone", Status: "offline", Transport: ""}, noCleanup
			},
			wantHold: "",
			verify: func(t *testing.T, ls int) {
				if ls != 0 {
					t.Errorf("offline tmux lastSeenS = %d, want 0 (no evidence, no guessing)", ls)
				}
			},
		},
		{
			// (g5) offline fallback clock: with no lease, the SetOffline strike
			// stamp (Contact.LastSeen) supplies the age — tmux rows included.
			name: "offline honesty: strike-stamp fallback supplies last seen on tmux",
			setup: func() (*Contact, func()) {
				return &Contact{ID: "rh-offline-struck", Name: "struck", Status: "offline",
					Transport: "", LastSeen: timeNowUnix() - 3600}, noCleanup
			},
			wantHold: "",
			verify: func(t *testing.T, ls int) {
				if ls < 3595 || ls > 3660 {
					t.Errorf("offline strike-stamp lastSeenS = %d, want ~3600", ls)
				}
			},
		},
		{
			// (g4) mid-drain is not stalled (2026-07-08 review, C8): the same
			// old-backlog tmux case as (e), but a guarded flush is actively
			// delivering (flushing[id] held) — the backlog is draining, not stuck.
			name: "no-fp tmux: draining backlog (flushing) is not stalled",
			setup: func() (*Contact, func()) {
				cid := "rh-draining"
				registry.mailbox[cid] = []MailMessage{mail(routeStallSeconds + 30)}
				registry.flushing[cid] = true
				return &Contact{ID: cid, Name: "drain", Status: "live", Transport: ""}, func() {
					delete(registry.flushing, cid)
				}
			},
			wantHold: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, cleanup := tc.setup()
			defer cleanup()
			gotHold, gotLastSeen := deriveRouteHealth(c)
			if gotHold != tc.wantHold {
				t.Errorf("deriveRouteHealth hold = %q, want %q", gotHold, tc.wantHold)
			}
			if tc.verify != nil {
				tc.verify(t, gotLastSeen)
			}
		})
	}
}
