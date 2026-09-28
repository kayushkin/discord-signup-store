# An event's scene

An event picture on the home page is a scene made for that event, with the
people going in it, doing what the event is about. The scene is `scene.js`:
plain JavaScript that defines one constant, and paints with `kit.js` exactly
as a drawing does.

```js
const SCENE = {
  width: 1200, height: 400,          // always this size: a wide banner
  // Everything behind the people: sky, room, furniture, props, lettering.
  background() { part(…); stroke(…); … },
  // Who stands where, doing what, for any n from 1 to 12.
  cast(n) {
    return [
      { x: 300, y: 370, height: 260, pose: { ...POSES.sit, expression: 'laugh' } },
      { x: 520, y: 370, height: 260, pose: { ...POSES.hold, look: 'left' }, mirror: true },
      …
    ];
  },
  // Optional: anything in front of the people — a table's edge, a prop in a
  // hand, confetti, a banner across the bottom.
  foreground() { … },
  seconds: 2,                         // optional: how long the loop is, 1 to 4
};
```

## Motion

The picture is a short loop, printed at 12 frames a second. `cast(n, t)`,
`background(t)` and `foreground(t)` are called for every frame with `t`, the
moment in the loop, from 0 up to 1 — and 1 is 0 again, so the loop must join
up. `loop(t, turns, offset)` is `sin(2π(turns·t + offset))`: anything moved by
whole turns of it joins up. `T` holds `t` too.

Make the scene move the way the event does: a flashlight sweeping, a ghost
bobbing, candles flickering, someone waving, dancers stepping, a ball flying
between two players, steam off a mug. Move people by changing their pose with
`t` in `cast` — `addToPose(pose, 'upperArmR', 25 * loop(t))` — or their `x` and
`y`. Keep it gentle and readable: one or two things moving clearly, the rest
still. Props held follow the hands by themselves, since `JOINTS` is worked out
again for every frame.

Every person also sways a little on their own — head, torso, arms — out of
step with the others. Cast someone `idle: false` to stop that, for a pose that
must hold exactly.

Each person is a **character** (`CHARACTER.md`): a whole body the page paints
for you, posed. For each one `cast(n)` gives:

- `x`, `y`: where their feet stand. The kit puts their lowest foot on `y`,
  whatever the pose — a seated person's legs fold and they come down with them.
- `height`: how tall they would stand, in pixels. Nearer people taller; at
  least 150 so faces read.
- `pose`: one of the kit's `POSES` — `stand`, `wave`, `cheer`, `point`, `sit`,
  `walk`, `dance`, `hold`, `scared`, `shrug`, `jump` — changed as you like, or
  your own. A pose turns bones at their joints, in degrees from standing with
  arms at the sides: `upperArmL`, `forearmL`, `handL`, `upperArmR`, `forearmR`,
  `handR`, `thighL`, `shinL`, `footL`, `thighR`, `shinR`, `footR`, `torso`,
  `neck`, `head`. A positive angle swings a limb outward, away from the body's
  middle; a negative one swings it inward, across the body; the same numbers
  work for either side. An upper arm at 90 is straight out to the side, at 170
  straight up. A forearm's angle is from its upper arm: -85 bends the elbow so
  the forearm crosses in front of the body, holding something; 85 bends it
  outward and up.
  A bone's value may be `{ angle, stretch }`, stretch below 1 shortening it as
  it points toward the viewer (a seated thigh is 0.3). Also: `expression`
  (`smile`, `grin`, `laugh`, `surprised`, `scared`, `wink`, `calm`), `look`
  (`ahead`, `left`, `right`), `open` and `hold` for the hands, `lift` to raise
  them off the ground (0.1 is a jump), `behind: ['armL']` to put an arm behind
  the body.
- `mirror`: flip them left to right, to face or turn toward someone.

People are painted after `background()`, back to front by where their feet
are: whoever stands higher up the picture is further away and is painted
first, overlapped by those nearer. While `background()` and `foreground()` run,
`CAST` is what `cast(PEOPLE_COUNT)` returned and `JOINTS[i]` is where person
`i`'s joints landed: `head`, `top`, `pelvis`, `handL`, `handR`, `footL`,
`footR` (each `[x, y]`), and `scale`, pixels per character unit. Draw a torch in
`JOINTS[i].handR`, a chair under `JOINTS[i].pelvis`, a hat on `JOINTS[i].top`.

`PEOPLE[i].avatar` says whether person `i` has an avatar of their own. The
people with avatars come first, in the order they signed up; the rest are
**stand-ins**: faceless background characters from the kit, one for each
person going without an avatar, so the picture has as many people as are
going, up to 12. Give the people with avatars the leading places and actions,
nearer and bigger; put the stand-ins behind them, smaller, filling out the
crowd. A stand-in poses like anyone else.

A person whose avatar is an older portrait, not a character, is painted as
their round portrait where their head would be, the same size as a head; the
scene need do nothing different for them.

Rules:

- `cast(n)` must return exactly `n` people for every `n` from 1 to 12, all
  inside the picture, faces not covered by one another or by the foreground.
- **People stay where they are as the crowd grows.** `cast(n + 1)` gives the
  first `n` people the same places and heights `cast(n)` gave them, and only
  adds one: someone joining the event adds a person to the picture and moves
  nobody. Plan every place from the start — who stands where when there are
  12 — and have `cast(n)` take the first `n` of them. The painter checks this.
- Paint only with the kit's functions and inks (`part`, `stroke`, `capsule`,
  `ellipsePoints`, `halftone`, `harlequin`, `tilt`, `pushTransform`,
  `translateBy`, `rotateBy`, `scaleBy`, `popTransform`, `INK` …). Extra colours
  go in a constant of your own, in the same muted print palette. No fetch, no
  images, no DOM beyond the canvas a `colourDetail` or `blackDetail` callback
  is handed. It runs in a page with no network and no files.
- Short lettering is welcome (the event's name, a date) with `ctx.fillText` in
  a `colourDetail` or `blackDetail`; keep it clear of the people.
