package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleStatusExposesDaemonGeneration(t *testing.T) {
	previous, previousGeneration := daemonEpoch, daemonGeneration
	daemonEpoch = "test-daemon-epoch"
	daemonGeneration = 42
	t.Cleanup(func() {
		daemonEpoch = previous
		daemonGeneration = previousGeneration
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	handleStatus(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d", w.Code, http.StatusOK)
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if got["server_epoch"] != "test-daemon-epoch" {
		t.Fatalf("server_epoch = %v, want test-daemon-epoch", got["server_epoch"])
	}
	if got["server_generation"] != float64(42) {
		t.Fatalf("server_generation = %v, want 42", got["server_generation"])
	}
}

func TestHandleSyncBindsCursorToDaemonEpoch(t *testing.T) {
	previousEpoch := daemonEpoch
	daemonEpoch = "epoch-current"
	t.Cleanup(func() { daemonEpoch = previousEpoch })

	eventsMu.Lock()
	previousEvents, previousCounter := events, eventCounter
	events = []Event{
		{ID: 2, Type: "reply", Text: "new"},
		{ID: 1, Type: "reply", Text: "old"},
	}
	eventCounter = 2
	eventsMu.Unlock()
	t.Cleanup(func() {
		eventsMu.Lock()
		events, eventCounter = previousEvents, previousCounter
		eventsMu.Unlock()
	})

	for _, tc := range []struct {
		name       string
		url        string
		wantReset  bool
		wantEvents int
	}{
		{"same generation gets delta", "/api/sync?since=1&epoch=epoch-current", false, 1},
		{"replacement generation ignores stale cursor", "/api/sync?since=900&epoch=epoch-old", true, 2},
		{"cold client gets full snapshot", "/api/sync?since=900", true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			handleSync(w, httptest.NewRequest(http.MethodGet, tc.url, nil))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", w.Code)
			}
			var got struct {
				Epoch  string  `json:"server_epoch"`
				Reset  bool    `json:"reset"`
				Events []Event `json:"events"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode sync: %v", err)
			}
			if got.Epoch != daemonEpoch || got.Reset != tc.wantReset || len(got.Events) != tc.wantEvents {
				t.Fatalf("sync = epoch %q reset %v events %d, want %q %v %d",
					got.Epoch, got.Reset, len(got.Events), daemonEpoch, tc.wantReset, tc.wantEvents)
			}
		})
	}
}

func TestHistoryAndJSONResponsesNameDaemonEpoch(t *testing.T) {
	previous := daemonEpoch
	daemonEpoch = "epoch-history"
	t.Cleanup(func() { daemonEpoch = previous })

	w := httptest.NewRecorder()
	handleHistory(w, httptest.NewRequest(http.MethodGet, "/api/history?since=0", nil))
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode history: %v", err)
	}
	if got["server_epoch"] != daemonEpoch {
		t.Fatalf("history epoch = %v, want %q", got["server_epoch"], daemonEpoch)
	}
	if header := w.Header().Get("X-Bridge-Server-Epoch"); header != daemonEpoch {
		t.Fatalf("response epoch header = %q, want %q", header, daemonEpoch)
	}
}

func TestEventsRejectsStaleEpochBeforeOpeningStream(t *testing.T) {
	previous := daemonEpoch
	daemonEpoch = "epoch-live"
	t.Cleanup(func() { daemonEpoch = previous })

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/events?since=4&epoch=epoch-stale", nil)
	handleEvents(w, r)
	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusPreconditionFailed)
	}
	if got := w.Header().Get("Content-Type"); got == "text/event-stream" {
		t.Fatalf("stale stream opened with content type %q", got)
	}
}

func TestRouteRejectsStaleMutationBeforeHandler(t *testing.T) {
	previousEpoch, previousConfig := daemonEpoch, authConfig
	daemonEpoch = "epoch-live"
	authConfig.RequireIdentity = false
	t.Cleanup(func() { daemonEpoch, authConfig = previousEpoch, previousConfig })

	tokensMu.Lock()
	previousTokens := tokens
	tokens = map[string]deviceToken{"test-token": {Device: "test"}}
	tokensMu.Unlock()
	t.Cleanup(func() {
		tokensMu.Lock()
		tokens = previousTokens
		tokensMu.Unlock()
	})

	body := bytes.NewBufferString(`{"agent":"nobody","text":"must not run","client_id":"x"}`)
	r := httptest.NewRequest(http.MethodPost, "/api/send", body)
	r.AddCookie(&http.Cookie{Name: "bridge_token", Value: "test-token"})
	r.Header.Set("X-Bridge-Server-Epoch", "epoch-stale")
	w := httptest.NewRecorder()
	route(w, r)

	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusPreconditionFailed)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("server-restarted")) {
		t.Fatalf("body = %q, want server-restarted", w.Body.String())
	}

	// The epoch header is additive: an older API client that does not send it
	// still reaches the endpoint instead of being mistaken for a stale PWA.
	legacy := httptest.NewRequest(http.MethodPost, "/api/send", bytes.NewBufferString(`{"agent":"nobody","text":""}`))
	legacy.AddCookie(&http.Cookie{Name: "bridge_token", Value: "test-token"})
	legacyW := httptest.NewRecorder()
	route(legacyW, legacy)
	if legacyW.Code != http.StatusBadRequest {
		t.Fatalf("legacy request status = %d, want handler's %d", legacyW.Code, http.StatusBadRequest)
	}
	if !bytes.Contains(legacyW.Body.Bytes(), []byte("empty")) {
		t.Fatalf("legacy request body = %q, want handler response", legacyW.Body.String())
	}
}
