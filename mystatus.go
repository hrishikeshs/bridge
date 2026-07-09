package main

import (
	"encoding/json"
	"net/http"
	"os"
	"sync"
)

// myStatusText holds the human's away line — the AIM auto-responder delivered to
// an agent the moment it messages the phone (handleLocalSend). It is persisted
// under ~/.bridge/mystatus.json so it survives a daemon restart, guarded by
// myStatusMu since both the HTTP handler and the send path touch it.
var (
	myStatusMu   sync.Mutex
	myStatusText string
)

// myStatusFile is the on-disk shape: an object, not a bare string, so the file
// can gain fields later without a format break (matching tokens.json's shape).
type myStatusFile struct {
	Text string `json:"text"`
}

// loadMyStatus restores the human's away line at startup (called from runServe).
// A missing or unparseable file leaves it empty — the secure, quiet default.
func loadMyStatus() {
	myStatusMu.Lock()
	defer myStatusMu.Unlock()
	data, err := os.ReadFile(bridgePath("mystatus.json"))
	if err != nil {
		return
	}
	var f myStatusFile
	if json.Unmarshal(data, &f) == nil {
		myStatusText = f.Text
	}
}

// setMyStatus records the human's away line and persists it 0600. Empty clears.
func setMyStatus(text string) {
	myStatusMu.Lock()
	defer myStatusMu.Unlock()
	myStatusText = text
	data, _ := json.Marshal(myStatusFile{Text: text})
	_ = writeFilePrivate(bridgePath("mystatus.json"), data)
}

// myStatus returns the current human away line ("" when none).
func myStatus() string {
	myStatusMu.Lock()
	defer myStatusMu.Unlock()
	return myStatusText
}

// handleMyStatus sets (or clears, on empty text) the human's away line. It is
// device-token authed like every /api mutation; the value is flattened/capped
// exactly like an agent status (clampAway), surfaced on /api/status as
// my_status, and pushed live as a "mystatus" event so every open phone syncs.
func handleMyStatus(w http.ResponseWriter, r *http.Request, id string) {
	data, ok := readBody(w, r)
	if !ok {
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(data, &req)

	text := clampAway(req.Text)
	setMyStatus(text)
	if text == "" {
		audit("mystatus-clear", "", id)
	} else {
		audit("mystatus", text, id)
	}
	Emit("mystatus", "", authConfig.UserMention, text)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
