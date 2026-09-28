// Prints one avatar drawing — a file defining DRAWING, as art/drawings do —
// and saves it as WebP at each size asked for: <out-prefix>-<size>.webp.
// Run: node render-avatar.mjs <drawing.js> <out-prefix> <size>…
// CHROME_PATH names the Chrome or Chromium to use.
//
// A model writes these drawings from a photo someone uploaded, so the code is
// not trusted. It runs in a blank page, not a file:// one, so it cannot read
// files on the host, and every network request is refused, so it cannot send
// anything anywhere. What it can do is paint.
import { chromium } from 'playwright-core';
import { readFileSync, writeFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const here = path.dirname(fileURLToPath(import.meta.url));
const [drawingFile, outPrefix, ...sizeArguments] = process.argv.slice(2);
const sizes = sizeArguments.map(Number);
if (!drawingFile || !outPrefix || !sizes.length || sizes.some(size => !(size > 0 && size <= 2048))) {
  console.error('usage: node render-avatar.mjs <drawing.js> <out-prefix> <size>…');
  process.exit(2);
}
const kit = readFileSync(path.join(here, 'kit.js'), 'utf8');
const drawing = readFileSync(drawingFile, 'utf8');

const browser = await chromium.launch({ executablePath: process.env.CHROME_PATH });
try {
  const page = await browser.newPage();
  await page.route('**/*', route => route.abort());
  const errors = [];
  page.on('pageerror', e => errors.push(e.message));
  page.on('console', m => { if (m.type() === 'error') errors.push(m.text()); });
  await page.setContent('<!doctype html><meta charset="utf-8"><canvas id="print"></canvas>');
  await page.addScriptTag({ content: kit });
  await page.addScriptTag({ content: drawing });
  if (errors.length) throw new Error(errors.join('\n'));
  const images = await page.evaluate(sizes => {
    if (typeof DRAWING !== 'object' || typeof DRAWING.paint !== 'function' || !(DRAWING.width > 0) || !(DRAWING.height > 0)) {
      throw new Error('the drawing does not define DRAWING with width, height and paint()');
    }
    const print = document.getElementById('print');
    printDrawing(print, DRAWING);
    return Object.fromEntries(sizes.map(size => {
      const scaled = document.createElement('canvas'); scaled.width = scaled.height = size;
      const ctx = scaled.getContext('2d'); ctx.imageSmoothingQuality = 'high';
      ctx.drawImage(print, 0, 0, size, size * DRAWING.height / DRAWING.width);
      return [size, scaled.toDataURL('image/webp', .9)];
    }));
  }, sizes);
  if (errors.length) throw new Error(errors.join('\n'));
  for (const [size, dataURL] of Object.entries(images)) {
    if (!dataURL.startsWith('data:image/webp;base64,')) throw new Error(`the canvas gave ${dataURL.slice(0, 40)}`);
    writeFileSync(`${outPrefix}-${size}.webp`, Buffer.from(dataURL.split(',')[1], 'base64'));
  }
} catch (error) {
  console.error(error.message);
  process.exitCode = 1;
} finally {
  await browser.close();
}
