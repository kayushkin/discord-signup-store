// The mascot: a jester in a harlequin hood with five points, a lace ruff and
// clown make-up, drawn head and shoulders in front of a red sun badge.
const DRAWING = { width: 800, height: 800, paint() {
  const badge = [400, 440], badgeRadius = 300;
  part(ellipsePoints(badge[0] + 14, badge[1] + 14, badgeRadius, badgeRadius, 40), { fill: INK.black, line: 0, wobble: 1 });
  part(ellipsePoints(...badge, badgeRadius, badgeRadius, 40), { fill: INK.red, line: 7, wobble: 1,
    colourDetail(ctx) {
      ctx.fillStyle = 'rgba(248,239,216,.28)';
      for (let i = 0; i < 18; i++) {
        const a = i / 18 * Math.PI * 2;
        ctx.beginPath(); ctx.moveTo(...badge);
        ctx.arc(...badge, badgeRadius + 20, a, a + Math.PI / 36); ctx.closePath(); ctx.fill();
      }
      halftone(ctx, [100, 140, 700, 740], 13, 5.5, INK.mustard,
        (x, y) => (Math.hypot(x - badge[0], y - badge[1]) - 190) / 110);
    } });

  // Five points of the hood, each a curved cone of diamonds.
  const spike = (a, b, tip, bend) => {
    const base = [(a[0] + b[0]) / 2, (a[1] + b[1]) / 2], dx = tip[0] - base[0], dy = tip[1] - base[1], len = Math.hypot(dx, dy);
    const perp = [-dy / len, dx / len], quad = (p, q, t) => {
      const c = [(p[0] + q[0]) / 2 + perp[0] * bend, (p[1] + q[1]) / 2 + perp[1] * bend];
      return [(1 - t) ** 2 * p[0] + 2 * (1 - t) * t * c[0] + t * t * q[0], (1 - t) ** 2 * p[1] + 2 * (1 - t) * t * c[1] + t * t * q[1]];
    };
    const points = [];
    for (let i = 0; i <= 8; i++) points.push(quad(a, tip, i / 8));
    for (let i = 1; i < 8; i++) points.push(quad(tip, b, i / 8));
    const angle = Math.atan2(dy, dx) - Math.PI / 2;
    part(points, { fill: INK.white, line: 5, blackDetail(ctx) { harlequin(ctx, [0, 0, 800, 800], 38, angle); } });
  };
  const crown = [400, 330];
  for (const [degrees, length, droop, bend] of [[-172, 380, 70, -30], [-128, 330, 0, 28], [-90, 330, 0, -22], [-52, 330, 0, -28], [-8, 380, 70, 30]]) {
    const r = degrees * Math.PI / 180, along = [Math.cos(r), Math.sin(r)], across = [-along[1], along[0]];
    const base = [crown[0] + along[0] * 95, crown[1] + along[1] * 95];
    spike([base[0] + across[0] * 62, base[1] + across[1] * 62], [base[0] - across[0] * 62, base[1] - across[1] * 62],
          [crown[0] + along[0] * length, crown[1] + along[1] * length + droop], bend);
  }

  // Shoulders: a white lace blouse with puffed sleeves and a harlequin corset.
  const blueShade = (cx, cy, r) => ctx => halftone(ctx, [0, 0, 800, 800], 8, 3, INK.blue, (x, y) => (Math.hypot(x - cx, y - cy) - r) / 90);
  for (const side of [-1, 1]) part(ellipsePoints(400 + side * 185, 715, 95, 66, 22, side * .25), { fill: INK.white,
    colourDetail: blueShade(400 + side * 175, 690, 40),
    blackDetail(ctx) { ctx.strokeStyle = INK.black; ctx.lineWidth = 2.5;
      for (const k of [0, 1, 2]) { ctx.beginPath(); ctx.moveTo(400 + side * (150 + k * 32), 660 + k * 4); ctx.quadraticCurveTo(400 + side * (170 + k * 36), 715, 400 + side * (155 + k * 34), 775); ctx.stroke(); } } });
  part([[228, 800], [242, 700], [300, 642], [400, 624], [500, 642], [558, 700], [572, 800]],
    { fill: INK.white, colourDetail: blueShade(400, 660, 120) });
  part([[292, 800], [300, 725], [350, 702], [400, 718], [450, 702], [500, 725], [508, 800]],
    { fill: INK.white, blackDetail(ctx) { harlequin(ctx, [290, 700, 510, 800], 40, 0, 10); } });

  // The hood, framing the face.
  part([[262, 335], [272, 262], [322, 218], [400, 200], [478, 218], [528, 262], [538, 335], [534, 425], [520, 500], [492, 562], [400, 580], [308, 562], [280, 500], [266, 425]],
    { fill: INK.white, line: 6, blackDetail(ctx) { harlequin(ctx, [250, 190, 550, 590], 46, 0, 8); } });

  // Face.
  part([[300, 372], [318, 332], [360, 314], [400, 310], [440, 314], [482, 332], [500, 372], [505, 428], [492, 482], [462, 526], [426, 549], [400, 553], [374, 549], [338, 526], [308, 482], [295, 428]],
    { fill: INK.skin, line: 5 });
  // A blunt black fringe under the lace.
  part([[304, 392], [310, 358], [345, 336], [400, 330], [455, 336], [490, 358], [496, 392], [478, 384], [462, 398], [444, 386], [424, 400], [402, 387], [380, 400], [360, 386], [340, 399], [322, 386]],
    { blackFill: true, line: 3, wobble: 1.5, smooth: false });
  for (let a = -168; a <= -12; a += 13) {
    const r = a * Math.PI / 180;
    part(ellipsePoints(400 + Math.cos(r) * 104, 432 + Math.sin(r) * 112, 12, 12, 10), { fill: INK.white, line: 2.5, wobble: 1 });
  }

  // Eyes: half-lidded, heavy lashes, looking off to the side.
  for (const [x, y, side] of [[352, 432, -1], [448, 432, 1]]) {
    // Clown marks: a long red teardrop under each eye.
    part([[x, y + 10], [x + 8, y + 42], [x, y + 80], [x - 8, y + 42]], { fill: INK.red, line: 2.5, wobble: 1 });
    part(ellipsePoints(x, y, 34, 20, 16), { fill: INK.white, line: 2.5, wobble: 1 });
    part(ellipsePoints(x + 8, y + 2, 17, 17, 14), { blackFill: true, line: 0, wobble: .5 });
    part(ellipsePoints(x + 2, y - 3, 4.5, 4.5, 8), { fill: INK.white, line: 0, wobble: .3 });
    part([[x - 36, y - 1], [x - 20, y - 7], [x, y - 8], [x + 20, y - 6], [x + 36, y - 1], [x + 36, y - 30], [x - 36, y - 30]], { fill: INK.skin, line: 0, wobble: .5 });
    stroke([[x - 34, y + 1], [x - 18, y - 7], [x, y - 8], [x + 18, y - 6], [x + 34, y - 1]], { width: 8, wobble: 1 });
    stroke([[x + side * 30, y - 2], [x + side * 44, y - 12]], { width: 5, wobble: .5 });
    stroke([[x - 24, y + 12], [x, y + 17], [x + 24, y + 12]], { width: 2.5, wobble: .5 });
    stroke([[x - 26, y - 34], [x, y - 40], [x + 26, y - 36]], { width: 4, wobble: .5 });
  }
  // Cheeks in pink with a red dot screen, a pink button nose, dark pouting lips.
  for (const x of [336, 464]) part(ellipsePoints(x, 488, 28, 23, 16), { fill: INK.pink, line: 0, wobble: 1,
    colourDetail(ctx) { halftone(ctx, [x - 30, 460, x + 30, 512], 6, 2.1, INK.red, (px, py) => 1 - Math.hypot(px - x, py - 486) / 26); } });
  part(ellipsePoints(400, 470, 7, 6, 10), { fill: INK.pink, line: 2, wobble: .5 });
  part([[380, 513], [390, 504], [400, 509], [410, 504], [420, 513], [411, 521], [400, 524], [389, 521]], { fill: INK.plum, line: 2.5, wobble: .8 });
  stroke([[378, 514], [372, 519]], { width: 2.5, wobble: .3 });
  stroke([[422, 514], [428, 519]], { width: 2.5, wobble: .3 });

  // The lace ruff: two gathered rings with pleats.
  const ruff = (cy, rx, ry, lobes) => {
    const points = [];
    for (let i = 0; i < lobes * 8; i++) {
      const a = i / (lobes * 8) * Math.PI * 2, k = .84 + .18 * Math.sqrt(Math.abs(Math.sin(a * lobes / 2)));
      points.push([400 + Math.cos(a) * rx * k, cy + Math.sin(a) * ry * k]);
    }
    part(points, { fill: INK.white, line: 4.5, wobble: 1.2,
      colourDetail(ctx) { halftone(ctx, [400 - rx, cy - ry, 400 + rx, cy + ry], 7, 2.6, INK.blue, (x, y) => (y - cy) / ry); },
      blackDetail(ctx) { ctx.strokeStyle = INK.black; ctx.lineWidth = 1.8;
        for (let i = 0; i < lobes; i++) { const a = (i + .5) / lobes * Math.PI * 2;
          ctx.beginPath(); ctx.moveTo(400 + Math.cos(a) * rx * .35, cy + Math.sin(a) * ry * .35); ctx.lineTo(400 + Math.cos(a) * rx * .86, cy + Math.sin(a) * ry * .86); ctx.stroke(); } } });
  };
  ruff(596, 180, 50, 22);
  ruff(582, 118, 30, 16);
}};
