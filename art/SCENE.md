# An event's scene

An event picture on the home page is a scene made for that event, with the
people going painted into it. The scene is `scene.js`: plain JavaScript that
defines one constant, and paints with `kit.js` exactly as a drawing does.

```js
const SCENE = {
  width: 1200, height: 400,          // always this size: a wide banner
  // Everything behind the people: sky, room, table, props, lettering.
  background() { part(…); stroke(…); … },
  // Where each of n people stands, for any n from 1 to 12: the centre and
  // diameter of their round portrait, and an optional tilt in radians.
  places(n) { return [{ x: 300, y: 220, size: 150, tilt: -0.05 }, …]; },
  // Optional: anything in front of the people — a table's edge, confetti,
  // a banner across the bottom.
  foreground() { … },
};
```

Each person is printed by the page, not by the scene: their own avatar, a
head-and-shoulders drawing on a round badge, cropped round at their place with
an ink rim and an offset shadow. The scene decides where they go and what is
around them; it never paints their faces.

While `background()` and `foreground()` run, two globals say who is there:
`PEOPLE_COUNT`, how many people, and `PLACES`, what `places(PEOPLE_COUNT)`
returned. Use them to give each person a body, a chair, a drink, a raised arm
— drawn in the background so the portrait sits on top as the head — or to
frame the group.

Rules:

- `places(n)` must return exactly `n` places for every `n` from 1 to 12, all
  inside the picture, portraits no smaller than 90 pixels across, not covering
  one another's faces. Bigger when there are fewer people.
- Paint only with the kit's functions and inks (`part`, `stroke`, `capsule`,
  `ellipsePoints`, `halftone`, `harlequin`, `tilt`, `INK` …). Extra colours go in
  a constant of your own, in the same muted print palette. No fetch, no
  images, no DOM beyond the canvas a `colourDetail` or `blackDetail` callback is
  handed. It runs in a page with no network and no files.
- Short lettering is welcome (the event's name, a date) with `ctx.fillText` in a
  `colourDetail` or `blackDetail`; keep it clear of the people.
