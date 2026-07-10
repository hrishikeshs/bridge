package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type bufferWriteCloser struct{ bytes.Buffer }

func (*bufferWriteCloser) Close() error { return nil }

func TestAppServerMessagesDeclareJSONRPC2(t *testing.T) {
	w := &bufferWriteCloser{}
	c := &appServerClient{stdin: w}
	if err := c.notify("initialized", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	var message map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(w.Bytes()), &message); err != nil {
		t.Fatal(err)
	}
	if message["jsonrpc"] != "2.0" || message["method"] != "initialized" {
		t.Fatalf("unexpected JSON-RPC notification: %#v", message)
	}
}

func TestLegacyApprovalKeysMapOnlyForSemanticContacts(t *testing.T) {
	cases := map[string]string{
		"1": "accept", "y": "accept", "2": "acceptForSession",
		"3": "decline", "n": "decline", "esc": "cancel",
	}
	for key, want := range cases {
		got, ok := semanticDecisionForLegacyKey(key)
		if !ok || got != want {
			t.Errorf("semanticDecisionForLegacyKey(%q) = %q, %v; want %q, true", key, got, ok, want)
		}
	}
	if _, ok := semanticDecisionForLegacyKey("q"); ok {
		t.Fatal("unexpected semantic mapping for an unapproved key")
	}
}

func TestSemanticApprovalTextRemainsCompatibleWithPWAButtons(t *testing.T) {
	text := formatSemanticApproval(SemanticEvent{Command: "go test ./...", Cwd: "/project"}, "Run tests?")
	for _, option := range []string{"1. Yes", "2. Yes, and allow for this session", "3. No"} {
		if !strings.Contains(text, option) {
			t.Errorf("approval text missing %q: %s", option, text)
		}
	}
}

func TestCodexPlanUsesCurrentTurnPlanShape(t *testing.T) {
	got := formatCodexPlan("Implementation", []codexPlanStep{
		{Step: "Add adapter", Status: "inProgress"},
		{Step: "Run tests", Status: "pending"},
	})
	want := "Implementation\n[inProgress] Add adapter\n[pending] Run tests"
	if got != want {
		t.Fatalf("formatCodexPlan() = %q, want %q", got, want)
	}
}

func TestV2RemoteDeliveryParksSemanticCommand(t *testing.T) {
	const contactID = "semantic-contact"
	const token = "semantic-lease"
	remoteMu.Lock()
	remoteLeases[token] = &remoteLease{
		token: token, protocol: 2, lastSeen: time.Now(),
		agents: map[string]bool{contactID: true}, states: map[string]remoteState{},
	}
	remoteLeaseByContact[contactID] = token
	remoteMu.Unlock()
	defer func() {
		remoteMu.Lock()
		remoteDeleteLeaseLocked(token)
		remoteMu.Unlock()
	}()

	c := &Contact{ID: contactID, Name: "semantic-agent", Transport: "remote"}
	done := make(chan error, 1)
	go func() { done <- (remoteTransport{}).Deliver(c, "hello") }()

	deadline := time.Now().Add(time.Second)
	var parked *parkedDelivery
	for parked == nil && time.Now().Before(deadline) {
		remoteMu.Lock()
		if lease := remoteLeases[token]; lease != nil && len(lease.outbox) > 0 {
			parked = lease.outbox[0]
		}
		remoteMu.Unlock()
		if parked == nil {
			time.Sleep(time.Millisecond)
		}
	}
	if parked == nil || parked.Command == nil {
		t.Fatal("v2 delivery did not park a semantic command")
	}
	if parked.Command.Type != SemanticCommandInput || parked.Command.Text != "hello" || parked.Text != "" {
		t.Fatalf("unexpected parked v2 delivery: %#v", parked)
	}

	remoteMu.Lock()
	parked.acked = true
	remoteRemoveParkedLocked(remoteLeases[token], parked.ID)
	remoteMu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
