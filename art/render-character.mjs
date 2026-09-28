// Prints a character (a file defining CHARACTER, see CHARACTER.md) as the
// pages and the pictures use it: <out-prefix>-portrait-256.webp and -512, the
// round avatar; <out-prefix>-full.webp, standing whole; and
// <out-prefix>-poses.webp, a sheet of it in eight poses with their
// expressions, to see that every part turns right at its joints.
// Run: node render-character.mjs <character.js> <out-prefix>
// CHROME_PATH names the Chrome or Chromium to use.
//
// A model writes characters from strangers' photos, so as in
// render-avatar.mjs the page is blank and every network request is refused.
import { chromium } from 'playwright-core';
import { readFileSync, writeFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const here = path.dirname(fileURLToPath(import.meta.url));
const [characterFile, outPrefix] = process.argv.slice(2);
if (!characterFile || !outPrefix) {
  console.error('usage: node render-character.mjs <character.js> <out-prefix>');
  process.exit(2);
}
const kit = readFileSync(path.join(here, 'kit.js'), 'utf8');
const character = readFileSync(characterFile, 'utf8');

const browser = await chromium.launch({ executablePath: process.env.CHROME_PATH });
try {
  const page = await browser.newPage();
  await page.route('**/*', route => route.abort());
  const errors = [];
  page.on('pageerror', e => errors.push(e.message));
  page.on('console', m => { if (m.type() === 'error') errors.push(m.text()); });
  await page.setContent('<!doctype html><meta charset="utf-8"><body></body>');
  await page.addScriptTag({ content: kit });
  await page.addScriptTag({ content: `window.CHARACTER_DEFINED = (function () {\n${character}\n;return CHARACTER;})();` });
  if (errors.length) throw new Error(errors.join('\n'));
  const images = await page.evaluate(() => {
    const C = window.CHARACTER_DEFINED;
    const missing = ['head', 'neck', 'torso', 'pelvis', 'upperArmL', 'forearmL', 'handL', 'upperArmR', 'forearmR', 'handR',
                     'thighL', 'shinL', 'footL', 'thighR', 'shinR', 'footR'].filter(b => typeof C?.draw?.[b] !== 'function');
    const needed = ['head', 'neck', 'torso', 'shoulderWidth', 'hipWidth', 'upperArm', 'forearm', 'hand', 'thigh', 'shin'];
    if (typeof C !== 'object' || missing.length || needed.some(k => !(C.proportions?.[k] > 0))) {
      throw new Error(`CHARACTER needs proportions (${needed.join(', ')}) and a draw function for every bone; missing: ${missing.join(', ') || 'proportions'}`);
    }
    const print = (drawing, sizes) => {
      const canvas = document.createElement('canvas');
      printDrawing(canvas, drawing);
      return Object.fromEntries(sizes.map(([name, w, h]) => {
        const scaled = document.createElement('canvas'); scaled.width = w; scaled.height = h;
        const ctx = scaled.getContext('2d'); ctx.imageSmoothingQuality = 'high';
        ctx.drawImage(canvas, 0, 0, w, h);
        return [name, scaled.toDataURL('image/webp', .9)];
      }));
    };
    const sheetPoses = [['stand', 'smile'], ['wave', 'grin'], ['cheer', 'laugh'], ['point', 'surprised'],
                        ['sit', 'calm'], ['walk', 'wink'], ['dance', 'laugh'], ['scared', 'scared']];
    const sheet = {
      width: 1760, height: 620,
      paint() {
        sheetPoses.forEach(([name, expression], i) => {
          paintCharacter(C, { ...POSES[name], expression }, { x: 110 + i * 220, y: 590, height: 520 });
        });
      },
    };
    return {
      ...print(characterPortrait(C), [['portrait-256', 256, 256], ['portrait-512', 512, 512]]),
      ...print(characterFullBody(C), [['full', 500, 900]]),
      ...print(sheet, [['poses', 1760, 620]]),
    };
  });
  if (errors.length) throw new Error(errors.join('\n'));
  for (const [name, dataURL] of Object.entries(images)) {
    if (!dataURL.startsWith('data:image/webp;base64,')) throw new Error(`${name}: the canvas gave ${dataURL.slice(0, 40)}`);
    writeFileSync(`${outPrefix}-${name}.webp`, Buffer.from(dataURL.split(',')[1], 'base64'));
  }
} catch (error) {
  console.error(error.message);
  process.exitCode = 1;
} finally {
  await browser.close();
}
