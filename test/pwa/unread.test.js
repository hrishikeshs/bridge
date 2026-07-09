'use strict';
/* Behaviour — the unread cursor (markSeen / unreadCount), peeled into
   pwa/messagemath.js in the tier-2 ES-module split. Pins the read/write of the
   per-contact lastSeen cursor in localStorage: an unseen reply raises the row
   badge; markSeen (opening the thread) drops it to 0 and persists the cursor;
   and a fresh load seeded with that cursor + the same history stays cleared —
   the reopen doesn't re-surface already-read messages. */

const { test } = require('node:test');
const assert = require('node:assert/strict');
const { loadApp } = require('./harness');

function rowByName(h, name) {
  return h.qsa('#contact-list .row').find((r) => {
    const n = r.querySelector('.row-name');
    return n && n.textContent === name;
  });
}

test('an unseen reply raises the badge; markSeen clears it and persists the cursor', async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());

  assert.equal(rowByName(h, 'vint').querySelector('.row-badge'), null, 'no unread yet');

  // An inbound reply while on the list is unseen → unreadCount > 0 (badge shown).
  await h.sse({ id: 100, type: 'reply', agent: 'vint', name: 'vint', text: 'need a hand', ts: '2026-07-07T10:00:00Z' });
  assert.equal(rowByName(h, 'vint').querySelector('.row-badge').textContent, '1', 'unreadCount > 0 → badge');
  assert.equal(h.title(), '(1) bridge', 'title carries the unread total');

  // Opening the thread calls markSeen → the unread total clears (the title is the
  // live observable; the hidden list's row badge only repaints on the next
  // renderList, i.e. when you return to it — asserted in the reopen test below).
  await h.openContact('vint');
  assert.equal(h.title(), 'bridge', 'markSeen → unread total cleared');

  // The cursor was written to localStorage (the lastSeen map, keyed by contact).
  const stored = JSON.parse(h.win.localStorage.getItem('lastSeen') || '{}');
  assert.ok((stored.vint || 0) >= 100, 'lastSeen cursor persisted at/after the seen event id');
});

test('a fresh load seeded with the cursor stays cleared (the reopen honours it)', async (t) => {
  // First session: read the reply, capture the persisted lastSeen cursor.
  const h1 = await loadApp();
  await h1.sse({ id: 100, type: 'reply', agent: 'vint', name: 'vint', text: 'hi', ts: '2026-07-07T10:00:00Z' });
  await h1.openContact('vint');   // markSeen
  const lastSeen = h1.win.localStorage.getItem('lastSeen');
  assert.ok(lastSeen, 'first session persisted a cursor');
  h1.teardown();

  // Second session (a "reopen"): same history, seeded with that cursor. The
  // already-read reply must NOT re-surface as unread.
  const h2 = await loadApp({
    localStorage: { lastSeen },
    history: [{ id: 100, type: 'reply', agent: 'vint', name: 'vint', text: 'hi', ts: '2026-07-07T10:00:00Z' }],
  });
  t.after(() => h2.teardown());

  assert.equal(rowByName(h2, 'vint').querySelector('.row-badge'), null, 'persisted cursor → no unread on reopen');
  assert.equal(h2.title(), 'bridge', 'no unread total on reopen');
});
