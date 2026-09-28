// Prints an event's picture: its scene, with each person going posed in it,
// and saves it as WebP.
// Run: node render-event-picture.mjs <scene.js> <people.json> <out.webp> [count]
// people.json is [{"discord_user_id", "format", "drawing_code"}, …] in the
// order they signed up; format is "character" (CHARACTER.md), painted whole
// and posed as the scene casts them, or "portrait", an older head-and-shoulders
// drawing, painted round where their head would be. count, when given, prints
// that many people, taking people.json round again if it holds fewer — to see
// how a scene holds a crowd. CHROME_PATH names the Chrome or Chromium to use.
//
// A model wrote the scene from an event's description, and each person from a
// stranger's photo, so none of this code is trusted. The page is blank rather
// than file://, and every network request is refused: the code can paint and
// do nothing else.
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
  await page.addScriptTag({ content: kit + `\nwindow.people = []; var PEOPLE_COUNT = ${people.length}, CAST = [], JOINTS = [];` });
  // Each person, and the scene, declares its own constant, so each runs in a
  // function of its own and hands the constant back.
  for (const person of people) {
    const name = person.format === 'character' ? 'CHARACTER' : 'DRAWING';
    await page.addScriptTag({ content: `window.people.push({ format: ${JSON.stringify(person.format)}, it: (function () {\n${person.drawing_code}\n;return ${name};})() });` });
  }
  await page.addScriptTag({ content: `window.SCENE_DEFINED = (function () {\n${scene}\n;return SCENE;})();` });
  if (errors.length) throw new Error(errors.join('\n'));
  const dataURL = await page.evaluate(() => {
    const scene = window.SCENE_DEFINED, n = window.people.length;
    if (typeof scene !== 'object' || !(scene.width > 0) || !(scene.height > 0) ||
        typeof scene.background !== 'function' || typeof scene.cast !== 'function') {
      throw new Error('scene.js does not define SCENE with width, height, background() and cast(n)');
    }
    const cast = scene.cast(n);
    if (!Array.isArray(cast) || cast.length !== n ||
        cast.some(c => ![c && c.x, c && c.y, c && c.height].every(Number.isFinite) || !(c.height > 0))) {
      throw new Error(`SCENE.cast(${n}) must give ${n} people, each with a finite x, y and height`);
    }
    // A portrait-only person is placed as a grown-up character would be.
    const bodyOf = person => person.format === 'character' ? person.it : { proportions: STANDARD_PROPORTIONS };
    CAST = cast;
    JOINTS = cast.map((c, i) => characterJoints(bodyOf(window.people[i]), c.pose ?? {}, c));
    const paintPerson = (person, c, joints) => {
      if (person.format === 'character') { paintCharacter(person.it, c.pose ?? {}, c); return; }
      // Their round portrait where the head would be, a little bigger than one.
      const d = STANDARD_PROPORTIONS.head * joints.scale * 1.5, [cx, cy] = joints.head;
      pushTransform();
      translateBy(cx - d / 2, cy - d / 2); scaleBy(d / person.it.width);
      person.it.paint({});
      tilt(null);
      popTransform();
      part(ellipsePoints(cx, cy, d / 2, d / 2, 32), { line: Math.max(2, d / 45) });
    };
    const out = document.createElement('canvas');
    printDrawing(out, {
      width: scene.width, height: scene.height, misregister: scene.misregister,
      paint() {
        scene.background();
        window.people.forEach((person, i) => paintPerson(person, cast[i], JOINTS[i]));
        if (typeof scene.foreground === 'function') scene.foreground();
      },
    });
    const flat = document.createElement('canvas'); flat.width = scene.width; flat.height = scene.height;
    const ctx = flat.getContext('2d');
    ctx.fillStyle = INK.paperLight; ctx.fillRect(0, 0, scene.width, scene.height);
    ctx.drawImage(out, 0, 0);
    return flat.toDataURL('image/webp', .88);
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
