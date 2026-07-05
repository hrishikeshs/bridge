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

	localToken = randomHex(32)
	if err := writeLockfile(port, localToken); err != nil {
		return err
	}
	defer os.Remove(lockfilePath())

	startHeartbeat()
	startSessionManager() // assigned in session.go: tail loops + liveness

	srv := &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", port),
		Handler: http.HandlerFunc(route),
	}

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
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
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
	case r.Method == http.MethodPost && r.URL.Path == "/api/upload":
		handleUpload(w, r, id)
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
	seenMu  sync.Mutex
	seenIDs []string
)

// duplicateClientID records id and reports whether it was already seen.
// Retrying a send is always safe: a repeated id is acknowledged, never
// redelivered.
func duplicateClientID(id string) bool {
	if id == "" {
		return false
	}
	seenMu.Lock()
	defer seenMu.Unlock()
	for _, s := range seenIDs {
		if s == id {
			return true
		}
	}
	seenIDs = append([]string{id}, seenIDs...)
	if len(seenIDs) > clientIDRing {
		seenIDs = seenIDs[:clientIDRing]
	}
	return false
}

// formatInbound builds the prefix a delivered message wears in the agent's
// terminal, collapsing newlines so it lands as a single send-keys line.
func formatInbound(from, via, text string) string {
	flat := strings.TrimSpace(strings.NewReplacer("\n", " ", "\r", " ").Replace(text))
	return fmt.Sprintf("[From %s (%s)]: %s", from, via, flat)
}

var staticTypes = map[string]string{
	".html":        "text/html; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".json":        "application/json",
	".webmanifest": "application/manifest+json",
	".png":         "image/png",
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
	_, _ = w.Write(data)
}

// handlePair redeems a pairing code and, on success, sets the device token as
// an HttpOnly Secure SameSite=Strict cookie.
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
		SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleStatus returns the roster and daemon version.
func handleStatus(w http.ResponseWriter, r *http.Request) {
	type item struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Directory string `json:"directory"`
		Status    string `json:"status"`
		Health    string `json:"health"`
		Attention bool   `json:"attention"`
	}
	items := []item{}
	for _, c := range registry.Roster() {
		items = append(items, item{c.ID, c.Name, c.Directory, c.Status, c.Health, c.PromptOpen})
	}
	writeJSON(w, http.StatusOK, map[string]any{"contacts": items, "version": version})
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
	if duplicateClientID(req.ClientID) {
		audit("send-duplicate-dropped", req.Text, id)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true, "duplicate": true})
		return
	}

	c := registry.Resolve(req.Agent)
	if c == nil || c.Status != "live" {
		if c != nil {
			registry.Queue(c.ID, MailMessage{From: authConfig.UserMention, Via: "phone", Text: req.Text, TS: nowUTC()})
		}
		audit("send-offline", req.Agent, id)
		writeJSON(w, http.StatusConflict, map[string]string{"error": "offline"})
		return
	}

	if err := deliverToSession(c, formatInbound(authConfig.UserMention, "phone", req.Text)); err != nil {
		audit("send-failed", err.Error(), id)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	audit("send", c.Name+": "+req.Text, id)
	Emit("sent", c.ID, c.Name, req.Text)
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
	if duplicateClientID(req.ClientID) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true, "duplicate": true})
		return
	}
	c := registry.Resolve(req.Agent)
	if c == nil || c.Status != "live" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "offline"})
		return
	}
	pathOnDisk, err := saveAttachment(img)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "save-failed"})
		return
	}
	msg := fmt.Sprintf("%s [photo saved at %s — use the Read tool to view it]", strings.TrimSpace(req.Text), pathOnDisk)
	if err := deliverToSession(c, formatInbound(authConfig.UserMention, "phone", msg)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	audit("upload", fmt.Sprintf("%s <- %s (%d bytes)", c.Name, pathOnDisk, len(img)), id)
	Emit("sent", c.ID, c.Name, strings.TrimSpace(req.Text)+" 📷 photo")
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
	case r.Method == http.MethodPost && r.URL.Path == "/local/lockdown":
		revokeAllDevices()
		audit("lockdown", "revoke-all + shutdown", "local")
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		requestShutdown()
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

// flushMailbox delivers a revived contact's queued messages in order, stopping
// and requeuing the remainder at the first delivery failure.
func flushMailbox(c *Contact) {
	msgs := registry.TakeMailbox(c.ID)
	for i, m := range msgs {
		text := m.Text
		if m.From != "" {
			text = formatInbound(m.From, m.Via, m.Text)
		}
		if err := deliverToSession(c, text); err != nil {
			registry.Requeue(c.ID, msgs[i:])
			return
		}
		Emit("sent", c.ID, c.Name, m.Text)
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
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true, "ignored": true})
		return
	}
	if strings.Contains(strings.ToLower(req.Kind), "idle") {
		registry.SetPrompt(c.ID, false)
		Emit("attention-clear", c.ID, c.Name, "")
	} else {
		snapshot := capturePrompt(c)
		if snapshot == "" {
			snapshot = req.Message
		}
		registry.SetPrompt(c.ID, true)
		Emit("attention", c.ID, c.Name, snapshot)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
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

	senderName, senderID := req.Contact, ""
	if s := registry.Resolve(req.Contact); s != nil {
		senderName, senderID = s.Name, s.ID
	}

	if req.To == "" {
		audit("agent-send", senderName+": "+req.Text, "local")
		Emit("reply", senderID, senderName, req.Text)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}

	target := registry.Resolve(req.To)
	if target == nil || target.Status != "live" {
		if target != nil {
			registry.Queue(target.ID, MailMessage{From: senderName, Via: "bridge", Text: req.Text, TS: nowUTC()})
		}
		writeJSON(w, http.StatusConflict, map[string]string{"error": "offline"})
		return
	}
	if err := deliverToSession(target, formatInbound(senderName, "bridge", req.Text)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	audit("switchboard", senderName+" -> "+target.Name+": "+req.Text, "local")
	Emit("sent", target.ID, target.Name, req.Text)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
