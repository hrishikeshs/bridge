package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// pairingTTL is how long a printed pairing code stays redeemable. Ten minutes
// so a real phone setup needn't re-enter the code repeatedly; the brute-force
// defence is the maxPairingFails attempt-cap below, not the window length (five
// guesses out of 10^6 whether the window is 2 or 10 minutes), so the longer TTL
// costs nothing (R0).
const pairingTTL = 10 * time.Minute

// maxPairingFails is how many wrong guesses a live pairing code tolerates
// before it is invalidated and must be re-issued. This attempt-cap — not the
// digit count — is what defeats the brute-force (C1).
//
// Design tension: the cap is global, so any identity-gated-but-unpaired tailnet
// peer can burn 5 wrong guesses to invalidate every code you issue (a pairing
// lockout-DoS). Contained on a solo tailnet — the only peer is the owner. If
// bridge ever goes multi-user, cap per-source instead, or lock only after N
// *distinct* device ids fail; don't add per-source tracking before then.
const maxPairingFails = 5

// bridgeDir returns the ~/.bridge configuration directory.
func bridgeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".bridge"
	}
	return filepath.Join(home, ".bridge")
}

// ensureBridgeDir creates ~/.bridge with owner-only permissions if absent.
// MkdirAll won't tighten a pre-existing directory, so chmod unconditionally.
func ensureBridgeDir() error {
	if err := os.MkdirAll(bridgeDir(), 0o700); err != nil {
		return err
	}
	return os.Chmod(bridgeDir(), 0o700)
}

// bridgePath joins name onto the bridge configuration directory.
func bridgePath(name string) string {
	return filepath.Join(bridgeDir(), name)
}

// writeFilePrivate atomically writes data to path with 0600 permissions,
// creating the ~/.bridge directory first. Secrets and history never touch a
// wider mode. The write goes to a sibling temp file then os.Rename's over the
// target, so a crash mid-write can never leave a truncated file (M3).
func writeFilePrivate(path string, data []byte) error {
	if err := ensureBridgeDir(); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	// WriteFile only honours the mode when creating; force 0600 in case the
	// temp file pre-existed with a wider mode.
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// randomHex returns n cryptographically random bytes as a hex string.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is unrecoverable for a security daemon.
		panic("bridge: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// nowUTC returns the current time as an RFC 3339 UTC timestamp.
func nowUTC() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// bridgeConfig is the security policy loaded from ~/.bridge/config.json.
type bridgeConfig struct {
	AllowedLogins   []string `json:"allowed_logins"`
	RequireIdentity bool     `json:"require_identity"`
	UserMention     string   `json:"user_mention"`
	// PaperHour is the local hour The Bridge Herald publishes (paper.go).
	// Unset means the default (7); a negative value disables the scheduled
	// edition — `bridge paper` still prints one on demand.
	PaperHour *int `json:"paper_hour"`
	// RemoteTTLs is the freshness window (seconds) for a remote client's lease
	// (docs/transports.md): a lease not re-attested within it reads as dead and
	// its agents go offline through the normal two-strike path. Unset means the
	// default (30); floored at 2s in remoteTTL().
	RemoteTTLs *int `json:"remote_ttl_s"`
	// RemoteAckTimeoutS is how long (seconds) a delivery to a remote agent blocks
	// waiting for the client to ack the parked line before it is redelivered via
	// the mailbox. Unset means the default (10); floored at 1s. Kept well under
	// the 90s reconcile watchdog so a blocking flush never trips it.
	RemoteAckTimeoutS *int `json:"remote_ack_timeout_s"`
	// FanoutStaggerMs is the per-recipient step (milliseconds) the daemon spreads
	// a multi-recipient fan-out over — a #crew room message, or a plugin nudge that
	// prods many agents in one tick — so N agents don't all fire their next Claude
	// API call at the same instant and trip a server-side 429 (the thundering-herd
	// fix). The k-th LIVE recipient's in-memory delivery timer is armed at
	// min(k*step, max)+jitter; the durable mailbox is untouched, so nothing is ever
	// delayed off disk or dropped. Unset means the default (2500); 0 turns
	// staggering OFF (every recipient delivered at once, the pre-fix behavior), so
	// the fix is reversible by config. Floored at 0 in fanoutStaggerStep().
	// BRIDGE_FANOUT_STAGGER_MS overrides it (tests set a tiny step).
	FanoutStaggerMs *int `json:"fanout_stagger_ms"`
	// FanoutStaggerMaxMs caps the TOTAL spread (milliseconds) so a large crew's
	// last recipient is never delayed unboundedly: the k*step term is clamped to
	// this before jitter. Unset means the default (20000); floored at 0 in
	// fanoutStaggerMax().
	FanoutStaggerMaxMs *int `json:"fanout_stagger_max_ms"`
}

// authConfig holds the loaded policy; secure defaults apply until loadConfig runs.
var authConfig = bridgeConfig{RequireIdentity: true}

// loadConfig reads ~/.bridge/config.json, falling back to secure defaults
// (identity required, no login allowlist) when the file is absent.
func loadConfig() {
	authConfig = bridgeConfig{RequireIdentity: true}
	if data, err := os.ReadFile(bridgePath("config.json")); err == nil {
		_ = json.Unmarshal(data, &authConfig)
	}
	if authConfig.UserMention == "" {
		authConfig.UserMention = defaultUserMention()
	}
}

// defaultUserMention names the phone user in delivered messages when the
// config leaves it unset — the local login name, or "you".
func defaultUserMention() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "you"
}

// identity resolves the Tailscale-User-Login header against the allowlist.
// It returns the accepted identity and true, or "" and false when the request
// must be rejected (no acceptable tailnet identity). An empty login is accepted
// as "anonymous" only when identity is not required.
func identity(login string) (string, bool) {
	switch {
	case login != "" && len(authConfig.AllowedLogins) > 0:
		for _, a := range authConfig.AllowedLogins {
			if a == login {
				return login, true
			}
		}
		return "", false
	case login != "":
		return login, true
	case !authConfig.RequireIdentity:
		return "anonymous", true
	default:
		return "", false
	}
}

// deviceToken is the record kept for a paired phone.
type deviceToken struct {
	Device  string `json:"device"`
	Created string `json:"created"`
}

var (
	tokensMu sync.Mutex
	tokens   = map[string]deviceToken{}
)

// loadTokens restores persisted device tokens from ~/.bridge/tokens.json.
func loadTokens() {
	tokensMu.Lock()
	defer tokensMu.Unlock()
	tokens = map[string]deviceToken{}
	if data, err := os.ReadFile(bridgePath("tokens.json")); err == nil {
		_ = json.Unmarshal(data, &tokens)
	}
}

// saveTokens persists device tokens 0600. Caller holds tokensMu.
func saveTokens() {
	data, _ := json.Marshal(tokens)
	_ = writeFilePrivate(bridgePath("tokens.json"), data)
}

var (
	pairingMu    sync.Mutex
	pairingCode  string
	pairingExp   time.Time
	pairingFails int // wrong guesses against the current code; guarded by pairingMu
)

// pairingDigits returns a uniform six-digit value in [0, 1000000). It delegates
// to randInt so the reject-sampling and panic-on-RNG-failure posture lives in
// one place (an RNG failure must never silently yield a guessable 000000).
func pairingDigits() int {
	return randInt(1000000)
}

// issuePairingCode mints a single-use, six-digit code valid for pairingTTL.
// It is returned only to the on-machine caller (bridge pair); the code is the
// second factor precisely because it is displayed nowhere the network reaches.
// Issuing a fresh code resets the failed-attempt counter.
func issuePairingCode() string {
	n := pairingDigits()
	pairingMu.Lock()
	pairingCode = fmt.Sprintf("%06d", n)
	pairingExp = time.Now().Add(pairingTTL)
	pairingFails = 0
	code := pairingCode
	pairingMu.Unlock()
	audit("pair-code-issued", "", "local")
	return code
}

// tryPair redeems code for device, returning a fresh device token or "".
// A successful redemption consumes the code (single use). Each wrong guess
// against a live code is counted, and after maxPairingFails the code is
// invalidated so it must be re-issued — this defeats the brute-force (C1).
func tryPair(code, device string) string {
	pairingMu.Lock()
	// No live/unexpired code to redeem.
	if pairingCode == "" || time.Now().After(pairingExp) {
		pairingMu.Unlock()
		return ""
	}
	// Constant-time comparison (L3). ConstantTimeCompare returns 0 on any
	// length or content mismatch, so an empty guess fails here too.
	if subtle.ConstantTimeCompare([]byte(code), []byte(pairingCode)) != 1 {
		pairingFails++
		if pairingFails >= maxPairingFails {
			pairingCode = "" // too many wrong guesses; force a re-issue
			audit("pair-code-locked", "", "local")
		}
		pairingMu.Unlock()
		return ""
	}
	pairingCode = "" // single use
	pairingFails = 0
	pairingMu.Unlock()

	if device == "" {
		device = "unknown"
	}
	token := randomHex(32)
	tokensMu.Lock()
	tokens[token] = deviceToken{Device: device, Created: nowUTC()}
	saveTokens()
	tokensMu.Unlock()
	return token
}

// tokenValid reports whether token belongs to a paired device.
func tokenValid(token string) bool {
	if token == "" {
		return false
	}
	tokensMu.Lock()
	defer tokensMu.Unlock()
	_, ok := tokens[token]
	return ok
}

// revokeAllDevices deletes every paired device token and drops every push
// subscription. A locked-down or lost phone must stop both authenticating and
// receiving pushes (whose bodies carry agent command lines) — M2.
func revokeAllDevices() {
	tokensMu.Lock()
	tokens = map[string]deviceToken{}
	saveTokens()
	tokensMu.Unlock()
	clearPushSubs()
	audit("revoke-all", "", "local")
}

// requestToken extracts the device token from a request: the HttpOnly cookie
// set at pairing, or an Authorization: Bearer header (the CLI/hook path).
func requestToken(r *http.Request) string {
	if c, err := r.Cookie("bridge_token"); err == nil && c.Value != "" {
		return c.Value
	}
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}

var auditMu sync.Mutex

// audit appends one tab-separated line — ts, identity, action, detail — to
// ~/.bridge/audit.log. Tabs and newlines in detail are flattened so every
// event stays exactly one line.
func audit(action, detail, identity string) {
	if identity == "" {
		identity = "-"
	}
	detail = strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(detail)
	line := fmt.Sprintf("%s\t%s\t%s\t%s\n", nowUTC(), identity, action, detail)

	auditMu.Lock()
	defer auditMu.Unlock()
	if ensureBridgeDir() != nil {
		return
	}
	f, err := os.OpenFile(bridgePath("audit.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line)
}
