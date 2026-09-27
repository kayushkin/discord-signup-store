// Paints each drawing in headless Chrome and saves it, at each size the
// drawing lists, as ../static/art/<name>-<size>.webp. Those files are built
// into the server binary (art.go). Run: node render.mjs [name…]   (default:
// every drawing). CHROME_PATH names the Chrome or Chromium to use.
import { chromium } from 'playwright-core';
import { readdirSync, writeFileSync, mkdirSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const here = path.dirname(fileURLToPath(import.meta.url));
const outDir = path.join(here, '..', 'static', 'art');
mkdirSync(outDir, { recursive: true });
const all = readdirSync(path.join(here, 'drawings')).filter(f => f.endsWith('.js')).map(f => f.slice(0, -3));
const names = process.argv.slice(2).length ? process.argv.slice(2) : all;

const browser = await chromium.launch({
  executablePath: process.env.CHROME_PATH,
  args: ['--allow-file-access-from-files'],
});
for (const name of names) {
  const page = await browser.newPage();
  page.on('pageerror', e => { console.error(`${name}: ${e.message}`); process.exitCode = 1; });
  page.on('console', m => { if (m.type() === 'error') console.error(`${name}: ${m.text()}`); });
  await page.goto(`file://${path.join(here, 'studio.html')}?drawing=${name}`);
  await page.waitForFunction('window.paintedImages !== undefined', null, { timeout: 120000 });
  const images = await page.evaluate('window.paintedImages');
  for (const [size, dataURL] of Object.entries(images)) {
    if (!dataURL.startsWith('data:image/webp;base64,')) throw new Error(`${name}: canvas gave ${dataURL.slice(0, 40)}`);
    writeFileSync(path.join(outDir, `${name}-${size}.webp`), Buffer.from(dataURL.split(',')[1], 'base64'));
    console.log(`painted ${name}-${size}.webp`);
  }
  await page.close();
}
await browser.close();
