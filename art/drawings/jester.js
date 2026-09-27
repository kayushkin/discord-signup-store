// The mascot: a cheerful jester in a harlequin hood with five points, a lace
// ruff and clown make-up, waving from in front of a red sun badge.
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

  const blueShade = (cx, cy, r, reach = 90) => ctx =>
    halftone(ctx, [0, 0, 800, 800], 8, 3, INK.blue, (x, y) => (Math.hypot(x - cx, y - cy) - r) / reach);

  // The body: a white lace blouse with puffed shoulders, all one shape so
  // the sleeves grow out of it, and a harlequin corset laced in red.
  part([[176, 800], [182, 738], [198, 690], [236, 660], [290, 652], [340, 656], [400, 660], [460, 656], [510, 652], [564, 660], [602, 690], [618, 738], [624, 800]],
    { fill: INK.white, colourDetail: blueShade(400, 700, 150, 110),
      blackDetail(ctx) {
        ctx.strokeStyle = INK.black; ctx.lineWidth = 2.5;
        for (const side of [-1, 1]) for (const k of [0, 1, 2]) {
          ctx.beginPath(); ctx.moveTo(400 + side * (150 + k * 26), 664); ctx.quadraticCurveTo(400 + side * (160 + k * 30), 720, 400 + side * (150 + k * 28), 790); ctx.stroke();
        }
        ctx.lineWidth = 1.6;
        for (let i = 0; i < 22; i++) {
          const x = 200 + hash(i) * 400, y = 700 + hash(i + 50) * 100;
          if (Math.abs(x - 400) < 110) continue;
          ctx.beginPath(); ctx.arc(x, y, 5, 0, Math.PI * 2); ctx.stroke();
        }
      } });
  part([[292, 800], [298, 734], [348, 708], [400, 730], [452, 708], [502, 734], [508, 800]],
    { fill: INK.white, line: 5, blackDetail(ctx) { harlequin(ctx, [290, 700, 510, 800], 40, 0, 12); } });
  for (let y = 746; y < 800; y += 18) {
    stroke([[386, y], [414, y + 12]], { colour: INK.red, width: 5, taper: false, wobble: .5 });
    stroke([[414, y], [386, y + 12]], { colour: INK.red, width: 5, taper: false, wobble: .5 });
  }

  // The waving arm: a lace sleeve that flares at the elbow, a black lace
  // glove on the forearm, and a big open hand.
  const shoulder = [572, 700], elbow = [688, 612], wrist = [672, 478];
  part(capsule(elbow, wrist, 48), { fill: INK.skin, blackFill: true, line: 5,
    blackDetail(ctx) {
      ctx.globalCompositeOperation = 'destination-out';
      for (let i = 0; i < 46; i++) {
        const t = hash(i + 7), across = (hash(i + 90) - .5) * 36;
        ctx.beginPath(); ctx.arc(elbow[0] + (wrist[0] - elbow[0]) * t + across, elbow[1] + (wrist[1] - elbow[1]) * t, 3, 0, Math.PI * 2); ctx.fill();
      }
      ctx.globalCompositeOperation = 'source-over';
    } });
  part([[548, 668], [600, 640], [660, 596], [700, 578], [742, 604], [762, 660], [744, 676], [728, 664], [712, 682], [694, 668], [676, 686], [650, 690], [600, 720], [566, 730]],
    { fill: INK.white, line: 5, wobble: 1.5, colourDetail: blueShade(640, 660, 20, 70),
      blackDetail(ctx) { ctx.strokeStyle = INK.black; ctx.lineWidth = 2.2;
        for (const k of [0, 1, 2]) { ctx.beginPath(); ctx.moveTo(596 + k * 34, 656 - k * 26); ctx.quadraticCurveTo(620 + k * 36, 668 - k * 20, 614 + k * 36, 700 - k * 18); ctx.stroke(); } } });
  const palm = [668, 432], hand = 1.4;
  for (const [dx, length, lean] of [[-22, 44, -.25], [-8, 54, -.08], [7, 52, .08], [21, 42, .22]]) {
    const from = [palm[0] + dx * hand, palm[1] - 6 * hand];
    part(capsule(from, [from[0] + Math.sin(lean) * length * hand, from[1] - Math.cos(lean) * length * hand], 15 * hand), { fill: INK.skin, line: 4, wobble: .8 });
  }
  part(capsule([palm[0] - 26 * hand, palm[1] + 8 * hand], [palm[0] - 50 * hand, palm[1] - 16 * hand], 16 * hand), { fill: INK.skin, line: 4, wobble: .8 });
  part(ellipsePoints(palm[0], palm[1] + 4 * hand, 32 * hand, 30 * hand, 18), { fill: INK.skin, line: 4.5, wobble: 1 });
  part([-32, -20, -8, 4, 16, 30].map((dx, i) => [palm[0] + dx * hand, palm[1] + (i % 2 ? 2 : 10) * hand])
        .concat([[palm[0] + 30 * hand, palm[1] + 48 * hand], [palm[0] - 32 * hand, palm[1] + 48 * hand]]),
    { fill: INK.skin, blackFill: true, line: 3, wobble: 1 });
  for (const [a, r] of [[-.35, 104], [-.1, 124], [.15, 104]]) {
    const points = [];
    for (let k = -2; k <= 2; k++) points.push([palm[0] + Math.cos(a + k * .08) * r, palm[1] - 36 + Math.sin(a + k * .08) * r]);
    stroke(points, { width: 7, wobble: 1 });
  }

  // The head, tipped a little to one side.
  tilt(-.1, [400, 560]);

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

  // Hood.
  part([[262, 335], [272, 262], [322, 218], [400, 200], [478, 218], [528, 262], [538, 335], [534, 425], [520, 500], [496, 556], [400, 574], [304, 556], [280, 500], [266, 425]],
    { fill: INK.white, line: 6, blackDetail(ctx) { harlequin(ctx, [250, 190, 550, 590], 46, 0, 8); } });

  // Face.
  part([[300, 372], [318, 332], [360, 314], [400, 310], [440, 314], [482, 332], [500, 372], [505, 428], [494, 484], [466, 530], [428, 553], [400, 557], [372, 553], [334, 530], [306, 484], [295, 428]],
    { fill: INK.skin, line: 5 });
  part([[304, 392], [310, 358], [345, 336], [400, 330], [455, 336], [490, 358], [496, 392], [478, 384], [462, 398], [444, 386], [424, 400], [402, 387], [380, 400], [360, 386], [340, 399], [322, 386]],
    { blackFill: true, line: 3, wobble: 1.5, smooth: false });
  for (let a = -168; a <= -12; a += 13) {
    const r = a * Math.PI / 180;
    part(ellipsePoints(400 + Math.cos(r) * 104, 432 + Math.sin(r) * 112, 12, 12, 10), { fill: INK.white, line: 2.5, wobble: 1 });
  }

  // Clown diamonds under the eyes, short and bright.
  for (const x of [350, 450]) part([[x, 452], [x + 9, 470], [x, 490], [x - 9, 470]], { fill: INK.red, line: 2.5, wobble: .8 });
  // Left eye open wide with two sparkles.
  const left = [350, 428];
  part(ellipsePoints(...left, 28, 26, 18), { fill: INK.white, line: 3, wobble: 1 });
  part(ellipsePoints(left[0] + 2, left[1] + 3, 19, 21, 16), { blackFill: true, line: 0, wobble: .5 });
  part(ellipsePoints(left[0] - 5, left[1] - 5, 6.5, 6.5, 10), { fill: INK.white, line: 0, wobble: .3 });
  part(ellipsePoints(left[0] + 8, left[1] + 11, 3.5, 3.5, 8), { fill: INK.white, line: 0, wobble: .3 });
  stroke([[left[0] - 32, left[1] - 4], [left[0] - 16, left[1] - 26], [left[0] + 8, left[1] - 28], [left[0] + 30, left[1] - 12]], { width: 8, wobble: .8 });
  stroke([[left[0] - 30, left[1] - 10], [left[0] - 44, left[1] - 20]], { width: 5, wobble: .4 });
  stroke([[left[0] - 30, left[1] - 58], [left[0] - 4, left[1] - 70], [left[0] + 24, left[1] - 62]], { width: 5, wobble: .5 });
  // Right eye winking: a happy upward curve with lashes.
  const right = [450, 432];
  stroke([[right[0] - 30, right[1] + 4], [right[0] - 12, right[1] - 12], [right[0] + 12, right[1] - 12], [right[0] + 30, right[1] + 4]], { width: 8, wobble: .8 });
  stroke([[right[0] + 26, right[1] - 2], [right[0] + 42, right[1] - 10]], { width: 5, wobble: .4 });
  stroke([[right[0] + 18, right[1] - 8], [right[0] + 28, right[1] - 22]], { width: 4, wobble: .4 });
  stroke([[right[0] - 26, right[1] - 58], [right[0] + 2, right[1] - 70], [right[0] + 28, right[1] - 60]], { width: 5, wobble: .5 });
  // Round pink cheeks with a red dot screen, a pink button nose.
  for (const x of [332, 468]) part(ellipsePoints(x, 480, 28, 22, 16), { fill: INK.pink, line: 0, wobble: 1,
    colourDetail(ctx) { halftone(ctx, [x - 32, 452, x + 32, 508], 6, 2.1, INK.red, (px, py) => 1 - Math.hypot(px - x, py - 480) / 28); } });
  part(ellipsePoints(400, 468, 8, 7, 10), { fill: INK.pink, line: 2.5, wobble: .5 });
  // A big open smile: dark lips, teeth, and a pink tongue.
  const smile = [[362, 496], [380, 500], [400, 501], [420, 500], [438, 496], [430, 520], [414, 534], [400, 537], [386, 534], [370, 520]];
  part(smile, { fill: INK.plum, line: 4, wobble: .8,
    colourDetail(ctx) {
      ctx.fillStyle = INK.white; ctx.fillRect(360, 494, 80, 11);
      ctx.fillStyle = INK.pink; ctx.beginPath(); ctx.ellipse(402, 533, 20, 13, 0, 0, Math.PI * 2); ctx.fill();
    } });
  stroke([[356, 492], [362, 497]], { width: 3.5, wobble: .3 });
  stroke([[444, 492], [438, 497]], { width: 3.5, wobble: .3 });
  tilt(null);

  // The ruff: a pleated ring around the neck, seen from the front, in two
  // tiers. Each pleat is a rounded lobe with a fold line down its middle.
  const ruff = (cy, rx, ry, lobes) => {
    const points = [];
    for (let i = 0; i < lobes * 8; i++) {
      const a = i / (lobes * 8) * Math.PI * 2, k = .82 + .2 * Math.sqrt(Math.abs(Math.sin(a * lobes / 2)));
      points.push([400 + Math.cos(a) * rx * k, cy + Math.sin(a) * ry * k]);
    }
    part(points, { fill: INK.white, line: 4.5, wobble: 1,
      colourDetail(ctx) { halftone(ctx, [400 - rx, cy - ry, 400 + rx, cy + ry], 7, 2.6, INK.blue, (x, y) => .2 + (y - cy) / ry); },
      blackDetail(ctx) { ctx.strokeStyle = INK.black; ctx.lineWidth = 2;
        for (let i = 0; i < lobes; i++) {
          const a = (i + .5) / lobes * Math.PI * 2 + Math.PI / lobes, c = Math.cos(a), s = Math.sin(a);
          ctx.beginPath(); ctx.moveTo(400 + c * rx * .68, cy + s * ry * .68);
          ctx.quadraticCurveTo(400 + c * rx * .8 + s * 8, cy + s * ry * .8, 400 + c * rx * .9, cy + s * ry * .92); ctx.stroke();
        } } });
  };
  ruff(612, 150, 50, 16);
}};
