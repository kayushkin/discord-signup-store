# A character

A person's avatar is a character: the whole person, drawn part by part on the
kit's skeleton, so the same drawing can stand in their round portrait, cheer in
a crowd, sit at a table or hold a torch in a haunted house. `character.js` is
plain JavaScript that defines one constant and paints with `kit.js`, exactly as
a drawing does. `example-character.js` is a complete one to copy the shape of.

```js
const CHARACTER = {
  // Lengths in the character's own units. A grown-up is about 1070 tall;
  // a child has a bigger head for their height, shorter limbs.
  proportions: { head: 240, neck: 40, torso: 300, shoulderWidth: 188, shoulderDrop: 34,
                 hipWidth: 140, upperArm: 170, forearm: 150, hand: 70, thigh: 230, shin: 220, footHeight: 40 },
  badge() { … },          // the portrait's round backdrop, in an 800 by 800 square
  portraitPose: { … },    // optional: how they stand in the portrait
  draw: {                 // one function per bone, each drawing in that bone's frame
    head({ expression, look }) { … }, neck() { … }, torso() { … }, pelvis() { … },
    upperArmL() { … }, forearmL() { … }, handL({ open, hold }) { … },
    upperArmR, forearmR, handR, thighL, shinL, footL, thighR, shinR, footR,
  },
};
```

## The frames

Every part is drawn in its own bone's frame, whose origin is the joint with the
bone it hangs from. The kit turns each frame for a pose, so draw every part as
if the person stood straight, facing the viewer, arms hanging at their sides.

- **torso**: origin between the hips; the shoulders are at `y = -torso`.
  Clothes on the body, from hips to shoulders.
- **pelvis**: origin between the hips; the waist and seat, a little below.
- **neck**: origin at the top of the torso, running up to `y = -neck`.
- **head**: origin where the chin meets the neck, running up to `y = -head`,
  centred on `x = 0`. Hair, hat, face, ears.
- **upperArmL, forearmL, handL** and **thighL, shinL, footL**: each runs down
  from its joint along `+y` for its length. L is the limb on the viewer's left.
  A foot's sole is at `y = footHeight`; the left foot points to the viewer's
  left.
- The right side: draw it as its own functions when the clothes differ side to
  side (a harlequin costume), or reuse the left with `mirrored(upperArmL)`.

Make each part run a little past its joints, rounded, so the joints stay
covered when a limb turns: an upper arm's top rounds over the shoulder, a shin
starts a little above the knee.

## What the kit asks of it

- **head** gets `{ expression, look }`. Draw every expression: `smile`,
  `grin`, `laugh`, `surprised`, `scared`, `wink`, `calm`. `look` is `ahead`,
  `left` or `right`: move the pupils a little.
- **handL** and **handR** get `{ open, hold }`: an open hand with fingers
  spread, a hand closed round something held, or a loose relaxed hand.
- **badge** paints the round backdrop of their portrait, in an 800 by 800
  square: a disc of radius about 390 at the middle, flat colour and something
  of where their photo was taken. The kit then paints them standing, head and
  shoulders filling it.

Paint only with the kit's functions and inks (`part`, `stroke`, `capsule`,
`ellipsePoints`, `halftone`, `harlequin`, `mirrored` …); extra colours go in
constants of your own in the same muted print palette. No fetch, no images, no
DOM beyond the canvas a `colourDetail` or `blackDetail` callback is handed.

## Poses, for reference

A scene poses characters with the kit's `POSES` — `stand`, `wave`, `cheer`,
`point`, `sit`, `walk`, `dance`, `hold`, `scared`, `shrug`, `jump` — or its own
angles. Your character must look right in all of them: `render-character.mjs`
prints a sheet of eight.
