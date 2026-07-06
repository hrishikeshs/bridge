package main

import (
	"crypto/rand"
	"encoding/binary"
)

// randInt returns a cryptographically random int uniform in [0, n). It uses
// rejection sampling, so there is no modulo bias for any n (not just powers of
// two, unlike a bare `% n` on a single byte). It panics on an RNG failure or
// n <= 0 — the same fail-hard posture as randomHex and pairingDigits, since a
// security daemon must never silently fall back to a biased or predictable
// value.
func randInt(n int) int {
	if n <= 0 {
		panic("bridge: randInt requires n > 0")
	}
	un := uint64(n)
	// Reject the low (2^64 mod n) draws; the remaining values divide evenly into
	// n buckets, so v % n is uniform. threshold == 0 when n divides 2^64 (powers
	// of two), which correctly rejects nothing.
	threshold := (-un) % un
	var b [8]byte
	for {
		if _, err := rand.Read(b[:]); err != nil {
			panic("bridge: crypto/rand unavailable: " + err.Error())
		}
		v := binary.BigEndian.Uint64(b[:])
		if v >= threshold {
			return int(v % un)
		}
	}
}
