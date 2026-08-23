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
    status                     TEXT NOT NULL DEFAULT 'open',-- 'open' | 'closed' | 'cancelled'
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
-- position is arrival order and the ONLY ordering that exists. It is not
-- recoverable from anywhere else: Discord's own subscriber endpoint returns
-- users ascending by user_id, which is snowflake order — account creation date
-- — so rebuilding a waitlist from Discord would sort the oldest accounts to the
-- front. Assigned once, monotonic per event, never reused, never renumbered.
--
-- state is STORED rather than derived from position < capacity, and that is not
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
    position         INTEGER NOT NULL,
    state            TEXT NOT NULL,               -- 'attending' | 'waitlisted' | 'withdrawn'
    signed_up_at     INTEGER NOT NULL,
    state_changed_at INTEGER NOT NULL,
    UNIQUE(event_id, discord_user_id)
);
CREATE INDEX IF NOT EXISTS idx_signups_roster ON signups(event_id, state, position);

-- transitions: append-only. Every state change, with the time this service
-- received it.
--
-- This table is the durable record Discord does not keep. Its own gateway tells
-- you a subscription happened *now* and its REST endpoint tells you the current
-- set; neither carries a timestamp, and someone who joins and leaves between
-- two reads leaves no trace at all. Nothing updates or deletes a row here.
CREATE TABLE IF NOT EXISTS transitions (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id        INTEGER NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    discord_user_id TEXT NOT NULL,
    action          TEXT NOT NULL,               -- see action.go for the vocabulary
    from_state      TEXT NOT NULL DEFAULT '',    -- '' when there was no prior row
    to_state        TEXT NOT NULL,
    position        INTEGER NOT NULL,
    -- Who caused it: 'user' for a button click, 'promotion' for an automatic
    -- move off the waitlist, or an operator name for an API override. Without
    -- this, an automatic promotion and an admin's manual add are the same row.
    actor           TEXT NOT NULL DEFAULT 'user',
    at              INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_transitions_event ON transitions(event_id, at);
CREATE INDEX IF NOT EXISTS idx_transitions_user  ON transitions(discord_user_id, at);

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
