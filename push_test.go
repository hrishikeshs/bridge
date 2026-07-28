package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// subscribeDevice drives handlePushSubscribe as a paired device would.
func subscribeDevice(t *testing.T, token, endpoint string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"endpoint": endpoint,
		"keys":     map[string]string{"p256dh": "k", "auth": "a"},
	})
	r := httptest.NewRequest(http.MethodPost, "/api/push/subscribe", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handlePushSubscribe(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("subscribe %s: %d %s", token, w.Code, w.Body.String())
	}
}

func TestPushUnsubscribeRemovesOnlyCallersSubscription(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // keep savePushSubs/audit off the live ~/.bridge
	pushMu.Lock()
	pushSubs = map[string]*webpush.Subscription{}
	pushMu.Unlock()

	subscribeDevice(t, "tok-a", "https://web.push.apple.com/aaa")
	subscribeDevice(t, "tok-b", "https://web.push.apple.com/bbb")

	r := httptest.NewRequest(http.MethodPost, "/api/push/unsubscribe", nil)
	r.Header.Set("Authorization", "Bearer tok-a")
	w := httptest.NewRecorder()
	handlePushUnsubscribe(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("unsubscribe: %d %s", w.Code, w.Body.String())
	}

	pushMu.Lock()
	_, aPresent := pushSubs["tok-a"]
	_, bPresent := pushSubs["tok-b"]
	pushMu.Unlock()
	if aPresent {
		t.Fatal("caller's subscription was not removed")
	}
	if !bPresent {
		t.Fatal("another device's subscription was removed")
	}

	// Second call finds nothing to remove and still reports ok (idempotent).
	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest(http.MethodPost, "/api/push/unsubscribe", nil)
	r2.Header.Set("Authorization", "Bearer tok-a")
	handlePushUnsubscribe(w2, r2)
	if w2.Code != http.StatusOK {
		t.Fatalf("repeat unsubscribe: %d", w2.Code)
	}
}
