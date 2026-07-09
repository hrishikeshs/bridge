package main

import (
	"crypto/rand"
	"fmt"
)

// newID mints a random RFC 4122 v4 UUID string (no external dependency).
func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("bridge: crypto/rand unavailable: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// nameAdjectives and nameAnimals seed auto-generated agent addresses
// (adjective-animal, e.g. "swift-fox"). Ported from Magnus's wordlists; indices
// are drawn with the shared randInt (uniform, rejection-sampled), so the list
// lengths no longer have to divide 256 to stay unbiased.
var (
	nameAdjectives = []string{
		"swift", "bright", "calm", "bold", "keen", "wise", "quick", "sharp",
		"cool", "warm", "brave", "clever", "gentle", "lively", "noble", "merry",
	}
	nameAnimals = []string{
		"fox", "owl", "hawk", "wolf", "bear", "deer", "crow", "lynx",
		"hare", "wren", "otter", "raven", "finch", "seal", "moth", "toad",
	}
)

// generateName returns an "adjective-animal" address absent from `taken`,
// regenerating on collision up to 100 times before falling back to an
// id-suffixed form. It lives on the registry side so both the connect CLI (via
// the /local/contacts roster) and the daemon can reuse it. `taken` may be nil.
func generateName(taken map[string]bool) string {
	for i := 0; i < 100; i++ {
		n := nameAdjectives[randInt(len(nameAdjectives))] + "-" + nameAnimals[randInt(len(nameAnimals))]
		if !taken[n] {
			return n
		}
	}
	return nameAdjectives[randInt(len(nameAdjectives))] + "-" +
		nameAnimals[randInt(len(nameAnimals))] + "-" + newID()[:4]
}
