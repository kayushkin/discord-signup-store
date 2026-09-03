# How this is supposed to work

Written to be argued with. `CONTRACT.md` says what the routes and payloads are;
this says what the thing *is*, so that every piece of text shown to a person can
be checked against one description instead of against itself.

## 1. Where the truth is

**The `signups` table in SQLite is the roster. Nothing else is.**

Its history is `signup_updates`; an event's own history of edits is
`event_updates`. Both append-only, both named for what they are an update to.

One row per person per event, carrying their state (`attending`, `waitlisted`,
`withdrawn`), their arrival `position`, and how they got there. Everything a
human ever sees is a projection of those rows.

**Counts are never stored.** "2 of 7" is a `COUNT(*)` executed at the moment
something is rendered. There is no cached count anywhere in this service, so no
count can drift from the roster — only a *copy of a rendered count* can, and
section 3 is about that.

Two numbers are **not** the roster and never feed a decision:

- **Discord's own Interested count.** Discord will not tell us who is on it in a
  way we can act on, cannot cap it, and offers no endpoint to remove anyone.
  It is stored for display and labelled as Discord's.
- **The `Attending` / `Waitlist` Discord roles.** Written when someone's state
  changes, never read back. They exist so channel permissions can key off them.

## 2. Every way onto and off the roster

Seven, and they all go through the same two store methods (`Join`, `Leave`), so
the capacity rule and the waitlist order cannot differ between them.

| # | how | joins | leaves | `joined_via` |
|---|---|---|---|---|
| 2 | **Join / Leave** on a table row | press Join | press Leave | `button` |
| 3 | **Join / Leave** on your My events view | press Join | press Leave | `button` |
| 4 | **✅ on the forum post** | add the reaction | remove it | `reaction` |
| 5 | **Interested on Discord's own event** | press Interested | un-press it | `interested` |
| 6 | **The web page**, by an organiser | Add by user id | Remove | `operator` |
| 7 | **The machine API** | `POST /api/events/{id}/signups` | `DELETE …/{userID}` | `operator` |
| 8 | **Creating the event** | automatic | — | `organiser` |

Rules that hold for all eight:

- **Full means waitlisted, never refused.** You are told your number.
- **Leaving promotes the longest-waiting person**, who is sent a DM. If their
  DMs are shut, they are pinged in the channel instead — the one deliberate ping
  this service makes.
- **Lowering a limit never removes anyone.** Raising it promotes in arrival
  order.
- **Arrival order is ours.** Discord's own list sorts by account age, so a
  waitlist rebuilt from Discord would put the oldest accounts first.

## 3. What updates, when

Two kinds of surface, and the difference is the whole reason anything can ever
look wrong.

**Rendered on demand — cannot go stale.** Built from SQLite at the moment
somebody asks, and thrown away: the web pages, the Details view, My events, and
every private reply to a button press.

**Stored by Discord — as current as the last write.** Discord keeps message
text; there is no live rendering and no callback on view. These are copies:
the two tables, the forum post and its title, and the native event's title and
description. There is no board card: the database holds every row a card would
show, and the table row already shows them. They are kept true by three rules:

1. **One writer per event.** Overlapping changes fold into the run already
   going, which re-reads before each pass, so the last write is the newest
   state.
2. **Write only what changed.** A fingerprint of everything that feeds a copy is
   recorded on a fully successful publish; a pass that matches makes no Discord
   calls.
3. **Sweep and repair, every minute.** Anything that disagrees is rewritten, and
   named in the log. A partial publish is not recorded, so it is retried.

**Reminders** are the exception to everything above: not a copy of anything, and
the only messages this service sends that deliberately ping. One an hour before
an event, one when it starts, naming everyone who has a place, in the reminders channel and nowhere else.
Each is sent once
and stamped on the event row, and one that missed its moment by more than 15
minutes is written off rather than sent — coming back from an outage must not
ping everybody about events that already happened.

On a timer, and nothing else: **finished events archived** every 5 minutes;
**native events imported** every 10 (plus instantly over the gateway);
**reminders checked** every minute. A finished event leaves **one line** in
past-events — the table row folded flat — not a card of dead buttons.

**A recurring event does not finish; it rolls.** It is one row, like Discord's
one scheduled event. When an occurrence ends the date moves to the next one
the rule gives, everyone on the roster is withdrawn and told nothing (the
occurrence they signed up for happened), the reminders are owed again, and the
occurrence that ran leaves its line in past events. Discord slides its own
event forward the same way, and the import treats that as the same rollover.
Every surface says `🔁 weekly` (or every 2 weeks, or monthly) beside the
event; the card adds that signups are for this date. There is no series end,
because Discord has none to give.

## 4. What each surface is for

| surface | who sees it | what it is |
|---|---|---|
| **Forum post** | everyone | The card again, plus discussion, in a channel that lists every event. |
| **Event table** (`#events`) | everyone | Every upcoming event: its thread, its place, the live count and who is going, with Join / Leave / Details. |
| **Management table** (`#event-management`) | organisers | The same events with **Edit**, **Repeat**, **Close/Reopen signups** and **Cancel** on each row, and **Create an event** on the end. Where anything about an event is changed. |
| **Details** | just you | The full roster, by name, read-only, without pinging anyone. |
| **My events** | just you | The events *you* are on, with Join / Leave in place. |
| **Discord's own event** | everyone | Discord's native event, linked to a roster here. Its title carries the count. |
| **Web — list** | logged in | Every event, live from the database. |
| **Web — detail** | logged in | One roster, its history, and the organiser's controls. |
| **Web — form** | organisers | Create and edit, with the fields a Discord form has no room for. |

## 5. Who may do what

| act | who |
|---|---|
| Join, leave | anyone who can see the message |
| Create an event | Administrator, Manage Events, or Create Events |
| **Edit an event** | **Administrator (which the server owner always has), Manage Events, or whoever created it** |
| Add or remove someone else | the same as Edit |
| Rebuild the table | Administrator or Manage Events |

Checked when the button is pressed, not when it is drawn: Discord cannot show a
component to some readers and not others, so an unauthorised press costs one
private no rather than a form that fails after being filled in.

## 6. Things that are true and read as bugs

- **Discord's Interested number will not match the roster**, even though
  pressing Interested joins. Someone can be Interested from before the event was
  linked, or Interested after leaving here. The roster is the answer.
- **The table moves to the bottom of the channel every hour.** That is the price
  of it staying editable.
- **A forum post's title can lag** under fast signups: Discord rate-limits
  thread renames to about two per ten minutes, so the count in it moves at
  most every ten. Becoming Full shows at once; a place opening up waits, on
  purpose — it is usually taken again before the title could say so. The card
  inside stays current.
- **Every rename leaves "Event-Manager changed the title" in the post.** That
  is Discord's system message, and Discord refuses to let anyone delete it
  (measured: `403 code 50021`). The count in the title is worth the line; the
  throttle keeps it to a few.
- **Last week's people are gone from a recurring event.** The roster is per
  date, not per series. Discord's Interested list carries over because it is a
  subscription, not a seat.
