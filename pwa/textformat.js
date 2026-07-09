// @ts-check — type-checked against ./types.d.ts (see tsconfig.json). Dev-only:
// `@ts-check` + JSDoc are comments the browser ignores, so nothing ships changes.
/* bridge — text & time formatters.
   Peeled out of app.js (the ES-module split); behaviour unchanged.

   Pure (text|time) → string | DOM-node helpers: the bubble renderer's markdown /
   linkify / thinking-pill builders, the paragraph→bubble splitters, the row-
   preview flattener, and the timestamp / date-label formatters. They touch no
   app state, api, or delivery — every caller just imports what it needs — so
   this is a leaf module (it imports nothing from ./app.js).

   CRITICAL (H9/XSS): appendLinkified / appendRich build their DOM node-by-node,
   NEVER via innerHTML, so message content still cannot inject markup. Kept
   byte-identical to the pre-split app.js. */

'use strict';

const BUBBLE_TARGET = 550;  // chars a bubble aims for
const BUBBLE_MAX = 900;     // a text (or lone paragraph) beyond this gets split

/** @param {string} text @returns {string[]} */
export function splitPleasing(text) {
  if (!text || text.length <= BUBBLE_MAX) return [text];
  // Atomic units never split: [thinking] blocks and fenced code.
  const units = [];
  const atomicRe = /(\[thinking\][\s\S]*?(?:\[end[- ]?thinking\]|\[\/thinking\]|$)|```[\s\S]*?(?:```|$))/g;
  let last = 0, m;
  while ((m = atomicRe.exec(text)) !== null) {
    if (m.index > last) units.push(...text.slice(last, m.index).split(/\n{2,}/));
    units.push(m[1]);
    last = m.index + m[0].length;
  }
  if (last < text.length) units.push(...text.slice(last).split(/\n{2,}/));
  const clean = units.map((u) => u.trim()).filter(Boolean);

  // Pack paragraphs toward the target. A list stays glued to the short
  // intro that ends with ":"; a giant lone paragraph splits at sentences.
  const bubbles = [];
  let cur = '';
  for (const u of clean) {
    const atomic = /^(```|\[thinking\])/.test(u);
    if (u.length > BUBBLE_MAX && !atomic) {
      if (cur) { bubbles.push(cur); cur = ''; }
      bubbles.push(...sentencePack(u));
      continue;
    }
    const isList = /^([-*•]|\d+[.)])\s/.test(u);
    const fits = cur && cur.length + u.length + 2 <= BUBBLE_TARGET;
    const glued = cur && isList && /:\s*$/.test(cur);
    if (fits || glued) cur += '\n\n' + u;
    else { if (cur) bubbles.push(cur); cur = u; }
  }
  if (cur) bubbles.push(cur);
  return bubbles.length ? bubbles : [text];
}

/** @param {string} par @returns {string[]} */
export function sentencePack(par) {
  const parts = par.split(/(?<=[.!?…])\s+/);
  const out = [];
  let cur = '';
  for (const s of parts) {
    if (cur && cur.length + s.length + 1 > BUBBLE_TARGET) { out.push(cur); cur = s; }
    else cur = cur ? cur + ' ' + s : s;
  }
  if (cur) out.push(cur);
  return out;
}

/* A photo in a fixed-size box (.photo-box owns the dimensions in CSS). The box
   reserves its full space before the image decodes, so a loading photo can't
   shift the feed — zero layout shift by construction. loading="lazy" keeps
   offscreen history photos off the wire until scrolled near; decoding="async"
   keeps the decode off the main thread. Tap toggles .full to lift the
   cover-crop and show the whole photo (no lightbox, no new chrome). */
/** @param {string} src @returns {HTMLElement} */
export function photoBox(src) {
  const box = document.createElement('div');
  box.className = 'photo-box';
  const img = document.createElement('img');
  img.src = src;
  img.alt = '';
  img.loading = 'lazy';
  img.decoding = 'async';
  box.appendChild(img);
  box.onclick = () => box.classList.toggle('full');
  return box;
}

/** @param {string} name @returns {HTMLElement} */
export function typingBubble(name) {
  const el = document.createElement('div');
  el.className = 'msg typing';
  const label = document.createElement('span');
  label.className = 'who';
  label.textContent = name + ' is working';
  el.appendChild(label);
  const dots = document.createElement('span');
  dots.className = 'dots';
  for (let i = 0; i < 3; i++) dots.appendChild(document.createElement('i'));
  el.appendChild(dots);
  return el;
}

/** @param {string} label @returns {HTMLElement} */
export function who(label) {
  const el = document.createElement('span');
  el.className = 'who';
  el.textContent = label;
  return el;
}

// Timestamp inside the bubble, bottom-right (styled by .msg .stamp). No-op
// when the event carries no parseable time.
/** @param {HTMLElement} bubble @param {string | number} [ts] */
export function appendStamp(bubble, ts) {
  const t = localTime(ts);
  if (!t) return;
  const el = document.createElement('span');
  el.className = 'stamp';
  el.textContent = t;
  bubble.appendChild(el);
}

/** @param {string | number} [ts] @returns {string} */
export function localTime(ts) {
  const d = new Date(ts);
  return isNaN(d.getTime()) ? '' :
    d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
}

// iMessage-convention compact stamp for a conversation row:
// today → time; yesterday → "Yesterday"; this week → weekday; older → date.
/** @param {string | number} [ts] @returns {string} */
export function listTime(ts) {
  const d = new Date(ts);
  if (isNaN(d.getTime())) return '';
  const days = daysAgo(d);
  if (days <= 0) return d.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });
  if (days === 1) return 'Yesterday';
  if (days < 7) return d.toLocaleDateString([], { weekday: 'short' });
  return d.toLocaleDateString([], { month: 'numeric', day: 'numeric', year: '2-digit' });
}

// Day-separator label inside a thread feed.
/** @param {string | number} [ts] @returns {string} */
export function dayLabel(ts) {
  const d = new Date(ts);
  if (isNaN(d.getTime())) return '';
  const days = daysAgo(d);
  if (days <= 0) return 'Today';
  if (days === 1) return 'Yesterday';
  if (days < 7) return d.toLocaleDateString([], { weekday: 'long' });
  return d.toLocaleDateString([], { month: 'short', day: 'numeric', year: 'numeric' });
}

// Whole calendar days between D and now (0 = today, 1 = yesterday, …).
/** @param {Date} d @returns {number} */
export function daysAgo(d) {
  const startOfDay = (x) => new Date(x.getFullYear(), x.getMonth(), x.getDate()).getTime();
  return Math.round((startOfDay(new Date()) - startOfDay(d)) / 86400000);
}

/* Render text with [thinking] blocks collapsed into tappable pills and
   very long remainders clamped behind "show more". */
/** @param {string} [text] @returns {HTMLElement} */
export function richText(text) {
  const container = document.createElement('div');
  container.className = 'rich';
  const re = /\[thinking\]([\s\S]*?)(?:\[end-thinking\]|\[\/thinking\]|(?=\[response\])|$)/g;
  let cursor = 0;
  let match;
  while ((match = re.exec(text)) !== null) {
    appendPlain(container, text.slice(cursor, match.index));
    appendThinking(container, match[1].trim());
    cursor = re.lastIndex;
  }
  appendPlain(container, text.slice(cursor).replace(/\[response\]/g, '').trim());
  return container;
}

/** @param {HTMLElement} container @param {string} chunk */
export function appendPlain(container, chunk) {
  chunk = chunk.trim();
  if (!chunk) return;
  const el = document.createElement('span');
  el.className = 'plain';
  if (chunk.length > 1200) {
    const short = chunk.slice(0, 1000) + '…';
    appendRich(el, short);
    const more = document.createElement('button');
    more.className = 'show-more';
    more.textContent = 'show more';
    more.onclick = () => {   // a toggle, not a one-way door
      const expanded = more.textContent === 'collapse';
      el.textContent = '';
      appendRich(el, expanded ? short : chunk);
      more.textContent = expanded ? 'show more' : 'collapse';
    };
    container.appendChild(el);
    container.appendChild(more);
  } else {
    appendRich(el, chunk);
    container.appendChild(el);
  }
}

/* Minimal inline markdown for bubbles — **bold**, *italic*, `code` — built
   from DOM nodes exactly like the linkifier (never innerHTML), so message
   content still cannot inject markup. Single-level on purpose: code spans
   don't linkify, bold/italic contents still do. Anything unmatched renders
   as the literal text it always was. */
/** @param {HTMLElement} parent @param {string} text */
export function appendRich(parent, text) {
  const re = /(`[^`\n]+`)|(\*\*(?=\S)[^*]+?(?<=\S)\*\*)|(\*(?=\S)[^*\n]+?(?<=\S)\*)/g;
  let cursor = 0;
  let m;
  while ((m = re.exec(text)) !== null) {
    if (m.index > cursor) appendLinkified(parent, text.slice(cursor, m.index));
    if (m[1]) {
      const code = document.createElement('code');
      code.className = 'md-code';
      code.textContent = m[1].slice(1, -1);
      parent.appendChild(code);
    } else if (m[2]) {
      const b = document.createElement('strong');
      appendLinkified(b, m[2].slice(2, -2));
      parent.appendChild(b);
    } else {
      const i = document.createElement('em');
      appendLinkified(i, m[3].slice(1, -1));
      parent.appendChild(i);
    }
    cursor = m.index + m[0].length;
  }
  if (cursor < text.length) appendLinkified(parent, text.slice(cursor));
}

/* Append TEXT to PARENT, turning http(s) URLs into tappable links. Builds
   text and anchor nodes directly — never innerHTML — so message content
   cannot inject markup. Trailing sentence punctuation stays out of the href. */
/** @param {HTMLElement} parent @param {string} text */
export function appendLinkified(parent, text) {
  const re = /https?:\/\/[^\s]+/g;
  let cursor = 0;
  let m;
  while ((m = re.exec(text)) !== null) {
    let url = m[0];
    const trail = url.match(/[.,!?;:'")\]}>]+$/);
    if (trail) url = url.slice(0, -trail[0].length);
    if (!url) continue;
    if (m.index > cursor) {
      parent.appendChild(document.createTextNode(text.slice(cursor, m.index)));
    }
    const a = document.createElement('a');
    a.href = url;                 // regex guarantees an http(s) scheme
    a.textContent = url;
    a.target = '_blank';
    a.rel = 'noopener noreferrer';
    parent.appendChild(a);
    cursor = m.index + url.length;
  }
  if (cursor < text.length) {
    parent.appendChild(document.createTextNode(text.slice(cursor)));
  }
}

/** @param {HTMLElement} container @param {string} thought */
export function appendThinking(container, thought) {
  if (!thought) return;
  const words = thought.split(/\s+/).length;
  const pill = document.createElement('button');
  pill.className = 'think-pill';
  pill.textContent = '💭 thinking · ' + words + ' words';
  const body = document.createElement('div');
  body.className = 'think-body hidden';
  body.textContent = thought;
  pill.onclick = () => {
    const open = body.classList.toggle('hidden');
    pill.textContent = open ? '💭 thinking · ' + words + ' words' : '💭 hide thinking';
  };
  container.appendChild(pill);
  container.appendChild(body);
}

// First meaningful line of a captured prompt for the collapsed card: strip
// TUI box-drawing / bullet noise and return the first line with real content.
/** @param {string} [text] @returns {string} */
export function firstLine(text) {
  const lines = (text || '').split('\n');
  let fallback = '';
  for (const raw of lines) {
    const cleaned = raw.replace(/[│╭╮╰╯─┌┐└┘|>❯•*\s]+/g, ' ').trim();
    if (/[A-Za-z0-9]/.test(cleaned)) return cleaned;
    if (!fallback && raw.trim()) fallback = raw.trim();
  }
  return fallback || '(prompt)';
}

// Strip thinking/response markers and markdown syntax, collapsing to a single
// preview line — a row preview renders as plain text, so literal **stars** and
// `backticks` are just noise there (spotted in the field, 2026-07-06).
/** @param {string} [text] @returns {string} */
export function plainPreview(text) {
  return (text || '')
    .replace(/\[thinking\][\s\S]*?(?:\[end-thinking\]|\[\/thinking\]|(?=\[response\])|$)/g, '')
    .replace(/\[response\]/g, '')
    .replace(/\*\*([^*]+)\*\*/g, '$1')
    .replace(/\*([^*\n]+)\*/g, '$1')
    .replace(/`([^`]+)`/g, '$1')
    .replace(/^#+\s+/gm, '')
    .replace(/\s+/g, ' ')
    .trim();
}
