/* bridge — appearance: theme, scenery (wallpaper + fog), and the light drift.
   Peeled out of app.js (the ES-module split); behaviour unchanged.

   This module owns the three visual layers and applies them at import time,
   exactly as the top of app.js used to: the saved theme, the wallpaper veil +
   marine-layer fog, and the Golden-Hour palette drift. Its import-time
   side-effects (the applyTheme / applyWallpaper / applyPhase calls, the 30-min
   re-check interval, and the phase-animate rAF) run when app.js imports it —
   the same point in startup as before. It imports nothing from ./app.js; app.js
   imports the handful of symbols its settings sheet and visibilitychange
   handler still call. */

'use strict';

/* ---------- theme ----------
   Every colour is a CSS custom property; three [data-theme] palettes live in
   style.css. Golden Hour is the default (the base :root values + the manifest
   colours), so an unset attribute renders it with no flash. We apply the saved
   theme as the very first thing app.js does — the CSP forbids an inline <script>
   in <head>, so this is the earliest hook; a non-default theme may show one
   Golden-Hour frame before this runs, which is acceptable. */

export const THEMES = ['golden-hour', 'dusk', 'international-orange'];
// <meta name="theme-color"> per theme (browser/PWA chrome tint). The manifest
// is static, so its theme_color/background_color track the default only.
const THEME_META = {
  'golden-hour': '#FAF5EC',
  'dusk': '#141B26',
  'international-orange': '#1C3A5E',
};
// Picker copy + a 5-swatch preview (ground, outbound, inbound, accent, resolved).
export const THEME_INFO = {
  'golden-hour': { name: 'Golden Hour',
    swatches: ['#FAF5EC', '#D3653B', '#EAF0F6', '#4E739F', '#59805D'] },
  'dusk': { name: 'Dusk',
    swatches: ['#141B26', '#DF7B4E', '#26344A', '#8AAAC9', '#7FB287'] },
  'international-orange': { name: 'International Orange',
    swatches: ['#1C3A5E', '#C8432B', '#EEF2F6', '#4C7FB5', '#55875B'] },
};

export function currentTheme() {
  const t = localStorage.getItem('theme');
  return THEMES.includes(t) ? t : 'golden-hour';
}

function applyTheme(theme) {
  if (!THEMES.includes(theme)) theme = 'golden-hour';
  document.documentElement.setAttribute('data-theme', theme);
  const meta = document.querySelector('meta[name="theme-color"]');
  if (meta) meta.setAttribute('content', THEME_META[theme]);
}

export function setTheme(theme) {
  if (!THEMES.includes(theme)) return;
  localStorage.setItem('theme', theme);
  applyTheme(theme);
}

applyTheme(currentTheme());

/* ---------- background (the scenery layer) ----------
   Off / airy / whisper — the veil strength over the Golden Gate photo.
   Fog density follows the real San Francisco marine-layer schedule: dense
   mornings, burned off by afternoon, rolling back at dusk. */

export const WALLPAPERS = ['airy', 'whisper', 'off'];
export const WALLPAPER_NAMES = { airy: 'Bridge · airy veil', whisper: 'Bridge · whisper veil', off: 'Off' };

export function currentWallpaper() {   // exported: screensaver.js reads it for eligibility
  const w = localStorage.getItem('wallpaper');
  return WALLPAPERS.includes(w) ? w : 'airy';
}

function applyWallpaper(w) {
  if (!WALLPAPERS.includes(w)) w = 'airy';
  document.documentElement.setAttribute('data-wallpaper', w);
  updateFog();
}

export function setWallpaper(w) {
  if (!WALLPAPERS.includes(w)) return;
  localStorage.setItem('wallpaper', w);
  applyWallpaper(w);
}

// SF marine layer, by local hour: thick mornings, clear afternoons, the bank
// rolls back in around dusk, settles overnight. The app has weather.
function fogDensity(hour) {
  if (hour >= 5 && hour < 11) return 1.0;
  if (hour >= 11 && hour < 17) return 0.35;
  if (hour >= 17 && hour < 22) return 0.85;
  return 0.6;
}

function updateFog() {
  document.documentElement.style.setProperty(
    '--fog-density', String(fogDensity(new Date().getHours())));
}

/* ---------- light (the Golden-Hour palette drift) ----------
   The default theme is named for a moment of light, so it follows one. Same
   spirit as the fog above — a month-keyed table and a little arithmetic, no
   astronomy — but here the palette drifts through five phases across the day.
   Each phase is a data-phase attribute on <html>; style.css maps it to a small
   set of hue-token overrides, scoped to the golden-hour theme so the other
   palettes never drift. 'day' is the anchor: it sets no overrides, so mid-day
   is exactly the Golden Hour of before. */

// Approximate SF sunrise/sunset by month, as the clock reads them (DST folded
// in). Off by twenty minutes is fine — this is weather, not an almanac.
//               Jan   Feb   Mar   Apr   May   Jun   Jul   Aug   Sep   Oct   Nov   Dec
const SUNRISE = [7.4,  7.0,  7.1,  6.4,  6.0,  5.8,  6.0,  6.4,  6.9,  7.3,  6.9,  7.2];
const SUNSET  = [17.2, 17.8, 19.0, 19.7, 20.2, 20.5, 20.4, 20.0, 19.3, 18.5, 17.0, 16.9];

// The phase for a moment, keyed off that month's sun. The narrow bands
// (dawn / golden / dusk) hug sunrise and sunset; day fills the long middle;
// night is everything left over — the evening and the small hours.
function solarPhase(date) {
  const rise = SUNRISE[date.getMonth()];
  const set  = SUNSET[date.getMonth()];
  const h = date.getHours() + date.getMinutes() / 60;   // decimal local hour
  if (h >= rise - 0.75 && h < rise + 1)   return 'dawn';    // 45m before → 1h after sunrise
  if (h >= set - 1.5   && h < set)        return 'golden';  // last 90m of daylight
  if (h >= set         && h < set + 0.75) return 'dusk';    // sunset → 45m after
  if (h >= rise + 1    && h < set - 1.5)  return 'day';     // mid-morning → afternoon (anchor)
  return 'night';
}

// Default ON; the settings toggle stores 'off' to opt out. Only an explicit
// 'off' disables it, so the drift is on for everyone who never opens settings.
export function paletteFollowsSun() {
  return localStorage.getItem('paletteSun') !== 'off';
}

// Off → the static day anchor (today's exact look). The attribute is inert
// under the other themes; the CSS scoping guarantees it.
export function applyPhase() {
  const phase = paletteFollowsSun() ? solarPhase(new Date()) : 'day';
  document.documentElement.setAttribute('data-phase', phase);
}

export function setPaletteSun(on) {
  localStorage.setItem('paletteSun', on ? 'on' : 'off');
  applyPhase();
}

// The weather (fog) and the light (palette) both key off the local hour, so
// they re-check together on one timer, twice an hour.
setInterval(() => { updateFog(); applyPhase(); }, 30 * 60 * 1000);
applyWallpaper(currentWallpaper());
applyPhase();
// Arm the drift transition only after the first paint, so a phase correction
// on a slow cold-load lands as a quiet snap (like the theme does) rather than
// a 2.4s smear; every flip from here on dissolves gently.
requestAnimationFrame(() => document.documentElement.classList.add('phase-animate'));
