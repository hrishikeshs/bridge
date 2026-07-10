// @ts-check — type-checked against ./types.d.ts (see tsconfig.json). Dev-only:
// `@ts-check` + JSDoc are comments the browser ignores, so nothing ships changes.
/* bridge — message & unread math.
   Peeled out of app.js (round 2 of the ES-module split); behaviour unchanged.

   The per-contact/per-room derivations the list and the badges read off the
   event log + outbox: the newest message-like item, the last-activity /
   last-message / my-last-sent timestamps, the unread count, and the seen
   cursor. Every function is keyed on a string id, so a room ("room:crew") works
   through the same helpers as a contact. It reads (and, for markSeen, writes)
   live app state through the shared `state` object; it imports nothing else, so
   it is a near-leaf module (only ./app.js's state). The markSeen / unreadCount
   pair read+write the per-contact lastSeen cursor in localStorage, kept
   byte-identical to the pre-split app.js (existing devices need no migration —
   the old boolean hasUnread was just this count > 0). */

'use strict';

import { state } from './app.js';

/** @param {string} contactId */
export function markSeen(contactId) {
  state.lastSeen[contactId] = state.lastEventId;
  localStorage.setItem('lastSeen', JSON.stringify(state.lastSeen));
}

// Unread = agent replies/mentions newer than the stored cursor. Outbound and
// self events never count. This reads the SAME lastSeen cursor the old boolean
// used (a per-contact event id), so existing devices need no migration — the
// old hasUnread was just this count > 0.
/** @param {string} contactId @returns {number} */
export function unreadCount(contactId) {
  const seen = state.lastSeen[contactId] || 0;
  let n = 0;
  for (const e of state.events) {
    if (e.agent === contactId && e.id > seen &&
        (e.type === 'reply' || e.type === 'mention' || e.type === 'peer')) n++;
  }
  return n;
}

// Newest message-like item touching the contact — a stored reply/mention/sent
// event or a not-yet-confirmed outbox echo — whichever is more recent.
/** @param {string} id @returns {MessageLike | null} */
export function newestMessage(id) {
  /** @type {BridgeEvent | null} */
  let ev = null;
  for (let i = state.events.length - 1; i >= 0; i--) {
    const e = state.events[i];
    if (e.agent === id && (e.type === 'reply' || e.type === 'mention' ||
        e.type === 'sent' || e.type === 'peer')) {
      ev = e; break;
    }
  }
  /** @type {PendingMsg | null} */
  let pend = null;
  for (const m of state.pending) if (m.agent === id) pend = m;   // last wins (chronological)
  if (ev && pend) return (Date.parse(pend.ts) || 0) >= (Date.parse(ev.ts) || 0) ? pend : ev;
  return ev || pend;
}

// Milliseconds of the last activity of ANY kind touching the contact (drives
// ordering). 0 for a contact the phone has never seen an event for.
/** @param {Contact | Room} contact @returns {number} */
export function lastActivityMs(contact) {
  const id = contact.id;
  let ms = 0;
  for (let i = state.events.length - 1; i >= 0; i--) {
    if (state.events[i].agent === id) { ms = Date.parse(state.events[i].ts) || 0; break; }
  }
  for (const m of state.pending) {
    if (m.agent === id) { const t = Date.parse(m.ts) || 0; if (t > ms) ms = t; }
  }
  return ms;
}

// Milliseconds of MY last message to a contact — the newest 'sent' event or a
// pending outbox echo (both are mine). 0 if I haven't messaged them within the
// loaded window. Drives the Focus floor ordering.
/** @param {string} id @returns {number} */
export function myLastSentMs(id) {
  let ms = 0;
  for (let i = state.events.length - 1; i >= 0; i--) {
    const e = state.events[i];
    if (e.agent === id && e.type === 'sent') { ms = Date.parse(e.ts) || 0; break; }
  }
  for (const m of state.pending) {
    if (m.agent === id) { const t = Date.parse(m.ts) || 0; if (t > ms) ms = t; }
  }
  return ms;
}

// The newest MESSAGE (reply/sent/mention/peer, or a pending echo) touching a
// contact — what "chatted with" means for Focus. Distinct from lastActivityMs,
// which counts ANY event: a reconnect's 'connected' or a status/health tick
// would otherwise keep a quiet agent in focus (e.g. right after a daemon restart).
/** @param {string} id @returns {number} */
export function lastMessageMs(id) {
  const m = newestMessage(id);
  return m ? (Date.parse(m.ts) || 0) : 0;
}

/* ---- bubble gestures (feature #31) ---- */

// Whether a second tap completes a double-tap: same bubble, quick (≤350ms — a
// beat slower than the ~300ms convention, forgiving of one-thumb use), and
// near (≤40px — two relaxed taps land wider than a drag's 10px drift budget).
// Pure math so the timing/geometry contract is testable without a DOM; the
// caller owns the "same bubble" check via the ids it stores.
/** @param {{t:number, x:number, y:number}} prev @param {{t:number, x:number, y:number}} cur @returns {boolean} */
export function isDoubleTap(prev, cur) {
  if (!prev) return false;
  return (cur.t - prev.t) <= 350 &&
         Math.hypot(cur.x - prev.x, cur.y - prev.y) <= 40;
}

// The forwarded line as it rides to the OTHER agent: attribution + the pressed
// bubble's text. The receiving agent already gets a daemon-authored
// "[From Hrishi (phone)]:" provenance frame around the whole line — this prefix
// only says who SAID the forwarded words. Plain "↪ from" (not "[fwd …]"): never
// bracket-frame-shaped, so it can't even resemble the H9 surface.
/** @param {string} name @param {string} text @returns {string} */
export function forwardText(name, text) {
  return '↪ from ' + (name || '?') + ': ' + (text || '');
}
