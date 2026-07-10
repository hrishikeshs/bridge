// @ts-check — type-checked against ./types.d.ts (see tsconfig.json). Dev-only:
// `@ts-check` + JSDoc are comments the browser ignores, so nothing ships changes.
/* bridge — feature #34: export a selected conversation fragment as a PNG.

   People keep asking what bridge looks and feels like; this answers with an
   ARTIFACT, not a screenshot: the selected messages are re-drawn on a canvas
   in the app's own visual language — theme colours read live off the CSS
   tokens, agent bubbles left, yours right, who-lines, timestamps, reaction
   badges, a quiet wordmark — at 2× for crispness, then handed to the OS share
   sheet. Everything happens on-device: no daemon call, no new permission; the
   image leaves the phone only through the user's own share action.

   The module is deliberately split: pure layout math (wrapLines, the export
   palette merge) is exported for unit tests; the canvas painting is thin and
   calibrated by eye on the actual phone. */

'use strict';

import { state, threadName } from './app.js';

/* ---------- pure layout helpers (unit-tested) ---------- */

/* Greedy word-wrap against an injected measure function (tests pass a fake;
   the painter passes ctx.measureText). Explicit newlines are respected; a
   single word wider than maxWidth is emitted alone on its line rather than
   looping forever (the canvas clips it — same behaviour as CSS word-wrap on
   an unbreakable token). */
/** @param {(s: string) => number} measure @param {string} text @param {number} maxWidth @returns {string[]} */
export function wrapLines(measure, text, maxWidth) {
  const out = [];
  for (const para of String(text || '').split('\n')) {
    const words = para.split(' ').filter((w) => w !== '');
    if (!words.length) { out.push(''); continue; }
    let line = '';
    for (const w of words) {
      const probe = line ? line + ' ' + w : w;
      if (line && measure(probe) > maxWidth) {
        out.push(line);
        line = w;
      } else if (!line && measure(w) > maxWidth) {
        out.push(w);          // unbreakable oversize token: own line, clipped
        line = '';
      } else {
        line = probe;
      }
    }
    if (line) out.push(line);
  }
  return out;
}

/* The events a selection exports, in thread order: stored message-like events
   of the CURRENT thread whose ids are selected. Pending echoes are excluded
   (they may still change); attention cards and system rows never export. */
/** @param {BridgeEvent[]} events @param {string | null} selected @param {Set<number>} ids @returns {BridgeEvent[]} */
export function selectedEventsInOrder(events, selected, ids) {
  return events.filter((e) =>
    e.agent === selected && ids.has(e.id) &&
    (e.type === 'reply' || e.type === 'mention' || e.type === 'peer' || e.type === 'sent'));
}

/* ---------- theme palette (read live off the CSS tokens) ---------- */

/** @returns {{ink:string, ground:string, hairline:string, glass:string, sent:string, sentInk:string, dim:string}} */
function exportPalette() {
  const css = getComputedStyle(document.documentElement);
  const v = (name, fallback) => (css.getPropertyValue(name) || '').trim() || fallback;
  return {
    ink: v('--ink', '#1F2933'),
    ground: v('--ground', '#FAF5EC'),
    hairline: v('--hairline', 'rgba(31,41,51,0.12)'),
    glass: v('--glass', 'rgba(255,255,255,0.72)'),
    sent: v('--export-sent', '#B3402A'),   // solid stand-in for the sent gradient
    sentInk: '#FFFFFF',
    dim: v('--ink-dim', 'rgba(31,41,51,0.55)'),
  };
}

/* ---------- the painter ---------- */

const S = 2;              // supersample: draw at 2× logical pixels
const W = 560 * S;        // logical 560pt wide — reads well in a chat share
const PAD = 20 * S;
const BUBBLE_MAX = Math.round(W * 0.74);
const FONT = (px, weight = 400) =>
  `${weight} ${px * S}px -apple-system, "SF Pro Text", "Helvetica Neue", "Segoe UI", sans-serif`;

/** @param {BridgeEvent[]} events @returns {HTMLCanvasElement | null} */
function paint(events) {
  const canvas = document.createElement('canvas');
  const probe = canvas.getContext && canvas.getContext('2d');
  if (!probe) return null;                       // jsdom / ancient engine
  const ctx = probe;
  const pal = exportPalette();

  // ---- measure pass: compute every bubble's box, then size the canvas ----
  ctx.font = FONT(15);
  const measure = (s) => ctx.measureText(s).width;
  const lineH = 20 * S, whoH = 16 * S, stampH = 14 * S, gap = 8 * S;
  const bubblePadX = 13 * S, bubblePadY = 9 * S;
  /** @type {Array<{ev: BridgeEvent, lines: string[], w: number, h: number, mine: boolean, whoLabel: string, reactions: string}>} */
  const boxes = [];
  let lastDay = '';
  let total = PAD + 34 * S;                      // header strip
  for (const ev of events) {
    const day = (ev.ts || '').slice(0, 10);
    if (day && day !== lastDay) { lastDay = day; total += 22 * S; }
    ctx.font = FONT(15);
    const lines = wrapLines(measure, ev.text || '', BUBBLE_MAX - bubblePadX * 2);
    let w = 0;
    for (const ln of lines) w = Math.max(w, measure(ln));
    w = Math.min(BUBBLE_MAX, Math.ceil(w) + bubblePadX * 2);
    const mine = ev.type === 'sent';
    const whoLabel = mine ? 'you → ' + (ev.name || '?') : (ev.name || 'agent');
    const arr = state.reactions.get(ev.id);
    const reactions = arr && arr.length ? arr.join(' ') : '';
    const h = whoH + lines.length * lineH + bubblePadY * 2 + stampH +
              (reactions ? 18 * S : 0);
    boxes.push({ ev, lines, w, h, mine, whoLabel, reactions });
    total += h + gap;
  }
  total += PAD + 22 * S;                         // wordmark strip

  canvas.width = W;
  canvas.height = total;

  // ---- paint pass ----
  ctx.fillStyle = pal.ground;
  ctx.fillRect(0, 0, W, total);
  ctx.textBaseline = 'top';

  // Header: the thread name, quietly.
  ctx.fillStyle = pal.dim;
  ctx.font = FONT(12, 600);
  ctx.textAlign = 'center';
  ctx.fillText(threadName(state.selected) || 'bridge', W / 2, PAD);
  ctx.textAlign = 'left';

  let y = PAD + 34 * S;
  lastDay = '';
  for (const b of boxes) {
    const day = (b.ev.ts || '').slice(0, 10);
    if (day && day !== lastDay) {
      lastDay = day;
      ctx.fillStyle = pal.dim;
      ctx.font = FONT(11, 600);
      ctx.textAlign = 'center';
      ctx.fillText(day, W / 2, y + 4 * S);
      ctx.textAlign = 'left';
      y += 22 * S;
    }
    const x = b.mine ? W - PAD - b.w : PAD;
    // who-line above the bubble, on its side
    ctx.fillStyle = pal.dim;
    ctx.font = FONT(11, 600);
    ctx.fillText(b.whoLabel, b.mine ? x + b.w - Math.min(b.w, ctxWidth(ctx, b.whoLabel)) : x, y);
    const by = y + whoH;
    const bh = b.lines.length * lineH + bubblePadY * 2;
    // bubble
    ctx.fillStyle = b.mine ? pal.sent : pal.glass;
    roundRect(ctx, x, by, b.w, bh, 14 * S);
    ctx.fill();
    if (!b.mine) { ctx.strokeStyle = pal.hairline; ctx.lineWidth = S; ctx.stroke(); }
    // text
    ctx.fillStyle = b.mine ? pal.sentInk : pal.ink;
    ctx.font = FONT(15);
    b.lines.forEach((ln, i) => ctx.fillText(ln, x + bubblePadX, by + bubblePadY + i * lineH));
    // stamp under the bubble, on its side
    const t = (b.ev.ts || '').slice(11, 16);
    ctx.fillStyle = pal.dim;
    ctx.font = FONT(10);
    ctx.fillText(t, b.mine ? x + b.w - ctxWidth(ctx, t) : x, by + bh + 3 * S);
    // reactions pill
    if (b.reactions) {
      ctx.font = FONT(12);
      ctx.fillText(b.reactions, b.mine ? x - 4 * S - ctxWidth(ctx, b.reactions) : x + b.w + 4 * S, by + bh - 12 * S);
    }
    y += b.h + gap;
  }

  // Wordmark, bottom right.
  ctx.fillStyle = pal.dim;
  ctx.font = FONT(11, 600);
  const mark = 'sent over bridge 🌉';
  ctx.fillText(mark, W - PAD - ctxWidth(ctx, mark), total - PAD - 12 * S);
  return canvas;
}

/** @param {CanvasRenderingContext2D} ctx @param {string} s @returns {number} */
function ctxWidth(ctx, s) { return ctx.measureText(s).width; }

/** @param {CanvasRenderingContext2D} ctx @param {number} x @param {number} y @param {number} w @param {number} h @param {number} r */
function roundRect(ctx, x, y, w, h, r) {
  const rr = Math.min(r, w / 2, h / 2);
  ctx.beginPath();
  ctx.moveTo(x + rr, y);
  ctx.arcTo(x + w, y, x + w, y + h, rr);
  ctx.arcTo(x + w, y + h, x, y + h, rr);
  ctx.arcTo(x, y + h, x, y, rr);
  ctx.arcTo(x, y, x + w, y, rr);
  ctx.closePath();
}

/* ---------- share / fallback ---------- */

/** @param {(text: string) => void} toast @returns {Promise<boolean>} whether an export left the building */
export async function exportSelectionPNG(toast) {
  const events = selectedEventsInOrder(state.events, state.selected, state.selectedIds);
  if (!events.length) { toast('Nothing selected'); return false; }
  const canvas = paint(events);
  if (!canvas) { toast('Export isn’t supported here'); return false; }
  /** @type {Blob | null} */
  const blob = await new Promise((r) => canvas.toBlob ? canvas.toBlob(r, 'image/png') : r(null));
  if (!blob) { toast('Couldn’t render the image'); return false; }
  const name = 'bridge-' + (threadName(state.selected) || 'thread').replace(/[^\w#-]+/g, '_')
             + '-' + new Date().toISOString().slice(0, 10) + '.png';
  const file = new File([blob], name, { type: 'image/png' });
  // The OS share sheet is the whole point on the phone; the anchor download is
  // the desktop fallback. Share can reject (user cancels) — that's not an error.
  if (navigator.canShare && navigator.canShare({ files: [file] }) && navigator.share) {
    try { await navigator.share({ files: [file] }); return true; }
    catch { return false; }                      // user dismissed the sheet
  }
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = name;
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 10_000);
  toast('Image saved');
  return true;
}
