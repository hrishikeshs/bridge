'use strict';
/* Behaviour #6 — idle screensaver.
   On the list, after IDLE_SCREENSAVER_MS of no interaction the UI dims to reveal
   the scenery; any activity or an incoming moment-event wakes it. Uses the fake
   clock. */

const { test } = require('node:test');
const assert = require('node:assert/strict');
const { loadApp } = require('./harness');

const IDLE = 45000; // IDLE_SCREENSAVER_MS in app.js

function engaged(h) { return h.document.documentElement.classList.contains('screensaver'); }

test('idle for the timeout engages the screensaver on the list', async (t) => {
  const h = await loadApp(); // default wallpaper "airy" → eligible; view = list
  t.after(() => h.teardown());

  assert.equal(engaged(h), false, 'not dimmed immediately');
  await h.tick(IDLE + 1);
  assert.equal(engaged(h), true, 'dimmed after the idle timeout');
});

test('user activity wakes the screensaver', async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());

  await h.tick(IDLE + 1);
  assert.equal(engaged(h), true);

  h.dispatch('pointerdown'); // capture-phase idle listener swallows + wakes
  assert.equal(engaged(h), false, 'a tap wakes the UI');
});

test('an incoming moment-event (reply) wakes the screensaver', async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());

  await h.tick(IDLE + 1);
  assert.equal(engaged(h), true);

  await h.sse({ id: 100, type: 'reply', agent: 'vint', name: 'vint', text: 'ping', ts: '2026-07-07T10:00:00Z' });
  assert.equal(engaged(h), false, 'a new message wakes the UI');
});

test('the screensaver does not engage inside a thread (no scenery there)', async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());

  await h.openContact('vint'); // thread view is ineligible
  await h.tick(IDLE + 1);
  assert.equal(engaged(h), false, 'threads stay lit');
});

test('the screensaver does not engage when the wallpaper is off', async (t) => {
  const h = await loadApp();
  t.after(() => h.teardown());
  h.win.localStorage.setItem('wallpaper', 'off'); // currentWallpaper() → off → ineligible

  await h.tick(IDLE + 1);
  assert.equal(engaged(h), false, 'no scenery, no screensaver');
});
