package main

// push.go — Web Push (RFC 8291) so the phone can ring with the app closed.
// This is the keystone of the async loop: agents reach you when they need you.
// VAPID keys identify this daemon to the browser's push service; payloads are
// E2E-encrypted per the spec, so nothing legible transits Apple/Mozilla/Google.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// vapidKeys is this daemon's application-server keypair (persisted 0600).
type vapidKeys struct {
	Public  string `json:"public"`
	Private string `json:"private"`
}

var (
	vapid     vapidKeys
	pushMu    sync.Mutex
	pushSubs  = map[string]*webpush.Subscription{} // device token -> subscription
	vapidPath = func() string { return bridgePath("vapid.json") }
	subsPath  = func() string { return bridgePath("push-subs.json") }
)

// loadVAPID loads the VAPID keypair, generating one on first run.
func loadVAPID() error {
	if data, err := os.ReadFile(vapidPath()); err == nil {
		if json.Unmarshal(data, &vapid) == nil && vapid.Public != "" {
			return nil
		}
	}
	priv, pub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		return err
	}
	vapid = vapidKeys{Public: pub, Private: priv}
	data, _ := json.Marshal(vapid)
	return writeFilePrivate(vapidPath(), data)
}

// loadPushSubs restores stored push subscriptions.
func loadPushSubs() {
	if data, err := os.ReadFile(subsPath()); err == nil {
		_ = json.Unmarshal(data, &pushSubs)
	}
}

func savePushSubs() {
	pushMu.Lock()
	defer pushMu.Unlock()
	data, _ := json.Marshal(pushSubs)
	_ = writeFilePrivate(subsPath(), data)
}

// handlePushKey returns the VAPID public key the client needs to subscribe.
func handlePushKey(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"key": vapid.Public})
}

// handlePushSubscribe stores a device's push subscription, keyed by its token.
func handlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	data, ok := readBody(w, r)
	if !ok {
		return
	}
	var sub webpush.Subscription
	if json.Unmarshal(data, &sub) != nil || sub.Endpoint == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad-subscription"})
		return
	}
	token := requestToken(r)
	pushMu.Lock()
	pushSubs[token] = &sub
	pushMu.Unlock()
	savePushSubs()
	audit("push-subscribed", sub.Endpoint, "-")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// pushPayload is the notification shape the service worker renders.
type pushPayload struct {
	Title   string `json:"title"`
	Body    string `json:"body"`
	Tag     string `json:"tag,omitempty"`
	Contact string `json:"contact,omitempty"` // deep-link target on tap
}

// sendPush delivers one notification to every subscribed device. Dead
// subscriptions (410/404) are pruned.
func sendPush(p pushPayload) (int, error) {
	if vapid.Public == "" {
		return 0, fmt.Errorf("no VAPID keys")
	}
	body, _ := json.Marshal(p)
	pushMu.Lock()
	subs := make(map[string]*webpush.Subscription, len(pushSubs))
	for k, v := range pushSubs {
		subs[k] = v
	}
	pushMu.Unlock()

	sent, dead := 0, []string{}
	for token, sub := range subs {
		resp, err := webpush.SendNotification(body, sub, &webpush.Options{
			Subscriber:      "https://github.com/hrishikeshs/bridge",
			VAPIDPublicKey:  vapid.Public,
			VAPIDPrivateKey: vapid.Private,
			TTL:             30,
		})
		if err != nil {
			continue
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		audit("push-send", fmt.Sprintf("status=%d body=%s", resp.StatusCode, string(respBody)), "-")
		if resp.StatusCode == http.StatusGone || resp.StatusCode == http.StatusNotFound {
			dead = append(dead, token)
			continue
		}
		if resp.StatusCode >= 400 {
			continue // Apple rejected it; don't count as delivered
		}
		sent++
	}
	if len(dead) > 0 {
		pushMu.Lock()
		for _, t := range dead {
			delete(pushSubs, t)
		}
		pushMu.Unlock()
		savePushSubs()
	}
	return sent, nil
}

var (
	pushLast   = map[string]int64{} // tag -> last-sent unix seconds (debounce)
	pushLastMu sync.Mutex
)

// notifyPush fires a phone notification for a high-signal event, off the
// request path (async) and debounced per tag so a burst can't spam the lock
// screen. This is the async loop: an agent reaches you when it needs you.
func notifyPush(title, body, tag, contact string) {
	pushLastMu.Lock()
	now := nowUnix()
	if tag != "" && now-pushLast[tag] < 3 {
		pushLastMu.Unlock()
		return
	}
	if tag != "" {
		pushLast[tag] = now
	}
	pushLastMu.Unlock()
	go sendPush(pushPayload{Title: title, Body: truncateRunes(body, 160), Tag: tag, Contact: contact})
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// handlePushTest fires a test notification to all subscribed devices.
func handlePushTest(w http.ResponseWriter, r *http.Request) {
	n, err := sendPush(pushPayload{
		Title: "bridge",
		Body:  "🌉 Push works — your phone can ring now.",
		Tag:   "bridge-test",
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"sent": n})
}

// nowUnix is the current time in whole seconds (for push debounce).
func nowUnix() int64 { return timeNowUnix() }
