package main

// rooms.go — Party Line: a room is a shared thread the human and every
// registered agent see at once. v1 ships exactly one, #crew, whose membership
// is the whole roster — no creation, no management. A room lives in its own
// "room:" id namespace so it can never collide with a contact (a uuid) or a
// contact name, and it is never registered — so registry.Resolve returns nil for
// it, which is what keeps the pane-keyed endpoints (approve/interrupt/react)
// refusing it by construction.

import (
	"strings"
	"sync"
)

const (
	// roomCrewID is the thread/event key for the one built-in party line. It is
	// never a registered contact, so registry.Resolve can never hand one back.
	roomCrewID = "room:crew"
	// roomCrewName is the room's display handle — the " in #crew" a delivered
	// frame wears and the name /api/status advertises. Daemon-authored only; it
	// never comes from body text (H9).
	roomCrewName = "#crew"
)

// isRoom reports whether a target handle addresses a room rather than a contact
// — the branch point the send paths take before registry.Resolve.
func isRoom(id string) bool { return strings.HasPrefix(id, "room:") }

// isRoomTarget reports whether a `bridge send --to` value names the crew room.
// The CLI accepts the friendly "#crew" and bare "crew" as well as the raw room
// id, so an agent never has to know the internal key.
func isRoomTarget(to string) bool {
	return to == roomCrewName || to == "crew" || to == roomCrewID
}

// roomInfo is the shape /api/status advertises so the phone renders the room row
// without hardcoding the id or name.
type roomInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// roomList is the static set of rooms for v1 — exactly #crew. It rides on
// /api/status beside the contact roster.
func roomList() []roomInfo {
	return []roomInfo{{ID: roomCrewID, Name: roomCrewName}}
}

// fanoutRoom delivers one #crew message to every registered contact except
// skipID ("" skips no one — the phone is not a contact), returning whether any
// member was live. A live member gets the same hold-and-batch a 1:1 send rides
// (never a bare send-keys, never past an open dialog); an offline member gets
// durable mailbox delivery on revival. So a durably-queued fan-out IS success
// even when the whole crew is offline (the round-2 rule).
//
// Every fan-out message is Emitted:true on purpose: the room event is emitted
// exactly ONCE by the caller, so no member's flush may emit a second one — an
// offline member reviving must not forge a duplicate room bubble (and it would
// land in that member's OWN 1:1 thread, since flushMailbox keys the emit to the
// recipient). The recipient's frame is room-aware and daemon-authored (Room ->
// " in #crew"), so the body never carries the room fragment.
func fanoutRoom(from, via, text, skipID string) bool {
	anyLive := false
	liveIndex := 0 // counts LIVE recipients only — the stagger index (thundering-herd fix)
	for _, c := range registry.Roster() {
		if c.ID == skipID {
			continue
		}
		m := MailMessage{From: from, Via: via, Text: text, TS: nowUTC(), Room: roomCrewName, Emitted: true}
		if c.Status == "live" {
			// Spread live members' delivery timers so the crew's N Claude processes
			// don't all continue their turn (and hit the API) at one instant — the
			// server-side 429 the room fan-out caused live. Offset 0 == feature off
			// == plain holdInbound, so this is a clean superset of the old behavior.
			holdInboundStaggered(c, m, fanoutOffset(liveIndex))
			liveIndex++
			anyLive = true
		} else {
			registry.Queue(c.ID, m) // offline: durable, delivered on revival — no herd
		}
	}
	return anyLive
}

// ---------------------------------------------------------------------------
// Room cooldown — the crew's own rule, minted in #crew the night the room
// demonstrated the problem live (eight agent messages in two minutes).
// Proposed by Marvin ("no agent posts twice in a row without a human message
// in between"), seconded by Ludwig, ordered by Hrishi ("👍 cooldown loop
// please", 2026-07-06). Between human messages, each agent may speak in a
// room AT MOST ONCE: any agent-only exchange is bounded at one post per
// member, so the obligation spiral is structurally impossible rather than
// merely impolite. State is in-memory by design — a daemon restart hands
// everyone one fresh slot, which is harmless.
// ---------------------------------------------------------------------------

var (
	roomCooldownMu  sync.Mutex
	spokeSinceHuman = map[string]map[string]bool{} // room id -> contact id -> spoke this turn
)

// roomHumanSpoke resets a room's cooldown: a human message opens one new
// speaking slot for every agent.
func roomHumanSpoke(room string) {
	roomCooldownMu.Lock()
	delete(spokeSinceHuman, room)
	roomCooldownMu.Unlock()
}

// roomAgentMaySpeak consumes an agent's speaking slot for the current human
// turn. False means it already spoke — the send is refused until the next
// human message reopens the room.
func roomAgentMaySpeak(room, contact string) bool {
	roomCooldownMu.Lock()
	defer roomCooldownMu.Unlock()
	m := spokeSinceHuman[room]
	if m == nil {
		m = map[string]bool{}
		spokeSinceHuman[room] = m
	}
	if m[contact] {
		return false
	}
	m[contact] = true
	return true
}
