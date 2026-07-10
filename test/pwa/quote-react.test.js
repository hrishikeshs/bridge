'use strict';
/* Behaviours added on branch pwa-quote-react-fixes.

   BUG 1 — tapping "Quote" ARMS the quote (chip up, next reply carries it) but
           must NOT focus the composer: focusing the textarea pops the iOS
           keyboard the instant Quote is tapped (iPhone daily-driver report).
   BUG 2 — the reaction row keeps its 6 quick taps AND adds a "+" that opens the
           phone's NATIVE emoji keyboard; the first emoji entered flows through
           the SAME react()/POST /api/react path as a quick tap ("take the first
           emoji" guard). Also exercises the previously-uncovered outbound
           react() path.

   NOTE: the harness's fake fetch returns 200 for /api/react regardless of the
   emoji, so these tests assert the PWA-side contract (the POST fires with the
   chosen emoji). The real daemon still whitelists 6 emoji and 400s the rest
   (httpapi.go reactionEmoji) — opening that up is a companion daemon change,
   out of this branch's PWA file scope. */

const { test } = require('node:test');
const assert = require('node:assert/strict');
const { loadApp } = require('./harness');

const REPLY = { id: 100, type: 'reply', agent: 'vint', name: 'vint', text: 'shipped the fix', ts: '2026-07-07T10:00:00Z' };

// Raise the SELECT menu (feature #31: long-press / right-click). The desktop
// 'contextmenu' seam opens the same sheet as a touch long-press (app.js wires
// both to openActionSheet), without needing the fake clock's 500ms hold.
async function raiseSheetOnReply(h) {
  await h.openContact('vint');
  await h.sse(REPLY);
  const bubble = h.qs('#feed .msg.reply');
  assert.ok(bubble, 'a reply bubble to long-press');
  h.dispatch('contextmenu', bubble);
  await h.flush();
  assert.equal(h.$('action-sheet').classList.contains('hidden'), false, 'action sheet is open');
  return bubble;
}

// Raise the REACTION sheet (feature #31: reactions moved off the long-press
// menu onto double-tap; 'dblclick' is its desktop seam).
async function raiseReactionsOnReply(h) {
  await h.openContact('vint');
  await h.sse(REPLY);
  const bubble = h.qs('#feed .msg.reply');
  assert.ok(bubble, 'a reply bubble to double-tap');
  h.dispatch('dblclick', bubble);
  await h.flush();
  assert.equal(h.$('action-sheet').classList.contains('hidden'), false, 'reaction sheet is open');
  return bubble;
}

test('BUG 1 — Quote arms the quote WITHOUT focusing the composer (no keyboard pop)', async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());
  await raiseSheetOnReply(h);

  await h.click('action-quote');

  // The quote feature still works end-to-end: chip shown, carrying the excerpt.
  assert.equal(h.$('quote-chip').classList.contains('hidden'), false, 'quote chip is shown');
  assert.match(h.$('quote-chip-text').textContent, /shipped the fix/, 'chip carries the quoted excerpt');
  // …but the composer did NOT steal focus — that is exactly what pops the keyboard.
  assert.notEqual(h.document.activeElement, h.$('msg-input'), 'composer must NOT be focused after Quote');
  assert.equal(h.$('action-sheet').classList.contains('hidden'), true, 'action sheet closed after Quote');
});

test('BUG 2 — the 6 quick reactions remain, plus a "+" more affordance', async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());
  await raiseReactionsOnReply(h);

  const quick = h.qsa('#action-reactions .action-reaction:not(.action-reaction-more)');
  assert.equal(quick.length, 6, 'the 6 quick-tap reactions are still offered');
  assert.ok(quick.some((b) => b.textContent === '👍'), 'thumbs-up still offered');
  assert.ok(quick.some((b) => b.textContent === '🚀'), 'rocket still offered');

  const more = h.qs('#action-reactions .action-reaction-more');
  assert.ok(more, 'a "+" affordance is present');
  assert.equal(more.textContent, '+');
  // the native-keyboard capture input exists but is hidden until "+" is tapped.
  assert.equal(h.$('action-emoji-input').classList.contains('hidden'), true, 'emoji input hidden until +');
});

test('BUG 2 — a quick tap sends its emoji via /api/react (covers react())', async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());
  await raiseReactionsOnReply(h);

  const thumbs = h.qsa('#action-reactions .action-reaction').find((b) => b.textContent === '👍');
  await h.click(thumbs);

  assert.deepEqual(h.lastCallTo('/api/react').body, { agent: 'vint', event_id: 100, emoji: '👍' });
  assert.equal(h.$('action-sheet').classList.contains('hidden'), true, 'sheet closed after a reaction');
});

test('BUG 2 — "+" reveals the native picker; the first emoji routes through /api/react', async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());
  await raiseReactionsOnReply(h);

  await h.click(h.qs('#action-reactions .action-reaction-more'));
  const picker = h.$('action-emoji-input');
  assert.equal(picker.classList.contains('hidden'), false, '+ reveals the capture input');
  assert.equal(h.document.activeElement, picker, '+ focuses the input so the OS keyboard opens');

  // The emoji keyboard inserts a NON-whitelisted emoji, then a second one — proving
  // "take the first emoji" (multi-grapheme guard).
  picker.value = '🎯🔥';
  h.dispatch('input', picker);
  await h.flush();

  assert.deepEqual(h.lastCallTo('/api/react').body, { agent: 'vint', event_id: 100, emoji: '🎯' },
    'the chosen (first) emoji flows through the SAME react()/POST as a quick tap');
  assert.equal(h.$('action-sheet').classList.contains('hidden'), true, 'sheet closes on pick');
});

test('BUG 2 — a stray non-emoji keystroke does not fire a reaction', async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());
  await raiseReactionsOnReply(h);

  await h.click(h.qs('#action-reactions .action-reaction-more'));
  const picker = h.$('action-emoji-input');
  const before = h.callsTo('/api/react').length;

  picker.value = 'a';            // keyboard still on letters — nothing pickable yet
  h.dispatch('input', picker);
  await h.flush();
  assert.equal(h.callsTo('/api/react').length, before, 'no reaction fired on a letter');
  assert.equal(h.$('action-sheet').classList.contains('hidden'), false, 'sheet stays open, awaiting an emoji');

  picker.value = 'a😎';          // then an emoji arrives — the first emoji wins
  h.dispatch('input', picker);
  await h.flush();
  assert.deepEqual(h.lastCallTo('/api/react').body, { agent: 'vint', event_id: 100, emoji: '😎' });
});
