package main

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
)

// PWA is the embedded progressive web app served to phones. The pwa/ tree is
// maintained by another teammate; the daemon only serves it, path-traversal safe.
//
//go:embed pwa
var PWA embed.FS

const (
	// defaultPort is the loopback port the daemon binds unless overridden.
	defaultPort = 8378
	// maxBodyBytes caps every request body (photos included).
	maxBodyBytes = 20 * 1024 * 1024
	// maxMessageLength caps an inbound chat message.
	maxMessageLength = 4000
	// clientIDRing is how many recent client_ids are retained for send dedup.
	clientIDRing = 200
)

// errSessionAdapter is returned by the session-adapter stubs until session.go
// wires the real tmux implementation.
var errSessionAdapter = errors.New("session adapter not loaded")

// deliverToSession injects a message into a live contact's managed tmux session
// (send-keys, literal, followed by Enter). session.go assigns the real
// implementation at startup; until then the stub reports no adapter is loaded.
var deliverToSession = func(c *Contact, text string) error { return errSessionAdapter }

// capturePrompt returns a single tmux capture-pane snapshot of a contact's
// terminal, used to render a permission card. session.go assigns the real
// implementation; the stub returns the empty string.
var capturePrompt = func(c *Contact) string { return "" }

// sendKey delivers one whitelisted approval keystroke to a live contact:
// "1"/"2"/"3"/"y"/"n" answered with Enter, or "esc" as a bare Escape. It is
// separate from deliverToSession because approvals carry no sender prefix and
// "esc" takes no Enter. session.go assigns the real implementation; the stub
// reports no adapter is loaded.
var sendKey = func(c *Contact, key string) error { return errSessionAdapter }

// approveKeys is the closed set of keystrokes the approve endpoint will deliver.
var approveKeys = map[string]bool{"1": true, "2": true, "3": true, "y": true, "n": true, "esc": true}

// localToken authenticates same-machine CLI/hook callers on /local/*; it is
// written into the lockfile on start.
var localToken string

var (
	shutdownOnce sync.Once
	shutdownCh   = make(chan struct{})
)

// requestShutdown triggers a graceful stop of the daemon (used by lockdown).
func requestShutdown() {
	shutdownOnce.Do(func() { close(shutdownCh) })
}

// lockfile is ~/.bridge/daemon.json: the port and local-trust token a CLI or
// hook on this machine uses to reach the running daemon.
type lockfile struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
}

func lockfilePath() string { return bridgePath("daemon.json") }

// writeLockfile records the port and local token 0600 for CLI/hook callers.
func writeLockfile(port int, token string) error {
	data, _ := json.Marshal(lockfile{Port: port, Token: token})
	return writeFilePrivate(lockfilePath(), data)
}

// removeOwnLockfile deletes the lockfile only when it still carries our token —
// so a daemon that took over after us keeps its lockfile intact (review H4).
func removeOwnLockfile(token string) {
	if lf, err := readLockfile(); err == nil && lf.Token != token {
		return // someone else owns it now; leave it
	}
	_ = os.Remove(lockfilePath())
}

// readLockfile reads the running daemon's port and local token.
func readLockfile() (lockfile, error) {
	var lf lockfile
	data, err := os.ReadFile(lockfilePath())
	if err != nil {
		return lf, err
	}
	return lf, json.Unmarshal(data, &lf)
}

// runServe starts the daemon: it loads persisted state, writes the lockfile,
// and serves the API on 127.0.0.1:port until interrupted or locked down.
func runServe(port int) error {
	if err := ensureBridgeDir(); err != nil {
		return err
	}
	loadConfig()
	loadTokens()
	loadRegistry()
	loadHistory()
	loadTails() // restore per-contact tail offsets so a restart resumes, not skips (4b)

	daemonStartUnix = timeNowUnix() // exposed on /api/status; anchors the wake watchdog

	// Bind the port BEFORE claiming the lockfile (review H4). If another daemon
	// already owns the port, fail here having touched nothing — so a stray
	// second `bridge serve` (or a KeepAlive relaunch racing a manual start)
	// can't overwrite the live daemon's local-trust token, and its deferred
	// cleanup can't delete the live lockfile out from under it.
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("could not bind 127.0.0.1:%d (bridge already running?): %w", port, err)
	}

	localToken = randomHex(32)
	if err := writeLockfile(port, localToken); err != nil {
		ln.Close()
		return err
	}
	// Remove the lockfile on exit only if it is still OURS. A daemon that
	// started after us owns the lockfile now; deleting it would blind every
	// CLI/hook caller (they'd present a token no daemon knows).
	defer removeOwnLockfile(localToken)

	startHeartbeat()
	startSessionManager() // assigned in session.go: tail loops + liveness
	if err := loadVAPID(); err != nil {
		fmt.Printf("(push disabled: %v)\n", err)
	}
	loadPushSubs()
	initPlugins() // hook runtime: docs/plugins.md

	srv := &http.Server{Handler: http.HandlerFunc(route)}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		select {
		case <-sig:
		case <-shutdownCh:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	fmt.Printf("bridge daemon on http://127.0.0.1:%d\n", port)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// route is the single dispatch point. Local CLI/hook calls authenticate with
// the lockfile token; everything else passes the tailnet identity gate first,
// then (for /api, except pairing) the per-device token gate.
func route(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/local/") {
		handleLocal(w, r)
		return
	}

	// Perimeter: no acceptable tailnet identity -> drop.
	id, ok := identity(r.Header.Get("Tailscale-User-Login"))
	if !ok {
		audit("rejected-identity", r.URL.Path, "")
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}

	// Static assets and the app shell need no device token: the pairing
	// screen itself lives there. The identity check above still applied.
	if r.Method == http.MethodGet && !strings.HasPrefix(r.URL.Path, "/api/") {
		serveStatic(w, r)
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == "/api/pair" {
		handlePair(w, r, id)
		return
	}

	// Everything else under /api requires a paired device.
	if !tokenValid(requestToken(r)) {
		audit("rejected-token", r.URL.Path, id)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "pair-required"})
		return
	}

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/events":
		handleEvents(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/api/status":
		handleStatus(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/api/history":
		handleHistory(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/api/send":
		handleSend(w, r, id)
	case r.Method == http.MethodPost && r.URL.Path == "/api/approve":
		handleApprove(w, r, id)
	case r.Method == http.MethodPost && r.URL.Path == "/api/interrupt":
		handleInterrupt(w, r, id)
	case r.Method == http.MethodPost && r.URL.Path == "/api/upload":
		handleUpload(w, r, id)
	case r.Method == http.MethodGet && r.URL.Path == "/api/push/key":
		handlePushKey(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/api/push/subscribe":
		handlePushSubscribe(w, r)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not-found"})
	}
}

// writeJSON sends v as a no-store JSON response with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// readBody reads a size-capped request body, writing a 413 (or 400) response
// and returning false when the client exceeds the cap or the read fails.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "too-large"})
		} else {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad-body"})
		}
		return nil, false
	}
	return data, true
}

var (
	seenMu   sync.Mutex
	seenIDs  []string
	inFlight = map[string]bool{}
)

// claimClientID reserves a client_id for delivery under a single lock, closing
// the TOCTOU window the H1 dedup split reintroduced (#3). It returns true only
// when id is neither already committed (a completed send) nor currently
// in-flight, and in that case marks it in-flight: the caller then owns the claim
// and MUST resolve it once via releaseClientID. A false return is a real
// duplicate — the first attempt already succeeded, or a racing retry's original
// delivery is still running — so the caller acks it without delivering, and two
// racing retries can never both send-keys. A blank id can't be deduped, so it is
// always claimed and never tracked (its release is a no-op).
func claimClientID(id string) bool {
	if id == "" {
		return true
	}
	seenMu.Lock()
	defer seenMu.Unlock()
	for _, s := range seenIDs {
		if s == id {
			return false
		}
	}
	if inFlight[id] {
		return false
	}
	inFlight[id] = true
	return true
}

// releaseClientID resolves a claim taken by claimClientID. It clears the
// in-flight mark either way; when ok it also records id as durably handled
// (delivered to a live session or queued to a mailbox) so a later retry is
// acknowledged without redelivery. A failed attempt (ok=false) leaves no trace,
// so its retry runs for real instead of being swallowed as a "duplicate" —
// preserving H1's guarantee that an id is committed only after a durable accept.
func releaseClientID(id string, ok bool) {
	if id == "" {
		return
	}
	seenMu.Lock()
	defer seenMu.Unlock()
	delete(inFlight, id)
	if !ok {
		return
	}
	for _, s := range seenIDs {
		if s == id {
			return // already recorded
		}
	}
	seenIDs = append([]string{id}, seenIDs...)
	if len(seenIDs) > clientIDRing {
		seenIDs = seenIDs[:clientIDRing]
	}
}

// formatInbound builds the prefix a delivered message wears in the agent's
// terminal, collapsing whitespace so it lands as a single send-keys line and
// stripping control bytes. newline/CR/tab become a single space; every other
// non-printable rune (ESC, arrow/cursor sequences, other C0/C1 controls, format
// characters) is dropped so a peer message can't drive the recipient agent's TUI
// (L4). Printable characters, including non-ASCII graphics, pass through.
func formatInbound(from, via, text string) string {
	flat := strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t':
			return ' '
		}
		if !unicode.IsPrint(r) {
			return -1 // drop
		}
		return r
	}, text)
	return fmt.Sprintf("[From %s (%s)]: %s", from, via, strings.TrimSpace(flat))
}

// neutralizeFrame rewrites the daemon's framing alphabet out of a message
// body: "[From " heads become "['From " and the "⏎" batch separator becomes
// "↵". After this, the one line an agent receives can only carry
// daemon-authored provenance and message boundaries — a body cannot forge a
// second sender or splice a fake message into a coalesced batch (H9). The
// rewrite is visible but gentle: quoted frames stay readable, they just stop
// matching the daemon's exact format.
func neutralizeFrame(s string) string {
	s = strings.ReplaceAll(s, "[From ", "['From ")
	return strings.ReplaceAll(s, "⏎", "↵")
}

// handleLocalRetire removes an OFFLINE contact from the roster (`bridge
// retire <name>`). Live contacts are refused by Registry.Retire — a running
// agent must lose its window before it can lose its registration — so a
// shared name can only ever match the ghost, never the living.
func handleLocalRetire(w http.ResponseWriter, r *http.Request) {
	data, ok := readBody(w, r)
	if !ok {
		return
	}
	var req struct {
		Contact string `json:"contact"`
	}
	_ = json.Unmarshal(data, &req)
	c := registry.Retire(strings.TrimSpace(req.Contact))
	if c == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not-found-or-live"})
		return
	}
	audit("retire", c.Name+" ("+c.ID[:8]+")", "local")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": c.Name})
}

// cspPolicy is the strict Content-Security-Policy served with the app shell and
// every static asset (M7). Same-origin only, no inline/eval script or style,
// the page cannot be framed and cannot set a <base>. It is the second line of
// defense if agent output ever reaches an innerHTML sink. Images additionally
// allow data: (attachment preview thumbnails) and blob: — downscale() loads the
// picked photo through URL.createObjectURL before re-encoding, and without
// blob: the CSP silently kills photo attach (found live 2026-07-06; blob:
// object URLs are same-origin-created, so this widens nothing for injected
// content).
const cspPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data: blob:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'"

var staticTypes = map[string]string{
	".html":        "text/html; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".json":        "application/json",
	".webmanifest": "application/manifest+json",
	".png":         "image/png",
	".jpg":         "image/jpeg",
	".svg":         "image/svg+xml",
	".ico":         "image/x-icon",
}

// serveStatic serves the embedded PWA. Traversal is impossible: fs.ValidPath
// rejects "..", absolute, and dot segments, and an embed.FS holds no symlinks.
func serveStatic(w http.ResponseWriter, r *http.Request) {
	sub, err := fs.Sub(PWA, "pwa")
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" {
		name = "index.html"
	}
	if !fs.ValidPath(name) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	data, err := fs.ReadFile(sub, name)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	ct := staticTypes[strings.ToLower(path.Ext(name))]
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Security-Policy", cspPolicy)
	_, _ = w.Write(data)
}

// handlePair redeems a pairing code and, on success, sets the device token as
// an HttpOnly Secure SameSite=Lax cookie.
func handlePair(w http.ResponseWriter, r *http.Request, id string) {
	data, ok := readBody(w, r)
	if !ok {
		return
	}
	var req struct {
		Code   string `json:"code"`
		Device string `json:"device"`
	}
	_ = json.Unmarshal(data, &req)

	token := tryPair(req.Code, req.Device)
	if token == "" {
		audit("pair-failed", "", id)
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "bad-code"})
		return
	}
	audit("paired", req.Device, id)
	http.SetCookie(w, &http.Cookie{
		Name:     "bridge_token",
		Value:    token,
		Path:     "/",
		MaxAge:   31536000,
		HttpOnly: true,
		Secure:   true,
		// Lax, not Strict: standalone iOS PWAs drop Strict cookies on
		// EventSource/background requests, breaking the live event stream.
		// The tailnet perimeter is the real CSRF defense, and no GET mutates.
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleStatus returns the roster and daemon version.
func handleStatus(w http.ResponseWriter, r *http.Request) {
	type item struct {
		ID        string            `json:"id"`
		Name      string            `json:"name"`
		Directory string            `json:"directory"`
		Status    string            `json:"status"`
		Health    string            `json:"health"`
		Attention bool              `json:"attention"`
		Fields    map[string]string `json:"fields,omitempty"` // plugin annotations
	}
	items := []item{}
	for _, c := range registry.Roster() {
		items = append(items, item{c.ID, c.Name, c.Directory, c.Status, c.Health, c.PromptOpen, c.Fields})
	}
	// Clocks for phone-side presence truth (round 4): the phone compares `now`
	// to its own clock and its last-contact timestamp to distinguish "my
	// network is down" from "Mac unreachable"; `started` reveals a daemon
	// restart; `wake_from`/`wake_to` surface the most recent sleep window so a
	// reconnecting phone can say "Mac was asleep 10:02–12:55" instead of a bare
	// "unreachable".
	resp := map[string]any{
		"contacts": items,
		"version":  version,
		"now":      timeNowUnix(),
		"started":  daemonStartUnix,
	}
	if from, to := wakeGap(); to != 0 {
		resp["wake_from"], resp["wake_to"] = from, to
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleHistory returns stored events newer than ?since=N.
func handleHistory(w http.ResponseWriter, r *http.Request) {
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	evs := eventsSince(since)
	if evs == nil {
		evs = []Event{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": evs})
}

// handleEvents streams Server-Sent Events, replaying anything past the client's
// cursor (Last-Event-ID header or ?since) before switching to live delivery.
func handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	since := int64(0)
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		since, _ = strconv.ParseInt(v, 10, 64)
	} else if v := r.URL.Query().Get("since"); v != "" {
		since, _ = strconv.ParseInt(v, 10, 64)
	}

	// Register before replaying so events emitted during replay are not lost;
	// an event landing in both is harmless (the client dedups by id).
	client := &sseClient{ch: make(chan string, 64)}
	addSSEClient(client)
	defer removeSSEClient(client)

	for _, ev := range eventsSince(since) {
		_, _ = io.WriteString(w, frameFor(ev))
	}
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case frame, ok := <-client.ch:
			if !ok {
				return
			}
			if _, err := io.WriteString(w, frame); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// handleSend delivers a phone message to a live contact, or mailboxes it and
// returns 409 when the contact is offline.
func handleSend(w http.ResponseWriter, r *http.Request, id string) {
	data, ok := readBody(w, r)
	if !ok {
		return
	}
	var req struct {
		Agent    string `json:"agent"`
		Text     string `json:"text"`
		ClientID string `json:"client_id"`
	}
	_ = json.Unmarshal(data, &req)

	if strings.TrimSpace(req.Text) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty"})
		return
	}
	if len(req.Text) > maxMessageLength {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "too-long"})
		return
	}
	// Reserve the client_id before delivering: a completed send, or a racing
	// retry whose original delivery is still in flight, is acked as a duplicate
	// without re-delivering — closing the window where the phone's 10s-timeout
	// retry and the slow original both reach send-keys (#3). The claim is
	// released below: committed on a durable accept, dropped on failure so a
	// genuine retry runs for real (H1).
	if !claimClientID(req.ClientID) {
		audit("send-duplicate-dropped", req.Text, id)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true, "duplicate": true})
		return
	}

	c := registry.Resolve(req.Agent)
	if c == nil || c.Status != "live" {
		if c != nil {
			registry.Queue(c.ID, MailMessage{From: authConfig.UserMention, Via: "phone", Text: req.Text, TS: nowUTC()})
			releaseClientID(req.ClientID, true) // durably queued: a retry must not re-queue
			// message.in fires on durable ACCEPT, not send-keys success — a
			// queued message is in the system (same standard H1 holds the
			// client_id to). data.queued lets plugins tell the paths apart.
			dispatchPluginEvent("message.in", c, map[string]any{"text": req.Text, "via": "phone", "queued": true})
		} else {
			releaseClientID(req.ClientID, false) // nothing durable happened: allow the retry
		}
		audit("send-offline", req.Agent, id)
		writeJSON(w, http.StatusConflict, map[string]string{"error": "offline"})
		return
	}

	// Live contact: hold-and-batch instead of immediate send-keys. The message
	// is durably queued (mailbox) before the ack, so "ok" still means "the
	// daemon has it"; delivery follows when the burst window closes
	// (coalesce.go). Emitted:true — the "sent" event below is the one the
	// thread shows; the flush must not emit a duplicate.
	holdInbound(c, MailMessage{From: authConfig.UserMention, Via: "phone", Text: req.Text, TS: nowUTC(), Emitted: true})
	releaseClientID(req.ClientID, true) // durably queued: a retry is now a safe duplicate ack
	audit("send", c.Name+": "+req.Text, id)
	Emit("sent", c.ID, c.Name, req.Text, req.ClientID)
	dispatchPluginEvent("message.in", c, map[string]any{"text": req.Text, "via": "phone", "queued": false})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleInterrupt delivers a bare Escape to a live contact — stop mid-thought,
// from the phone (field request, 2026-07-06: rejecting permission prompts was
// the only interrupt available). Unlike approve, this is honored with or
// without an open prompt: Escape aborts a running turn, and on an open dialog
// it is the dialog's own cancel. Any held burst is then delivered immediately —
// the agent just went idle and should hear what was waiting.
func handleInterrupt(w http.ResponseWriter, r *http.Request, id string) {
	data, ok := readBody(w, r)
	if !ok {
		return
	}
	var req struct {
		Agent string `json:"agent"`
	}
	_ = json.Unmarshal(data, &req)

	c := registry.Resolve(req.Agent)
	if c == nil || c.Status != "live" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "offline"})
		return
	}
	if err := sendKey(c, "esc"); err != nil {
		audit("interrupt-failed", err.Error(), id)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	audit("interrupt", c.Name, id)
	Emit("interrupted", c.ID, c.Name, "")
	go coalesceDeliver(c.ID)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleApprove delivers a whitelisted keystroke, honored only while the
// contact is hook-attested as prompting (no blind key injection).
func handleApprove(w http.ResponseWriter, r *http.Request, id string) {
	data, ok := readBody(w, r)
	if !ok {
		return
	}
	var req struct {
		Agent string `json:"agent"`
		Key   string `json:"key"`
	}
	_ = json.Unmarshal(data, &req)

	if !approveKeys[req.Key] {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "key-not-allowed"})
		return
	}
	c := registry.Resolve(req.Agent)
	if c == nil || c.Status != "live" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "offline"})
		return
	}
	if !c.PromptOpen {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "not-waiting"})
		return
	}
	if err := sendKey(c, req.Key); err != nil {
		audit("approve-failed", err.Error(), id)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	audit("approve", c.Name+" <- "+req.Key, id)
	Emit("approved", c.ID, c.Name, req.Key)
	// The lock-screen "needs you" notification is now stale; replace it with a
	// same-tag ✓ so it can't sit there demanding attention it already got.
	clearAttnPush(c.ID, c.Name)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleUpload saves a photo locally under a server-chosen name (0600) and
// hands the agent the path to Read. Client filenames are never trusted.
func handleUpload(w http.ResponseWriter, r *http.Request, id string) {
	data, ok := readBody(w, r)
	if !ok {
		return
	}
	var req struct {
		Agent    string `json:"agent"`
		Text     string `json:"text"`
		Image    string `json:"image"`
		ClientID string `json:"client_id"`
	}
	_ = json.Unmarshal(data, &req)

	img, err := base64.StdEncoding.DecodeString(req.Image)
	if err != nil || len(img) < 100 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad-image"})
		return
	}
	// Reserve the client_id (same TOCTOU-closing claim as handleSend, #3): a
	// completed or still-in-flight upload is acked as a duplicate; the claim is
	// released below — committed on delivery, dropped on any failure so the retry
	// re-runs (H1).
	if !claimClientID(req.ClientID) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true, "duplicate": true})
		return
	}
	c := registry.Resolve(req.Agent)
	if c == nil || c.Status != "live" {
		releaseClientID(req.ClientID, false) // never delivered: allow the retry
		writeJSON(w, http.StatusConflict, map[string]string{"error": "offline"})
		return
	}
	pathOnDisk, err := saveAttachment(img)
	if err != nil {
		releaseClientID(req.ClientID, false) // nothing durable: allow the retry
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "save-failed"})
		return
	}
	msg := fmt.Sprintf("%s [photo saved at %s — use the Read tool to view it]", strings.TrimSpace(req.Text), pathOnDisk)
	// Same hold-and-batch as handleSend, so a photo can never overtake the
	// held texts that were sent before it (ordering lives in one queue).
	holdInbound(c, MailMessage{From: authConfig.UserMention, Via: "phone", Text: msg, TS: nowUTC(), Emitted: true})
	releaseClientID(req.ClientID, true) // durably queued: a retry is now a safe duplicate ack
	audit("upload", fmt.Sprintf("%s <- %s (%d bytes)", c.Name, pathOnDisk, len(img)), id)
	Emit("sent", c.ID, c.Name, strings.TrimSpace(req.Text)+" 📷 photo", req.ClientID)
	dispatchPluginEvent("message.in", c, map[string]any{"text": msg, "via": "phone", "queued": false})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// saveAttachment writes image bytes under ~/.bridge/attachments with a
// server-chosen, timestamped name (0600).
func saveAttachment(img []byte) (string, error) {
	dir := bridgePath("attachments")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := "photo-" + time.Now().UTC().Format("20060102-150405.000000000") + imageExt(img)
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, img, 0o600); err != nil {
		return "", err
	}
	_ = os.Chmod(p, 0o600)
	return p, nil
}

// imageExt sniffs the file type so the saved photo carries a sensible extension.
func imageExt(img []byte) string {
	switch http.DetectContentType(img) {
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ".jpg"
	}
}

// handleLocal routes same-machine CLI/hook requests, authenticated by the
// lockfile token rather than the tailnet identity + device token.
func handleLocal(w http.ResponseWriter, r *http.Request) {
	if localToken == "" || requestToken(r) != localToken {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "local-auth"})
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/local/connect":
		handleLocalConnect(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/local/event":
		handleLocalEvent(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/local/send":
		handleLocalSend(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/local/contacts":
		cs := registry.Roster()
		if cs == nil {
			cs = []*Contact{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"contacts": cs})
	case r.Method == http.MethodPost && r.URL.Path == "/local/pair":
		writeJSON(w, http.StatusOK, map[string]string{"code": issuePairingCode()})
	case r.Method == http.MethodPost && r.URL.Path == "/local/retire":
		handleLocalRetire(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/local/lockdown":
		revokeAllDevices()
		audit("lockdown", "revoke-all + shutdown", "local")
		pluginOff.Store(true) // no plugin runs again this process (docs/plugins.md)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		requestShutdown()
	case r.Method == http.MethodPost && r.URL.Path == "/local/push-test":
		handlePushTest(w, r)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not-found"})
	}
}

// handleLocalConnect registers or revives a contact and flushes any queued
// messages it accumulated while offline.
func handleLocalConnect(w http.ResponseWriter, r *http.Request) {
	data, ok := readBody(w, r)
	if !ok {
		return
	}
	var req struct {
		Name       string `json:"name"`
		Directory  string `json:"directory"`
		SessionID  string `json:"session_id"`
		TmuxTarget string `json:"tmux_target"`
	}
	if json.Unmarshal(data, &req) != nil || req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad-request"})
		return
	}
	c := registry.Connect(req.Name, req.Directory, req.SessionID, req.TmuxTarget)
	audit("connect", c.Name+" "+c.Directory, "local")
	Emit("connected", c.ID, c.Name, "")
	flushMailbox(c)
	writeJSON(w, http.StatusOK, map[string]string{"id": c.ID, "name": c.Name})
}

// flushMailbox delivers a contact's queued messages in order, combining each
// run of consecutive messages from one sender+channel into a single delivery
// line ("msg1 ⏎ msg2 ⏎ msg3") — a burst of phone texts lands as one thought,
// not three turn-starting fragments (the coalescing UX, coalesce.go). Groups
// are removed from disk only after their delivery succeeds, so a crash
// mid-drain redelivers at most one group instead of losing the batch (M6,
// group-granular). Concurrent flushes of one mailbox coalesce to one.
func flushMailbox(c *Contact) {
	if !registry.BeginFlush(c.ID) {
		return
	}
	defer registry.EndFlush(c.ID)
	for {
		group := registry.PeekMailboxGroup(c.ID)
		if len(group) == 0 {
			return
		}
		// THE guard — the one the four criticals converge on (review
		// 2026-07-06). Never send-keys into a pane that is the pre-Claude bare
		// shell or is showing a permission dialog (a trailing Enter would
		// blind-select its highlighted option and the mail would be swallowed).
		// Leave everything queued and re-arm; the reconcile loop retries every
		// tick and coalesceRearm covers the fast path — the mailbox is durable,
		// so a deferral is never a loss.
		if !paneReadyForDelivery(c) {
			coalesceRearm(c.ID)
			return
		}
		parts := make([]string, 0, len(group))
		for _, m := range group {
			// Neutralize per part, before the join: only the daemon may write
			// a "[From …]" head or a "⏎" boundary into the delivered line (H9).
			parts = append(parts, neutralizeFrame(m.Text))
		}
		text := strings.Join(parts, " ⏎ ")
		if group[0].From != "" {
			text = formatInbound(group[0].From, group[0].Via, text)
		}
		if err := deliverToSession(c, text); err != nil {
			return // leave the group (and the rest) queued for the next flush
		}
		for _, m := range group {
			if m.Emitted {
				continue
			}
			// Offline-queued mail emits on delivery: peer messages wear their
			// author (see handleLocalSend), phone messages remain "sent".
			if m.Via == "bridge" {
				Emit("peer", c.ID, m.From, m.Text)
			} else {
				Emit("sent", c.ID, c.Name, m.Text)
			}
		}
		registry.DropMailbox(c.ID, group)
	}
}

// handleLocalEvent processes a Notification-hook POST {session_id, message,
// kind}: an idle notification clears the prompt state; anything else captures
// one pane snapshot and marks the contact as prompting.
func handleLocalEvent(w http.ResponseWriter, r *http.Request) {
	data, ok := readBody(w, r)
	if !ok {
		return
	}
	var req struct {
		SessionID string `json:"session_id"`
		Message   string `json:"message"`
		Kind      string `json:"kind"`
	}
	_ = json.Unmarshal(data, &req)

	c := registry.BySession(req.SessionID)
	if c == nil {
		// No contact claims this session id. For a bridge-managed agent this
		// is the signature of a session roll (/clear, auto-compaction) the
		// daemon hasn't adopted yet — and the hook fires exactly ONCE per
		// event, so dropping it here would lose the prompt permanently (H8).
		// Park it: the reconcile loop nudges the chain scan and retries until
		// the roll is adopted or the event expires. (Most misses are OTHER
		// Claude sessions on this machine — the hook install is global — and
		// those simply age out.)
		parkHookEvent(req.SessionID)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true, "parked": true})
		return
	}
	applyHookEvent(c)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// applyHookEvent reconciles a contact's attention state with its pane after a
// Notification-hook event. The hook fires for both permission prompts and
// routine idle/waiting notifications; don't trust the notification kind alone —
// gate the attention card on the terminal actually showing a permission dialog
// right now. An idle notification, or a screen with no prompt, clears any open
// prompt instead of raising a false card.
//
// The dialog may still be painting when the hook fires, so the capture retries
// briefly until it looks like a prompt rather than grabbing a stale frame.
func applyHookEvent(c *Contact) {
	snapshot := capturePrompt(c)
	for i := 0; i < 5 && !looksLikePrompt(snapshot); i++ {
		time.Sleep(150 * time.Millisecond)
		snapshot = capturePrompt(c)
	}
	if looksLikePrompt(snapshot) {
		registry.SetPrompt(c.ID, true)
		Emit("attention", c.ID, c.Name, snapshot)
		notifyPush(c.Name+" needs you", firstPromptLine(snapshot), "attn-"+c.ID, c.ID)
		markAttnPushed(c.ID)
		dispatchPluginEvent("permission.prompt", c, map[string]any{"prompt": firstPromptLine(snapshot)})
	} else {
		if c.PromptOpen {
			registry.SetPrompt(c.ID, false)
			Emit("attention-clear", c.ID, c.Name, "")
			clearAttnPush(c.ID, c.Name)
		}
	}
}

// pendingHooks parks hook events whose session id resolved to no contact (H8).
// Guarded by pendingHookMu; drained by the reconcile goroutine.
type hookEvent struct {
	sessionID string
	at        time.Time
	nudged    bool
}

var (
	pendingHookMu sync.Mutex
	pendingHooks  []hookEvent
)

const (
	maxPendingHooks   = 32
	pendingHookMaxAge = 60 * time.Second
)

// parkHookEvent queues an unresolvable hook event for retry, one entry per
// session id (re-applying reads the CURRENT pane, so the latest state wins).
func parkHookEvent(sessionID string) {
	if sessionID == "" {
		return
	}
	pendingHookMu.Lock()
	defer pendingHookMu.Unlock()
	for _, ev := range pendingHooks {
		if ev.sessionID == sessionID {
			return
		}
	}
	if len(pendingHooks) >= maxPendingHooks {
		pendingHooks = pendingHooks[1:]
	}
	pendingHooks = append(pendingHooks, hookEvent{sessionID: sessionID, at: time.Now()})
}

// drainPendingHooks retries parked hook events. Reconcile goroutine only (it
// writes chainForce). For each parked id: once a contact claims it, apply the
// event against the live pane; until then, nudge every live contact whose
// project dir holds that session file, so sessionFileFor re-scans its rollover
// chain on this very pass instead of after the two-minute quiet window — the
// gap that used to swallow the one hook delivery a roll ever gets.
func drainPendingHooks() {
	pendingHookMu.Lock()
	events := append([]hookEvent(nil), pendingHooks...)
	pendingHookMu.Unlock()
	if len(events) == 0 {
		return
	}
	keep := events[:0]
	for _, ev := range events {
		if time.Since(ev.at) > pendingHookMaxAge {
			continue // an uninstrumented session's hook; let it age out
		}
		if c := registry.BySession(ev.sessionID); c != nil {
			applyHookEvent(c)
			continue
		}
		if !ev.nudged {
			file := ev.sessionID + ".jsonl"
			for _, c := range registry.Roster() {
				if c.Status != "live" {
					continue
				}
				if _, err := os.Stat(filepath.Join(projectDir(c.Directory), file)); err == nil {
					chainForce[c.ID] = true
				}
			}
			ev.nudged = true
		}
		keep = append(keep, ev)
	}
	pendingHookMu.Lock()
	// New events may have parked while we worked; keep those arrivals too.
	pendingHooks = append(append([]hookEvent(nil), keep...), pendingHooks[len(events):]...)
	pendingHookMu.Unlock()
}

// handleLocalSend handles an agent-composed outbound message. Without "to" the
// text is surfaced to the phone in the sender's thread; with "to" it is routed
// agent-to-agent through the same mailboxes (the switchboard).
func handleLocalSend(w http.ResponseWriter, r *http.Request) {
	data, ok := readBody(w, r)
	if !ok {
		return
	}
	var req struct {
		Contact string `json:"contact"` // the calling agent (from BRIDGE_CONTACT)
		Text    string `json:"text"`
		To      string `json:"to"` // optional switchboard target
	}
	_ = json.Unmarshal(data, &req)

	if strings.TrimSpace(req.Text) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty"})
		return
	}

	// The sender must resolve to a registered contact: senderName becomes the
	// "From" in the recipient's provenance frame and the byline on the phone,
	// so relaying an arbitrary string here is an identity-forgery vector (H9).
	s := registry.Resolve(req.Contact)
	if s == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown sender"})
		return
	}
	senderName, senderID := s.Name, s.ID

	if req.To == "" {
		audit("agent-send", senderName+": "+req.Text, "local")
		Emit("reply", senderID, senderName, req.Text)
		notifyPush(senderName, req.Text, "msg-"+senderID, senderID)
		dispatchPluginEvent("reply.out", registry.Resolve(senderID), map[string]any{"text": req.Text})
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}

	target := registry.Resolve(req.To)
	if target == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such agent"})
		return
	}
	if target.Status != "live" {
		// Durably queued IS success — the daemon delivers it on revival. The
		// old 409 here read as failure, so well-behaved senders retried and
		// the recipient woke up to duplicates (review round 2).
		registry.Queue(target.ID, MailMessage{From: senderName, Via: "bridge", Text: req.Text, TS: nowUTC()})
		audit("switchboard-queued", senderName+" -> "+target.Name+": "+req.Text, "local")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "queued": true})
		return
	}
	// Switchboard rides the same hold-and-batch as phone sends: it keeps one
	// ordered queue per recipient and, critically, never types past an open
	// permission dialog (the hold refuses to fire while one is up).
	//
	// The event is "peer", Name = the SENDER: an agent-to-agent message in the
	// recipient's thread must wear its author, not render as one of the user's
	// own bubbles (found live 2026-07-06 — Marvin's status check showed as
	// "you → vint" on the phone).
	audit("switchboard", senderName+" -> "+target.Name+": "+req.Text, "local")
	Emit("peer", target.ID, senderName, req.Text)
	holdInbound(target, MailMessage{From: senderName, Via: "bridge", Text: req.Text, TS: nowUTC(), Emitted: true})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
