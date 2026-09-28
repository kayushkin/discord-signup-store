// Prints an event's picture: its scene, with each person going painted in
// their place from their avatar's drawing code, and saves it as WebP.
// Run: node render-event-picture.mjs <scene.js> <people.json> <out.webp> [count]
// people.json is [{"discord_user_id": "…", "drawing_code": "…"}, …], in the
// order they signed up. count, when given, prints that many people, taking
// people.json round again if it holds fewer — to see how a scene holds a
// crowd. CHROME_PATH names the Chrome or Chromium to use.
//
// scene.js defines SCENE (see SCENE.md in this folder): its size, background()
// and foreground() painted with the kit, and places(n), where each of n people
// stands. While either paint runs, PEOPLE_COUNT and PLACES say how many people
// there are and where, so a scene can draw a chair or a body for each.
//
// A model wrote the scene from an event's description, and each drawing from
// a stranger's photo, so none of this code is trusted. The page is blank
// rather than file://, and every network request is refused: the code can
// paint and do nothing else.
import { chromium } from 'playwright-core';
import { readFileSync, writeFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const here = path.dirname(fileURLToPath(import.meta.url));
const [sceneFile, peopleFile, outFile, countArgument] = process.argv.slice(2);
if (!sceneFile || !peopleFile || !outFile) {
  console.error('usage: node render-event-picture.mjs <scene.js> <people.json> <out.webp> [count]');
  process.exit(2);
}
let people = JSON.parse(readFileSync(peopleFile, 'utf8'));
if (!Array.isArray(people) || !people.length) throw new Error('people.json names nobody');
if (countArgument) {
  const count = Number(countArgument);
  people = Array.from({ length: count }, (_, i) => people[i % people.length]);
}
const kit = readFileSync(path.join(here, 'kit.js'), 'utf8');
const scene = readFileSync(sceneFile, 'utf8');

const browser = await chromium.launch({ executablePath: process.env.CHROME_PATH });
try {
  const page = await browser.newPage();
  await page.route('**/*', route => route.abort());
  const errors = [];
  page.on('pageerror', e => errors.push(e.message));
  page.on('console', m => { if (m.type() === 'error') errors.push(m.text()); });
  await page.setContent('<!doctype html><meta charset="utf-8"><body></body>');
  await page.addScriptTag({ content: kit + `\nwindow.drawings = []; var PEOPLE_COUNT = ${people.length}, PLACES = [];` });
  // Each drawing, and the scene, declares its own constant, so each runs in
  // a function of its own and hands the constant back.
  for (const person of people) {
    await page.addScriptTag({ content: `window.drawings.push((function () {\n${person.drawing_code}\n;return DRAWING;})());` });
  }
  await page.addScriptTag({ content: `window.SCENE_DEFINED = (function () {\n${scene}\n;return SCENE;})();` });
  if (errors.length) throw new Error(errors.join('\n'));
  const dataURL = await page.evaluate(() => {
    const scene = window.SCENE_DEFINED, n = window.drawings.length;
    if (typeof scene !== 'object' || !(scene.width > 0) || !(scene.height > 0) ||
        typeof scene.background !== 'function' || typeof scene.places !== 'function') {
      throw new Error('scene.js does not define SCENE with width, height, background() and places(n)');
    }
    const places = scene.places(n);
    if (!Array.isArray(places) || places.length !== n ||
        places.some(p => ![p && p.x, p && p.y, p && p.size].every(Number.isFinite) || !(p.size > 0))) {
      throw new Error(`SCENE.places(${n}) must give ${n} places, each with a finite x, y and size`);
    }
    PLACES = places;
    const { width, height } = scene;
    const layer = paint => {
      const canvas = document.createElement('canvas');
      printDrawing(canvas, { width, height, misregister: scene.misregister, paint });
      return canvas;
    };
    const out = document.createElement('canvas'); out.width = width; out.height = height;
    const ctx = out.getContext('2d');
    ctx.fillStyle = INK.paperLight; ctx.fillRect(0, 0, width, height);
    ctx.drawImage(layer(() => scene.background()), 0, 0);
    // Each person: their drawing cropped round, with an ink rim and the
    // offset shadow the site's cards have, turned by their place's tilt.
    window.drawings.forEach((drawing, i) => {
      const print = document.createElement('canvas');
      printDrawing(print, drawing);
      const { x, y, size } = places[i], r = size / 2, tilt = places[i].tilt || 0;
      ctx.save();
      ctx.translate(x, y); ctx.rotate(tilt);
      ctx.fillStyle = INK.black;
      ctx.beginPath(); ctx.arc(r * .05, r * .05, r, 0, Math.PI * 2); ctx.fill();
      ctx.save();
      ctx.beginPath(); ctx.arc(0, 0, r, 0, Math.PI * 2); ctx.clip();
      ctx.fillStyle = INK.paperLight; ctx.fillRect(-r, -r, size, size);
      const scale = size / Math.min(print.width, print.height);
      ctx.drawImage(print, -print.width * scale / 2, -print.height * scale / 2, print.width * scale, print.height * scale);
      ctx.restore();
      ctx.strokeStyle = INK.black; ctx.lineWidth = Math.max(2, size / 45);
      ctx.beginPath(); ctx.arc(0, 0, r, 0, Math.PI * 2); ctx.stroke();
      ctx.restore();
    });
    if (typeof scene.foreground === 'function') ctx.drawImage(layer(() => scene.foreground()), 0, 0);
    return out.toDataURL('image/webp', .88);
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
