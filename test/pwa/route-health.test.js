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
    { id: 'e', name: 'prompt-agent', status: 'live', health: 'prompt' },
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

// 2026-07-08 refactor review, C9: Focus filters on message recency, but an
// ALERTING row (held/stale route, open permission prompt) must never be hidden
// — a held route stops producing messages by definition. With no messages in
// history at all, every row is "quiet": pre-fix, Focus showed only the 2-row
// floor; post-fix, every alerting row survives and the quiet healthy row is
// still filtered (proving the filter itself still works).
test('Focus never hides an alerting row (held/stale/prompt exempt from the quiet filter)', async (t) => {
  const h = await loadApp({ status, localStorage: { focus: '1' } });
  t.after(() => h.teardown());
  for (const name of ['held-agent', 'busy-agent', 'stale-agent', 'prompt-agent']) {
    assert.ok(rowByName(h, name), name + ' visible under Focus despite no recent messages');
  }
  assert.equal(rowByName(h, 'healthy-agent'), undefined,
    'quiet healthy row is still focus-filtered (the exemption is alert-only)');
});
