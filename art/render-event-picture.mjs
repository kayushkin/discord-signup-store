// Prints an event's picture: its scene, with each person going posed in it,
// as a looping animated WebP.
// Run: node render-event-picture.mjs <scene.js> <people.json> <out.webp> [count] [--still | --sheet]
// people.json is [{"discord_user_id", "format", "drawing_code"}, …]: the
// people going with avatars first, in the order they signed up, then a
// "background" entry for each of the rest. format is "character"
// (CHARACTER.md), painted whole and posed as the scene casts them; "portrait",
// an older head-and-shoulders drawing, painted round where their head would
// be; or "background", a faceless stand-in from the kit (backgroundCharacter),
// no code of its own. count, when given, prints
// that many people, taking people.json round again if it holds fewer — to see
// how a scene holds a crowd. --still prints one frame, the first; --sheet
// prints four moments of the loop one above another, to see the motion in one
// still picture. CHROME_PATH names the Chrome or Chromium to use; img2webp
// (libwebp's tools) joins the frames.
//
// The loop is SCENE.seconds long (2 unless it says), at 12 frames a second.
// Every frame is the scene at a moment t from 0 up to 1: the scene's cast,
// background and foreground are given t, and each person also sways a little
// on their own (the kit's idleMotion) unless the scene casts them idle: false.
//
// A model wrote the scene from an event's description, and each person from a
// stranger's photo, so none of this code is trusted. The page is blank rather
// than file://, and every network request is refused: the code can paint and
// do nothing else.
import { chromium } from 'playwright-core';
import { readFileSync, writeFileSync, mkdtempSync, rmSync } from 'node:fs';
import { execFileSync } from 'node:child_process';
import os from 'node:os';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const here = path.dirname(fileURLToPath(import.meta.url));
const flags = process.argv.slice(2).filter(a => a.startsWith('--'));
const [sceneFile, peopleFile, outFile, countArgument] = process.argv.slice(2).filter(a => !a.startsWith('--'));
const mode = flags.includes('--still') ? 'still' : flags.includes('--sheet') ? 'sheet' : 'loop';
if (!sceneFile || !peopleFile || !outFile || flags.some(f => !['--still', '--sheet'].includes(f))) {
  console.error('usage: node render-event-picture.mjs <scene.js> <people.json> <out.webp> [count] [--still | --sheet]');
  process.exit(2);
}
const FRAMES_PER_SECOND = 12;
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
  await page.addScriptTag({ content: kit + `\nwindow.people = []; var PEOPLE_COUNT = ${people.length}, CAST = [], JOINTS = [], T = 0,
    PEOPLE = ${JSON.stringify(people.map(p => ({ avatar: p.format !== 'background' })))};` });
  // Each person, and the scene, declares its own constant, so each runs in a
  // function of its own and hands the constant back.
  let standIns = 0;
  for (const person of people) {
    if (person.format === 'background') {
      await page.addScriptTag({ content: `window.people.push({ format: 'character', it: backgroundCharacter(${++standIns}) });` });
      continue;
    }
    const name = person.format === 'character' ? 'CHARACTER' : 'DRAWING';
    await page.addScriptTag({ content: `window.people.push({ format: ${JSON.stringify(person.format)}, it: (function () {\n${person.drawing_code}\n;return ${name};})() });` });
  }
  await page.addScriptTag({ content: `window.SCENE_DEFINED = (function () {\n${scene}\n;return SCENE;})();` });
  if (errors.length) throw new Error(errors.join('\n'));
  const seconds = await page.evaluate(() => {
    const scene = window.SCENE_DEFINED;
    if (typeof scene !== 'object' || !(scene.width > 0) || !(scene.height > 0) ||
        typeof scene.background !== 'function' || typeof scene.cast !== 'function') {
      throw new Error('scene.js does not define SCENE with width, height, background() and cast(n)');
    }
    const seconds = scene.seconds ?? 2;
    if (!(seconds >= 1 && seconds <= 4)) throw new Error('SCENE.seconds must be from 1 to 4');
    // A portrait-only person is placed as a grown-up character would be.
    const bodyOf = person => person.format === 'character' ? person.it : { proportions: STANDARD_PROPORTIONS };
    const paintPerson = (person, c, joints, pose) => {
      if (person.format === 'character') { paintCharacter(person.it, pose, c); return; }
      // Their round portrait where the head would be, a little bigger than one.
      const d = STANDARD_PROPORTIONS.head * joints.scale * 1.5, [cx, cy] = joints.head;
      pushTransform();
      translateBy(cx - d / 2, cy - d / 2); scaleBy(d / person.it.width);
      person.it.paint({});
      tilt(null);
      popTransform();
      part(ellipsePoints(cx, cy, d / 2, d / 2, 32), { line: Math.max(2, d / 45) });
    };
    // One frame, at moment t, as a canvas.
    window.frameAt = t => {
      const n = window.people.length, cast = scene.cast(n, t);
      if (!Array.isArray(cast) || cast.length !== n ||
          cast.some(c => ![c && c.x, c && c.y, c && c.height].every(Number.isFinite) || !(c.height > 0))) {
        throw new Error(`SCENE.cast(${n}, ${t}) must give ${n} people, each with a finite x, y and height`);
      }
      const poses = cast.map((c, i) => c.idle === false ? (c.pose ?? {}) : idleMotion(c.pose ?? {}, t, i + 1));
      CAST = cast; T = t;
      JOINTS = cast.map((c, i) => characterJoints(bodyOf(window.people[i]), poses[i], c));
      const out = document.createElement('canvas');
      printDrawing(out, {
        width: scene.width, height: scene.height, misregister: scene.misregister,
        paint() {
          scene.background(t);
          // Back to front: whoever stands further up the picture is further
          // away, so is painted first and overlapped by those nearer.
          const order = cast.map((c, i) => i).sort((a, b) => cast[a].y - cast[b].y || a - b);
          order.forEach(i => paintPerson(window.people[i], cast[i], JOINTS[i], poses[i]));
          if (typeof scene.foreground === 'function') scene.foreground(t);
        },
      });
      const flat = document.createElement('canvas'); flat.width = scene.width; flat.height = scene.height;
      const ctx = flat.getContext('2d');
      ctx.fillStyle = INK.paperLight; ctx.fillRect(0, 0, scene.width, scene.height);
      ctx.drawImage(out, 0, 0);
      return flat;
    };
    return seconds;
  });
  if (errors.length) throw new Error(errors.join('\n'));
  const png = dataURL => Buffer.from(dataURL.split(',')[1], 'base64');
  if (mode === 'still') {
    writeFileSync(outFile, png(await page.evaluate(() => window.frameAt(0).toDataURL('image/webp', .88))));
  } else if (mode === 'sheet') {
    writeFileSync(outFile, png(await page.evaluate(() => {
      const frames = [0, .25, .5, .75].map(t => window.frameAt(t)), gap = 12;
      const sheet = document.createElement('canvas');
      sheet.width = frames[0].width; sheet.height = frames.reduce((h, f) => h + f.height + gap, -gap);
      const ctx = sheet.getContext('2d'); ctx.fillStyle = INK.black; ctx.fillRect(0, 0, sheet.width, sheet.height);
      frames.reduce((y, f) => { ctx.drawImage(f, 0, y); return y + f.height + gap; }, 0);
      return sheet.toDataURL('image/webp', .85);
    })));
  } else {
    const count = Math.round(seconds * FRAMES_PER_SECOND);
    const folder = mkdtempSync(path.join(os.tmpdir(), 'event-picture-frames-'));
    try {
      const files = [];
      for (let i = 0; i < count; i++) {
        const file = path.join(folder, `frame-${String(i).padStart(3, '0')}.png`);
        writeFileSync(file, png(await page.evaluate(t => window.frameAt(t).toDataURL('image/png'), i / count)));
        files.push(file);
      }
      if (errors.length) throw new Error(errors.join('\n'));
      execFileSync('img2webp', ['-loop', '0', '-lossy', '-q', '72', '-m', '4', '-d', String(Math.round(1000 / FRAMES_PER_SECOND)),
        ...files, '-o', outFile], { stdio: ['ignore', 'ignore', 'pipe'] });
    } finally {
      rmSync(folder, { recursive: true, force: true });
    }
  }
  if (errors.length) throw new Error(errors.join('\n'));
} catch (error) {
  console.error(error.message);
  process.exitCode = 1;
} finally {
  await browser.close();
}
