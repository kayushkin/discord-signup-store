// Paints the people going to an event side by side, each from their avatar's
// drawing code, and saves the picture as WebP.
// Run: node render-event-picture.mjs <people.json> <out.webp>
// people.json is [{"discord_user_id": "…", "drawing_code": "…"}, …], in the
// order they signed up. CHROME_PATH names the Chrome or Chromium to use.
//
// The drawings were written by a model from photos strangers uploaded, so as
// in render-avatar.mjs the page is blank rather than file://, and every
// network request is refused: the code can paint and do nothing else.
import { chromium } from 'playwright-core';
import { readFileSync, writeFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const here = path.dirname(fileURLToPath(import.meta.url));
const [peopleFile, outFile] = process.argv.slice(2);
if (!peopleFile || !outFile) {
  console.error('usage: node render-event-picture.mjs <people.json> <out.webp>');
  process.exit(2);
}
const people = JSON.parse(readFileSync(peopleFile, 'utf8'));
if (!Array.isArray(people) || !people.length) throw new Error('people.json names nobody');
const kit = readFileSync(path.join(here, 'kit.js'), 'utf8');

const browser = await chromium.launch({ executablePath: process.env.CHROME_PATH });
try {
  const page = await browser.newPage();
  await page.route('**/*', route => route.abort());
  const errors = [];
  page.on('pageerror', e => errors.push(e.message));
  page.on('console', m => { if (m.type() === 'error') errors.push(m.text()); });
  await page.setContent('<!doctype html><meta charset="utf-8"><body></body>');
  await page.addScriptTag({ content: kit + '\nwindow.drawings = [];' });
  // Each drawing declares its own const DRAWING, so each runs in a function
  // of its own and hands the constant back.
  for (const person of people) {
    await page.addScriptTag({ content: `window.drawings.push((function () {\n${person.drawing_code}\n;return DRAWING;})());` });
  }
  if (errors.length) throw new Error(errors.join('\n'));
  const dataURL = await page.evaluate(() => {
    const width = 960, height = 300, n = window.drawings.length;
    const scene = document.createElement('canvas'); scene.width = width; scene.height = height;
    const ctx = scene.getContext('2d');
    // Paper, and a sun of mustard rays rising behind the crowd.
    ctx.fillStyle = INK.paperLight; ctx.fillRect(0, 0, width, height);
    ctx.save();
    ctx.fillStyle = 'rgba(201,138,16,.28)';
    for (let i = 0; i < 24; i++) {
      const a = Math.PI + i / 24 * Math.PI;
      ctx.beginPath(); ctx.moveTo(width / 2, height + 40);
      ctx.arc(width / 2, height + 40, width, a, a + Math.PI / 48); ctx.closePath(); ctx.fill();
    }
    ctx.restore();
    halftone(ctx, [0, 0, width, height], 12, 4, 'rgba(221,63,42,.35)', (x, y) => (y / height) * 1.1 - .45);
    // One row up to six; past that, a back row behind and above the front.
    const rows = n <= 6 ? [n] : [Math.floor(n / 2), n - Math.floor(n / 2)];
    const places = [];
    rows.forEach((count, row) => {
      const back = rows.length === 2 && row === 0;
      const largest = rows.length === 1 ? 220 : (back ? 150 : 180);
      const diameter = Math.min(largest, (width - 60) / (count * .82 + .18));
      const step = diameter * .82, left = (width - (step * (count - 1) + diameter)) / 2;
      // The back row stands high enough that its faces clear the front row.
      const cy = rows.length === 1 ? height / 2 + 10 : (back ? height * .29 : height * .69);
      for (let i = 0; i < count; i++) places.push({ cx: left + diameter / 2 + i * step, cy, diameter });
    });
    window.drawings.forEach((drawing, i) => {
      const print = document.createElement('canvas');
      printDrawing(print, drawing);
      const { cx, cy, diameter } = places[i], r = diameter / 2;
      // A shadow of ink, offset, as the page's cards have.
      ctx.fillStyle = INK.black;
      ctx.beginPath(); ctx.arc(cx + 5, cy + 5, r, 0, Math.PI * 2); ctx.fill();
      ctx.save();
      ctx.beginPath(); ctx.arc(cx, cy, r, 0, Math.PI * 2); ctx.clip();
      ctx.fillStyle = INK.paperLight; ctx.fillRect(cx - r, cy - r, diameter, diameter);
      const scale = diameter / Math.min(print.width, print.height);
      ctx.drawImage(print, cx - print.width * scale / 2, cy - print.height * scale / 2, print.width * scale, print.height * scale);
      ctx.restore();
      ctx.strokeStyle = INK.black; ctx.lineWidth = 4;
      ctx.beginPath(); ctx.arc(cx, cy, r, 0, Math.PI * 2); ctx.stroke();
    });
    return scene.toDataURL('image/webp', .88);
  });
  if (errors.length) throw new Error(errors.join('\n'));
  if (!dataURL.startsWith('data:image/webp;base64,')) throw new Error(`the canvas gave ${dataURL.slice(0, 40)}`);
  writeFileSync(outFile, Buffer.from(dataURL.split(',')[1], 'base64'));
} catch (error) {
  console.error(error.message);
  process.exitCode = 1;
} finally {
  await browser.close();
}
