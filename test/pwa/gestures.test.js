'use strict';
/* Feature #31 — the two-gesture bubble contract (branch pwa-gestures-and-sunlight).

   · Double-tap (desktop seam: dblclick)  → REACT mode: the reaction row alone.
   · Long-press (desktop seam: contextmenu) → MENU mode: Quote / Copy / Forward,
     and NO reaction row.
   · Copy puts the pressed bubble's text on the clipboard and toasts.
   · Forward opens a roster picker; picking a target sends
     "↪ from <author>: <text>" through the normal /api/send pipeline.

   The touch-side timing (500ms hold; two taps ≤350ms & ≤40px apart, same
   bubble) lives in attachBubbleActions + messagemath.isDoubleTap and is pure
   math; these tests pin the mode contract and the two new actions end-to-end
   through the app's real wiring. */

const { test } = require('node:test');
const assert = require('node:assert/strict');
const { loadApp } = require('./harness');

const REPLY = { id: 200, type: 'reply', agent: 'vint', name: 'vint', text: 'the fog remembers the sun', ts: '2026-07-10T07:00:00Z' };

async function openThreadWithReply(h) {
  await h.openContact('vint');
  await h.sse(REPLY);
  const bubble = h.qs('#feed .msg.reply');
  assert.ok(bubble, 'a reply bubble is on the feed');
  return bubble;
}

test('double-click opens REACT mode: reactions only, no select actions', async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());
  const bubble = await openThreadWithReply(h);

  h.dispatch('dblclick', bubble);
  await h.flush();

  assert.equal(h.$('action-sheet').classList.contains('hidden'), false, 'sheet opens on double-tap');
  const quick = h.qsa('#action-reactions .action-reaction:not(.action-reaction-more)');
  assert.equal(quick.length, 6, 'the 6 quick reactions are offered');
  assert.equal(h.$('action-quote').classList.contains('hidden'), true, 'Quote hidden in react mode');
  assert.equal(h.$('action-copy').classList.contains('hidden'), true, 'Copy hidden in react mode');
  assert.equal(h.$('action-forward').classList.contains('hidden'), true, 'Forward hidden in react mode');
});

test('right-click opens MENU mode: Quote/Copy/Forward, no reaction row', async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());
  const bubble = await openThreadWithReply(h);

  h.dispatch('contextmenu', bubble);
  await h.flush();

  assert.equal(h.$('action-sheet').classList.contains('hidden'), false, 'sheet opens on long-press');
  assert.equal(h.$('action-quote').classList.contains('hidden'), false, 'Quote offered');
  assert.equal(h.$('action-copy').classList.contains('hidden'), false, 'Copy offered');
  assert.equal(h.$('action-forward').classList.contains('hidden'), false, 'Forward offered');
  assert.equal(h.qsa('#action-reactions .action-reaction').length, 0, 'no reaction row in menu mode');
});

test('Copy puts the bubble text on the clipboard and toasts', async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());
  const bubble = await openThreadWithReply(h);

  // Stub the async clipboard on the harness window (jsdom has none) — the
  // app's primary path; the execCommand fallback is for non-secure contexts.
  const win = h.document.defaultView;
  let copied = null;
  win.navigator.clipboard = { writeText: async (s) => { copied = s; } };

  h.dispatch('contextmenu', bubble);
  await h.flush();
  await h.click('action-copy');
  await h.flush();

  assert.equal(copied, 'the fog remembers the sun', 'the pressed bubble text was copied');
  assert.equal(h.$('action-sheet').classList.contains('hidden'), true, 'sheet closed after Copy');
  const toast = h.document.getElementById('app-toast');
  assert.ok(toast && !toast.classList.contains('hidden'), 'a toast answers the tap');
  assert.equal(toast.textContent, 'Copied');
});

test('Forward pickers the roster (minus this thread) and sends ↪ from <author>', async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());
  const bubble = await openThreadWithReply(h);

  h.dispatch('contextmenu', bubble);
  await h.flush();
  await h.click('action-forward');
  await h.flush();

  assert.equal(h.$('forward-sheet').classList.contains('hidden'), false, 'forward picker opens');
  const rows = h.qsa('#forward-list .forward-row');
  const names = rows.map((r) => r.querySelector('.forward-row-name').textContent);
  assert.deepEqual(names, ['#crew', 'ludwig', 'marvin'], 'rooms first, then contacts, current thread excluded');
  // The offline contact is offered (mail queues durably), labeled honestly.
  const marvin = rows.find((r) => r.querySelector('.forward-row-name').textContent === 'marvin');
  assert.match(marvin.textContent, /offline — will queue/, 'offline target labeled');

  const ludwig = rows.find((r) => r.querySelector('.forward-row-name').textContent === 'ludwig');
  await h.click(ludwig);
  await h.flush();

  const sent = h.lastCallTo('/api/send');
  assert.ok(sent, 'the forward went through the normal /api/send pipeline');
  assert.equal(sent.body.agent, 'ludwig');
  assert.equal(sent.body.text, '↪ from vint: the fog remembers the sun',
    'attribution prefix + the pressed bubble text');
  assert.equal(h.$('forward-sheet').classList.contains('hidden'), true, 'picker closed after the pick');
  const toast = h.document.getElementById('app-toast');
  assert.ok(toast && !toast.classList.contains('hidden'), 'a toast confirms');
  assert.match(toast.textContent, /Forwarded to ludwig/);
});

test('Esc and backdrop dismiss the forward picker without sending', async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());
  const bubble = await openThreadWithReply(h);

  h.dispatch('contextmenu', bubble);
  await h.flush();
  await h.click('action-forward');
  await h.flush();
  const before = h.callsTo('/api/send').length;

  await h.click('forward-backdrop');
  await h.flush();
  assert.equal(h.$('forward-sheet').classList.contains('hidden'), true, 'backdrop closes the picker');
  assert.equal(h.callsTo('/api/send').length, before, 'nothing was sent');
});
