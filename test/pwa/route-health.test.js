'use strict';
/* Behaviour — Route Health L1 render (docs/route-health.md).
   A canned /api/status carrying the daemon-derived hold_reason / last_seen_s →
   the row shows a held (amber) or stale (grey) dot plus an amber "held/stale"
   preview; a healthy contact (no hold_reason, e.g. an older daemon) renders
   exactly as before. Pins the graceful-degradation + labeling contract. */

const { test } = require('node:test');
const assert = require('node:assert/strict');
const { loadApp } = require('./harness');

function rowByName(h, name) {
  return h.qsa('#contact-list .row').find((r) => {
    const n = r.querySelector('.row-name');
    return n && n.textContent === name;
  });
}

const status = {
  contacts: [
    { id: 'a', name: 'held-agent', status: 'live', health: 'ok', hold_reason: 'at-prompt' },
    { id: 'b', name: 'busy-agent', status: 'live', health: 'working', hold_reason: 'busy' },
    { id: 'c', name: 'stale-agent', status: 'live', health: 'ok', hold_reason: 'stale', last_seen_s: 720 },
    { id: 'd', name: 'healthy-agent', status: 'live', health: 'ok' },
  ],
  rooms: [{ id: 'room:crew', name: '#crew' }],
  my_status: '',
  started: 1,
};

test('held route → amber held dot + alert "held — <reason>" preview', async (t) => {
  const h = await loadApp({ status });
  t.after(() => h.teardown());
  const row = rowByName(h, 'held-agent');
  assert.match(row.querySelector('.status-dot').className, /\bheld\b/, 'held dot class');
  const pv = row.querySelector('.row-preview');
  assert.match(pv.className, /\balert\b/, 'amber alert-styled preview');
  assert.match(pv.textContent, /held — at a prompt/, 'reason labeled');
});

test('busy route → held dot + "held — busy"', async (t) => {
  const h = await loadApp({ status });
  t.after(() => h.teardown());
  const row = rowByName(h, 'busy-agent');
  assert.match(row.querySelector('.status-dot').className, /\bheld\b/);
  assert.match(row.querySelector('.row-preview').textContent, /held — busy/);
});

test('stale route → grey stale dot + humanized last-seen age', async (t) => {
  const h = await loadApp({ status });
  t.after(() => h.teardown());
  const row = rowByName(h, 'stale-agent');
  assert.match(row.querySelector('.status-dot').className, /\bstale\b/, 'stale dot class');
  assert.match(row.querySelector('.row-preview').textContent, /stale — last seen 12m/, 'last_seen_s 720 → 12m');
});

test('healthy route (no hold_reason) → normal health dot, no held/stale', async (t) => {
  const h = await loadApp({ status });
  t.after(() => h.teardown());
  const cls = rowByName(h, 'healthy-agent').querySelector('.status-dot').className;
  assert.match(cls, /\bok\b/, 'falls through to the health dot');
  assert.doesNotMatch(cls, /\b(held|stale)\b/, 'no route-health class when the field is absent');
});
