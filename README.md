# discord-signup-store

Event rosters with a **capacity** and a **waitlist** — the thing Discord's own
scheduled events cannot do.

## Why this exists

Discord's "Interested" button is a notification subscription, not a seat
reservation. The scheduled event object has no capacity field at all
(`id`, `guild_id`, `channel_id`, `creator_id`, `name`, `description`,
`scheduled_start_time`, `scheduled_end_time`, `privacy_level`, `status`,
`entity_type`, `entity_id`, `entity_metadata`, `creator`, `user_count`,
`image`, `recurrence_rule` — that is the whole list), and the API offers **no
endpoint to remove a subscriber**. So a cap cannot be enforced on Discord's own
list even by a bot with `MANAGE_EVENTS`: it can watch, count and tell people,
but it cannot bounce anyone.

Three further things make "watch the native list and annotate it" unworkable,
and they are why the button here belongs to this service:

1. **No ordering survives.** `GET /guilds/{id}/scheduled-events/{id}/users`
   returns users ascending by `user_id` — snowflake order, which is *account
   creation* order. Rebuild a waitlist from it and the oldest Discord accounts
   sort to the front. Arrival order exists only if you record it yourself.
2. **No timestamps, no history.** The gateway says a subscription happened
   *now*; the REST endpoint says who is subscribed *now*. Someone who joins and
   leaves between two reads leaves no trace. The audit log covers event
   create/update/delete (action types 100–102), not subscriptions.
3. **Discord's own notification cannot be suppressed.** If 47 people are
   Interested in a 20-place event, all 47 get "this is starting now" — including
   the 27 who were told last week they were waitlisted.

So the roster lives here, the native scheduled event is demoted to an
announcement, and the two numbers are labelled rather than reconciled.

## How it works

A message in the channel carries **Join** and **Leave** buttons. A click is an
interaction, which means:

- The answer is **ephemeral and instant** — "You're in, 18/20" or "Waitlisted,
  3rd" — visible only to the person who clicked. No DM to bounce with error
  50007, no public noise.
- The **cap is enforced inside the click**, in one `BEGIN IMMEDIATE`
  transaction, so two people racing for the last place cannot both get it.
- **Promotion is automatic.** Someone leaves, the person who has waited longest
  moves up and is told by DM — falling back to a channel mention if their DMs
  are closed, because a promotion nobody hears about is a place nobody takes.

A third button, **Limit**, opens a modal with the current capacity prefilled.
That round trip exists because Discord allows a free text field only inside a
modal, never on a message — so changing a limit without leaving Discord means a
button that opens a form. The button is visible to everyone, since Discord
cannot hide a component from some readers, and the press is checked against
`MANAGE_EVENTS` before the form appears.

Raising a limit admits the queue: people come off the waitlist in arrival order
and are messaged. Lowering it removes nobody.

Optional `Attending` and `Waitlisted` roles are kept in sync as a *projection*
of the roster. The database is the source of truth; the roles are written, never
read back.

## Where credentials come from

This reads the bot token and the OAuth2 client secret from **auth-store**, a
credential vault that is not part of this repository. That is a real dependency,
not a suggestion, and it is stated here rather than buried so nobody discovers
it at deploy time.

Swapping it is one function. `TokenResolver` is just `func() (string, error)`,
and `AuthStoreTokenResolver` is one implementation of it — hand `NewDiscordClient`
a closure that reads an environment variable, a file, or your own vault, and
nothing else changes:

```go
discord := discordsignup.NewDiscordClient("", func() (string, error) {
    token := os.Getenv("DISCORD_BOT_TOKEN")
    if token == "" {
        return "", errors.New("DISCORD_BOT_TOKEN is not set")
    }
    return token, nil
})
```

It is deliberately a function rather than a string so the token is fetched at
use and re-fetched after a 401, which is what makes rotating one a no-restart
operation.

## Requirements

- **No gateway connection.** This is a pure HTTP-interactions app, so there is
  no persistent WebSocket, no privileged intents, and no always-on requirement
  beyond serving the endpoint.
- **`PIN_MESSAGES`** (bit 51) to pin the how-to. Discord split this out of
  `MANAGE_MESSAGES`, so a bot that can delete other people's messages can still
  be refused a pin — a 403 that reads like a mistake until you know. Everything
  works without it; the how-to just sits unpinned.
- **`MANAGE_ROLES`**, only if you use the role sync. The bot's own highest role
  must sit **above** `Attending` and `Waitlisted` in Server Settings → Roles.
  Get the hierarchy wrong and every role call returns 403 while the permission
  looks correctly granted — the most common way this breaks.
- **A Discord application of its own**, not one shared with another bot you
  already run. Two processes identifying on the same token both receive every
  gateway event, and each handles it.

## Setup

1. Create an application in the Developer Portal and add a bot to it.
2. File the bot token wherever your `TokenResolver` reads from. With auth-store
   that is a static credential:
   ```bash
   curl -s -X POST http://127.0.0.1:8303/api/credentials \
     -H "Authorization: Bearer $AUTHSTORE_TOKEN" \
     -H 'Content-Type: application/json' \
     -d '{"provider":"discord","account":"default","auth_type":"api_key",
          "refresh_mode":"none","api_key":"<bot token>"}'
   ```
   A bot token is a static secret, not an OAuth grant — hence `api_key` and
   `refresh_mode: none`. There is nothing to refresh.
3. Put the application's **Public Key** (Developer Portal → General
   Information) in `DISCORD_APPLICATION_PUBLIC_KEY` in
   `discord-signup-store.service`. It is public by design — it verifies
   Discord's signature and signs nothing.
4. Put `AUTH_STORE_TOKEN` in `~/.config/discord-signup-store/env`, mode 0600.
5. Add `nginx-interactions.conf` to the `YOUR_EXISTING_DOMAIN` server block,
   `sudo nginx -t && sudo systemctl reload nginx`.
6. `./deploy.sh`
7. Set the Interactions Endpoint URL in the Developer Portal to
   `https://YOUR_EXISTING_DOMAIN/discord/interactions`. Discord validates it on
   save by sending a PING and some deliberately bad signatures; `deploy.sh`
   checks both behaviours before it reports success.

## Running one

```bash
# Create the roster
curl -s -X POST http://127.0.0.1:8312/events -H 'Content-Type: application/json' \
  -d '{"name":"Friday workshop","guild_id":"…","channel_id":"…",
       "capacity":20,"starts_at":1780000000,
       "attending_role_id":"…","waitlist_role_id":"…"}'

# Post the message with the buttons
curl -s -X POST http://127.0.0.1:8312/events/1/message

# Who is on it
curl -s http://127.0.0.1:8312/events/1/signups

# Everything that ever happened to it
curl -s http://127.0.0.1:8312/events/1/history
```

`CONTRACT.md` is the full route table.

## Layout

| File | What it holds |
|---|---|
| `schema.sql` | Three tables: `events`, `signups`, `transitions`. Heavily commented — read it first. |
| `store.go` | Event CRUD, and the DSN whose `_txlock=immediate` is the whole concurrency design. |
| `roster.go` | `Join` and `Leave`. The cap, the waitlist ordering, the automatic promotion. |
| `interactions.go` | Ed25519 verification, PING/PONG, button dispatch, ephemeral replies. |
| `discord.go` | The REST client: roles, messages, DMs, the bot token from auth-store. |
| `message.go` | Rendering the public roster message, and the role/message projections. |
| `server.go` | Routes. One public, the rest loopback. |
| `vocabulary.go` | The closed sets: states, actions, statuses. |

## Notes for whoever touches this next

- **Capacity `0` means unlimited**, matching Discord's own convention for
  channel `user_limit` and invite `max_uses`. It does not mean "unset".
- **Lowering capacity never demotes anyone** who was already admitted. They were
  told they had a place; the new cap governs the next join instead. Pinned by
  `TestLoweringCapacityDoesNotDemoteAnyone`.
- **`position` is never renumbered and never reused.** Withdrawn rows keep
  theirs, which is why `nextPosition` uses `MAX+1` and not `COUNT+1`.
- **Re-joining after withdrawing puts you at the back**, or leaving and
  rejoining would be a way to jump the queue.
- **`state` is stored rather than derived** from `position < capacity`. It
  records a decision that was communicated to a person, and a derived view would
  silently rewrite it. That is deliberate, not redundancy.
- **The `_txlock=immediate` in the DSN is load-bearing.** Remove it and
  `TestCapacityHoldsUnderConcurrentJoins` fails — measured, not assumed.
