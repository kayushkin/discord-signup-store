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
function part(points, options) { PARTS.push({ points, tilt: TILT, xform: XFORM, ...options }); }
function stroke(points, options) { PARTS.push({ points, tilt: TILT, xform: XFORM, openStroke: true, ...options }); }

// Parts are drawn through the current transform, so a character's arm can
// be drawn once in its own frame and posed anywhere. [a, b, c, d, e, f] as
// the canvas takes it: x' = a·x + c·y + e, y' = b·x + d·y + f. Each change
// makes a new array, so a part keeps the transform it was drawn under.
const IDENTITY = [1, 0, 0, 1, 0, 0];
let XFORM = IDENTITY;
const XFORM_STACK = [];
function multiplyTransforms(m, n) {
  return [m[0] * n[0] + m[2] * n[1], m[1] * n[0] + m[3] * n[1], m[0] * n[2] + m[2] * n[3], m[1] * n[2] + m[3] * n[3],
          m[0] * n[4] + m[2] * n[5] + m[4], m[1] * n[4] + m[3] * n[5] + m[5]];
}
function applyTransform(m, [x, y]) { return [m[0] * x + m[2] * y + m[4], m[1] * x + m[3] * y + m[5]]; }
function pushTransform() { XFORM_STACK.push(XFORM); }
function popTransform() { XFORM = XFORM_STACK.pop() ?? IDENTITY; }
function translateBy(x, y) { XFORM = multiplyTransforms(XFORM, [1, 0, 0, 1, x, y]); }
function rotateBy(angle) { const c = Math.cos(angle), s = Math.sin(angle); XFORM = multiplyTransforms(XFORM, [c, s, -s, c, 0, 0]); }
function scaleBy(sx, sy = sx) { XFORM = multiplyTransforms(XFORM, [sx, 0, 0, sy, 0, 0]); }
// Draws with paint() flipped left to right about x = 0: a right arm from a left one.
function mirrored(paint) { return (...args) => { pushTransform(); scaleBy(-1, 1); paint(...args); popTransform(); }; }
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

function printDrawing(canvas, drawing, paintOptions) {
  const { width, height } = drawing;
  canvas.width = width; canvas.height = height;
  PARTS = []; TILT = null; XFORM = IDENTITY; XFORM_STACK.length = 0;
  drawing.paint(paintOptions);
  const layer = () => { const c = document.createElement('canvas'); c.width = width; c.height = height; return c; };
  const colourLayer = layer(), blackLayer = layer();
  const colour = colourLayer.getContext('2d'), black = blackLayer.getContext('2d');
  PARTS.forEach((p, index) => {
    for (const ctx of [colour, black]) {
      ctx.save();
      if (p.xform && p.xform !== IDENTITY) ctx.transform(...p.xform);
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

// ---------------------------------------------------------------- characters
// A character is a whole person, drawn once, part by part, each part in its
// own bone's frame, and posed anywhere: a portrait, a group picture, sitting
// at a table or cheering. CHARACTER.md is the contract. The skeleton is the
// same for everyone; a character sets its proportions and draws its parts.
//
// Every bone's frame has its origin at the joint with its parent. Limbs hang
// down: an arm or a leg runs from its joint along +y. The torso, neck and head
// run up, along -y. L is the limb on the viewer's left. A pose turns bones at
// their joints, in degrees; for the right side the angle is mirrored, so the
// same number raises either arm outward.
const CHARACTER_BONES = [
  ['pelvis', null], ['torso', 'pelvis'], ['neck', 'torso'], ['head', 'neck'],
  ['upperArmL', 'torso'], ['forearmL', 'upperArmL'], ['handL', 'forearmL'],
  ['upperArmR', 'torso'], ['forearmR', 'upperArmR'], ['handR', 'forearmR'],
  ['thighL', 'pelvis'], ['shinL', 'thighL'], ['footL', 'shinL'],
  ['thighR', 'pelvis'], ['shinR', 'thighR'], ['footR', 'shinR'],
];
const CHARACTER_PARENT = Object.fromEntries(CHARACTER_BONES);

// Where a bone starts, in its parent's frame.
function characterJointOffset(bone, P) {
  const shoulderY = -P.torso + (P.shoulderDrop ?? 0);
  switch (bone) {
    case 'pelvis': case 'torso': return [0, 0];
    case 'neck': return [0, -P.torso];
    case 'head': return [0, -P.neck];
    case 'upperArmL': return [-P.shoulderWidth / 2, shoulderY];
    case 'upperArmR': return [P.shoulderWidth / 2, shoulderY];
    case 'forearmL': case 'forearmR': return [0, P.upperArm];
    case 'handL': case 'handR': return [0, P.forearm];
    case 'thighL': return [-P.hipWidth / 2, 0];
    case 'thighR': return [P.hipWidth / 2, 0];
    case 'shinL': case 'shinR': return [0, P.thigh];
    case 'footL': case 'footR': return [0, P.shin];
  }
  throw new Error(`no bone ${bone}`);
}
// A grown-up's proportions, for placing someone whose avatar is only a
// portrait: their portrait goes where a character's head would be.
const STANDARD_PROPORTIONS = {
  head: 240, neck: 40, torso: 300, shoulderWidth: 188, shoulderDrop: 34, hipWidth: 140,
  upperArm: 170, forearm: 150, hand: 70, thigh: 230, shin: 220, footHeight: 40,
};
function characterStandingHeight(P) { return P.head + P.neck + P.torso + P.thigh + P.shin + (P.footHeight ?? 0); }

// Poses: angles in degrees; a value may be { angle, stretch }, stretch
// shortening the bone as it points toward or away from the viewer — a seated
// thigh is one. The lowest foot always stands on the ground; lift raises the
// whole character off it. expression and look go to the head; open and hold
// to the hands. behind lists limbs to draw behind the torso: 'armL', 'armR'.
const POSES = {
  stand: {},
  wave: { upperArmR: 145, forearmR: 30, handR: 0, expression: 'smile' },
  cheer: { upperArmL: 160, forearmL: 15, upperArmR: 160, forearmR: 15, expression: 'grin', open: true },
  point: { upperArmL: 95, forearmL: 0, expression: 'smile' },
  sit: { thighL: { angle: -8, stretch: .3 }, thighR: { angle: -8, stretch: .3 }, shinL: 6, shinR: 6 },
  jump: { upperArmL: 150, forearmL: 20, upperArmR: 150, forearmR: 20, thighL: 20, shinL: -40, thighR: 20, shinR: -40, lift: .12, expression: 'laugh', open: true },
  walk: { thighL: -12, shinL: 10, thighR: 14, shinR: 4, upperArmL: -14, forearmL: -10, upperArmR: -10, forearmR: -12 },
  dance: { upperArmL: 130, forearmL: 40, upperArmR: 40, forearmR: 60, thighL: 18, shinL: -20, torso: 6, head: -8, expression: 'laugh' },
  hold: { upperArmL: 12, forearmL: -85, upperArmR: 12, forearmR: -85, hold: true },
  scared: { upperArmL: 70, forearmL: 110, upperArmR: 70, forearmR: 110, expression: 'scared', open: true },
  shrug: { upperArmL: 40, forearmL: 80, upperArmR: 40, forearmR: 80, head: 6, expression: 'smile', open: true },
};
const CHARACTER_EXPRESSIONS = ['smile', 'grin', 'laugh', 'surprised', 'scared', 'wink', 'calm'];

function poseAngle(pose, bone) {
  const value = pose[bone];
  const angle = typeof value === 'number' ? value : (value?.angle ?? 0);
  const mirror = bone.endsWith('R') ? -1 : 1;
  return mirror * angle * Math.PI / 180;
}
function poseStretch(pose, bone) { const value = pose[bone]; return typeof value === 'object' ? (value.stretch ?? 1) : 1; }

// The frame of every bone, in the character's own units with the pelvis at
// [0, 0], for a pose: frame is where its children hang from, draw is where
// its own parts are drawn, shortened by its stretch. A child's joint moves
// in as its parent is shortened.
function characterFrames(C, pose = {}) {
  const P = C.proportions, frames = {};
  for (const [bone, parent] of CHARACTER_BONES) {
    const [x, y] = characterJointOffset(bone, P);
    const from = parent ? frames[parent] : { frame: IDENTITY, stretch: 1 };
    const angle = poseAngle(pose, bone), c = Math.cos(angle), s = Math.sin(angle);
    const frame = multiplyTransforms(multiplyTransforms(from.frame, [1, 0, 0, 1, x, y * from.stretch]), [c, s, -s, c, 0, 0]);
    const stretch = poseStretch(pose, bone);
    frames[bone] = { frame, stretch, draw: multiplyTransforms(frame, [1, 0, 0, stretch, 0, 0]) };
  }
  return frames;
}

// The transform from a character's units to the picture: standing height
// pixels tall, flipped when mirror, with its lowest foot on the ground at
// [x, y] whatever the pose — a seated character's shortened legs bring the
// rest of them down with them. lift raises them off it, as a fraction of their
// height: a jump.
function characterPlacement(C, pose, placement) {
  const P = C.proportions, s = placement.height / characterStandingHeight(P), frames = characterFrames(C, pose);
  const sole = P.footHeight ?? 0;
  const lowest = Math.max(...['footL', 'footR'].map(foot => applyTransform(frames[foot].draw, [0, sole])[1]));
  const lift = (pose.lift ?? 0) * characterStandingHeight(P);
  return multiplyTransforms([placement.mirror ? -s : s, 0, 0, s, placement.x, placement.y], [1, 0, 0, 1, 0, -lowest - lift]);
}

// Where a posed character's joints land in the picture: the head's middle,
// each hand's and foot's end, the pelvis, and the top of the head — for a
// scene to put a drink in a hand or a hat on a head.
function characterJoints(C, pose = {}, placement) {
  const P = C.proportions, frames = characterFrames(C, pose), base = characterPlacement(C, pose, placement);
  const at = (bone, point) => applyTransform(multiplyTransforms(base, frames[bone].draw), point);
  return {
    head: at('head', [0, -P.head / 2]), top: at('head', [0, -P.head]), pelvis: at('pelvis', [0, 0]),
    handL: at('handL', [0, P.hand]), handR: at('handR', [0, P.hand]),
    footL: at('footL', [0, P.footHeight ?? 0]), footR: at('footR', [0, P.footHeight ?? 0]),
    scale: placement.height / characterStandingHeight(P),
  };
}

// Paints a character posed and placed. pose is one of POSES, changed as
// wanted, or angles of its own.
function paintCharacter(C, pose = {}, placement) {
  const frames = characterFrames(C, pose), base = characterPlacement(C, pose, placement);
  const behind = new Set(pose.behind ?? []);
  const limb = { armL: ['upperArmL', 'forearmL', 'handL'], armR: ['upperArmR', 'forearmR', 'handR'],
                 legL: ['thighL', 'shinL', 'footL'], legR: ['thighR', 'shinR', 'footR'] };
  const order = [];
  for (const name of ['armL', 'armR']) if (behind.has(name)) order.push(...limb[name]);
  order.push(...limb.legL, ...limb.legR, 'pelvis', 'torso');
  for (const name of ['armL', 'armR']) if (!behind.has(name)) order.push(...limb[name]);
  order.push('neck', 'head');
  const options = { expression: pose.expression ?? 'smile', look: pose.look ?? 'ahead', open: !!pose.open, hold: !!pose.hold };
  pushTransform();
  const outer = XFORM;
  for (const bone of order) {
    const draw = C.draw[bone];
    if (!draw) continue;
    XFORM = multiplyTransforms(multiplyTransforms(outer, base), frames[bone].draw);
    draw(options);
  }
  popTransform();
}

// A character's portrait as a drawing: their badge, and them standing, head
// and shoulders filling it, for the pages' round avatar.
function characterPortrait(C) {
  return {
    width: 800, height: 800,
    paint() {
      if (C.badge) C.badge();
      const P = C.proportions, headPixels = 300, height = characterStandingHeight(P) * headPixels / P.head;
      // Put the middle of the head at [400, 360].
      const pose = C.portraitPose ?? {};
      const joints = characterJoints(C, pose, { x: 0, y: 0, height });
      paintCharacter(C, pose, { x: 400 - joints.head[0], y: 360 - joints.head[1], height });
    },
  };
}

// A character standing, whole, for the gallery.
function characterFullBody(C, pose = {}) {
  return { width: 500, height: 900, paint() { paintCharacter(C, pose, { x: 250, y: 870, height: 820 }); } };
}
