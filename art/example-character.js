// An example character, for CHARACTER.md: a person in a teal t-shirt, jeans
// and white sneakers. Nobody in particular. It shows the frame each part is
// drawn in, each expression, open and closed hands, and a right side made by
// mirroring the left.

// Colours the page's inks lack, in the same muted print palette.
const HAIR = '#4a2e1f', SHIRT = INK.teal, JEANS = INK.blue, SHOE = INK.white;

const PROPORTIONS = {
  head: 240, neck: 40, torso: 300, shoulderWidth: 188, shoulderDrop: 34, hipWidth: 140,
  upperArm: 170, forearm: 150, hand: 70, thigh: 230, shin: 220, footHeight: 40,
};

// The head's frame: the origin is where the chin meets the neck, and the head
// runs up to y = -240, centred on x = 0.
function head({ expression, look }) {
  const lx = look === 'left' ? -7 : look === 'right' ? 7 : 0;
  // Hair behind the face, then ears, then the face.
  part(ellipsePoints(0, -128, 102, 116, 28), { fill: HAIR });
  for (const side of [-1, 1]) part(ellipsePoints(side * 90, -100, 16, 24, 14), { fill: INK.skin, line: 4 });
  part(ellipsePoints(0, -104, 88, 102, 30), { fill: INK.skin,
    colourDetail(ctx) { halftone(ctx, [-90, -200, 90, 0], 7, 2.6, INK.pink, (x, y) => 1 - Math.hypot(Math.abs(x) - 50, y + 70) / 26); } });
  // The fringe.
  part([[-94, -112], [-86, -176], [-50, -214], [0, -226], [52, -214], [88, -178], [96, -114], [70, -150], [30, -168], [-10, -156], [-50, -166], [-78, -140]],
    { fill: HAIR, colourDetail(ctx) { ctx.strokeStyle = 'rgba(248,239,216,.35)'; ctx.lineWidth = 3;
      for (const x of [-50, -10, 30, 64]) { ctx.beginPath(); ctx.moveTo(x, -210); ctx.quadraticCurveTo(x + 8, -185, x - 4, -160); ctx.stroke(); } } });

  // Eyes and brows, by expression.
  const eyeY = -104, browY = -140;
  const brow = (side, lift) => stroke([[side * 20, browY - lift], [side * 36, browY - 6 - lift], [side * 52, browY - 2 - lift]], { width: 7 });
  if (expression === 'laugh') {
    for (const side of [-1, 1]) stroke([[side * 22, eyeY + 4], [side * 34, eyeY - 8], [side * 46, eyeY + 4]], { width: 6 });
  } else if (expression === 'surprised' || expression === 'scared') {
    for (const side of [-1, 1]) {
      part(ellipsePoints(side * 34, eyeY, 15, 19, 16), { fill: INK.white, line: 4 });
      part(ellipsePoints(side * 34 + lx, eyeY + 2, 6, 7, 10), { blackFill: true, line: 0 });
    }
  } else {
    for (const side of [-1, 1]) {
      if (expression === 'wink' && side === -1) { stroke([[-46, eyeY], [-34, eyeY + 6], [-22, eyeY]], { width: 6 }); continue; }
      part(ellipsePoints(side * 34 + lx, eyeY, 8, 12, 12), { blackFill: true, line: 0 });
    }
  }
  const lift = expression === 'surprised' ? 12 : expression === 'scared' ? 8 : 0;
  if (expression === 'scared') { stroke([[-20, browY - 10], [-52, browY + 2]], { width: 7 }); stroke([[20, browY - 10], [52, browY + 2]], { width: 7 }); }
  else { brow(-1, lift); brow(1, lift); }
  stroke([[-2, -92], [-8, -70], [4, -66]], { width: 4, taper: false });

  // The mouth.
  if (expression === 'grin' || expression === 'laugh') {
    part([[-36, -48], [36, -48], [22, -24], [0, -18], [-22, -24]], { fill: expression === 'laugh' ? INK.plum : INK.white, line: 5 });
    if (expression === 'laugh') part(ellipsePoints(0, -26, 14, 6, 10), { fill: INK.pink, line: 0 });
  } else if (expression === 'surprised') {
    part(ellipsePoints(0, -36, 12, 15, 12), { fill: INK.plum, line: 5 });
  } else if (expression === 'scared') {
    stroke([[-26, -34], [-14, -42], [0, -34], [14, -42], [26, -34]], { width: 5, smooth: false });
  } else if (expression === 'calm') {
    stroke([[-16, -38], [16, -38]], { width: 5 });
  } else {
    stroke([[-30, -46], [-14, -34], [0, -32], [14, -34], [30, -46]], { width: 6 });
  }
}

function neck() { part([[-24, 10], [24, 10], [22, -46], [-22, -46]], { fill: INK.skin }); }

// The torso's frame: the origin is between the hips, the shoulders are at
// y = -300.
function torso() {
  part([[-118, -282], [-70, -306], [-28, -302], [0, -292], [28, -302], [70, -306], [118, -282], [110, -150], [96, -20], [84, 12], [-84, 12], [-96, -20], [-110, -150]],
    { fill: SHIRT, colourDetail(ctx) { halftone(ctx, [-120, -310, 120, 20], 9, 3.2, INK.blue, (x, y) => (Math.abs(x) - 60) / 60); } });
  part([[-30, -302], [0, -262], [30, -302]], { fill: INK.skin, line: 4 });
  part([[34, -220], [74, -220], [72, -178], [36, -178]], { fill: SHIRT, line: 4 });
}

function pelvis() {
  part([[-86, -12], [86, -12], [88, 64], [-88, 64]], { fill: JEANS });
  stroke([[-84, 6], [84, 6]], { width: 5, taper: false });
  part([[-10, -6], [10, -6], [10, 16], [-10, 16]], { fill: INK.mustard, line: 3 });
}

// An arm's frames: each runs down from its joint along +y.
function upperArmL() {
  part(capsule([0, 40], [0, 176], 42), { fill: INK.skin });
  // The sleeve: round over the shoulder, open at the elbow end.
  part([[-34, 0], [-26, -26], [0, -36], [26, -26], [34, 0], [36, 92], [-36, 96]], { fill: SHIRT });
}
function forearmL() { part(capsule([0, 0], [0, 150], 38), { fill: INK.skin }); }
function handL({ open, hold }) {
  if (hold) { part(ellipsePoints(0, 30, 26, 30, 16), { fill: INK.skin }); stroke([[-18, 20], [18, 20]], { width: 4 }); return; }
  if (!open) { part(ellipsePoints(0, 30, 24, 30, 16), { fill: INK.skin }); return; }
  for (const [x, length] of [[-18, 62], [-6, 72], [6, 72], [18, 62]]) part(capsule([x, 30], [x * 1.3, length], 12), { fill: INK.skin, line: 3 });
  part(capsule([-20, 22], [-44, 44], 13), { fill: INK.skin, line: 3 });
  part(ellipsePoints(0, 28, 26, 26, 16), { fill: INK.skin });
}

// A leg's frames, the same way.
function thighL() { part(capsule([0, 0], [0, 236], 78), { fill: JEANS }); }
function shinL() {
  part(capsule([0, -20], [0, 222], 68), { fill: JEANS,
    colourDetail(ctx) { halftone(ctx, [-40, 150, 40, 230], 7, 2.6, INK.black, (x, y) => (y - 170) / 60); } });
}
// A foot's frame: the origin is the ankle, the sole is at y = 40. The left
// foot points to the viewer's left.
function footL() {
  part([[28, -10], [-8, -14], [-48, 4], [-66, 26], [-60, 42], [34, 42], [38, 18]], { fill: SHOE });
  stroke([[-62, 34], [36, 34]], { width: 5, taper: false });
}

const CHARACTER = {
  proportions: PROPORTIONS,
  // The portrait's round backdrop, in its 800 by 800 square.
  badge() {
    part(ellipsePoints(400, 400, 392, 392, 48), { fill: INK.mustard, line: 6,
      colourDetail(ctx) {
        ctx.fillStyle = 'rgba(248,239,216,.3)';
        for (let i = 0; i < 16; i++) { const a = i / 16 * Math.PI * 2; ctx.beginPath(); ctx.moveTo(400, 400); ctx.arc(400, 400, 400, a, a + Math.PI / 32); ctx.closePath(); ctx.fill(); }
      } });
  },
  draw: {
    head, neck, torso, pelvis,
    upperArmL, forearmL, handL, upperArmR: mirrored(upperArmL), forearmR: mirrored(forearmL), handR: mirrored(handL),
    thighL, shinL, footL, thighR: mirrored(thighL), shinR: mirrored(shinL), footR: mirrored(footL),
  },
};
