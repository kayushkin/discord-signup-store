# About discord-signup-store

## What it owns

`:8312`. Event rosters with a **capacity** and a **waitlist** — the thing Discord's own scheduled events cannot do. **The `signups` table is the roster, and nothing else is**; everything a person sees is a projection of its rows, and counts are never stored. Read `BEHAVIOUR.md` first: it says what the service *is*, so every piece of text shown to a person can be checked against one description. `CONTRACT.md` is the route table and `SCHEMA.md` the tables.

## Where this prompt lives

These sections are stored in agent-store as a project prompt collection and rendered, with identical text, to `AGENTS.md` and `CLAUDE.md` at the root of this repo, so that whichever file a harness reads it gets the same thing. Edit them on dash `/files`, or edit either rendered file: the 15-minute scan carries the edit back into the sections and out to the other file. The host prompt keeps one row for this repo with only what an agent elsewhere needs.

# How it works

## Why the roster cannot live in Discord

Do not "simplify" this back onto Discord's native list; each of these was checked against the API. Discord's "Interested" is a notification subscription, not a seat: the scheduled event object has no capacity field and the API has **no endpoint to remove a subscriber**, so a cap cannot be enforced there even with `MANAGE_EVENTS`. `GET /guilds/{id}/scheduled-events/{id}/users` returns users ascending by `user_id` — snowflake, which is account-creation order — so a waitlist rebuilt from Discord would put the oldest accounts first. Discord keeps no timestamps or history of subscriptions. And its "starting now" notification goes to everyone Interested, waitlisted or not, and cannot be suppressed. So the roster lives here, the native event is an announcement, and Discord's Interested count is stored for display and labelled as Discord's — it never feeds a decision.

## The cap is enforced inside one transaction

A Join click enforces the cap inside one `BEGIN IMMEDIATE` transaction, so two people racing for the last place cannot both get it. ⚠️ **`_txlock=immediate` in the DSN (`store.go`) is the whole concurrency design** — remove it and `TestCapacityHoldsUnderConcurrentJoins` fails. The click is answered **ephemerally**, which a DM cannot do reliably: error `50007` bounces anyone whose DMs from server members are off. When someone leaves, the person who has waited longest is promoted and told by DM, with a channel mention if their DMs are closed. Raising a limit admits the queue in arrival order; lowering it removes nobody.

## Arrival order, state and history

**`(signed_up_at, id)` is arrival order** — it cannot be recovered from Discord. The waitlist follows it unless an organiser reorders the line on the web page, which ranks everyone in `signups.waitlist_rank`; every "who is next" query orders by `waitlistOrder()` so the page and promotion cannot disagree. There is no `position` column any more: it was a second copy of `signed_up_at` that could disagree with it; older notes still name it. A signup's `state` (`attending`, `waitlisted`, `withdrawn`) is **stored, not derived**, on purpose: it records a decision that was made and told to a person. `signup_updates` and `event_updates` are the append-only histories (the first was once called `transitions`).

## Roles are a projection

Optional `Attending` and `Waitlisted` roles are written when someone's state changes and **never read back**; the database is the source of truth. The bot's own highest role must sit **above** both in Server Settings → Roles, or every role call is a 403 while the permission looks correctly granted — the most common way this breaks. Role sync needs `MANAGE_ROLES`.

## Interactions, the gateway and its own application

Buttons and modals arrive as **HTTP interactions** at `POST /interactions`, verified with the application's public key. Discord's own Interested button is different: `GUILD_SCHEDULED_EVENT_USER_ADD` is delivered **only over the gateway**, as are people's replies to the bot's DMs (DIRECT_MESSAGES, not privileged; a DM's text comes without Message Content — see `dmreplies.go`), so `gateway.go` holds one websocket (discordgo, for the socket alone; REST calls stay on this package's client). `DISCORD_GATEWAY_DISABLED=true` turns it off, and then the Interested button does not feed the roster. It needs a **Discord application of its own**: two processes identifying on one token both receive every gateway event, and si's bot already holds a connection on its token. discordgo's own reconnect is switched off: on 2026-09-15 it ended with no socket and no log line for five days. `gateway_supervisor.go` reopens the socket instead — on the same session, so it can RESUME — and replaces a session whose `Open()` never returns or whose heartbeat goes unacknowledged. `GET /healthz` carries the result as `gateway.state`; it is 200 either way, so read the field.

## Discord's limits on the forms

A modal holds at most five inputs, so the Edit modal has Name, Starts, Max attendees, Location and Description; end time, recurrence and roles are on the web page. A free text field exists only inside a modal, never on a message. A component cannot be hidden from some readers, so the Edit button is visible to all and the press is checked against `MANAGE_EVENTS` — or against a server's own editor role, when it set one (`guild_editing_rules`, `editauthority.go`). A message cannot move between channels, so archiving a finished event is a post to the past-events channel and **then** a delete — a failure leaves a duplicate rather than a hole.

# Access and operations

## What is published and what must never be

Binds `127.0.0.1:8312` (`DISCORD_SIGNUP_ADDR`). ⚠️ **The `/api/…` routes edit rosters and have no auth of their own; they must never be proxied.** nginx publishes two things: `discord.kayushkin.com` serves the web pages (`/`, `/login`, `/auth/callback`, `/events/…`) with `location /api/ { deny all; }`, and the dash vhost forwards exactly `/discord/interactions` to `/interactions`. The web pages log a person in with Discord OAuth and cache their per-guild permission bits on the session, so someone whose `MANAGE_EVENTS` is revoked keeps it until the session ends. ⚠️ A vhost on this host is installed from the repo that owns it — dash installs its own on every deploy — so **a hand-edit under `/etc/nginx` lasts only until that repo next deploys**; put the interactions location in dash's vhost file (`nginx-interactions.conf` here is the snippet).

## Unit, credentials and scheduled jobs

Unit `discord-signup-store.service`; health is `GET /healthz`. The bot token and the OAuth client secret come from **auth-store** (`AUTH_STORE_URL`; provider `DISCORD_CREDENTIAL_PROVIDER=discord`, accounts `default` and `oauth-client`), resolved at use and re-resolved after a 401, so rotating one needs no restart. `AUTH_STORE_TOKEN` goes in `~/.config/discord-signup-store/env` (mode 600), not in the tracked unit. `DISCORD_APPLICATION_PUBLIC_KEY` in the unit is public by design. Every variable is declared in `settings.go` and described at `GET /api/settings` (under `/api/` so the vhost refuses it); a set `DISCORD_` variable nobody declared stops the start, and `deploy.sh` checks the running unit's environment against the declarations before it replaces the binary. Three scheduler shell jobs drive it: `discord-event-sync` (65, `*/10`, `POST /api/sync`), `discord-signup-republish` (78, every minute, `POST /api/republish`) and `discord-signup-reminders` (80, every minute, `POST /api/reminders`).

# Working in this repo

## Build, test and deploy

Module `github.com/kayushkin/discord-signup-store`, package `discordsignup`, server in `cmd/discord-signup-store`. SQLite at `~/.config/discord-signup-store/discord-signup-store.db` (WAL, foreign keys on, `_busy_timeout=5000`, `_txlock=immediate`). Plain `go test ./...` and `go build` — no build tag. `deploy.sh` checks that the interactions endpoint answers a PING and refuses a bad signature, as Discord does when the endpoint URL is saved. Pushes to `github.com/kayushkin/discord-signup-store`.
