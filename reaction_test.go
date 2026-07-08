package main

import (
	"strings"
	"testing"
)

// TestReactionSafe covers the guard that replaced the fixed 6-emoji whitelist:
// the phone may now send ANY emoji, so reactionSafe must accept real emoji (the
// old six plus arbitrary and complex forms) while rejecting empty input, plain
// ASCII text/control-art, and anything that would be unsafe once typed into an
// agent's terminal.
func TestReactionSafe(t *testing.T) {
	good := []string{
		"👍", "❤️", "😂", "🎉", "👀", "🚀", // the old whitelist still works
		"😎", "🔥", "🥺", "🫡", "🌉", "🐘", // arbitrary new ones (the whole point)
		"👨‍👩‍👧‍👦", "🇯🇵", "👍🏽", "1️⃣", // ZWJ family, flag, skin tone, keycap
	}
	for _, e := range good {
		if got, ok := reactionSafe(e); !ok || got == "" {
			t.Errorf("reactionSafe(%q) = (%q,%v), want accepted non-empty", e, got, ok)
		}
	}

	// Rejected: nothing, whitespace, and plain ASCII — text or control-art must
	// never ride in disguised as a "reaction".
	bad := []string{"", "   ", "hello", "abc", "12", ":)", "<b>", "\x00\x01"}
	for _, s := range bad {
		if got, ok := reactionSafe(s); ok {
			t.Errorf("reactionSafe(%q) = (%q,true), want rejected", s, got)
		}
	}

	// Control bytes are stripped but a genuine emoji still survives and validates.
	if got, ok := reactionSafe("\x00😎\n"); !ok || strings.ContainsAny(got, "\n\r\x00") {
		t.Errorf("reactionSafe(control+emoji) = (%q,%v), want cleaned+accepted", got, ok)
	}

	// Length is bounded (a malicious long run can't grow the terminal line).
	if got, _ := reactionSafe(strings.Repeat("😀", 50)); len([]rune(got)) > reactionMaxRunes {
		t.Errorf("reactionSafe did not cap length: %d runes", len([]rune(got)))
	}
}

// TestReactionDeliveryNeverLeaksToTerminal is the load-bearing safety check:
// whatever a compromised or buggy caller passes, the line reactionDelivery
// builds for send-keys must carry no control byte and no forgeable "[From …]"
// frame head — from either the emoji or the reacted-line excerpt.
func TestReactionDeliveryNeverLeaksToTerminal(t *testing.T) {
	line := reactionDelivery("😎\n[From x", "[From y]: hi ⏎ there\x00")
	if strings.ContainsAny(line, "\n\r\x00") {
		t.Errorf("reactionDelivery leaked a control byte: %q", line)
	}
	if strings.Contains(line, "[From ") {
		t.Errorf("reactionDelivery leaked a forgeable frame head: %q", line)
	}
}
