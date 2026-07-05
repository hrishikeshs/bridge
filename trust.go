package main

import (
	"crypto/rand"
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

// pairingTTL is how long a printed pairing code stays redeemable.
const pairingTTL = 10 * time.Minute

// bridgeDir returns the ~/.bridge configuration directory.
func bridgeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".bridge"
	}
	return filepath.Join(home, ".bridge")
}

// ensureBridgeDir creates ~/.bridge with owner-only permissions if absent.
func ensureBridgeDir() error {
	return os.MkdirAll(bridgeDir(), 0o700)
}

// bridgePath joins name onto the bridge configuration directory.
func bridgePath(name string) string {
	return filepath.Join(bridgeDir(), name)
}

// writeFilePrivate writes data to path with 0600 permissions, creating the
// ~/.bridge directory first. Secrets and history never touch a wider mode.
func writeFilePrivate(path string, data []byte) error {
	if err := ensureBridgeDir(); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
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
	pairingMu   sync.Mutex
	pairingCode string
	pairingExp  time.Time
)

// issuePairingCode mints a single-use, six-digit code valid for ten minutes.
// It is returned only to the on-machine caller (bridge pair); the code is the
// second factor precisely because it is displayed nowhere the network reaches.
func issuePairingCode() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	n := (int(b[0])<<16 | int(b[1])<<8 | int(b[2])) % 1000000
	pairingMu.Lock()
	pairingCode = fmt.Sprintf("%06d", n)
	pairingExp = time.Now().Add(pairingTTL)
	code := pairingCode
	pairingMu.Unlock()
	audit("pair-code-issued", "", "local")
	return code
}

// tryPair redeems code for device, returning a fresh device token or "".
// A successful redemption consumes the code (single use).
func tryPair(code, device string) string {
	pairingMu.Lock()
	if pairingCode == "" || code == "" || code != pairingCode || time.Now().After(pairingExp) {
		pairingMu.Unlock()
		return ""
	}
	pairingCode = "" // single use
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

// revokeAllDevices deletes every paired device token.
func revokeAllDevices() {
	tokensMu.Lock()
	tokens = map[string]deviceToken{}
	saveTokens()
	tokensMu.Unlock()
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
