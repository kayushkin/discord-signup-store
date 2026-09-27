// A small screen-print kit. A drawing lists parts back to front; the kit
// prints them the way a riso does: one colour pass, one black pass laid
// slightly out of register, and uneven ink. Plain canvas 2D, no libraries.

// The page's own inks (templates/layout.html :root), plus skin and lip tones.
const INK = {
  black: '#221c17', white: '#fbf6ea', paperLight: '#f8efd8', skin: '#f6e4d2',
  red: '#dd3f2a', blue: '#255a86', teal: '#1d7766', mustard: '#c98a10',
  pink: '#ec8f8f', plum: '#4a1c2b',
};

// Repeatable randomness, so a drawing prints the same every time.
function hash(n) { const s = Math.sin(n * 127.1 + 311.7) * 43758.5453; return s - Math.floor(s); }
function wobble(points, amount, seed) {
  return points.map(([x, y], i) => [x + (hash(seed + i * 2) - .5) * amount, y + (hash(seed + i * 2 + 1) - .5) * amount]);
}

// A closed smooth path through points (Catmull-Rom as cubic Béziers).
function smoothPath(points, closed = true, tension = 1) {
  const path = new Path2D(), n = points.length, at = i => points[closed ? (i + n) % n : Math.max(0, Math.min(n - 1, i))];
  path.moveTo(...points[0]);
  for (let i = 0; i < (closed ? n : n - 1); i++) {
    const p0 = at(i - 1), p1 = at(i), p2 = at(i + 1), p3 = at(i + 2), k = tension / 6;
    path.bezierCurveTo(p1[0] + (p2[0] - p0[0]) * k, p1[1] + (p2[1] - p0[1]) * k,
                       p2[0] - (p3[0] - p1[0]) * k, p2[1] - (p3[1] - p1[1]) * k, p2[0], p2[1]);
  }
  if (closed) path.closePath();
  return path;
}
function straightPath(points, closed = true) {
  const path = new Path2D(); path.moveTo(...points[0]);
  for (const p of points.slice(1)) path.lineTo(...p);
  if (closed) path.closePath();
  return path;
}
function ellipsePoints(cx, cy, rx, ry, steps = 24, rotation = 0) {
  return Array.from({ length: steps }, (_, i) => {
    const a = i / steps * Math.PI * 2, x = Math.cos(a) * rx, y = Math.sin(a) * ry;
    return [cx + x * Math.cos(rotation) - y * Math.sin(rotation), cy + x * Math.sin(rotation) + y * Math.cos(rotation)];
  });
}

// The two passes. Colour: flat fills and colour halftones. Black: outlines,
// black patterns and black halftones. A part hides the black of the parts
// behind it, so lines never show through something in front.
let PARTS = [], TILT = null;
// Parts drawn while a tilt is set turn with it: tilt(angle, [x, y]) … tilt(null).
function tilt(angle, pivot) { TILT = angle === null ? null : { angle, pivot }; }
function part(points, options) { PARTS.push({ points, tilt: TILT, ...options }); }
function stroke(points, options) { PARTS.push({ points, tilt: TILT, openStroke: true, ...options }); }
// A thick rounded bar from a to b, for arms and fingers.
function capsule(a, b, width) {
  const angle = Math.atan2(b[1] - a[1], b[0] - a[0]), r = width / 2, points = [];
  for (let i = 0; i <= 8; i++) { const t = angle + Math.PI / 2 + i / 8 * Math.PI; points.push([a[0] + Math.cos(t) * r, a[1] + Math.sin(t) * r]); }
  for (let i = 0; i <= 8; i++) { const t = angle - Math.PI / 2 + i / 8 * Math.PI; points.push([b[0] + Math.cos(t) * r, b[1] + Math.sin(t) * r]); }
  return points;
}

// A line that swells in the middle and tapers at the ends, like a brush pen.
function taperedStroke(ctx, points, width) {
  const n = points.length;
  for (let i = 0; i < n - 1; i++) {
    const t = i / (n - 1), w = width * (0.35 + 0.65 * Math.sin(Math.PI * Math.min(1, Math.max(0, t + .5 / n))));
    ctx.lineWidth = w; ctx.beginPath(); ctx.moveTo(...points[i]); ctx.lineTo(...points[i + 1]); ctx.stroke();
  }
}
function resample(path2dPoints, smooth, steps = 14) {
  if (!smooth) return path2dPoints;
  const out = [], n = path2dPoints.length, at = i => path2dPoints[Math.max(0, Math.min(n - 1, i))];
  for (let i = 0; i < n - 1; i++) for (let s = 0; s < steps; s++) {
    const t = s / steps, p0 = at(i - 1), p1 = at(i), p2 = at(i + 1), p3 = at(i + 2);
    const f = (a, b, c, d) => .5 * (2 * b + (-a + c) * t + (2 * a - 5 * b + 4 * c - d) * t * t + (-a + 3 * b - 3 * c + d) * t * t * t);
    out.push([f(p0[0], p1[0], p2[0], p3[0]), f(p0[1], p1[1], p2[1], p3[1])]);
  }
  out.push(path2dPoints[n - 1]);
  return out;
}

// Dots on a grid, their radius following strength(x, y) from 0 to 1.
function halftone(ctx, box, spacing, maxRadius, colour, strength, angle = Math.PI / 4) {
  ctx.fillStyle = colour;
  const [x0, y0, x1, y1] = box, cx = (x0 + x1) / 2, cy = (y0 + y1) / 2, reach = Math.hypot(x1 - x0, y1 - y0) / 2;
  const cos = Math.cos(angle), sin = Math.sin(angle);
  for (let v = -reach; v <= reach; v += spacing) for (let u = -reach; u <= reach; u += spacing) {
    const x = cx + u * cos - v * sin, y = cy + u * sin + v * cos, r = maxRadius * Math.max(0, Math.min(1, strength(x, y)));
    if (r > .4) { ctx.beginPath(); ctx.arc(x, y, r, 0, Math.PI * 2); ctx.fill(); }
  }
}
// Harlequin diamonds, black on whatever is below, along a direction.
function harlequin(ctx, box, size, angle, offset = 0) {
  const [x0, y0, x1, y1] = box, cx = (x0 + x1) / 2, cy = (y0 + y1) / 2, reach = Math.hypot(x1 - x0, y1 - y0);
  ctx.save(); ctx.translate(cx, cy); ctx.rotate(angle); ctx.fillStyle = INK.black;
  const w = size * .62, h = size;
  for (let row = -Math.ceil(reach / h); row <= reach / h; row++) for (let col = -Math.ceil(reach / w); col <= reach / w; col++) {
    if ((row + col) % 2 === 0) continue;
    const x = col * w, y = row * h / 1 + offset;
    ctx.beginPath(); ctx.moveTo(x, y - h / 2); ctx.lineTo(x + w / 2, y); ctx.lineTo(x, y + h / 2); ctx.lineTo(x - w / 2, y); ctx.closePath(); ctx.fill();
  }
  ctx.restore();
}

function printDrawing(canvas, drawing) {
  const { width, height } = drawing;
  canvas.width = width; canvas.height = height;
  PARTS = [];
  drawing.paint();
  const layer = () => { const c = document.createElement('canvas'); c.width = width; c.height = height; return c; };
  const colourLayer = layer(), blackLayer = layer();
  const colour = colourLayer.getContext('2d'), black = blackLayer.getContext('2d');
  PARTS.forEach((p, index) => {
    for (const ctx of [colour, black]) {
      ctx.save();
      if (p.tilt) { ctx.translate(...p.tilt.pivot); ctx.rotate(p.tilt.angle); ctx.translate(-p.tilt.pivot[0], -p.tilt.pivot[1]); }
    }
    printPart(p, index, colour, black);
    colour.restore(); black.restore();
  });
  function printPart(p, index, colour, black) {
    const seed = index * 97;
    if (p.openStroke) {
      const pts = resample(wobble(p.points, p.wobble ?? 2, seed), p.smooth !== false);
      const ctx = p.colour ? colour : black;
      ctx.strokeStyle = p.colour ?? INK.black; ctx.lineCap = 'round'; ctx.lineJoin = 'round';
      if (p.taper === false) { ctx.lineWidth = p.width ?? 4; ctx.stroke(straightPath(pts, false)); }
      else taperedStroke(ctx, pts, p.width ?? 4);
      return;
    }
    const pts = wobble(p.points, p.wobble ?? 2.5, seed);
    const shape = p.smooth === false ? straightPath(pts) : smoothPath(pts);
    if (p.fill) { colour.fillStyle = p.fill; colour.fill(shape); }
    if (p.colourDetail) { colour.save(); colour.clip(shape); p.colourDetail(colour); colour.restore(); }
    // Hide the black of whatever is behind this part.
    if (p.fill || p.hides || p.blackFill) { black.save(); black.globalCompositeOperation = 'destination-out'; black.fill(shape); black.restore(); }
    if (p.blackFill) { black.fillStyle = INK.black; black.fill(shape); }
    if (p.blackDetail) { black.save(); black.clip(shape); p.blackDetail(black); black.restore(); }
    if (p.line !== 0) {
      black.strokeStyle = INK.black; black.lineWidth = p.line ?? 5; black.lineJoin = 'round'; black.lineCap = 'round';
      black.stroke(shape);
    }
  }
  const out = canvas.getContext('2d');
  // The colour drum sits a little out of register with the black one.
  const [dx, dy] = drawing.misregister ?? [3, 2];
  out.drawImage(colourLayer, dx, dy);
  out.drawImage(blackLayer, 0, 0);
  // Uneven ink: thin the coverage in soft blotches and fine speckle.
  const image = out.getImageData(0, 0, width, height), d = image.data;
  for (let y = 0; y < height; y++) for (let x = 0; x < width; x++) {
    const i = (y * width + x) * 4; if (!d[i + 3]) continue;
    const blotch = hash(Math.floor(x / 9) * 7.3 + Math.floor(y / 9) * 131.7), speck = hash(x * 12.9898 + y * 78.233);
    d[i + 3] = Math.round(d[i + 3] * (1 - .12 * blotch - (speck > .93 ? .5 : 0)));
  }
  out.putImageData(image, 0, 0);
}
