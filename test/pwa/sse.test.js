'use strict';
/* Behaviour #3 — SSE handling.
   Injecting an 'attention' frame raises the card; 'approved'/'attention-clear'
   resolves it; a resolved card re-expands on tap; 'reply' appends a bubble;
   'reaction'/'status' FOLD (no bubble). The reconnect/heartbeat path loads
   without throwing. */

const { test } = require('node:test');
const assert = require('node:assert/strict');
const { loadApp } = require('./harness');

const PROMPT = 'Claude wants to run a command\n❯ 1. Yes\n  3. No, and tell Claude what to do differently';

test("'attention' frame raises the live approval card", async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());
  await h.openContact('vint');

  await h.sse({ id: 100, type: 'attention', agent: 'vint', name: 'vint', text: PROMPT });

  const card = h.qs('#feed .attention');
  assert.ok(card, 'attention card rendered');
  assert.equal(card.classList.contains('resolved'), false, 'live, not resolved');
  assert.ok(h.qs('#feed .keys .key-opt'), 'approval buttons present');
});

test("'approved' resolves the card (Approved from phone)", async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());
  await h.openContact('vint');

  await h.sse({ id: 100, type: 'attention', agent: 'vint', name: 'vint', text: PROMPT });
  await h.sse({ id: 101, type: 'approved', agent: 'vint', name: 'vint', text: '1' });

  const card = h.qs('#feed .attention');
  assert.equal(card.classList.contains('resolved'), true, 'card collapsed');
  assert.match(h.qs('#feed .attn-resolved').textContent, /Approved from phone/);
  assert.equal(h.qs('#feed .keys'), null, 'no live buttons once resolved');
});

test("'attention-clear' resolves the card (Resolved)", async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());
  await h.openContact('vint');

  await h.sse({ id: 100, type: 'attention', agent: 'vint', name: 'vint', text: PROMPT });
  await h.sse({ id: 101, type: 'attention-clear', agent: 'vint', name: 'vint', text: '' });

  const card = h.qs('#feed .attention');
  assert.equal(card.classList.contains('resolved'), true);
  assert.match(h.qs('#feed .attn-resolved').textContent, /Resolved/);
});

test('a resolved card re-expands on tap and shows the captured prompt', async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());
  await h.openContact('vint');

  await h.sse({ id: 100, type: 'attention', agent: 'vint', name: 'vint', text: PROMPT });
  await h.sse({ id: 101, type: 'approved', agent: 'vint', name: 'vint', text: '1' });

  const card = h.qs('#feed .attention.resolved');
  assert.equal(card.classList.contains('expandable'), true, 'resolved card is expandable');
  const detail = h.qs('#feed .attn-detail');
  assert.equal(detail.classList.contains('hidden'), true, 'detail starts hidden');

  card.click();
  await h.flush();
  assert.equal(detail.classList.contains('hidden'), false, 'tap reveals the prompt snapshot');
  assert.match(detail.querySelector('.attn-detail-prompt').textContent, /wants to run a command/);

  card.click(); // tap again re-collapses
  await h.flush();
  assert.equal(detail.classList.contains('hidden'), true, 'tap again hides');
});

test("'reply' appends a message bubble", async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());
  await h.openContact('vint');

  const before = h.qsa('#feed .msg.reply').length;
  await h.sse({ id: 100, type: 'reply', agent: 'vint', name: 'vint', text: 'on it — pushing now', ts: '2026-07-07T10:00:00Z' });
  const after = h.qsa('#feed .msg.reply');
  assert.equal(after.length, before + 1, 'one reply bubble added');
  assert.match(after[after.length - 1].textContent, /on it — pushing now/);
});

test("'reaction' folds onto the target bubble — no new message", async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());
  await h.openContact('vint');

  await h.sse({ id: 100, type: 'reply', agent: 'vint', name: 'vint', text: 'shipped', ts: '2026-07-07T10:00:00Z' });
  const msgsBefore = h.qsa('#feed .msg').length;

  await h.sse({ id: 101, type: 'reaction', agent: 'vint', target: 100, text: '👍' });

  assert.equal(h.qsa('#feed .msg').length, msgsBefore, 'reaction created no bubble');
  const badges = h.qsa('#feed .reactions .reaction').map((r) => r.textContent);
  assert.deepEqual(badges, ['👍'], 'reaction folded onto the bubble as a badge');
});

test("'status' folds into the header — no bubble", async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());
  await h.openContact('vint');

  const msgsBefore = h.qsa('#feed .msg').length;
  await h.sse({ id: 100, type: 'status', agent: 'vint', name: 'vint', text: 'brb lunch' });

  assert.equal(h.qsa('#feed .msg').length, msgsBefore, 'status created no bubble');
  assert.match(h.$('thread-status').textContent, /brb lunch/, 'away line folded into header');
});

test('heartbeat marks connected; error triggers a reconnect that creates a new stream', async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());

  const first = h.esInstances.length;
  await h.sseNamed('hb');
  assert.ok(h.qsa('.dot.on').length >= 1, 'hb keeps the connection dot lit');

  h.es().emitError();
  await h.flush();
  assert.equal(h.qsa('.dot.on').length, 0, 'error drops the connection dot');

  await h.tick(1000); // capped-backoff reconnect timer
  assert.equal(h.esInstances.length, first + 1, 'reconnect opened a fresh EventSource');
});
