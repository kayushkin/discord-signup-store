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
| POST | `/api/events/{id}/message` | Post the signup message with its buttons and record its id. |
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
| POST | `/events/{id}/post-message` | Post or repost the signup card. |
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

- **signup state** — `attending`, `waitlisted`, `withdrawn`
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
the web page instead. Times are typed as `2026-09-05 19:00` and read in the zone
set by `DISCORD_DEFAULT_TIMEZONE`, which the form's own label prints.

`signup:capacity:{id}` is still accepted, because cards posted before the button
was widened are sitting in channels and their button must keep working.

Permission is checked on the press *and* on the submit, because they are
separate requests and nothing stops the second arriving alone.

Raising a limit promotes people off the waitlist in arrival order and messages
each of them. Lowering it promotes nobody and removes nobody.

## The consolidated table

**One Discord message per event**, each a single compact line with that event's
buttons, plus a header message above them.

That shape is forced, not chosen. A message carries **five action rows** and a
button row holds **five buttons**, so the whole table in one message tops out
around twelve events and leaves the buttons in a block that lines up with
nothing. Split across messages, every row gets its own five-button row, the
buttons sit beside the event they act on, and there is no ceiling on how many
events fit.

The buttons are the **same custom_ids the full card uses** — a row and a card
are two views of one event, and separate ids would mean two handlers to keep in
step. Only `signup:details:{id}` is new. It opens a **modal** rather than posting an
ephemeral message, so reading an event is a dismissible overlay that leaves the
channel alone.

Every component in it is a **Text Display** (type 10), which Discord allows in
modals and which is genuinely read-only. An earlier version prefilled ordinary
text inputs, because there is no read-only text input — boxes that looked
editable and were not. There are now no inputs at all.

Three blocks, in the order the questions get asked: **description**, **going**,
**waitlist**. The roster is listed by display name, not `<@id>` — a modal does
not resolve a mention, so one shows as a raw snowflake.

A closed event keeps Details and Edit and loses Join and Leave.

| | |
|---|---|
| A signup | rewrites that one row, in place, so it keeps its position |
| An event finishing | deletes its row and updates the header |
| `signup:table-rebuild:0` | deletes every message and reposts in date order |

**Discord will not reorder messages.** Rows sit in the order they were posted,
so a new event that starts sooner than existing ones lands at the bottom.
Rebuild is the only way to sort, which is why it is a button somebody presses
rather than something that happens on its own — it deletes and reposts
everything, and answers before it starts because it will not finish inside
Discord's three-second interaction window.

## Conventions

- **Capacity `0` means unlimited**, matching Discord's own convention for
  channel `user_limit` and invite `max_uses`. It does not mean "unset".
- **Snowflakes are text**, never JSON numbers — a 64-bit id does not survive a
  round trip through a double.
- **`discord_scheduled_event_id` is a link, not a source.** Discord's Interested
  count will disagree with this roster and there is no way to make it agree.
