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

  // Esc first (review F4: the handler must exist, not just the backdrop)…
  const esc = new (h.document.defaultView.KeyboardEvent)('keydown', { key: 'Escape', bubbles: true });
  h.document.dispatchEvent(esc);
  await h.flush();
  assert.equal(h.$('forward-sheet').classList.contains('hidden'), true, 'Esc closes the picker');

  // …then the backdrop on a fresh open.
  h.dispatch('contextmenu', bubble);
  await h.flush();
  await h.click('action-forward');
  await h.flush();
  await h.click('forward-backdrop');
  await h.flush();
  assert.equal(h.$('forward-sheet').classList.contains('hidden'), true, 'backdrop closes the picker');
  assert.equal(h.callsTo('/api/send').length, before, 'nothing was sent');
});

test('Forward from a ROOM thread excludes that room (review F2)', async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());
  await h.openContact('#crew');
  await h.sse({ id: 300, type: 'peer', agent: 'room:crew', name: 'quick-wolf', text: 'den tip', ts: '2026-07-10T07:30:00Z' });
  const bubble = h.qs('#feed .msg.peer');
  assert.ok(bubble, 'a peer bubble in the room');

  h.dispatch('contextmenu', bubble);
  await h.flush();
  await h.click('action-forward');
  await h.flush();

  const names = h.qsa('#forward-list .forward-row .forward-row-name').map((n) => n.textContent);
  assert.ok(!names.includes('#crew'), 'the room we are standing in is not a forward target');
  assert.deepEqual(names, ['vint', 'ludwig', 'marvin'], 'all contacts offered');
});

/* The touch path end-to-end — the actual phone gesture plus its ghost-click
   armor (the F1 fix). Touch events are hand-built (jsdom has no TouchEvent):
   a plain Event with a touches array is exactly what the handlers read. */
function touchTap(h, el, x, y) {
  const win = h.document.defaultView;
  const start = new win.Event('touchstart', { bubbles: true });
  start.touches = [{ clientX: x, clientY: y }];
  el.dispatchEvent(start);
  const end = new win.Event('touchend', { bubbles: true });
  el.dispatchEvent(end);
}

test('touch double-tap opens reactions; the armor eats the ghost click, then real taps land', async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());
  const bubble = await openThreadWithReply(h);

  // Two clean taps, 200 fake-ms apart, 4px drift — a double-tap.
  touchTap(h, bubble, 180, 300);
  await h.tick(200);
  touchTap(h, bubble, 184, 302);
  await h.flush();
  assert.equal(h.$('action-sheet').classList.contains('hidden'), false, 'double-tap opened the sheet');
  assert.equal(h.qsa('#action-reactions .action-reaction').length > 0, true, 'react mode');

  // The synthetic click lands on a reaction button within 350ms — swallowed.
  const before = h.callsTo('/api/react').length;
  const thumbs = h.qsa('#action-reactions .action-reaction').find((b) => b.textContent === '👍');
  await h.click(thumbs);
  assert.equal(h.callsTo('/api/react').length, before, 'ghost click within 350ms is swallowed');
  assert.equal(h.$('action-sheet').classList.contains('hidden'), false, 'sheet survives the ghost');

  // Past the armor window, a real tap reacts normally.
  await h.tick(400);
  await h.click(thumbs);
  assert.equal(h.callsTo('/api/react').length, before + 1, 'a real tap lands after the window');
  assert.equal(h.lastCallTo('/api/react').body.emoji, '👍');
});

test('two slow taps or a hold do NOT open the reaction sheet', async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());
  const bubble = await openThreadWithReply(h);

  // Slow pair (500 fake-ms apart) — not a double-tap.
  touchTap(h, bubble, 180, 300);
  await h.tick(500);
  touchTap(h, bubble, 180, 300);
  await h.flush();
  assert.equal(h.$('action-sheet').classList.contains('hidden'), true, 'slow taps stay inert');

  // A 500ms hold opens the MENU (and is not tap #1 of a double-tap).
  const win = h.document.defaultView;
  const start = new win.Event('touchstart', { bubbles: true });
  start.touches = [{ clientX: 180, clientY: 300 }];
  bubble.dispatchEvent(start);
  await h.tick(520);   // the hold fires
  bubble.dispatchEvent(new win.Event('touchend', { bubbles: true }));
  await h.flush();
  assert.equal(h.$('action-sheet').classList.contains('hidden'), false, 'hold opened the sheet');
  assert.equal(h.$('action-copy').classList.contains('hidden'), false, 'menu mode (Copy offered)');
  assert.equal(h.qsa('#action-reactions .action-reaction').length, 0, 'no reactions on a hold');
});
