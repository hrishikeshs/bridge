'use strict';
/* Behaviour #5 — compact flow.
   Tapping the enabled gauge opens the confirm sheet; confirm POSTs
   /api/compact {agent}; 409/400 show the gentle toast; a disabled gauge does
   nothing. */

const { test } = require('node:test');
const assert = require('node:assert/strict');
const { loadApp } = require('./harness');

function statusWith(fields) {
  return {
    contacts: [Object.assign({ id: 'vint', name: 'vint', status: 'live', health: 'ok', attention: false, context_pct: 88 }, fields)],
    rooms: [{ id: 'room:crew', name: '#crew' }],
    my_status: '', started: 1,
  };
}

test('tapping the gauge opens a confirm sheet naming the agent', async (t) => {
  const h = await loadApp({ status: statusWith({}) });
  t.after(() => h.teardown());
  await h.openContact('vint');

  assert.equal(h.$('ctx-confirm'), null, 'no confirm sheet before the tap');
  await h.click('ctx-gauge');
  assert.equal(h.$('ctx-confirm').classList.contains('hidden'), false, 'confirm sheet opened');
  assert.match(h.$('ctx-confirm-msg').textContent, /Compact vint/);
});

test('confirming POSTs /api/compact {agent} and holds a "compacting…" state', async (t) => {
  const h = await loadApp({ status: statusWith({}) });
  t.after(() => h.teardown());
  await h.openContact('vint');

  await h.click('ctx-gauge');
  await h.click('ctx-confirm-ok');

  const call = h.lastCallTo('/api/compact');
  assert.ok(call, '/api/compact was called');
  assert.deepEqual(call.body, { agent: 'vint' }, 'body carries just the agent id — never a typed command');
  assert.equal(h.$('ctx-confirm').classList.contains('hidden'), true, 'sheet closed on confirm');
  assert.equal(h.qs('.ctx-gauge-label').textContent, 'compacting…', 'optimistic in-flight state');
});

test('cancel closes the sheet without calling /api/compact', async (t) => {
  const h = await loadApp({ status: statusWith({}) });
  t.after(() => h.teardown());
  await h.openContact('vint');

  await h.click('ctx-gauge');
  await h.click('ctx-confirm-cancel');
  assert.equal(h.$('ctx-confirm').classList.contains('hidden'), true, 'sheet closed');
  assert.equal(h.callsTo('/api/compact').length, 0, 'nothing sent on cancel');
});

test('409 (busy/working) shows the gentle toast and undoes the optimistic state', async (t) => {
  const h = await loadApp({ status: statusWith({}) });
  t.after(() => h.teardown());
  h.handlers['/api/compact'] = () => ({ status: 409, body: { error: 'busy' } });
  await h.openContact('vint');

  await h.click('ctx-gauge');
  await h.click('ctx-confirm-ok');
  await h.flush();

  assert.match(h.$('ctx-toast').textContent, /working/i, 'toast: agent is working');
  assert.equal(h.$('ctx-toast').classList.contains('hidden'), false, 'toast visible');
  assert.notEqual(h.qs('.ctx-gauge-label').textContent, 'compacting…', 'optimistic state rolled back');
});

test('400 (offline) shows the offline toast', async (t) => {
  const h = await loadApp({ status: statusWith({}) });
  t.after(() => h.teardown());
  h.handlers['/api/compact'] = () => ({ status: 400, body: { error: 'offline' } });
  await h.openContact('vint');

  await h.click('ctx-gauge');
  await h.click('ctx-confirm-ok');
  await h.flush();

  assert.match(h.$('ctx-toast').textContent, /offline/i);
});

test('a disabled gauge does nothing on tap', async (t) => {
  const h = await loadApp({ status: statusWith({ health: 'working' }) });
  t.after(() => h.teardown());
  await h.openContact('vint');

  assert.equal(h.$('ctx-gauge').disabled, true, 'precondition: gauge disabled while working');
  await h.click('ctx-gauge');
  assert.equal(h.$('ctx-confirm'), null, 'no confirm sheet');
  assert.equal(h.callsTo('/api/compact').length, 0, 'no compact request');
});
