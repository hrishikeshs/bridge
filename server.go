package main

// server.go — daemon bootstrap and the HTTP front door: bind-then-lockfile
// startup (H4-ordered), graceful shutdown, the single route dispatcher with
// its identity/token gates, and the embedded PWA with its strict CSP.

import (
	"context"
	"embed"
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
	startSessionManager() // reconcile.go: tail loops + liveness
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
