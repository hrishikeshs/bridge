'use strict';
/* Behaviour #1 — roster render.
   A canned /api/status → the contact list renders the right rows, names,
   transport badges (emacs/magnus), health dots, and the #crew room row. */

const { test } = require('node:test');
const assert = require('node:assert/strict');
const { loadApp } = require('./harness');

function rowByName(h, name) {
  return h.qsa('#contact-list .row').find((r) => {
    const n = r.querySelector('.row-name');
    return n && n.textContent === name;
  });
}

test('renders one row per contact plus the #crew room row', async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());

  assert.ok(rowByName(h, 'vint'), 'vint row present');
  assert.ok(rowByName(h, 'ludwig'), 'ludwig row present');
  assert.ok(rowByName(h, 'marvin'), 'marvin row present');

  const room = h.qs('#contact-list .row.room');
  assert.ok(room, '#crew room row present');
  assert.equal(room.querySelector('.room-avatar').textContent, '📢', 'room monogram');
  assert.equal(room.querySelector('.row-name').textContent, '#crew', 'room name');
  assert.match(room.querySelector('.row-status').textContent, /party line/, 'room subtitle');

  assert.equal(h.$('list-empty').classList.contains('hidden'), true, 'empty state hidden when contacts exist');
});

test('transport badges show flavor for client-hosted agents, nothing for tmux', async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());

  assert.equal(rowByName(h, 'vint').querySelector('.transport-badge').textContent, 'emacs');
  assert.equal(rowByName(h, 'ludwig').querySelector('.transport-badge').textContent, 'magnus');
  assert.equal(rowByName(h, 'marvin').querySelector('.transport-badge'), null, 'no flavor → no badge');
});

test('health dots reflect ok / working / offline', async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());

  assert.match(rowByName(h, 'vint').querySelector('.status-dot').className, /\bok\b/);
  assert.match(rowByName(h, 'ludwig').querySelector('.status-dot').className, /\bworking\b/);
  assert.match(rowByName(h, 'marvin').querySelector('.status-dot').className, /\boffline\b/);
  assert.equal(rowByName(h, 'marvin').classList.contains('offline'), true, 'offline row styled');
});

test('names render from the roster', async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());
  const names = h.qsa('#contact-list .row .row-name').map((n) => n.textContent).sort();
  assert.deepEqual(names, ['#crew', 'ludwig', 'marvin', 'vint']);
});

test('empty roster shows onboarding and no room row', async (t) => {
  const h = await loadApp({ status: { contacts: [], rooms: [{ id: 'room:crew', name: '#crew' }], my_status: '', started: 1 } });
  t.after(() => h.teardown());

  assert.equal(h.$('list-empty').classList.contains('hidden'), false, 'onboarding shown');
  assert.equal(h.qs('#contact-list .row'), null, 'no rows (room hidden until first contact)');
});
