package main

// context_test.go — the context gauge's pure functions: reading the usage line
// off an assistant JSONL line (assistantUsage), the model→window table
// (contextWindowFor), the clamped percentage (contextPct), and the live-gated
// setter (SetContext). All parse/compute only (SetContext aside), so they test
// directly.

import "testing"

// A real-shaped Claude Code assistant JSONL line: message.model plus a usage
// block with the three fields that sum to the current context size (verified
// against live sessions — input_tokens + cache_read_input_tokens +
// cache_creation_input_tokens). Extra usage keys (output_tokens, service_tier,
// …) appear in the wild and must be ignored.
const usageAssistantLine = `{"type":"assistant","message":{"model":"claude-opus-4-8","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":3411,"cache_read_input_tokens":17956,"cache_creation_input_tokens":5625,"output_tokens":42,"service_tier":"standard"}}}`

func TestAssistantUsage(t *testing.T) {
	tokens, model, ok := assistantUsage([]byte(usageAssistantLine))
	if !ok {
		t.Fatal("assistantUsage(real line) ok=false, want true")
	}
	if want := 3411 + 17956 + 5625; tokens != want {
		t.Errorf("tokens = %d, want %d (input + cache_read + cache_creation)", tokens, want)
	}
	if model != "claude-opus-4-8" {
		t.Errorf("model = %q, want claude-opus-4-8", model)
	}

	// Lines carrying no usable usage reading return ok=false, so the tail records
	// nothing for them.
	no := map[string]string{
		"user line":             `{"type":"user","message":{"content":[{"type":"text","text":"hello"}]}}`,
		"assistant, no usage":   `{"type":"assistant","message":{"model":"claude-opus-4-8","content":[{"type":"text","text":"hi"}]}}`,
		"assistant, zero usage": `{"type":"assistant","message":{"model":"claude-opus-4-8","usage":{"input_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`,
		"empty line":            "",
		"garbage":               "not json at all }{",
	}
	for name, line := range no {
		if _, _, ok := assistantUsage([]byte(line)); ok {
			t.Errorf("%s: assistantUsage ok=true, want false", name)
		}
	}
}

func TestContextWindowFor(t *testing.T) {
	if got := contextWindowFor("claude-opus-4-8"); got != 1_000_000 {
		t.Errorf("contextWindowFor(opus-4-8) = %d, want 1000000", got)
	}
	// Unknown models (and "", and the synthetic placeholder) fall to the
	// conservative 200k floor, so pressure is never under-reported by assuming a
	// bigger window than a model actually has.
	for _, m := range []string{"claude-some-future-model", "", "<synthetic>"} {
		if got := contextWindowFor(m); got != defaultContextWindow {
			t.Errorf("contextWindowFor(%q) = %d, want %d (conservative default)", m, got, defaultContextWindow)
		}
	}
}

func TestContextPct(t *testing.T) {
	cases := []struct {
		tokens int
		model  string
		want   int
	}{
		{500_000, "claude-opus-4-8", 50},  // half of the 1M window
		{150_000, "unknown-model", 75},    // 150k of the 200k default
		{2_000_000, "unknown-model", 100}, // clamps at 100, never over
		{0, "claude-opus-4-8", 0},         // nothing parsed yet -> hidden (omitempty)
		{-5, "claude-opus-4-8", 0},        // defensive: a negative never underflows
		{100, "", 0},                      // 100 / 200000 floors to 0% (bar effectively empty)
	}
	for _, tc := range cases {
		if got := contextPct(tc.tokens, tc.model); got != tc.want {
			t.Errorf("contextPct(%d, %q) = %d, want %d", tc.tokens, tc.model, got, tc.want)
		}
	}
}

// TestSetContext records a live contact's reading and mirrors SetHealth's live
// gate: a stale tail must never move an offline agent's gauge.
func TestSetContext(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // SetContext save()s under $HOME/.bridge
	r := &Registry{
		contacts: map[string]*Contact{
			"live1": {ID: "live1", Name: "wolf", Status: "live", Health: "ok"},
			"off1":  {ID: "off1", Name: "ghost", Status: "offline", Health: "offline"},
		},
		mailbox:  map[string][]MailMessage{},
		flushing: map[string]bool{},
	}
	r.SetContext("live1", 26992, "claude-opus-4-8")
	if c := r.contacts["live1"]; c.ContextTokens != 26992 || c.ContextModel != "claude-opus-4-8" {
		t.Fatalf("live contact not updated: tokens=%d model=%q", c.ContextTokens, c.ContextModel)
	}
	// Offline contact: SetContext is a no-op (live-only, like SetHealth).
	r.SetContext("off1", 999, "claude-opus-4-8")
	if c := r.contacts["off1"]; c.ContextTokens != 0 || c.ContextModel != "" {
		t.Fatalf("offline contact was updated: tokens=%d model=%q, want untouched", c.ContextTokens, c.ContextModel)
	}
	// An unknown id is a safe no-op (must not panic).
	r.SetContext("ghost-id", 5, "m")
}
