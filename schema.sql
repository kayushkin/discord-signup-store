PRAGMA foreign_keys = ON;

-- Create-only. Every statement here is IF NOT EXISTS, so this file is what an
-- empty database gets and nothing more: a column added to a table that already
-- exists on this host will never appear by editing this file. Those go in the
-- ensureColumn loop in Open(), and any index naming such a column goes in the
-- index loop that runs after it.

-- events: one signup roster. Deliberately NOT a copy of a Discord scheduled
-- event — it is the thing Discord has no field for, a roster with a cap.
--
-- discord_scheduled_event_id is optional and is a LINK, never a source. Discord
-- has no capacity field and no endpoint to remove a subscriber, so its
-- "Interested" list will disagree with this roster and there is no way to make
-- it agree. Store the id so a human can follow it; never read a count back off
-- it and never treat it as the roster.
CREATE TABLE IF NOT EXISTS events (
    id                         INTEGER PRIMARY KEY AUTOINCREMENT,
    guild_id                   TEXT NOT NULL,               -- snowflake, as text: 64-bit ids do not survive JSON numbers
    channel_id                 TEXT NOT NULL,               -- where the signup message lives
    message_id                 TEXT NOT NULL DEFAULT '',    -- the signup message carrying the buttons; '' until posted
    discord_scheduled_event_id TEXT NOT NULL DEFAULT '',    -- link only; see above
    name                       TEXT NOT NULL,
    description                TEXT NOT NULL DEFAULT '',
    -- 0 means unlimited, matching Discord's own convention for channel
    -- user_limit and invite max_uses. It does not mean "unset".
    capacity                   INTEGER NOT NULL DEFAULT 0,
    status                     TEXT NOT NULL DEFAULT 'open',-- 'open' | 'closed' | 'completed' | 'cancelled'
    -- Roles this service grants and revokes as people move between states.
    -- Both optional: leave them '' and the roster is the only record. The bot's
    -- own highest role must sit ABOVE these in the guild's role list or every
    -- assignment returns 403 with the permission looking correctly granted.
    attending_role_id          TEXT NOT NULL DEFAULT '',
    waitlist_role_id           TEXT NOT NULL DEFAULT '',
    starts_at                  INTEGER NOT NULL DEFAULT 0,  -- unix seconds; 0 = unknown
    created_at                 INTEGER NOT NULL,
    updated_at                 INTEGER NOT NULL,
    deleted_at                 INTEGER NOT NULL DEFAULT 0   -- soft delete: 0 = live
);
CREATE INDEX IF NOT EXISTS idx_events_guild  ON events(guild_id, deleted_at);
CREATE INDEX IF NOT EXISTS idx_events_status ON events(status, deleted_at);
CREATE INDEX IF NOT EXISTS idx_events_message ON events(message_id);

-- signups: one row per user per event, for the life of the event.
--
-- (signed_up_at, id) is arrival order and the ONLY ordering that exists. It is
-- not recoverable from anywhere else: Discord's own subscriber endpoint returns
-- users ascending by user_id, which is snowflake order — account creation date
-- — so rebuilding a waitlist from Discord would sort the oldest accounts to the
-- front.
--
-- There was a `position` column holding that order as a number. It was a second
-- copy of what signed_up_at already said, able to disagree with it, and the
-- rule that rejoining sends you to the back was written twice: the position was
-- bumped AND signed_up_at was reset. The id breaks ties, and ties are not
-- hypothetical — two rows on one event already share a second on this host.
--
-- state is STORED rather than derived from arrival order and capacity, and that is not
-- redundancy. It records a decision that was made and then communicated to a
-- person. Lower the capacity from 20 to 15 and the five people already told
-- they were in stay attending; a derived view would silently demote them and
-- the message they are holding would become a lie.
--
-- display_name is display only, captured at signup for the roster message. It
-- is never a join key — discord_user_id is. Names change; ids do not.
CREATE TABLE IF NOT EXISTS signups (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id         INTEGER NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    discord_user_id  TEXT NOT NULL,
    display_name     TEXT NOT NULL DEFAULT '',
    state            TEXT NOT NULL,               -- 'attending' | 'waitlisted' | 'maybe' | 'withdrawn'
    signed_up_at     INTEGER NOT NULL,
    state_changed_at INTEGER NOT NULL,
    UNIQUE(event_id, discord_user_id)
);
CREATE INDEX IF NOT EXISTS idx_signups_roster ON signups(event_id, state, signed_up_at, id);

-- signup_updates: append-only. Every state change, with the time this service
-- received it.
--
-- This table is the durable record Discord does not keep. Its own gateway tells
-- you a subscription happened *now* and its REST endpoint tells you the current
-- set; neither carries a timestamp, and someone who joins and leaves between
-- two reads leaves no trace at all. Nothing updates or deletes a row here.
CREATE TABLE IF NOT EXISTS signup_updates (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id        INTEGER NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    discord_user_id TEXT NOT NULL,
    action          TEXT NOT NULL,               -- see action.go for the vocabulary
    from_state      TEXT NOT NULL DEFAULT '',    -- '' when there was no prior row
    to_state        TEXT NOT NULL,
    -- Who caused it: 'user' for a button click, 'promotion' for an automatic
    -- move off the waitlist, or an operator name for an API override. Without
    -- this, an automatic promotion and an admin's manual add are the same row.
    actor           TEXT NOT NULL DEFAULT 'user',
    at              INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_signup_updates_event ON signup_updates(event_id, at);
CREATE INDEX IF NOT EXISTS idx_signup_updates_user  ON signup_updates(discord_user_id, at);

-- web_sessions: browser logins on YOUR_DOMAIN.
--
-- Opaque random tokens in a table rather than a signed cookie carrying claims.
-- A signed cookie needs a signing secret to manage and cannot be revoked before
-- it expires; a row can be deleted. The cookie holds nothing but the token, so
-- there is no payload to tamper with and nothing to verify.
--
-- guild_permissions is what Discord reported for this user at login, per guild,
-- as JSON. Cached deliberately: re-asking Discord on every request would put a
-- network call in the path of every page load and rate-limit under any real
-- use. It goes stale, which is why it expires with the session.
CREATE TABLE IF NOT EXISTS web_sessions (
    token             TEXT PRIMARY KEY,          -- opaque, 32 random bytes hex
    discord_user_id   TEXT NOT NULL,
    display_name      TEXT NOT NULL DEFAULT '',  -- display only, never a key
    avatar            TEXT NOT NULL DEFAULT '',
    guild_permissions TEXT NOT NULL DEFAULT '{}',-- JSON: {guild_id: permission_bits_as_string}
    created_at        INTEGER NOT NULL,
    expires_at        INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_web_sessions_user    ON web_sessions(discord_user_id);
CREATE INDEX IF NOT EXISTS idx_web_sessions_expires ON web_sessions(expires_at);

-- oauth_states: one row per login attempt in flight.
--
-- The state parameter is the CSRF defence for the authorization code flow, and
-- it only works if it is single-use: a state that can be replayed lets an
-- attacker finish someone else's login. Consumed on callback and deleted.
CREATE TABLE IF NOT EXISTS oauth_states (
    state      TEXT PRIMARY KEY,
    redirect   TEXT NOT NULL DEFAULT '/',  -- where to land after login
    created_at INTEGER NOT NULL
);

-- One local roster per native Discord event, and no more.
--
-- Without this, two rosters can claim the same scheduled event and
-- EventByDiscordScheduledEventID — the lookup every RSVP goes through — picks
-- one of them arbitrarily. Half the Interested presses would land on one roster
-- and half on the other, with nothing in either to say why. Measured, not
-- hypothetical: it happened on this host the first time an imported event and a
-- hand-made one were pointed at the same id.
--
-- Partial, because the column is '' for every roster that has no native event
-- and a plain UNIQUE would allow only one of those.
CREATE UNIQUE INDEX IF NOT EXISTS idx_events_one_roster_per_discord_event
    ON events(discord_scheduled_event_id)
    WHERE discord_scheduled_event_id != '' AND deleted_at = 0;

-- guild_tables: where each guild's consolidated event table lives.
--
-- One row per guild, holding the message that gets rewritten in place rather
-- than reposted. A table that reposted itself on every signup would push the
-- channel down and lose its position in everyone's client.
--
-- Kept in its own table rather than as columns on events, because it belongs to
-- the guild and not to any one event — the whole point of it is that it outlives
-- the events it lists.
CREATE TABLE IF NOT EXISTS guild_tables (
    guild_id   TEXT PRIMARY KEY,
    channel_id TEXT NOT NULL,
    message_id TEXT NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL
);


-- table_pages: the consolidated table, which is more than one message when it
-- has to be.
--
-- Six events fit in a message — measured, not inferred: each costs a text
-- block, an action row and four buttons, and Discord allows 40 components in
-- total. Page 0 is posted first and stays first, and because a redraw rewrites
-- every page in place, events move BETWEEN pages while the messages stay put.
-- That is what keeps the table sorted without ever reposting it.
CREATE TABLE IF NOT EXISTS table_pages (
    guild_id   TEXT NOT NULL,
    page       INTEGER NOT NULL,
    message_id TEXT NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (guild_id, page)
);

-- guild_forums: the forum-channel surface, running alongside the cards and the
-- table so the two shapes can be compared on the same events.
--
-- The tag ids are stored, not the tag names: tags are joined on the id Discord
-- assigned, and a rename in the Discord UI must not orphan every post.
CREATE TABLE IF NOT EXISTS guild_forums (
    guild_id      TEXT PRIMARY KEY,
    channel_id    TEXT NOT NULL,
    tag_open      TEXT NOT NULL DEFAULT '',
    tag_full      TEXT NOT NULL DEFAULT '',
    tag_finished  TEXT NOT NULL DEFAULT '',
    tag_cancelled TEXT NOT NULL DEFAULT '',
    updated_at    INTEGER NOT NULL
);

-- standing_messages: the messages this service keeps written rather than
-- posts once, keyed by what they are and where they live.
--
-- Without this the how-to could only ever be re-posted, because the code has no
-- way to find a message it wrote and forgot. Every copy change meant a second
-- pinned how-to and somebody deleting the first by hand. A stored id means the
-- message is edited where it already sits, which is also what people expect of
-- a pinned message: it does not move when its wording improves.
CREATE TABLE IF NOT EXISTS standing_messages (
    kind       TEXT NOT NULL,
    channel_id TEXT NOT NULL,
    message_id TEXT NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (kind, channel_id)
);

-- event_updates: append-only. Every change to what an event IS, beside
-- signup_updates which is every change to who is on it.
--
-- One row per field per edit, so "who moved this to Tuesday" is a question with
-- an answer. Nothing here recorded that before: an event's name, time, place
-- and limit could all change and the only trace was the new value.
--
-- Bookkeeping is deliberately not logged — message ids, thread and forum ids,
-- the publish signature, reminder stamps. Those are this service noting what it
-- did, not somebody editing the event, and mixing them in would bury the four
-- rows a person cares about under a hundred nobody does.
CREATE TABLE IF NOT EXISTS event_updates (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id   INTEGER NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    field      TEXT NOT NULL,
    from_value TEXT NOT NULL DEFAULT '',
    to_value   TEXT NOT NULL DEFAULT '',
    -- Who did it, by Discord id where there is one: 'web:<id>' from the page,
    -- the raw id from a Discord form, 'api' from the machine API.
    actor      TEXT NOT NULL DEFAULT '',
    at         INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_event_updates_event ON event_updates(event_id, at);

-- roster_table_pages: the messages of the roster table, which is the event
-- table with everyone's names on it.
--
-- Its own pages rather than a second guild_tables row, because it is not a
-- second place to configure — it lives in the SAME channel as the event table
-- and is drawn from the same events. Only the messages differ, so only the
-- messages are stored.
--
-- Paged like table_pages, but NOT a fixed number of events per message: a row
-- carrying twenty names is many times the size of one carrying none, so the
-- packer measures each block and starts a new message when the next one would
-- not fit.
CREATE TABLE IF NOT EXISTS roster_table_pages (
    guild_id   TEXT NOT NULL,
    page       INTEGER NOT NULL,
    message_id TEXT NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (guild_id, page)
);

-- management_pages: the messages of the management table — the same events
-- as table_pages, drawn in the management channel with Edit on each row and
-- Create on the end. Same shape as table_pages for the same reason: a packed
-- table is more than one message when it has to be.
CREATE TABLE IF NOT EXISTS management_pages (
    guild_id   TEXT NOT NULL,
    page       INTEGER NOT NULL,
    message_id TEXT NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (guild_id, page)
);

-- guild_editing_rules: who may edit and create events in one server, when it
-- is not Discord's default (Manage Events edits everything, creators edit
-- their own). editor_role_id replaces Manage Events and Administrator with a
-- role of the server's choosing, alongside the owner; anyone_may_create lets
-- every member create. No row is the default rule.
CREATE TABLE IF NOT EXISTS guild_editing_rules (
    guild_id          TEXT PRIMARY KEY,
    editor_role_id    TEXT NOT NULL DEFAULT '',
    anyone_may_create INTEGER NOT NULL DEFAULT 0,
    updated_at        INTEGER NOT NULL
);

-- readable_names: the short name a person is shown by on Discord — "Matt" for
-- "Lil' Fascist Matt 🌟". Keyed on the Discord user id, so it follows them
-- across events and servers and survives a change of nickname. No row means
-- their Discord display name is shown.
CREATE TABLE IF NOT EXISTS readable_names (
    discord_user_id TEXT PRIMARY KEY,
    readable_name   TEXT NOT NULL,
    updated_at      INTEGER NOT NULL
);

-- site_admins: people who may do everything in every server the bot is in,
-- whatever their Discord roles say — whoever runs the bot. Set through the
-- machine API only.
CREATE TABLE IF NOT EXISTS site_admins (
    discord_user_id TEXT PRIMARY KEY,
    added_at        INTEGER NOT NULL
);

-- user_preferences: choices a person makes on the web pages, by Discord user
-- id so they hold across logins. home_guild_id narrows the home page to one
-- server; '' shows every server.
CREATE TABLE IF NOT EXISTS user_preferences (
    discord_user_id TEXT PRIMARY KEY,
    home_guild_id   TEXT NOT NULL DEFAULT '',
    updated_at      INTEGER NOT NULL
);

-- event_invites: an organiser asking someone to come, by DM with Join and
-- Maybe buttons. Append-only: a second invite to the same person is a second
-- row. Whether they came is not stored here — it is their row in signups, read
-- when the page is drawn. delivery says whether Discord took the DM: 'sent',
-- 'dms-closed' (error 50007, their DMs from server members are off) or
-- 'failed', with Discord's words in delivery_error.
CREATE TABLE IF NOT EXISTS event_invites (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id        INTEGER NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    discord_user_id TEXT NOT NULL,
    display_name    TEXT NOT NULL DEFAULT '',
    -- 'web:<id>', as event_updates records it.
    invited_by      TEXT NOT NULL,
    delivery        TEXT NOT NULL,
    delivery_error  TEXT NOT NULL DEFAULT '',
    at              INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_event_invites_event ON event_invites(event_id, at);

-- event_pins: people who get a place on every date of a recurring event. When
-- a date rolls over and the roster clears, each person with a live pin is put
-- back on as going. The host is pinned the first time the event repeats; an
-- unpin is kept (unpinned_at), so it is never pinned back by that rule. A
-- live pin is one with unpinned_at 0; at most one per person per event.
CREATE TABLE IF NOT EXISTS event_pins (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id        INTEGER NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    discord_user_id TEXT NOT NULL,
    display_name    TEXT NOT NULL DEFAULT '',
    pinned_by       TEXT NOT NULL,
    pinned_at       INTEGER NOT NULL,
    unpinned_at     INTEGER NOT NULL DEFAULT 0,
    unpinned_by     TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_event_pins_event ON event_pins(event_id, unpinned_at);

-- event_messages: an organiser's message to the people on an event, from the
-- web page — posted in its forum thread with each of them mentioned, or sent
-- to each by DM. Kept for the event's log, and to limit how often: at most
-- messageLimit in any messageWindow per event, counting every send that did
-- not fail outright. status is 'sending' while it goes out, then 'sent' or
-- 'failed'; delivered and failed count people (DMs) or are 1/0 (the post).
CREATE TABLE IF NOT EXISTS event_messages (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id   INTEGER NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    sent_by    TEXT NOT NULL,
    via        TEXT NOT NULL,                -- 'forum' | 'dm'
    audience   TEXT NOT NULL,                -- lists, comma separated: attending,waitlisted,maybe
    body       TEXT NOT NULL,
    recipients INTEGER NOT NULL DEFAULT 0,
    delivered  INTEGER NOT NULL DEFAULT 0,
    failed     INTEGER NOT NULL DEFAULT 0,
    status     TEXT NOT NULL,
    detail     TEXT NOT NULL DEFAULT '',
    at         INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_event_messages_event ON event_messages(event_id, at);

-- event_dms: every DM this service sent a person about an event — an
-- organiser's message, an invite, being put on a list or given a place — so a
-- reply in that DM can be traced to its event. summary is a short line saying
-- what the DM was, shown beside a reply.
CREATE TABLE IF NOT EXISTS event_dms (
    message_id      TEXT PRIMARY KEY,
    channel_id      TEXT NOT NULL,
    event_id        INTEGER NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    discord_user_id TEXT NOT NULL,
    kind            TEXT NOT NULL,
    summary         TEXT NOT NULL DEFAULT '',
    sent_at         INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_event_dms_user ON event_dms(discord_user_id, sent_at);

-- event_dm_replies: what people wrote back in those DMs. matched says how
-- the event was found: 'reply' — they used Discord's Reply on one of our DMs,
-- so replied_to is that DM — or 'latest' — a plain message, put with the
-- latest DM we sent them in the last two weeks.
CREATE TABLE IF NOT EXISTS event_dm_replies (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id        INTEGER NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    discord_user_id TEXT NOT NULL,
    display_name    TEXT NOT NULL DEFAULT '',
    message_id      TEXT NOT NULL UNIQUE,
    content         TEXT NOT NULL DEFAULT '',
    attachments     INTEGER NOT NULL DEFAULT 0,
    replied_to      TEXT NOT NULL DEFAULT '',
    matched         TEXT NOT NULL,
    at              INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_event_dm_replies_event ON event_dm_replies(event_id, at);
