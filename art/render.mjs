// Paints each drawing in headless Chrome and saves it, at each size the
// drawing lists, as ../static/art/<name>-<size>.webp; a drawing with an icon
// crop also gets <name>-icon-<size>.png, and the one marked favicon gets
// favicon.ico. art.go builds these files into the server binary.
// Run: node render.mjs [name…]   (default: every drawing). CHROME_PATH names
// the Chrome or Chromium to use.
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
  const icons = await page.evaluate('window.paintedIcons');
  for (const [size, dataURL] of Object.entries(icons)) {
    const png = Buffer.from(dataURL.split(',')[1], 'base64');
    writeFileSync(path.join(outDir, `${name}-icon-${size}.png`), png);
    console.log(`painted ${name}-icon-${size}.png`);
    // The site's /favicon.ico: an .ico file may hold a PNG as it is.
    if (Number(size) === 32 && (await page.evaluate('DRAWING.icon.favicon === true'))) {
      const header = Buffer.alloc(22);
      header.writeUInt16LE(0, 0); header.writeUInt16LE(1, 2); header.writeUInt16LE(1, 4);
      header.writeUInt8(32, 6); header.writeUInt8(32, 7); header.writeUInt16LE(1, 10); header.writeUInt16LE(32, 12);
      header.writeUInt32LE(png.length, 14); header.writeUInt32LE(22, 18);
      writeFileSync(path.join(outDir, 'favicon.ico'), Buffer.concat([header, png]));
      console.log('painted favicon.ico');
    }
  }
  await page.close();
}
await browser.close();
