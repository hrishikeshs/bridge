'use strict';
/* Behaviour #7 — push button + unread badges + deep-link.
   Exercised end-to-end under the stubs so these paths run without throwing. */

const { test } = require('node:test');
const assert = require('node:assert/strict');
const { loadApp } = require('./harness');

function rowByName(h, name) {
  return h.qsa('#contact-list .row').find((r) => {
    const n = r.querySelector('.row-name');
    return n && n.textContent === name;
  });
}

/* ---- push button ---- */

test('push button is offered when notifications are supported but not yet granted', async (t) => {
  const h = await loadApp({ notificationPermission: 'default' });
  t.after(() => h.teardown());
  assert.equal(h.$('enable-push').classList.contains('hidden'), false, 'button shown to prompt on tap');
});

test('push button hides and the subscribe flow runs when already granted', async (t) => {
  const h = await loadApp({ notificationPermission: 'granted' });
  t.after(() => h.teardown());

  assert.equal(h.$('enable-push').classList.contains('hidden'), true, 'nothing to prompt — button hidden');
  const ok = await h.waitUntil(() => h.callsTo('/api/push/subscribe').length > 0);
  assert.ok(ok, 'the subscribe path ran to /api/push/subscribe without throwing');
  assert.ok(h.callsTo('/api/push/key').length > 0, 'fetched the VAPID key en route');
});

/* ---- unread badges ---- */

test('an inbound reply while on the list raises the row badge and title count', async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());

  assert.equal(rowByName(h, 'vint').querySelector('.row-badge'), null, 'no unread yet');
  await h.sse({ id: 100, type: 'reply', agent: 'vint', name: 'vint', text: 'need a hand', ts: '2026-07-07T10:00:00Z' });

  assert.equal(rowByName(h, 'vint').querySelector('.row-badge').textContent, '1', 'row badge counts the unread');
  assert.equal(h.title(), '(1) bridge', 'title carries the total unread');
});

test('opening the thread clears its unread', async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());

  await h.sse({ id: 100, type: 'reply', agent: 'vint', name: 'vint', text: 'hi', ts: '2026-07-07T10:00:00Z' });
  assert.equal(h.title(), '(1) bridge');

  await h.openContact('vint'); // marks seen
  assert.equal(h.title(), 'bridge', 'title cleared once read');
});

/* ---- deep-link ---- */

test('a #/c/<id> deep-link cold-opens straight into that thread', async (t) => {
  const h = await loadApp({ url: 'http://localhost/#/c/vint' });
  t.after(() => h.teardown());

  assert.equal(h.$('thread-view').classList.contains('hidden'), false, 'landed in the thread');
  assert.equal(h.$('thread-name').textContent, 'vint');
});

test('a deep-link to an unknown id falls back to the list (never trusts the id)', async (t) => {
  const h = await loadApp({ url: 'http://localhost/#/c/not-a-real-agent' });
  t.after(() => h.teardown());

  assert.equal(h.$('list-view').classList.contains('hidden'), false, 'stayed on the list');
  assert.equal(h.$('thread-view').classList.contains('hidden'), true, 'no thread opened');
});
