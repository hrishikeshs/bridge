package main

import (
	"encoding/json"
	"errors"
	"os"
	"testing"
)

func preservePushGenerationGlobals(t *testing.T) {
	t.Helper()
	previousVAPID, previousGeneration := vapid, daemonGeneration
	previousEpoch := daemonEpoch
	previousWriter := writeVAPIDState
	t.Cleanup(func() {
		vapid = previousVAPID
		daemonGeneration = previousGeneration
		daemonEpoch = previousEpoch
		writeVAPIDState = previousWriter
	})
}

func writeVAPIDFixture(t *testing.T, keys vapidKeys) string {
	t.Helper()
	if err := ensureBridgeDir(); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(keys)
	if err != nil {
		t.Fatal(err)
	}
	path := vapidPath()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadVAPIDAdvancesDurableDaemonGeneration(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	preservePushGenerationGlobals(t)
	path := writeVAPIDFixture(t, vapidKeys{Public: "public", Private: "private"})

	if err := loadVAPID(); err != nil {
		t.Fatalf("first loadVAPID: %v", err)
	}
	if daemonGeneration != 1 || vapid.Generation != 1 {
		t.Fatalf("first generation = daemon %d, VAPID %d; want 1", daemonGeneration, vapid.Generation)
	}
	if err := loadVAPID(); err != nil {
		t.Fatalf("second loadVAPID: %v", err)
	}
	if daemonGeneration != 2 || vapid.Generation != 2 {
		t.Fatalf("second generation = daemon %d, VAPID %d; want 2", daemonGeneration, vapid.Generation)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stored vapidKeys
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Generation != 2 {
		t.Fatalf("stored generation = %d, want 2", stored.Generation)
	}
}

func TestLoadVAPIDWriteFailureDisablesPush(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	preservePushGenerationGlobals(t)
	path := writeVAPIDFixture(t, vapidKeys{Public: "public", Private: "private", Generation: 7})
	if err := os.Mkdir(path+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}

	if err := loadVAPID(); err == nil {
		t.Fatal("loadVAPID succeeded despite an unwritable atomic temp path")
	}
	if vapid.Public != "" || daemonGeneration != 0 {
		t.Fatalf("push remained enabled after failed generation write: vapid=%q generation=%d",
			vapid.Public, daemonGeneration)
	}
}

func TestLoadVAPIDUncertainDurableCommitDisablesPush(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	preservePushGenerationGlobals(t)
	path := writeVAPIDFixture(t, vapidKeys{Public: "public", Private: "private", Generation: 7})
	writeVAPIDState = func(path string, data []byte) error {
		// Model a parent-directory fsync failure: the rename may already be
		// visible, but this boot must not emit pushes under an uncertain number.
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return err
		}
		return errors.New("directory sync failed")
	}

	if err := loadVAPID(); err == nil {
		t.Fatal("loadVAPID accepted an uncertain durable commit")
	}
	if vapid.Public != "" || daemonGeneration != 0 {
		t.Fatalf("push remained enabled after uncertain commit: vapid=%q generation=%d",
			vapid.Public, daemonGeneration)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stored vapidKeys
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Generation != 8 {
		t.Fatalf("test did not model a post-rename failure: stored generation = %d", stored.Generation)
	}
}

func TestLoadVAPIDCorruptionFailsClosed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	preservePushGenerationGlobals(t)
	if err := ensureBridgeDir(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(vapidPath(), []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	vapid = vapidKeys{Public: "stale"}
	daemonGeneration = 99

	if err := loadVAPID(); err == nil {
		t.Fatal("loadVAPID accepted corrupt durable generation state")
	}
	if vapid.Public != "" || daemonGeneration != 0 {
		t.Fatalf("corrupt load did not fail closed: vapid=%q generation=%d",
			vapid.Public, daemonGeneration)
	}
}

func TestLoadVAPIDRejectsGenerationBeyondJSONSafeRange(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	preservePushGenerationGlobals(t)
	writeVAPIDFixture(t, vapidKeys{
		Public: "public", Private: "private", Generation: maxDaemonGeneration,
	})

	if err := loadVAPID(); err == nil {
		t.Fatal("loadVAPID advanced beyond JavaScript's safe integer range")
	}
	if vapid.Public != "" || daemonGeneration != 0 {
		t.Fatalf("push remained enabled after generation exhaustion: vapid=%q generation=%d",
			vapid.Public, daemonGeneration)
	}
}

func TestStampPushPayloadUsesCurrentDaemonGeneration(t *testing.T) {
	preservePushGenerationGlobals(t)
	daemonEpoch = "epoch-current"
	daemonGeneration = 42

	got := stampPushPayload(pushPayload{Title: "hello", Tag: "attn-vint", Seq: 3})
	if got.ServerEpoch != "epoch-current" || got.ServerGeneration != 42 {
		t.Fatalf("stamped payload = epoch %q generation %d", got.ServerEpoch, got.ServerGeneration)
	}

	explicit := stampPushPayload(pushPayload{ServerEpoch: "stale", ServerGeneration: 9})
	if explicit.ServerEpoch != "epoch-current" || explicit.ServerGeneration != 42 {
		t.Fatalf("caller bypassed current stamp: %#v", explicit)
	}
}
