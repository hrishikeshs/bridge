package main

// remote_test.go — unit tests for the remote transport's pure logic that
// smoke.sh can't reach over HTTP: flavor sanitization, the rune-safe screen_tail
// clamp, the ConnectRemote adoption rules (a–d), the tmux-reconnect flip-back,
// and transportFor resolving "remote". Lease timing is deliberately smoke's job
// (wall-clock dependent), not a unit test's.

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestSanitizeFlavor(t *testing.T) {
	cases := []struct{ in, want string }{
		{"emacs", "emacs"},
		{"Emacs", "emacs"}, // lowercased
		{"sim", "sim"},
		{"", "remote"},                // empty falls back to the mechanism name
		{"   ", "remote"},             // whitespace-only trims to empty
		{"!!!", "remote"},             // all-invalid strips to empty
		{"a b c", "abc"},              // spaces dropped
		{"emacs/vterm", "emacsvterm"}, // slash dropped
		{"UPPER-123", "upper-123"},    // lowercased, digits + dash kept
		{"this-is-a-very-long-flavor", "this-is-a-very-l"}, // capped at 16 runes
	}
	for _, c := range cases {
		if got := sanitizeFlavor(c.in); got != c.want {
			t.Errorf("sanitizeFlavor(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// The shape invariant: output is non-empty, at most 16 runes, [a-z0-9-] only.
	for _, c := range cases {
		out := sanitizeFlavor(c.in)
		if out == "" || len([]rune(out)) > 16 {
			t.Errorf("sanitizeFlavor(%q) = %q violates the shape invariant", c.in, out)
		}
		for _, r := range out {
			if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-') {
				t.Errorf("sanitizeFlavor(%q) = %q carries an illegal rune %q", c.in, out, r)
			}
		}
	}
}

func TestClampScreenTail(t *testing.T) {
	// A short snapshot is returned unchanged.
	if got := clampScreenTail("short pane"); got != "short pane" {
		t.Errorf("clampScreenTail(short) = %q, want unchanged", got)
	}
	// Exactly 4096 bytes: unchanged (the boundary itself is not over the cap).
	exact := strings.Repeat("a", 4096)
	if got := clampScreenTail(exact); got != exact {
		t.Error("clampScreenTail changed a string sitting exactly at the 4096 boundary")
	}
	// Over the cap, the TAIL is kept — the prompt lives at the bottom of a screen,
	// so the head is the disposable end.
	over := strings.Repeat("a", 5000) + "PROMPT-TAIL"
	got := clampScreenTail(over)
	if len(got) > 4096 {
		t.Errorf("clamped length %d exceeds 4096", len(got))
	}
	if !strings.HasSuffix(got, "PROMPT-TAIL") {
		t.Error("clamp dropped the tail (the prompt lives there)")
	}
	if !strings.HasSuffix(over, got) {
		t.Error("clamped result is not a suffix of the input — the tail was not preserved")
	}
	// Rune safety at the boundary: 1366 three-byte runes = 4098 bytes; keeping the
	// last 4096 cuts the first rune mid-sequence, so the clamp must drop it whole
	// rather than leave a broken leading continuation byte.
	multibyte := strings.Repeat("€", 1366) // € = 3 bytes; total 4098
	mg := clampScreenTail(multibyte)
	if !utf8.ValidString(mg) {
		t.Error("clamped multibyte string is not valid UTF-8 — a rune was split")
	}
	if len(mg) > 4096 {
		t.Errorf("clamped multibyte length %d exceeds 4096", len(mg))
	}
	if !strings.HasSuffix(multibyte, mg) {
		t.Error("clamped multibyte result is not a suffix of the input")
	}
	// The one split leading rune was dropped whole, leaving 1365 intact runes.
	if n := utf8.RuneCountInString(mg); n != 1365 {
		t.Errorf("clamped multibyte kept %d runes, want 1365 whole ones", n)
	}
}

// The ConnectRemote adoption rules, against a scratch registry so no global
// state leaks between cases.
func TestConnectRemoteRules(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // ConnectRemote's save() writes under $HOME/.bridge
	r := &Registry{
		contacts: map[string]*Contact{},
		mailbox:  map[string][]MailMessage{},
		flushing: map[string]bool{},
	}

	// (d) No match: a hello mints a new live remote contact with the flavor set
	// and no tmux target.
	c1 := r.ConnectRemote("sim-otter", "/proj/a", "sess-1", "sim")
	if c1.Transport != "remote" || c1.TransportFlavor != "sim" {
		t.Fatalf("new remote contact: transport=%q flavor=%q, want remote/sim", c1.Transport, c1.TransportFlavor)
	}
	if c1.Status != "live" || c1.TmuxTarget != "" {
		t.Errorf("new remote contact: status=%q tmux_target=%q, want live/empty", c1.Status, c1.TmuxTarget)
	}

	// (b) A live REMOTE match is the same client re-hello'ing: adopt it, keep the
	// id, refresh the session.
	c1b := r.ConnectRemote("sim-otter", "/proj/a", "sess-2", "sim")
	if c1b.ID != c1.ID {
		t.Errorf("live-remote re-hello minted a new id %s (want %s) — must adopt", c1b.ID, c1.ID)
	}
	if c1b.SessionID != "sess-2" {
		t.Errorf("live-remote re-hello didn't refresh the session: %q", c1b.SessionID)
	}

	// (c) An OFFLINE match (here a formerly-tmux row) is adopted and rehomed onto
	// remote, keeping the id and clearing the stale tmux target.
	off := &Contact{ID: "off-id-0000", Name: "old-tmux", Directory: "/proj/b",
		Transport: "tmux", TmuxTarget: "@7", Status: "offline", Health: "offline"}
	r.contacts[off.ID] = off
	c2 := r.ConnectRemote("old-tmux", "/proj/b", "sess-b", "emacs")
	if c2.ID != off.ID {
		t.Errorf("offline adopt minted a new id %s (want %s)", c2.ID, off.ID)
	}
	if c2.Transport != "remote" || c2.TransportFlavor != "emacs" || c2.TmuxTarget != "" {
		t.Errorf("offline adopt didn't rehome onto remote: transport=%q flavor=%q tmux=%q",
			c2.Transport, c2.TransportFlavor, c2.TmuxTarget)
	}

	// (a) A LIVE tmux contact must NEVER be hijacked: the colliding hello mints a
	// new, suffixed remote identity and leaves the running pane untouched.
	live := &Contact{ID: "live-id-0000", Name: "marvin", Directory: "/proj/c",
		Transport: "tmux", TmuxTarget: "@9", Status: "live", Health: "ok"}
	r.contacts[live.ID] = live
	c3 := r.ConnectRemote("marvin", "/proj/c", "sess-c", "emacs")
	if c3.ID == live.ID {
		t.Fatal("remote hello HIJACKED a live tmux agent (same id) — must never adopt")
	}
	if c3.Name != "marvin-2" {
		t.Errorf("live-tmux collision: name=%q, want the suffixed marvin-2", c3.Name)
	}
	if c3.Transport != "remote" {
		t.Errorf("new suffixed contact should be remote, got %q", c3.Transport)
	}
	if live.Transport != "tmux" || live.Status != "live" || live.TmuxTarget != "@9" {
		t.Errorf("the live tmux agent was mutated: transport=%q status=%q tmux=%q",
			live.Transport, live.Status, live.TmuxTarget)
	}
}

// A contact that lived on the remote transport, went offline, then reconnected
// via tmux (Connect) must flip back to the tmux transport, or it would keep
// Transport="remote" atop a fresh window id and never deliver.
func TestConnectRevivesRemoteToTmux(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	r := &Registry{
		contacts: map[string]*Contact{},
		mailbox:  map[string][]MailMessage{},
		flushing: map[string]bool{},
	}
	c := r.ConnectRemote("quick-wolf", "/proj/x", "sess-1", "emacs")
	r.SetOffline(c.ID)
	back := r.Connect("quick-wolf", "/proj/x", "sess-1", "@42")
	if back.ID != c.ID {
		t.Errorf("tmux reconnect minted a new id %s (want %s)", back.ID, c.ID)
	}
	if back.Transport != "" {
		t.Errorf("Transport = %q after a tmux revive, want empty (tmux)", back.Transport)
	}
	if back.TransportFlavor != "" {
		t.Errorf("TransportFlavor = %q after a tmux revive, want cleared", back.TransportFlavor)
	}
	if back.TmuxTarget != "@42" {
		t.Errorf("TmuxTarget = %q, want the fresh @42", back.TmuxTarget)
	}
}

// transportFor resolves "remote" to the remote implementation, and with no lease
// every read fails safe and every action errors — mail waits durably instead of
// anything being typed on a guess.
func TestTransportForRemote(t *testing.T) {
	if _, ok := transportFor(&Contact{Transport: "remote"}).(remoteTransport); !ok {
		t.Fatal(`Transport "remote" must resolve to remoteTransport`)
	}
	c := &Contact{ID: "no-lease-0000", Name: "ghost", Transport: "remote"}
	rt := remoteTransport{}
	if rt.Alive(c) || rt.Ready(c) || rt.Capture(c) != "" {
		t.Error("remote transport must fail safe when the contact has no lease")
	}
	if rt.Deliver(c, "hi") == nil {
		t.Error("Deliver with no fresh lease must error (mail stays queued)")
	}
	if rt.SendKey(c, "1") == nil {
		t.Error("SendKey with no fresh lease must error")
	}
	// A key outside the approve whitelist is refused at the transport boundary,
	// before any parking.
	if rt.SendKey(c, "q") == nil {
		t.Error("SendKey must reject a non-whitelisted key")
	}
}

// rtDialogTail is the permission-dialog screen_tail the smoke section attests to
// exercise the attest-time card raise and the delivery belt. It is duplicated
// VERBATIM in test/smoke.sh (RT_DIALOG_TAIL); these asserts pin its shape so an
// edit to the whitespace, the ❯ selector, the numbered option, or the proceed
// vocabulary on either side can't silently rot it into a tail looksLikePrompt no
// longer recognizes — which would make the smoke card/belt checks pass vacuously.
const rtDialogTail = "Bash(git push origin main)\n" +
	"Do you want to proceed?\n" +
	" ❯ 1. Yes\n" +
	"   2. No, and tell Claude what to do differently"

func TestRemoteDialogFixtureIsARealPrompt(t *testing.T) {
	if !looksLikePrompt(rtDialogTail) {
		t.Error("rtDialogTail must satisfy looksLikePrompt — the smoke card raise depends on it")
	}
	if !paneShowsDialog(rtDialogTail) {
		t.Error("rtDialogTail must satisfy paneShowsDialog — the smoke delivery belt depends on it")
	}
}

// The delivery dialog belt (remoteTransport.Ready, work item 2): a client that
// attests ready:true with a dialog still on its screen must read NOT ready, so
// the guarded flush defers instead of typing a line whose trailing Enter would
// blind-select the highlighted option (C2/C4, remote edition). A clean tail
// leaves the client's ready:true intact. Injects a scratch lease into the
// globals under remoteMu and cleans it up, matching the file's existing patterns.
func TestRemoteReadyDialogBelt(t *testing.T) {
	const cid = "belt-contact-0000"
	const token = "belt-lease-token-0000"
	remoteMu.Lock()
	remoteLeases[token] = &remoteLease{
		token:    token,
		lastSeen: time.Now(),
		agents:   map[string]bool{cid: true},
		states:   map[string]remoteState{cid: {Ready: true, ScreenTail: rtDialogTail}},
	}
	remoteLeaseByContact[cid] = token
	remoteMu.Unlock()
	defer func() {
		remoteMu.Lock()
		remoteDeleteLeaseLocked(token)
		remoteMu.Unlock()
	}()

	rt := remoteTransport{}
	c := &Contact{ID: cid, Name: "belt-otter", Transport: "remote"}
	// Alive is dialog-agnostic — the lease is fresh regardless of what's on screen.
	if !rt.Alive(c) {
		t.Error("Alive must be true for a fresh lease regardless of the dialog belt")
	}
	if rt.Ready(c) {
		t.Error("Ready must be false while the attested tail shows a dialog (belt held)")
	}
	// Swap in a clean tail: the same ready:true now reads deliverable.
	remoteMu.Lock()
	remoteLeases[token].states[cid] = remoteState{Ready: true, ScreenTail: ""}
	remoteMu.Unlock()
	if !rt.Ready(c) {
		t.Error("Ready must be true with ready:true and no dialog on a clean tail")
	}
}
