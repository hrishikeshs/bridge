package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

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
