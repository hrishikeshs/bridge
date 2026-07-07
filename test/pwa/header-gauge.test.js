'use strict';
/* Behaviour #2 — thread + header + context gauge.
   Opening a contact renders the thread; updateThreadHeader shows status; the
   CONTEXT GAUGE appears only at context_pct>=70, fills to the %, and is DISABLED
   when health!=="ok" (the just-shipped feature). */

const { test } = require('node:test');
const assert = require('node:assert/strict');
const { loadApp } = require('./harness');

function statusWith(fields) {
  return {
    contacts: [Object.assign({ id: 'vint', name: 'vint', status: 'live', health: 'ok', attention: false }, fields)],
    rooms: [{ id: 'room:crew', name: '#crew' }],
    my_status: '', started: 1,
  };
}

test('opening a contact swaps to the thread view with name + status', async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());

  await h.openContact('vint');
  assert.equal(h.$('thread-view').classList.contains('hidden'), false, 'thread view shown');
  assert.equal(h.$('list-view').classList.contains('hidden'), true, 'list view hidden');
  assert.equal(h.$('thread-name').textContent, 'vint');
  assert.equal(h.$('thread-status').textContent, 'live', 'health ok → bare "live"');
});

test('header status includes health label when working', async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());
  await h.openContact('ludwig'); // ludwig health "working" in default roster
  assert.equal(h.$('thread-name').textContent, 'ludwig');
  assert.match(h.$('thread-status').textContent, /live · working/);
});

test('no gauge below 70%', async (t) => {
  const h = await loadApp({ status: statusWith({ context_pct: 50 }) });
  t.after(() => h.teardown());
  await h.openContact('vint');
  assert.equal(h.$('ctx-gauge'), null, 'gauge absent under threshold');
});

test('no gauge when context_pct is unknown (field absent)', async (t) => {
  const h = await loadApp({ status: statusWith({}) });
  t.after(() => h.teardown());
  await h.openContact('vint');
  assert.equal(h.$('ctx-gauge'), null, 'gauge absent when % unknown');
});

test('gauge appears at >=70%, fills to the actual %, enabled when health ok', async (t) => {
  const h = await loadApp({ status: statusWith({ context_pct: 85, health: 'ok' }) });
  t.after(() => h.teardown());
  await h.openContact('vint');

  const g = h.$('ctx-gauge');
  assert.ok(g, 'gauge present at 85%');
  assert.equal(h.qs('.ctx-gauge-fill', g).style.width, '85%', 'fill width tracks %');
  assert.equal(h.qs('.ctx-gauge-label', g).textContent, '85%', 'label shows %');
  assert.equal(g.disabled, false, 'enabled while idle (health ok)');
  assert.equal(g.getAttribute('aria-disabled'), 'false');
});

test('gauge is DISABLED (greyed) when health is not ok — working', async (t) => {
  const h = await loadApp({ status: statusWith({ context_pct: 85, health: 'working' }) });
  t.after(() => h.teardown());
  await h.openContact('vint');

  const g = h.$('ctx-gauge');
  assert.ok(g, 'gauge still shown (>=70%)');
  assert.equal(g.disabled, true, 'disabled mid-thought');
  assert.equal(g.getAttribute('aria-disabled'), 'true');
});

test('gauge is DISABLED when the agent is prompting', async (t) => {
  const h = await loadApp({ status: statusWith({ context_pct: 92, health: 'prompt' }) });
  t.after(() => h.teardown());
  await h.openContact('vint');
  assert.equal(h.$('ctx-gauge').disabled, true, 'no compacting an agent mid-prompt');
});

test('rooms never get a gauge', async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());
  await h.openContact('#crew');
  assert.equal(h.$('thread-name').textContent, '#crew');
  assert.equal(h.$('thread-status').textContent, 'everyone', 'room header is fixed');
  assert.equal(h.$('ctx-gauge'), null, 'no gauge in a room');
});
