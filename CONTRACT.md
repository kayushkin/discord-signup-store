# discord-signup-store — route table

Listens on `127.0.0.1:8312`. **Three surfaces**, and which nginx vhost reaches which is the security boundary:

- `/interactions` — public on `YOUR_EXISTING_DOMAIN/discord/interactions`. Ed25519-verified.
- `/api/*` — the machine API. **No auth of its own; never proxy it.** Loopback only.
- everything else — the browser surface on `YOUR_DOMAIN`. Every route needs a Discord login and checks guild membership.

Exactly one route is published to the internet (through nginx, as
`https://YOUR_EXISTING_DOMAIN/discord/interactions`). Everything else is loopback
only and has **no auth of its own** — that is the deployment's job, not this
service's, and proxying any other route publishes roster editing to the world.

## Public

| Method | Path | Purpose |
|---|---|---|
| POST | `/interactions` | Discord's Interactions Endpoint URL. Ed25519-verified; a bad or missing signature is **401**, which is what Discord requires before it will save the URL. Answers PING with PONG, button clicks with an ephemeral reply, and the **Limit** button with a modal. |

## Machine API (loopback only — never proxied)

| Method | Path | Purpose |
|---|---|---|
| GET | `/healthz` | Liveness plus whether the Discord client and verifier are wired. |
| GET | `/api/events?guild_id=&status=&limit=` | List rosters, newest first, with live counts. |
| POST | `/api/events` | Create a roster. `name`, `guild_id`, `channel_id` required. |
| GET | `/api/events/{id}` | One roster with `attending_count` and `waitlist_count`. |
| PATCH | `/api/events/{id}` | Partial update. Every field is optional; omitting one leaves it alone. |
| DELETE | `/api/events/{id}` | Soft delete. Prefer `status: "cancelled"` if the record should stay visible. |
| GET | `/api/events/{id}/signups?include_withdrawn=` | The roster: attending first, then the waitlist in promotion order. |
| POST | `/api/events/{id}/signups` | Add someone by id. Goes through the same cap and waitlist as a click. |
| DELETE | `/api/events/{id}/signups/{userID}?actor=` | Remove someone. Promotes the next in line, same as a click. |
| GET | `/api/events/{id}/history?limit=` | The append-only transition log. |
| POST | `/api/sync` | Pull native events from **every** server the bot is in and post a card for any new one. What the scheduler job calls; names no guild, so adding a server needs no change. |
| POST | `/api/guilds/{guildID}/sync` | The same, for one guild. |
| POST | `/api/events/complete-finished` | Archive events whose time has passed and strip the buttons off their cards. Also runs on a five-minute ticker. |

## Browser surface (YOUR_DOMAIN — Discord login required)

| Method | Path | Purpose |
|---|---|---|
| GET | `/` | Events in servers you belong to. |
| GET | `/login` · `/auth/callback` · POST `/logout` | Discord OAuth2, scopes `identify guilds`. |
| GET · POST | `/events/new` | Create, with a real date picker and an IANA timezone. |
| GET | `/events/{id}` | Roster, history, and both counts labelled. |
| GET | `/events/{id}/edit` · POST `/events/{id}` | Edit. |
| POST | `/events/{id}/roster/remove` · `/roster/add` | Manage the roster; removal promotes the next in line. |
| POST | `/events/{id}/publish` | Create a native Discord event linked to this roster. |
| POST | `/sync` | Pull Discord events for every server you manage. |

**Authorization.** Reading an event needs guild membership. Editing needs `MANAGE_EVENTS` (or `ADMINISTRATOR`) in that guild, or having created the event — matched on `created_by`, the Discord user id, never on a name.

## Status codes

| Code | Meaning |
|---|---|
| 400 | Malformed JSON, a non-integer id, or a field outside its vocabulary. The body names the accepted values. |
| 401 | `/interactions` only: the signature did not verify. |
| 404 | No such event, or no such signup on it. |
| 409 | The event is not open for signups. |
| 500 | Anything unmapped. The real error is in the body **and** the log — never a 200 with an empty body. |

## Vocabularies

Closed sets, defined in `vocabulary.go` and validated on write.

- **signup state** — `attending`, `waitlisted`, `withdrawn`. Un-marking
  Interested on Discord removes someone **however they joined**, matching Leave.
- **event status** — `open`, `closed`, `completed`, `cancelled`. When an event
  becomes `completed`, its card is reposted to `DISCORD_PAST_CHANNEL_ID` and the
  original deleted; `message_id` and `channel_id` follow it, because they mean
  "where this event's card is". `closed` means signups are shut on an event that has **not happened yet**; `completed` means it has. The web page collapses `completed` and `cancelled` into the archive and leaves `closed` in the live list.
- **transition action** — `joined`, `waitlisted`, `withdrew`, `promoted`, `rejoined`
- **actor** — `user` (they pressed the button), `promotion` (this service moved
  them), or an operator name from the API's `?actor=`

## Buttons on the signup card

| Button | custom_id | Who |
|---|---|---|
| Join | `signup:join:{id}` | anyone |
| Leave | `signup:leave:{id}` | anyone |
| Edit | `signup:edit:{id}` | `MANAGE_EVENTS`, `ADMINISTRATOR`, or the event's creator |
| Create an event | `signup:create:0` | `CREATE_EVENTS`, `MANAGE_EVENTS` or `ADMINISTRATOR` |

**Edit** and **Create an event** open the same five-field modal — Name, Starts,
Max attendees, Location, Description — prefilled when editing and empty when
creating. There is no end-time field, and `ApplyEventForm` deliberately omits
`EndsAt` from its patch: sending zero for a field the form never collected is
how an end time set on the web page would get silently wiped. Five is
Discord's hard ceiling on a modal, so description, recurrence and roles live on
the web page instead.

Times are read in the zone set by `DISCORD_DEFAULT_TIMEZONE`, which the form's
own label prints, and one parser reads them for both the modal and the web page
— `ParseEventTime`. It accepts a date, a time, or both, in any of:

    date   9/29 · 09/29 · 9/29/26 · 9/29/2026 · 2026-09-29 · sep 29 · 29 sep
           sept 29th · today · tomorrow · friday · fri
    time   5pm · 5 pm · 5:30pm · 5:30 · 14:30 · 1730 · 1330pm · noon · midnight

A date with no time is midnight. A time with no date is the next day that time
comes round. A date with no year is the next year it comes round, compared by
**day** and not by instant, so `9/2` typed on 2 September is today.

⚠️ **A time with no am/pm and an hour of 1–11 is read as PM.** This field only
schedules events and 5:30am is not one. The rule does not turn on a leading
zero — `09:00` is 21:00, exactly as `9:00` is — because two nearly identical
strings landing twelve hours apart is worse than one rule that is always
stated. Consequently `FormatEventTime` **always writes an explicit am/pm**: a
form prefilled with `09:00` would read back as 21:00, so opening an event and
saving it unchanged would move it. That pairing is the contract between the
parser and the renderer, and `TestEveryRenderedTimeSurvivesBeingReadBack` holds
it.

Whoever fills in the form is **put on their own roster** as they create it,
`joined_via = organiser` — they are going to their own event, and an organiser
who forgets to press Join leaves a card reading "0 places taken" under an event
they are running. It applies to the Discord form and the web form, and
deliberately not to an event imported from a native Discord one (its creator
made a Discord event, not a roster) nor to `POST /api/events` (`created_by`
there is whatever a script sent, and a script is not going).

`signup:capacity:{id}` is still accepted, because cards posted before the button
was widened are sitting in channels and their button must keep working.

Permission is checked on the press *and* on the submit, because they are
separate requests and nothing stops the second arriving alone.

Raising a limit promotes people off the waitlist in arrival order and messages
each of them. Lowering it promotes nobody and removes nobody.

That holds on **every** surface — the Discord form, the web page and
`PATCH /api/events/{id}` — because all three run the same edit path. That path
also republishes every copy of the event afterwards: the signup card, the table
row, the forum post, and the count in the native scheduled event's title.

## The consolidated table

**As many events per message as fit.** **Join · Leave · Details · Edit**, wrapped in a Components V2 container. Past
five it spills onto another message.

The line is exactly `{slots} · {title} · {location} · {time}`, in **one** text
component, with a divider between events. No heading, no event count, no note
about sorting — the rows already say all of it.

The time is `8/29 4pm`, formatted in the **event's own zone** rather than
Discord's `<t:…>` markup. That markup localises per reader — the right default
everywhere else — but expands to "August 29, 2026 4:00 PM" with no short form
on offer, and a table row is for glancing. Details still carries the localised
time. An uncapped event shows **no count at all**: a bare number next to
nothing read as nothing.

Five per page now that dividers cost a component each: container + 5×6 + 4
separators = 35 of the measured 40-component budget; six would need 42. The five-action-row limit that caps
an ordinary message does **not** apply under Components V2 — the total budget
replaces it, which is what makes six rows of buttons in one message possible.

Everything about one event is **one** text block. Splitting it into title,
description and time would cost three components each and cut the page from six
events to two — identical to read, a third as much held.

**The table sorts itself.** Every page is rewritten in place on each change, so
events move *between* pages while the messages stay where they are. A page is
only ever posted when the table grows past its current page count, and a new
message belongs at the bottom anyway. `signup:table-rebuild:0` is now a repair
tool for when the recorded messages and the channel disagree, not the way to
sort.

A closed event keeps Details and Edit and loses Join and Leave: a button that
cannot act is a trap, not an affordance.

## The management table

`PUT /api/guilds/{guildID}/management {"channel_id"}` points the management
table at a channel and draws it. It is the event table drawn for organisers:
the same events, the same packing, with **Edit**, **Close signups** / **Reopen
signups** and **Cancel** on each row instead of Join, Leave and Details, and
**Create an event** on the last page. Cancel opens a confirm that asks for the
event's name typed back — it deletes the native Discord event and cannot be
undone, and that is the one gesture Discord offers that cannot happen by
accident. Close is reversible with the same button and is not cancel: the event
still happens, everyone on it stays, nobody new can join.
It hangs off the event table — `management_channel_id` on `guild_tables` — so
the call is a 404 until `PUT .../table` has been made. One trigger redraws both
tables, so they cannot show different rosters.

A row on either table is the thread link with the location beside it, then the
live count and who is going, then whoever is waiting:

    <#thread>  📍  in my butt
    2/10 👥 Twili Midna, Slava
    ⏳ Dan

**Details is read-only** — the roster, as a text input that is not required and
says so, because a modal cannot show read-only text any other way. Editing is
the management table's job.

## The personal dashboard

`signup:my-events:0`, on the **standing how-to message**, opens an **ephemeral**
Components V2 view — the one surface that is genuinely per-viewer, because a
channel message renders identically for everyone and Discord fires no event when
someone opens a channel.

It used to sit on the table's last page, which put a control about the reader on
a message about everybody. The table is about events now, and only events. ⚠️
This paragraph previously claimed the button was on the table's last page *and*
the standing message; it was only ever on the table, and the standing message
carried nothing but **Create an event**. The button is the dashboard's only
entry point, so moving it had to mean moving it somewhere — deleting it would
have stranded the whole feature behind nothing.

Each row conditions its buttons on the viewer: **Join** or **Leave** depending
on whether they are in the event, their own state written on the row ("going",
"waitlist #2"), and **Edit** only with `MANAGE_EVENTS`/`ADMINISTRATOR` or for
the event's creator. `signup:dash-join:{id}` and `signup:dash-leave:{id}`
answer with callback type 7 (**UPDATE_MESSAGE**), re-rendering the dashboard in
place so the row flips under the cursor.

Capped at six events: a row costs up to five components plus a separator, so
six is 37 of the 40 budget — seven measured out at 43.

## Discussion threads

Every open event's signup card grows a public thread, named after the event and
seeded with one line saying what it is for. Created from `RefreshSignupMessage`
— the choke point every maintained card passes through — so events that predate
the feature grow a thread on their next activity, with no backfill pass to
write or forget. Idempotent via `thread_id`, which is stored even though Discord
gives a message thread the same id as its parent: a reposted card gets a new
message id while the old thread lives on.

When the event completes, the sweep archives the thread (not locked — a
late "how did it go?" reopens it, and locking would turn that into a
permission error).

## The forum surface

`PUT /api/guilds/{guildID}/forum {"channel_id"}` adopts a forum channel: the
managed tags (`open`, `full`, `finished`, `cancelled` — the last two moderated,
since they state facts this service asserts) are created if missing, matched by
name once, and joined by id ever after. Every live event then gets a post.

A forum post is a thread whose required first message **shares the thread's
id**. That message is `RenderSignupMessage` verbatim — the same card and the
same button custom_ids as the board, so one handler serves both surfaces. The
post's title carries the count at the end (`Games — 8/29 4pm [3/8]`), renamed
at most every five minutes, or `[Full]` at the front the moment it fills — and
its tag
flips with the roster; the sweep tags `finished` and archives.

⚠️ Discord rate-limits **thread renames** to roughly two per ten minutes per
thread — far harder than message edits. Under signup churn the title badge may
lag; the card inside stays current.

⚠️ `429 code 30046` — "Maximum number of edits to messages older than 1 hour
reached" — exists, and **is not what its name suggests**. Measured 2026-09-02
against a message 196 hours old: **120 consecutive edits, one every ten seconds,
over twenty minutes, zero failures.** Discord documents none of this; the
clarification request (discord/discord-api-docs#4413) has been unanswered since
2022, and every third-party write-up repeats "a handful of edits per hour",
which is wrong by roughly two orders of magnitude.

Both times this service hit it, the write was part of a burst that rewrote the
table and several cards together — so the budget is far likelier to be
per-channel or shared than per-message-age. An hourly delete-and-repost of the
table was tried as a workaround and **removed**: it moved the table to the
bottom of the channel every hour to dodge something age-based, and age is
demonstrably not the limit. What actually reduces the pressure is
`published_signature`, which skips writes that would change nothing.

## The ✅ reaction

React ✅ on a forum post to join; remove it to leave. The forum's
`default_reaction_emoji` makes ✅ a click target on the **list view**, so
joining needs neither opening the post nor scrolling past its discussion.

The "only this emoji" restriction is permissions, not filtering: the channel
denies `ADD_REACTIONS` to `@everyone`, and a denied user can still click a
reaction that already exists — so the bot seeds ✅ on every post and that seed
is the only reaction there is. The bot's own role carries a counter-allow,
because a channel deny on `@everyone` binds the bot too (measured: the seeding
403'd until the allow was added).

Removing ✅ leaves **however the person joined**, matching the Interested rule.
Leaving by any other door clears the person's ✅ (needs `MANAGE_MESSAGES`), so
the reaction stays truthful; the bot's own removals echo back through the
gateway, land on an already-withdrawn row, and no-op — that is what breaks the
loop. A reaction carries no interaction token, so a waitlisted clicker is told
by DM, with the card as fallback.

## Native-event reconciliation

The sync's reconcile pass keeps this store and Discord's event list from
drifting, in both directions, within one sync interval:

| Local | Native | Action |
|---|---|---|
| live, unlinked, starts in the future | — | publish |
| live | **gone** (direct GET 404) | cancel locally, everywhere |
| live | CANCELED | cancel locally |
| live | COMPLETED | left to the time sweep, which owns completion |
| cancelled | still listed | delete the native event |

Absence from the LIST endpoint proves nothing — completed events drop out of it
too — so a missing id is checked with a direct GET, where a 404 is unambiguous.
The gateway's GUILD_SCHEDULED_EVENT_DELETE handler does the same cancellation
instantly; the poll is the backstop for socket downtime. Deleting a native
event is how a person cancels in Discord's own UI, and it now means that here
too.

## The native event's title, and the forum post's

Both carry the count — at the **end**, as `[3/8]` — and both are **renames**,
which Discord rate-limits to about two per ten minutes per thread. So the count
is renamed at most **every five minutes**, and the live number lives on the card
and the table row meanwhile, which are message edits and have no such limit.

Two changes rename at once, because they are rare and they are what a reader
most needs: becoming **Full**, which replaces the count with `[Full]` at the
**front**, and ceasing to be Full, which puts the count back. A rename by the
organiser or a moved date is also immediate. That spends the budget as one
scheduled count and one flip.

    Board game night — 8/29 4pm [3/8]           capped, room left
    [Full] Board game night — 8/29 4pm          capped, no room
    Open house — 8/29 4pm                       no limit

`title_written_at`, `native_title_written` and `forum_title_written` on the
event row are what makes the decision possible; `titleRenameDue` is the whole
of it. A due-but-throttled rename is picked up by the minute sweep, which wakes
an event for that alone even when nothing else about it has changed.

Every form this service has ever written — `[3/8]` at the front, `· 8 places` at
the end — is still stripped on import, or a name read back would round-trip and
grow a decoration per publish.

## Conventions

- **Capacity `0` means unlimited**, matching Discord's own convention for
  channel `user_limit` and invite `max_uses`. It does not mean "unset".
- **Snowflakes are text**, never JSON numbers — a 64-bit id does not survive a
  round trip through a double.
- **`discord_scheduled_event_id` is a link, not a source.** Discord's Interested
  count will disagree with this roster and there is no way to make it agree.
