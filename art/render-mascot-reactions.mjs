// Prints a mascot's reactions: for each name asked for, a short loop of the
// character (a file defining CHARACTER, see CHARACTER.md) moving as that
// reaction does — waving hello, cheering when the viewer joins, sighing when
// they leave — as an animated WebP with a clear background, <out-folder>/<name>.webp.
// The pages play one when the viewer does the thing it is named for
// (mascotReactions in mascots.go, which names them).
// Run: node render-mascot-reactions.mjs <character.js> <out-folder> <name,name,…>
// CHROME_PATH names the Chrome or Chromium to use; img2webp (libwebp's tools)
// joins the frames.
//
// A model wrote the character from a stranger's words or photo, so as in
// render-character.mjs the page is blank and every network request is refused.
import { chromium } from 'playwright-core';
import { readFileSync, writeFileSync, mkdtempSync, rmSync } from 'node:fs';
import { execFileSync } from 'node:child_process';
import os from 'node:os';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const here = path.dirname(fileURLToPath(import.meta.url));
const [characterFile, outFolder, namesArgument] = process.argv.slice(2);
if (!characterFile || !outFolder || !namesArgument) {
  console.error('usage: node render-mascot-reactions.mjs <character.js> <out-folder> <name,name,…>');
  process.exit(2);
}
const names = namesArgument.split(',');
const FRAMES_PER_SECOND = 10, SECONDS = 2;
const kit = readFileSync(path.join(here, 'kit.js'), 'utf8');
const character = readFileSync(characterFile, 'utf8');

// Each reaction is a pose at moment t, from 0 up to 1, built from the kit's
// POSES with loop() so the end meets the start. hop() is 0 on the ground and
// 1 at the top, twice a loop.
const REACTIONS = `
const hop = t => Math.abs(Math.sin(2 * Math.PI * t));
const REACTIONS = {
  // Waving hello: the forearm swings, the head tips toward it.
  hello: t => addToPose(addToPose({ ...POSES.wave, expression: 'grin' }, 'forearmR', 28 * loop(t, 2)), 'head', 5 * loop(t, 1)),
  // Joined: cheering, hopping twice, arms pumping.
  joined: t => addToPose(addToPose({ ...POSES.cheer, expression: 'laugh', lift: .07 * hop(t) }, 'upperArmL', 10 * loop(t, 2)),
    'upperArmR', 10 * loop(t, 2)),
  // On the waitlist: a friendly shrug, rocking side to side.
  waitlisted: t => addToPose(addToPose({ ...POSES.shrug, expression: 'smile', look: 'left' }, 'torso', 5 * loop(t, 1)),
    'head', -8 * loop(t, 1)),
  // Left: a sigh, head down, arms hanging, breathing slowly.
  left: t => addToPose({ head: 16, neck: 6, upperArmL: -4, forearmL: 6, upperArmR: -4, forearmR: 6, expression: 'calm', look: 'right' },
    'torso', 3 + 3 * loop(t, 1)),
  // Created an event: jumping for joy, twice.
  created: t => addToPose({ ...POSES.jump, expression: 'laugh', lift: .14 * hop(t) }, 'torso', 6 * loop(t, 1)),
  // Poked: startled, wobbling.
  poked: t => addToPose(addToPose({ ...POSES.scared, expression: 'surprised' }, 'torso', 7 * loop(t, 3)), 'head', -6 * loop(t, 3, .2)),
};`;

const browser = await chromium.launch({ executablePath: process.env.CHROME_PATH });
try {
  const page = await browser.newPage();
  await page.route('**/*', route => route.abort());
  const errors = [];
  page.on('pageerror', e => errors.push(e.message));
  page.on('console', m => { if (m.type() === 'error') errors.push(m.text()); });
  await page.setContent('<!doctype html><meta charset="utf-8"><body></body>');
  await page.addScriptTag({ content: kit + REACTIONS + '\nwindow.REACTIONS = REACTIONS;' });
  await page.addScriptTag({ content: `window.CHARACTER_DEFINED = (function () {\n${character}\n;return CHARACTER;})();` });
  if (errors.length) throw new Error(errors.join('\n'));
  const unknown = await page.evaluate(names => names.filter(n => typeof window.REACTIONS[n] !== 'function'), names);
  if (unknown.length) throw new Error(`no reaction is drawn for ${unknown.join(', ')}`);
  await page.evaluate(() => {
    const C = window.CHARACTER_DEFINED;
    if (typeof C !== 'object' || typeof C?.draw?.head !== 'function' || !(C.proportions?.head > 0)) {
      throw new Error('the file does not define a CHARACTER with proportions and a draw function for every bone');
    }
    // The whole character, feet near the bottom, with room above for a jump.
    window.frameAt = (name, t) => {
      const canvas = document.createElement('canvas');
      printDrawing(canvas, { width: 270, height: 360, paint() { paintCharacter(C, window.REACTIONS[name](t), { x: 135, y: 351, height: 285 }); } });
      return canvas;
    };
  });
  const count = SECONDS * FRAMES_PER_SECOND;
  const png = dataURL => Buffer.from(dataURL.split(',')[1], 'base64');
  for (const name of names) {
    const folder = mkdtempSync(path.join(os.tmpdir(), 'mascot-reaction-frames-'));
    try {
      const files = [];
      for (let i = 0; i < count; i++) {
        const file = path.join(folder, `frame-${String(i).padStart(3, '0')}.png`);
        writeFileSync(file, png(await page.evaluate(([name, t]) => window.frameAt(name, t).toDataURL('image/png'), [name, i / count])));
        files.push(file);
      }
      if (errors.length) throw new Error(errors.join('\n'));
      execFileSync('img2webp', ['-loop', '0', '-lossy', '-q', '60', '-m', '4', '-d', String(Math.round(1000 / FRAMES_PER_SECOND)),
        ...files, '-o', path.join(outFolder, `${name}.webp`)], { stdio: ['ignore', 'ignore', 'pipe'] });
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
