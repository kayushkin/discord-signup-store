# Every table, every column

SQLite at `~/.config/discord-signup-store/discord-signup-store.db` (WAL, foreign
keys on). Ten tables of ours, plus SQLite's own `sqlite_sequence`. Taken from the live database on 2026-09-02 and checked
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

The row everything else hangs off. 29 columns, of which about a third are not
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
| `starts_at` | INTEGER | When it starts. `0` means unset, which blocks publishing to Discord. |
| `ends_at` | INTEGER | When it ends. `0` means unset, and a run time is assumed — by one constant, so the archive sweep and the publisher cannot assume different lengths. |
| `location` | TEXT | Free text. Sent to Discord with a placeholder when empty, because Discord refuses an EXTERNAL event without one; the placeholder is stripped on the way back. |
| `entity_type` | TEXT | Discord's kind of event: `stage`, `voice` or `external`. |
| `recurrence_rule` | TEXT | An RFC 5545 RRULE, passed to Discord untouched. |
| `timezone` | TEXT | IANA zone name, never an offset — an offset cannot survive a daylight-saving change. Mandatory whenever `recurrence_rule` is set. |
| `origin` | TEXT | `local` (made here) or `discord` (imported). **Not** derivable from `discord_scheduled_event_id`: a local event published to Discord also has one. This records who owns the thing. |
| `discord_interested_count` | INTEGER | Discord's own Interested tally. Stored for display, labelled as Discord's, and **never** feeds a capacity decision. |
| `discord_synced_at` | INTEGER | When the native event was last read. |
| `created_by` | TEXT | Discord user id of whoever made it. Grants edit rights, and gets them a place on their own roster. |
| `thread_id` | TEXT | The discussion thread hanging off the card. Stored separately from `message_id` even though Discord gives a message thread the same id, because the card can be reposted while the thread lives on. |
| `forum_post_id` | TEXT | The event's post in the forum channel. One id reaches both the post and the card inside it. |
| `published_signature` | TEXT | Fingerprint of everything that feeds a Discord copy, written only when a publish fully succeeds. `''` means never published, or the last publish failed part way. The minute sweep republishes anything that does not match. |
| `reminded_before_at` | INTEGER | When the hour-before reminder went out, **or** when it was written off as too late. `0` means still owed. |
| `reminded_start_at` | INTEGER | The same for the starting reminder. |
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
| `position` | INTEGER | **Arrival order, and it exists nowhere else.** Discord's `/scheduled-events/{id}/users` returns users ascending by snowflake — account-creation order — so a waitlist rebuilt from Discord would sort the oldest accounts to the front. |
| `state` | TEXT | `attending`, `waitlisted` or `withdrawn`. A withdrawn row is kept, not deleted, so rejoining is distinguishable from never having left. |
| `signed_up_at` | INTEGER | First arrival. |
| `state_changed_at` | INTEGER | Last move between states. |
| `joined_via` | TEXT | How they got on: `button`, `interested`, `reaction`, `operator` or `organiser`. Not cosmetic — it is what makes an un-marked Interested readable as leaving rather than as noise. |
| `discord_interested` | INTEGER | Whether Discord currently lists them as Interested. Recorded even when the roster does not move, because without it un-marking and re-marking is indistinguishable from a duplicate event. |

**`UNIQUE(event_id, discord_user_id)`** — one row per person per event, enforced
by the database rather than by the code that inserts. Index
`(event_id, state, position)` is the roster read.

---

## `transitions` — the history Discord does not keep at all

Append-only. Never updated, never deleted except by cascade.

| column | type | description |
|---|---|---|
| `id` | INTEGER PK | |
| `event_id` | INTEGER | → `events(id)`, **ON DELETE CASCADE**. |
| `discord_user_id` | TEXT | Who moved. |
| `action` | TEXT | `joined`, `waitlisted`, `withdrew`, `promoted`, `rejoined`. |
| `from_state` | TEXT | `''` when there was no prior row. |
| `to_state` | TEXT | Where they landed. |
| `position` | INTEGER | Their place at the time. |
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
| `field` | TEXT | Which one changed: `name`, `description`, `capacity`, `status`, `starts_at`, `ends_at`, `location`, `timezone`, `recurrence_rule`, `attending_role_id`, `waitlist_role_id`. |
| `from_value` | TEXT | The old value, raw — a time as the integer it is stored as, not a rendering of it. Presentation belongs at the edge. |
| `to_value` | TEXT | The new one. |
| `actor` | TEXT | `web:<discord id>`, a raw Discord id from a form, or `api`. |
| `at` | INTEGER | When. |

Written from `applyEventEdit`, which is the one function every edit passes
through, so no surface can change an event and leave no trace. **Bookkeeping is
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

## `event_table_rows` — **dropped 2026-09-02**

A one-message-per-event table from before the consolidated table was paged,
superseded by `table_pages`. Nothing in the code had mentioned it for months,
yet it was still created on every fresh database and still carried an index. It
held five rows here, written 2026-08-23, addressing messages that were deleted
when paging shipped — none matched a live `events.message_id`.

`dropRetiredTables` runs `DROP TABLE IF EXISTS` at open, so it goes on the next
boot of every deployment and is a no-op on every boot after.
