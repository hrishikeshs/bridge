package main

// identity_test.go — the session-file resolver, decided by CONTENT not mtime.
// This is the logic that caused the 2026-07-06 hour-long outage: a touched
// dead-mirror file recaptured a healed pin because the tie-break trusted
// clocks. The rule these tests pin: the launch id beats the pin only when its
// file STRICTLY continues the pin (contains the pin's tail uuid while the pin
// does not contain its own) — a mirror pair that matches both ways, or a
// coincidental touch, must keep the pin.

import (
	"os"
	"path/filepath"
	"testing"
)

// writeTranscript writes a .jsonl of records each carrying a "uuid" field, in
// order, so lastRecordUUID/fileContainsUUID see real content.
func writeTranscript(t *testing.T, dir, id string, uuids ...string) {
	t.Helper()
	var b []byte
	for _, u := range uuids {
		b = append(b, []byte(`{"type":"assistant","uuid":"`+u+`","message":{}}`+"\n")...)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

const (
	uuidA = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	uuidB = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	uuidC = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
)

func TestResolveLaunchVsPin(t *testing.T) {
	newCtx := func(id string) *Contact { return &Contact{ID: id, Name: "tester"} }

	t.Run("true roll: launch strictly continues the pin", func(t *testing.T) {
		dir := t.TempDir()
		// pin ends at B; pane's file holds A,B,C — it continues the pin, and
		// the pin does not contain C. Follow the launch id.
		writeTranscript(t, dir, "pin", uuidA, uuidB)
		writeTranscript(t, dir, "pane", uuidA, uuidB, uuidC)
		lastResolve = map[string]resolveVerdict{}
		if got := resolveLaunchVsPin(dir, newCtx("c1"), "pin", "pane"); got != "pane" {
			t.Errorf("got %q, want pane — a real roll must be followed", got)
		}
	})

	t.Run("mirror pair: each contains the other's tail -> keep the pin", func(t *testing.T) {
		dir := t.TempDir()
		// The outage shape: two files with identical records. paneContainsPin
		// AND pinContainsPane are both true, so the strict test fails and the
		// pin — the last verified truth — is kept, regardless of mtimes.
		writeTranscript(t, dir, "pin", uuidA, uuidB)
		writeTranscript(t, dir, "pane", uuidA, uuidB)
		lastResolve = map[string]resolveVerdict{}
		if got := resolveLaunchVsPin(dir, newCtx("c2"), "pin", "pane"); got != "pin" {
			t.Errorf("got %q, want pin — a mirror pair must never demote the pin (the outage bug)", got)
		}
	})

	t.Run("diverged pair: neither continues the other -> keep the pin", func(t *testing.T) {
		dir := t.TempDir()
		writeTranscript(t, dir, "pin", uuidA, uuidB)
		writeTranscript(t, dir, "pane", uuidC)
		lastResolve = map[string]resolveVerdict{}
		if got := resolveLaunchVsPin(dir, newCtx("c3"), "pin", "pane"); got != "pin" {
			t.Errorf("got %q, want pin — a divergent launch id is a stale arg, not a roll", got)
		}
	})

	t.Run("pin file gone: fall back to the launch id", func(t *testing.T) {
		dir := t.TempDir()
		writeTranscript(t, dir, "pane", uuidA)
		lastResolve = map[string]resolveVerdict{}
		if got := resolveLaunchVsPin(dir, newCtx("c4"), "pin", "pane"); got != "pane" {
			t.Errorf("got %q, want pane — an empty/missing pin leaves only the launch id", got)
		}
	})
}

func TestSessionChainTipIsDirectional(t *testing.T) {
	dir := t.TempDir()
	// A links to B links to C (each successor contains its predecessor's tail).
	writeTranscript(t, dir, "s-A", uuidA)
	writeTranscript(t, dir, "s-B", uuidA, uuidB)
	writeTranscript(t, dir, "s-C", uuidA, uuidB, uuidC)
	if got := sessionChainTip(dir, "s-A"); got != "s-C" {
		t.Errorf("chain tip from s-A = %q, want s-C (follow the roll to the end)", got)
	}
	// The tip follows no further.
	if got := sessionChainTip(dir, "s-C"); got != "s-C" {
		t.Errorf("chain tip from s-C = %q, want s-C (already the tip)", got)
	}
	// Directionality: an ancestor never resolves backward from a descendant.
	if got := sessionChildOf(dir, "s-C"); got != "" {
		t.Errorf("sessionChildOf(s-C) = %q, want empty — nothing continues the tip", got)
	}
}
