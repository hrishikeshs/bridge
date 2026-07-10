'use strict';
/* Feature #34 — select a conversation fragment, export as PNG.

   Pins the selection-mode contract end-to-end through the app's real wiring:
   entry from the long-press menu (pressed bubble pre-selected), taps toggling
   whole messages, the count bar, gesture suspension while selecting, exits
   (cancel / thread switch), and the export handoff. The canvas painting
   itself can't run under jsdom (getContext is null) — the module degrades
   with a toast, which is itself pinned — so the PNG's looks are calibrated
   on-device; everything up to the canvas is pinned here. */

const { test } = require('node:test');
const assert = require('node:assert/strict');
const { loadApp } = require('./harness');

const REPLY_A = { id: 400, type: 'reply', agent: 'vint', name: 'vint', text: 'the fog knows the sun now', ts: '2026-07-10T18:00:00Z' };
const REPLY_B = { id: 401, type: 'reply', agent: 'vint', name: 'vint', text: 'and the bridge surfaces at burn-off', ts: '2026-07-10T18:01:00Z' };

async function enterSelection(h) {
  await h.openContact('vint');
  await h.sse(REPLY_A);
  await h.sse(REPLY_B);
  const bubble = h.qsa('#feed .msg.reply')[0];
  h.dispatch('contextmenu', bubble);
  await h.flush();
  assert.equal(h.$('action-select').classList.contains('hidden'), false, 'Select… offered in the menu');
  await h.click('action-select');
  await h.flush();
}

test('Select… enters selection mode with the pressed bubble pre-selected', async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());
  await enterSelection(h);

  assert.equal(h.$('action-sheet').classList.contains('hidden'), true, 'menu closed');
  assert.equal(h.$('select-bar').classList.contains('hidden'), false, 'selection bar up');
  assert.equal(h.$('select-count').textContent, '1 selected');
  assert.equal(h.qsa('#feed .msg.sel-on').length, 1, 'the pressed bubble wears the ring');
  assert.equal(h.$('msg-input').closest('#composer').isConnected, true, 'composer still in DOM (CSS hides it)');
});

test('taps toggle messages; the bar counts; export disables at zero', async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());
  await enterSelection(h);

  const bubbles = h.qsa('#feed .msg.reply');
  await h.click(bubbles[1]);           // second message joins
  assert.equal(h.$('select-count').textContent, '2 selected');
  assert.equal(h.qsa('#feed .msg.sel-on').length, 2);

  await h.click(h.qsa('#feed .msg.reply')[0]);   // re-query: renderFeed rebuilt the DOM
  await h.click(h.qsa('#feed .msg.reply')[1]);
  assert.equal(h.$('select-count').textContent, '0 selected');
  assert.equal(/** @type {any} */ (h.$('select-export')).disabled, true, 'Export disabled with nothing selected');
});

test('gestures sleep while selecting: no sheets from dblclick or contextmenu', async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());
  await enterSelection(h);

  const bubble = h.qsa('#feed .msg.reply')[1];
  h.dispatch('dblclick', bubble);
  await h.flush();
  assert.equal(h.$('action-sheet').classList.contains('hidden'), true, 'double-tap raises nothing while selecting');
  h.dispatch('contextmenu', bubble);
  await h.flush();
  assert.equal(h.$('action-sheet').classList.contains('hidden'), true, 'long-press raises nothing while selecting');
  // and the dblclick's click toggled the bubble instead (1 pre-selected + this one… dblclick fires no click in jsdom, so count is unchanged)
  assert.equal(h.$('select-bar').classList.contains('hidden'), false, 'still selecting');
});

test('✕ cancels; leaving the thread abandons the selection', async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());
  await enterSelection(h);

  await h.click('select-cancel');
  assert.equal(h.$('select-bar').classList.contains('hidden'), true, 'bar gone on cancel');
  assert.equal(h.qsa('#feed .msg.sel-on').length, 0, 'rings cleared');

  // Re-enter, then navigate away — the mode must not survive the trip.
  const bubble = h.qsa('#feed .msg.reply')[0];
  h.dispatch('contextmenu', bubble);
  await h.flush();
  await h.click('action-select');
  await h.flush();
  assert.equal(h.$('select-bar').classList.contains('hidden'), false);
  await h.click('back-btn');
  await h.flush();
  await h.openContact('vint');
  assert.equal(h.$('select-bar').classList.contains('hidden'), true, 'selection died at the thread boundary');
  assert.equal(h.qsa('#feed .msg.sel-on').length, 0);
});

test('Export under jsdom degrades honestly (no canvas → toast, mode stays)', async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());
  await enterSelection(h);

  await h.click('select-export');
  await h.flush();
  const toast = h.document.getElementById('app-toast');
  assert.ok(toast && !toast.classList.contains('hidden'), 'a toast explains');
  assert.match(toast.textContent, /isn.t supported here/, 'the no-canvas guard fired');
  assert.equal(h.$('select-bar').classList.contains('hidden'), false, 'selection preserved for retry');
});
