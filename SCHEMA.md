# Every table, every column

SQLite at `~/.config/discord-signup-store/discord-signup-store.db` (WAL, foreign
keys on). Eleven tables of ours, plus SQLite's own `sqlite_sequence`. Taken from the live database on 2026-09-02 and checked
against a database built fresh from `schema.sql` plus the migrations — every
table's column *set* matches.

**Times** are Unix seconds, integers, UTC. **Ids from Discord** — guilds,
channels, messages, users, roles, tags — are TEXT, never numbers: a snowflake is
64-bit and does not survive a JSON double. **Absent means empty string or zero,
never NULL.** Every column in every table is `NOT NULL` with a default, so there
is one way to say "not set" and no code has to handle two.

⚠️ **`events` columns are in a different physical order on different hosts.**
Columns added by migration land in the order the migrations first ran, so a
database upgraded over months and one created today hold the same columns in
different positions. Every read in the code names its columns — there is no
`SELECT *` anywhere — and adding one would break on exactly one machine.

---

## `events` — an event, and every id needed to find its copies on Discord

The row everything else hangs off. 32 columns, of which about a third are not
the event at all but the addresses of messages this service has written about it.

| column | type | description |
|---|---|---|
| `id` | INTEGER PK | Ours, autoincrement. What every other table joins on. |
| `guild_id` | TEXT | The Discord server. Never blank on a real row. |
| `channel_id` | TEXT | Where the signup card lives. Repointed when a finished card is moved to the past-events channel, because it means "where the card is", not "where it started". |
| `message_id` | TEXT | The signup card itself. `''` until posted. This is what a button press resolves through. |
| `discord_scheduled_event_id` | TEXT | The linked native Discord event, if there is one. A **link, not a source** — see `origin`. |
| `name` | TEXT | What the event is called. Discord caps it at 100 characters. |
| `description` | TEXT | What somebody wrote. Stored stripped of anything this service appends, so it round-trips through Discord unchanged. |
| `capacity` | INTEGER | How many places. **`0` means unlimited**, matching Discord's own convention for `user_limit` and `max_uses`. It does not mean "unset". |
| `status` | TEXT | `open`, `closed`, `completed` or `cancelled`. `closed` still happens; signups are shut. |
| `attending_role_id` | TEXT | Role granted to whoever has a place. Optional. Written, never read back — a projection for channel permissions. |
| `waitlist_role_id` | TEXT | The same for the waitlist. |
| `starts_at` | INTEGER | When it starts. Required: `CreateEvent` refuses `0` and a PATCH cannot clear it, so every event can answer whether it is over. |
| `ends_at` | INTEGER | When it ends. `0` means unset, and a run time is assumed — by one constant, so the archive sweep and the publisher cannot assume different lengths. |
| `location` | TEXT | Free text. Sent to Discord with a placeholder when empty, because Discord refuses an EXTERNAL event without one; the placeholder is stripped on the way back. |
| `entity_type` | TEXT | Discord's kind of event: `stage`, `voice` or `external`. |
| `recurrence_rule` | TEXT | RFC 5545 RRULE. Encoded into Discord's `recurrence_rule` object on every publish since 2026-09-03 — before that it was stored and never sent. `''` means the event does not repeat, and sends `null`. A row with a rule is never completed by the sweep: when its occurrence ends, `starts_at`/`ends_at` move to the next date, the roster is withdrawn and the reminder stamps clear (`RollOverOccurrence`). |
| `timezone` | TEXT | IANA zone name, never an offset — an offset cannot survive a daylight-saving change. Mandatory whenever `recurrence_rule` is set. |
| `origin` | TEXT | `local` (made here) or `discord` (imported). **Not** derivable from `discord_scheduled_event_id`: a local event published to Discord also has one. This records where it came from, not who owns it: this service's row is the source of truth for both kinds. |
| `discord_interested_count` | INTEGER | Discord's own Interested tally. Stored for display, labelled as Discord's, and **never** feeds a capacity decision. |
| `discord_synced_at` | INTEGER | When the native event was last read. |
| `created_by` | TEXT | Discord user id of whoever made it. Grants edit rights, and gets them a place on their own roster. |
| `native_name_written`, `native_description_written`, `native_starts_at_written`, `native_ends_at_written`, `native_location_written` | TEXT / INTEGER | What this service last wrote into the native event, in its own terms (name without the count, description without the roster, end as sent). Each is set only when that field was sent. The sync compares Discord's copy with these to tell an edit made in Discord's event screen from a change of ours Discord has not taken yet. Bookkeeping, not logged. |
| `native_written_at` | INTEGER | When the above were last recorded; `0` means never, and the sync then treats a difference as ours not yet sent. |
| `waitlist_disabled` | INTEGER | `1` when a full event refuses a Join instead of waitlisting it; set on the web page. `0`, the default, is the ordinary waitlist. Turning it on removes nobody already waiting. Logged in `event_updates`. |
| `new_events_message_id` | TEXT | The event's message in its guild's `#new-events`; `''` when it has none. Cleared when the message is deleted — as the event's line goes to past events, or on cancelling. Bookkeeping. |
| `thread_id` | TEXT | A discussion thread from when cards existed; nothing writes it now. Old ones are still archived when their event finishes. |
| `forum_post_id` | TEXT | The event's post in the forum channel. One id reaches both the post and the card inside it. |
| `published_signature` | TEXT | Fingerprint of everything that feeds a Discord copy, written only when a publish fully succeeds. `''` means never published, or the last publish failed part way. The minute sweep republishes anything that does not match. |
| `reminded_before_at` | INTEGER | When the hour-before reminder went out, **or** when it was written off as too late. `0` means still owed. |
| `reminded_start_at` | INTEGER | The same for the starting reminder. |
| `title_written_at` | INTEGER | When the titles were last renamed. A title is a rename, rate-limited to about two per ten minutes, so anything short of becoming Full or an organiser's rename waits ten; this is how the publisher knows whether one is due. |
| `native_title_written` | TEXT | What the native event's name last said, e.g. `Games [3/8]` or `[Full] Games`. Compared to what it *should* say to spot a Full flip or an organiser's rename, both of which go at once. |
| `forum_title_written` | TEXT | The same for the forum post's title. |
| `created_at` / `updated_at` | INTEGER | `updated_at` deliberately does **not** move when a publish signature or a reminder stamp is written — those are the service noting what it did, not somebody editing the event. |
| `deleted_at` | INTEGER | Soft delete. `0` is live. |

**Indexes.** `(guild_id, deleted_at)` and `(status, deleted_at)` for listing;
`(message_id)` to resolve a button press. Plus a **partial unique index** on
`discord_scheduled_event_id` where it is non-empty and the row is live — one
roster per native event, and partial because `''` is the common case and a plain
UNIQUE would allow only one of those.

---

## `signups` — **the roster. This table is the truth.**

One row per person per event. Everything a human ever sees is a projection of
these rows, and the counts printed everywhere are a `COUNT(*)` over them
computed at read time and never stored.

| column | type | description |
|---|---|---|
| `id` | INTEGER PK | |
| `event_id` | INTEGER | → `events(id)`, **ON DELETE CASCADE**. |
| `discord_user_id` | TEXT | The person. The only thing joined on. |
| `display_name` | TEXT | Carried for display only, never read back to find a row. Names collide; ids do not. |
| `state` | TEXT | `attending`, `waitlisted`, `maybe` or `withdrawn`. `maybe` holds no place and no spot in line. A withdrawn row is kept, not deleted, so rejoining is distinguishable from never having left. |
| `signed_up_at` | INTEGER | Arrival, and **half the ordering key**: the roster is `ORDER BY signed_up_at, id`. Reset on a rejoin, which is what sends a rejoiner to the back. |
| `state_changed_at` | INTEGER | Last move between states. |
| `joined_via` | TEXT | How they got on: `button`, `interested`, `reaction`, `operator`, `organiser`, `regular` or `web` (the home page's Join). Not cosmetic — it is what makes an un-marked Interested readable as leaving rather than as noise. |
| `waitlist_rank` | INTEGER | A place in the waitlist an organiser set, 1 at the front; `0` means none. A move ranks the whole line, so everyone with a rank arrived before everyone without one, and the line is `(waitlist_rank = 0), waitlist_rank, signed_up_at, id` — `waitlistOrder` in `waitlistorder.go`, the one ORDER BY every "who is next" query uses. A rejoin is a new row, so it starts at `0`. `signed_up_at` is never rewritten by a move. |
| `discord_interested` | INTEGER | Whether Discord currently lists them as Interested. Recorded even when the roster does not move, because without it un-marking and re-marking is indistinguishable from a duplicate event. |

**`UNIQUE(event_id, discord_user_id)`** — one row per person per event, enforced
by the database rather than by the code that inserts. Index
`(event_id, state, signed_up_at, id)` is the roster read.

⚠️ **There is no `position` column** — dropped 2026-09-02. It held arrival order
as a number, which was a second copy of what `signed_up_at` already said and
able to disagree with it; the rule that rejoining sends you to the back was
written twice, bumping the position *and* resetting the timestamp. Order is now
`(signed_up_at, id)` — and, for the waitlist, `waitlist_rank` ahead of it once an organiser has moved someone. Checked against every event in the live database before
the column went: **15 events, 0 where the derived order differed from the stored
one.**

`id` is not decoration. `signed_up_at` is accurate to the second and two rows on
one event already share a second here, so the id breaks the tie. That is also
why a **rejoin deletes its row and inserts a new one** rather than updating in
place: an updated row keeps its old, lower id and would sort a rejoiner *ahead*
of somebody who never left whenever both landed in the same second — which is
the common case, a leave and a rejoin being two clicks. `discord_interested` is
carried across, being a fact about Discord's list rather than about the row.

---

## `transitions` — the history Discord does not keep at all

Append-only. Never updated, never deleted except by cascade.

| column | type | description |
|---|---|---|
| `id` | INTEGER PK | |
| `event_id` | INTEGER | → `events(id)`, **ON DELETE CASCADE**. |
| `discord_user_id` | TEXT | Who moved. |
| `action` | TEXT | `joined`, `waitlisted`, `withdrew`, `promoted`, `rejoined`, `maybe`, and `added` — put on a list by an organiser from the web page, who is the `actor`. |
| `from_state` | TEXT | `''` when there was no prior row. |
| `to_state` | TEXT | Where they landed. |
| `actor` | TEXT | Who caused it: `user` for a press, `promotion` for an automatic move, `reaction`, or an operator's own id. Without this an automatic promotion and an admin's manual add are the same row. |
| `at` | INTEGER | When. |

Indexed by `(event_id, at)` and `(discord_user_id, at)` — the two questions
anyone asks of a history.

---

## `event_updates` — 7 columns · append-only, new 2026-09-02

Every change to what an event **is**, beside `signup_updates` which is every
change to who is on it. Nothing recorded this before: an event's name, time,
place and limit could all change and the only trace was the new value.

| column | type | description |
|---|---|---|
| `id` | INTEGER PK | |
| `event_id` | INTEGER | → `events(id)` ON DELETE CASCADE. |
| `field` | TEXT | Which one changed: `name`, `description`, `capacity`, `status`, `starts_at`, `ends_at`, `location`, `timezone`, `recurrence_rule`, `attending_role_id`, `waitlist_role_id`, `waitlist_disabled`, `created_by`. |
| `from_value` | TEXT | The old value, raw — a time as the integer it is stored as, not a rendering of it. Presentation belongs at the edge. |
| `to_value` | TEXT | The new one. |
| `actor` | TEXT | `web:<discord id>` from the web page, `discord:<discord id>` from a Discord form, `api`, `discord-event-screen` for an edit made in Discord's own event screen, or `discord-event-sync` for a completion or cancellation copied from Discord. |
| `at` | INTEGER | When. |

Written from `applyEventEdit`, which is the one function every edit passes
through, so no surface can change an event and leave no trace, and from the
Discord sync, which changes events without passing through it. **Bookkeeping is
deliberately not logged** — message ids, thread and forum ids,
`published_signature`, the reminder stamps. Those change without anybody editing
anything, and recording them would bury the rows somebody cares about.

## The address book

Discord gives no way to ask "which message did I post for this?" A bot posts,
gets an id back, and if it forgets that id it can never edit that message again
— only post another one. These four tables are addresses of messages this
service has written. They are why a *UI* change needs storage at all.

### `guild_tables` — where a guild's event table lives

| column | type | description |
|---|---|---|
| `guild_id` | TEXT PK | One table per guild. |
| `channel_id` | TEXT | The channel it is posted in. |
| `message_id` | TEXT | The header message. `''` if never posted. |
| `management_channel_id` | TEXT | Where the management table lives — the same events with Edit on each row and Create on the end. `''` means there is not one. On this row because it is the same table for a different audience, not a second thing to configure. |
| `board_channel_id` | TEXT | Where this guild's cards are posted and what its native events point at. Per guild since 2026-09-04; before that a process-wide env var. Required before the Create form works. |
| `past_channel_id` | TEXT | Where this guild's finished events leave their line. `''` keeps finished cards where they are. |
| `reminder_channel_id` | TEXT | Where this guild's hour-before and starting-now reminders go. `''` turns reminders off for the guild without stamping anything. |
| `new_events_channel_id` | TEXT | Where each of this guild's events keeps a message until it goes to past events. `''` means no such channel. |
| `updated_at` | INTEGER | |

⚠️ **"Table" here means the UI thing** — a Discord message listing every
upcoming event in rows — not a SQL table. The name collides with the thing it is
stored in and is due to be renamed.

### `table_pages` — the table's messages when it needs more than one

| column | type | description |
|---|---|---|
| `guild_id` | TEXT | PK part 1. |
| `page` | INTEGER | PK part 2. Zero-based. |
| `message_id` | TEXT | The message holding that page. |
| `updated_at` | INTEGER | |

No foreign key to `guild_tables`, so deleting a table's row leaves its pages
behind. Rebuilding deletes them explicitly.

### `management_pages` — the management table's messages

| column | type | description |
|---|---|---|
| `guild_id` | TEXT | PK part 1. |
| `page` | INTEGER | PK part 2, zero-based. |
| `message_id` | TEXT | The message holding that page. |
| `updated_at` | INTEGER | |

Same shape as `table_pages`, for the same reason: a packed table is more than one
message when it has to be. Its last page also carries the Create and My events
buttons, so the packer reserves a row's worth of components on every page.

### `standing_messages` — messages kept written rather than posted once

| column | type | description |
|---|---|---|
| `kind` | TEXT | PK part 1. Currently only `how-to`. |
| `channel_id` | TEXT | PK part 2. |
| `message_id` | TEXT | What to edit. |
| `updated_at` | INTEGER | |

The newest table, and it exists because the pinned how-to could previously only
be re-posted: every improvement to its wording meant a second pinned copy and
somebody deleting the first by hand.

### `guild_forums` — the forum surface and its tags

| column | type | description |
|---|---|---|
| `guild_id` | TEXT PK | One forum per guild. |
| `channel_id` | TEXT | The forum channel. |
| `tag_open` · `tag_full` · `tag_finished` · `tag_cancelled` | TEXT | Discord tag **ids**, not names — tags are joined on the id Discord assigned, so renaming one in the Discord UI must not orphan every post. |
| `updated_at` | INTEGER | |

---

### `guild_editing_rules` — who edits and creates in one server

No row means Discord's default: `MANAGE_EVENTS` or `ADMINISTRATOR` edits every event, a creator edits their own, and `CREATE_EVENTS` and up create.

| Column | Type | Meaning |
|---|---|---|
| `guild_id` | TEXT PK | The server. |
| `editor_role_id` | TEXT | When set, this role — with the owner and each event's creator — replaces `MANAGE_EVENTS` and `ADMINISTRATOR` as the right to edit every event. A role id, never a name. |
| `anyone_may_create` | INTEGER | 1 lets every member create events. |
| `updated_at` | INTEGER | Unix seconds. |

### `readable_names` — the short name a person is shown by

| Column | Type | Meaning |
|---|---|---|
| `discord_user_id` | TEXT PK | The person, by Discord user id — so the name follows them across events and servers and survives a nickname change. |
| `readable_name` | TEXT | "Matt" for "Lil' Fascist Matt 🌟". Set by hand; nothing guesses one. |
| `updated_at` | INTEGER | Unix seconds. |

No row means their Discord display name is shown. Read by `Roster` with a LEFT JOIN, into `Signup.ReadableName`.

### Avatars, new 2026-09-28

Drawings of a person, kept in a gallery; they choose which shows beside their name. The first shape, one row per person in a table named `avatars`, lasted an hour; `migrateSingleAvatarsTable` moves any such rows here at start and drops it.

**`avatar_people`** — `discord_user_id` PK; `photo_file_id`, the kept photo's file-store id (`owner_service` `discord-signup-store`, `owner_ref` `avatar:<user id>`), `''` once deleted; `photo_consented_at`, when they ticked consent with it; `chosen_drawing_id`, the drawing shown, 0 for none; `updated_at`.

**`avatar_drawings`** — `id`; `discord_user_id`; `format`, `character` (a whole person on the kit's skeleton, `CHARACTER` in `art/CHARACTER.md`'s form, which scenes pose) or `portrait` (a `DRAWING` of head and shoulders, every drawing before 2026-09-28's characters); `drawing_code`; `image_webp`, the round portrait printed at 256 pixels; `full_body_webp`, a character standing, NULL for a portrait; `request_id`, 0 for one an operator set; `created_at`. Kept until the person deletes it.

**`avatar_requests`** — `id`; `discord_user_id`; `kind` (`new_photo`, `redraw_photo`, `edit_drawing`); `base_drawing_id` for an edit; `comment`, in their words; `state` (`waiting`, `drawing`, `done`, `failed`), at most one `waiting` or `drawing` per person; `failure`; `requested_at`, `started_at`, `finished_at`; `drawing_id`, what it made. The six-a-day limit counts `requested_at`.

**`avatar_updates`** — append-only: `id`, `discord_user_id`, `action` (`requested_<kind>`, `drawing_started`, `drawing_saved`, `drawing_failed`, `chose`, `deleted_drawing`, `deleted_photo`, `removed`, `set_by_operator`), `detail` and `at`. Kept after everything else is removed.

`events.pictures_disabled` (INTEGER, 0): no picture of who is going for this event; switching it on deletes the picture and scene. Logged in `event_updates` as `picture`.

### `event_scenes` — the scene an event's picture is set in, new 2026-09-28

`event_id` PK; `request_kind` (`change`, `new`, `''`), `request_comment`, `requested_at`, `requested_by`: an organiser asking for the scene again, cleared by the scene or failure that answers it; `details_signature`, the name, description, place, weekday and time and repeat rule it was written from, and the scene format (`eventDetailsSignature`, `eventSceneFormat`: a new format asks every event for a new scene); `scene_code`, the scene in `art/SCENE.md`'s form, casting each person as a posed character; `failure`, `failed_details_signature` and `failed_at`, the last scene that did not come out and from which details, tried again an hour later; `updated_at`. A model writes it; a change to the details asks for a new one, a change to who is going does not.

### `event_pictures` — a picture of who is going, new 2026-09-28

`event_id` PK; `signature`, which scene, which people and which drawing of each it shows (`eventPictureSignature`); `image_webp`, 1200 by 400; `painted_at`. A picture whose signature no longer matches is repainted and shown until then, so a card is never empty while a new picture is painted; none shows once nobody going has an avatar. Animated: a loop of frames (`art/render-event-picture.mjs`).

### `site_admins` — whoever runs the bot

| Column | Type | Meaning |
|---|---|---|
| `discord_user_id` | TEXT PK | May see and edit everything in every server the bot is in, whatever their Discord roles. |
| `added_at` | INTEGER | Unix seconds. |

### `user_preferences` — choices a person makes on the web pages

| Column | Type | Meaning |
|---|---|---|
| `discord_user_id` | TEXT PK | The person, so a choice holds across logins and devices. |
| `home_guild_id` | TEXT | The one server the home page shows; `''` for every server. Ignored if they can no longer see it. |
| `updated_at` | INTEGER | Unix seconds. |

## The browser surface

### `web_sessions` — a logged-in browser

| column | type | description |
|---|---|---|
| `token` | TEXT PK | The cookie value. 32 random bytes, hex. |
| `discord_user_id` | TEXT | Who. |
| `display_name` · `avatar` | TEXT | For the page header. |
| `guild_permissions` | TEXT | JSON, guild id → permission bits **as of login**. This is why sessions are short: someone whose Manage Events is revoked in Discord keeps it here until their session ends. A week-long session would make that gap a week long. |
| `created_at` · `expires_at` | INTEGER | 12 hours. Swept hourly. |

Indexed by `expires_at` (the sweep) and `discord_user_id`.

### `oauth_states` — a login in progress

| column | type | description |
|---|---|---|
| `state` | TEXT PK | The CSRF value handed to Discord and checked on the way back. |
| `redirect` | TEXT | Where to send them after, default `/`. Only ever a path — an absolute URL here would be an open redirect that finishes a real login and then hands the person to somebody else's page. |
| `created_at` | INTEGER | Swept hourly; without that this grows by one row per abandoned login. |

---

## `event_invites` — 8 columns · append-only, new 2026-09-25

An organiser asking someone to come: a DM with the event's own Join and Maybe
buttons, sent from the web page. A second invite to the same person is a second
row. **Whether they came is not stored here** — it is their row in `signups`,
joined when the page is drawn.

| column | type | description |
|---|---|---|
| `id` | INTEGER PK | |
| `event_id` | INTEGER | → `events(id)` ON DELETE CASCADE. |
| `discord_user_id` | TEXT | Who was invited. |
| `display_name` | TEXT | Their name in the server when invited. Display only. |
| `invited_by` | TEXT | `web:<discord id>`, as `event_updates.actor`. |
| `delivery` | TEXT | `sent`, `dms-closed` (Discord's 50007) or `failed`. |
| `delivery_error` | TEXT | Discord's words when not `sent`. |
| `at` | INTEGER | When. |
| `holds_place` | INTEGER | `1` when the invite keeps a place until they answer. |
| `past_limit` | INTEGER | `1` when it keeps no place but lets their Join past the limit; it ends the same ways a hold does, and never counts against the limit. |
| `hold_ended_at` | INTEGER | When a held place stopped being held; `0` while it is. A live hold counts against the limit like someone going, in every query that decides whether the event is full. |
| `hold_outcome` | TEXT | `joined`, `declined` (Maybe or Can't go), `released` (an organiser, or a second invite replacing the first), `undelivered` (the DM bounced) or `expired` (the date rolled over). |
| `hold_ended_by` | TEXT | The actor that ended it, as elsewhere; `''` when the service did. |

## `event_pins` — regulars: people on every date of a repeating event, new 2026-09-25

Named from when regulars were called pins; the table and its `pinned_*` /
`unpinned_*` columns kept the name, and mean became and stopped being a
regular. Values stored under the old name — `joined_via = pinned`, actor
`pin` — are rewritten to `regular` at start.

| column | type | description |
|---|---|---|
| `id` | INTEGER PK | |
| `event_id` | INTEGER | → `events(id)` ON DELETE CASCADE. |
| `discord_user_id` | TEXT | Who. At most one current row per person per event. |
| `display_name` | TEXT | Their name when made a regular, kept current by the sync. Display only. |
| `pinned_by` / `unpinned_by` | TEXT | Who made them a regular and who stopped it; `regular` when the service made the host one. |
| `pinned_at` / `unpinned_at` | INTEGER | `unpinned_at` 0 is a current regular. At each rollover every current regular is seated as going (`joined_via = regular`, logged `added` by `regular`). The host is made one only if they never had a row, so stopping stands. |

## `event_messages` — organisers' messages to the people on an event, new 2026-09-26

| column | type | description |
|---|---|---|
| `id` | INTEGER PK | |
| `event_id` | INTEGER | → `events(id)` ON DELETE CASCADE. |
| `sent_by` | TEXT | `web:<discord id>`. |
| `via` | TEXT | `forum` (a post in the event's thread, mentioning each person) or `dm`. |
| `audience` | TEXT | The lists it went to, comma separated: `attending`, `waitlisted`, `maybe`. |
| `body` | TEXT | What was sent. |
| `recipients` / `delivered` / `failed` | INTEGER | People it was for; for a DM how many got it and how many did not, for a post 1 or 0. |
| `status` | TEXT | `sending`, then `sent`, or `failed` when it reached nobody. Every `dm` row not `failed` counts toward the limit of 2 DMs in 10 minutes per event, checked in the same transaction that writes the row. |
| `detail` | TEXT | Who had DMs closed, or what Discord said. |
| `at` | INTEGER | When. |

## `event_dms` and `event_dm_replies` — DMs about events, and what came back, new 2026-09-26

`event_dms` is every DM sent about an event, keyed by Discord's message id:
`channel_id`, `event_id`, `discord_user_id`, `kind` (`message`, `invite`,
`placed`, `promoted`), a one-line `summary`, `sent_at`.

`event_dm_replies` is what people wrote back: `event_id`, `discord_user_id`,
`display_name` (their name in the event's server when it came), `message_id`
(unique, so a gateway replay stores it once), `content`, `attachments` (a
count; the files stay in the DM), `replied_to` (our DM's id when they used
Reply), `matched` (`reply`, or `latest` for a plain message put with the
latest DM in the last 14 days), `at`.

## `roster_watchers` — organisers told by DM who joins and leaves, new 2026-09-26

One row per organiser per event, from the switch on the event's page; turning
it off deletes the row. `event_id`, `discord_user_id` (the organiser),
`watching_since` (nothing before it is told), `reported_through` (changes up to
this time have been told), `last_sent_at`, and `last_error` (why the last DM
was not sent, `''` when it was). Key `(event_id, discord_user_id)`.

## `forum_post_follows` — people this service made follow a forum post, new 2026-09-27

`event_id`, `discord_user_id`, `followed_at`, `unfollowed_at` (0 while they
follow it). Key `(event_id, discord_user_id)`; following again resets the row.
Only people in here are ever taken off a post.

## `bot_guilds` — the servers the bot is in, new 2026-09-27

`guild_id` (PK), `name`, `owner_id` (`''` until read), `updated_at`. The
gateway writes it: `GUILD_CREATE` and `GUILD_UPDATE` save a server with its
owner, `GUILD_DELETE` removes one (not during an outage), and `READY` drops any
the bot left while disconnected. The ten-minute sync (`POST /api/sync`)
rewrites it from REST, so it holds with the gateway off. The web pages and the
owner checks read it instead of asking Discord, whose list of the bot's servers
allows one call a second — three on one home page load was two seconds of 429s.
A person's roles are still read from Discord each time, so taking a role away
takes effect at once.

## `member_names` — what people are called in each server, new 2026-09-27

Key `(guild_id, discord_user_id)`; `display_name` (the last name Discord
gave), `left_guild` (1 when Discord says they are no longer in the server; the
name stays), `updated_at`. An event page reads it for the people in its
history and roster, and asks Discord only about someone with no row, recording
the answer. It used to ask about every person on every load, one call after
another. The ten-minute sync looks everyone in it up again, so a new nickname,
a departure or a return shows within ten minutes.

## `event_table_rows` — **dropped 2026-09-02**

A one-message-per-event table from before the consolidated table was paged,
superseded by `table_pages`. Nothing in the code had mentioned it for months,
yet it was still created on every fresh database and still carried an index. It
held five rows here, written 2026-08-23, addressing messages that were deleted
when paging shipped — none matched a live `events.message_id`.

`dropRetiredTables` runs `DROP TABLE IF EXISTS` at open, so it goes on the next
boot of every deployment and is a no-op on every boot after.
