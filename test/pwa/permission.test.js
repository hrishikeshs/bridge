'use strict';
/* Behaviour #4 — permission card.
   promptOptions parses the dialog's numbered options; each approve button sends
   the correct BARE key to /api/approve; the deny path points the composer at
   the guidance ("what to do differently") input. */

const { test } = require('node:test');
const assert = require('node:assert/strict');
const { loadApp } = require('./harness');

const PROMPT = [
  'Claude wants to run: rm -rf /tmp/x',
  'Do you want to proceed?',
  '❯ 1. Yes',
  "  2. Yes, and don't ask again this session",
  '  3. No, and tell Claude what to do differently',
].join('\n');

function keyButton(h, prefix) {
  return h.qsa('#feed .keys .key-opt').find((b) => b.textContent.startsWith(prefix));
}

async function raiseCard(h) {
  await h.openContact('vint');
  await h.sse({ id: 100, type: 'attention', agent: 'vint', name: 'vint', text: PROMPT });
}

test('parses the dialog options into labelled buttons plus Esc', async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());
  await raiseCard(h);

  const labels = h.qsa('#feed .keys .key-opt').map((b) => b.textContent);
  assert.deepEqual(labels, [
    '1. Yes',
    "2. Yes, and don't ask again this session",
    '3. No, and tell Claude what to do differently',
    '⎋ Esc',
  ]);
  // the "No…" option carries the deny styling hook
  assert.equal(keyButton(h, '3.').classList.contains('deny'), true);
});

test('approve buttons POST the correct bare key', async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());
  await raiseCard(h);

  await h.click(keyButton(h, '1.'));
  assert.deepEqual(h.lastCallTo('/api/approve').body, { agent: 'vint', key: '1' });

  await h.click(keyButton(h, '2.'));
  assert.deepEqual(h.lastCallTo('/api/approve').body, { agent: 'vint', key: '2' });

  await h.click(h.qs('#feed .keys .key-opt.esc'));
  assert.deepEqual(h.lastCallTo('/api/approve').body, { agent: 'vint', key: 'esc' });
});

test('the deny path sends key 3 AND opens the guidance composer', async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());
  await raiseCard(h);

  await h.click(keyButton(h, '3.'));
  assert.deepEqual(h.lastCallTo('/api/approve').body, { agent: 'vint', key: '3' }, 'bare key on the wire is still 3');
  assert.match(h.$('msg-input').placeholder, /what to do differently/i, 'composer redirected to guidance');
});

test('fallback: an unparseable prompt still offers Yes/No/Esc with the canonical keys', async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());
  await h.openContact('vint');
  await h.sse({ id: 100, type: 'attention', agent: 'vint', name: 'vint', text: 'A prompt with no numbered options at all.' });

  const labels = h.qsa('#feed .keys .key-opt').map((b) => b.textContent);
  assert.deepEqual(labels, ['1. Yes', '3. No', '⎋ Esc']);

  await h.click(keyButton(h, '1.'));
  assert.equal(h.lastCallTo('/api/approve').body.key, '1');
});
