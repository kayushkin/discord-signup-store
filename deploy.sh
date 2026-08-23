#!/usr/bin/env bash
# Build, install and restart discord-signup-store, then prove it is actually
# answering — not merely running.
#
# The unit is installed FROM ./discord-signup-store.service, so there is one
# copy of every value and nothing here to drift from it.
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")" && pwd)"
BIN_DIR="$HOME/bin"
SERVICE="discord-signup-store.service"
BINARY="discord-signup-store"
UNIT_SRC="$REPO_DIR/$SERVICE"
UNIT_DEST="$HOME/.config/systemd/user/$SERVICE"

cd "$REPO_DIR"

# go lives behind mise shims on this host; a non-login shell (automation, an
# agent session) does not get them on PATH.
export PATH="$HOME/.local/share/mise/shims:$PATH"
# systemctl --user needs these to reach the user manager. Without them it prints
# NOTHING and exits 0 — silence that reads exactly like success.
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
export DBUS_SESSION_BUS_ADDRESS="${DBUS_SESSION_BUS_ADDRESS:-unix:path=${XDG_RUNTIME_DIR}/bus}"

step() { printf '\n==> %s\n' "$*"; }

# The service refuses to start without the application public key, which is the
# correct behaviour but produces a restart loop that is easy to misread as a
# build problem. Catch it here, where the message can say what to do.
# A fresh clone has only the .example. Say so plainly rather than failing on a
# missing file three lines later.
if [ ! -f "$UNIT_SRC" ]; then
  echo "ERROR: $UNIT_SRC does not exist." >&2
  echo "       It is gitignored, because it names one host and one Discord" >&2
  echo "       application. Start from the template:" >&2
  echo "         cp $UNIT_SRC.example $UNIT_SRC" >&2
  echo "       then fill in every empty Environment= value in it." >&2
  exit 1
fi

step "Checking the unit carries a Discord public key…"
if ! grep -qE '^Environment=DISCORD_APPLICATION_PUBLIC_KEY=.+' "$UNIT_SRC"; then
  echo "ERROR: DISCORD_APPLICATION_PUBLIC_KEY is empty in $UNIT_SRC" >&2
  echo "       Copy it from the Discord Developer Portal → your application →" >&2
  echo "       General Information → Public Key, and put it in that line." >&2
  exit 1
fi

step "Testing…"
go test ./...

step "Building $BINARY…"
go build -o "$BINARY" ./cmd/discord-signup-store
echo "    built: $(ls -lh "$BINARY" | awk '{print $5}')"

step "Installing systemd unit…"
mkdir -p "$(dirname "$UNIT_DEST")"
cp "$UNIT_SRC" "$UNIT_DEST"

step "Stopping $SERVICE…"
systemctl --user stop "$SERVICE" 2>/dev/null || true

step "Installing binary to $BIN_DIR…"
mkdir -p "$BIN_DIR"
cp "$BINARY" "$BIN_DIR/$BINARY"

step "Starting $SERVICE…"
systemctl --user daemon-reload
systemctl --user enable "$SERVICE" >/dev/null
systemctl --user start "$SERVICE"

step "Verifying the process is up…"
for _ in $(seq 1 10); do
  systemctl --user is-active --quiet "$SERVICE" && break
  sleep 1
done
if ! systemctl --user is-active --quiet "$SERVICE"; then
  echo "ERROR: $SERVICE failed to start" >&2
  journalctl --user -u "$SERVICE" -n 30 --no-pager >&2
  exit 1
fi

# A running process is not a working one. Read the address out of the unit
# rather than restating it, so this check cannot drift from what was deployed.
ADDR="$(systemctl --user show "$SERVICE" -p Environment --value \
        | tr ' ' '\n' | sed -n 's/^DISCORD_SIGNUP_ADDR=//p')"
: "${ADDR:?could not read DISCORD_SIGNUP_ADDR out of the running unit}"

step "Smoke-checking http://$ADDR/healthz…"
HEALTH="$(curl -sfS --max-time 5 "http://$ADDR/healthz")" || {
  echo "ERROR: /healthz did not answer" >&2
  journalctl --user -u "$SERVICE" -n 30 --no-pager >&2
  exit 1
}
echo "    $HEALTH"

# The endpoint is useless if it does not reject a bad signature: Discord probes
# a registered URL with deliberately invalid ones and refuses to accept an
# endpoint that answers anything but 401.
step "Proving an unsigned interaction is refused…"
CODE="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
        -X POST "http://$ADDR/interactions" \
        -H 'Content-Type: application/json' -d '{"type":1}')"
if [ "$CODE" != "401" ]; then
  echo "ERROR: unsigned POST /interactions answered $CODE, want 401." >&2
  echo "       Discord will refuse to save an endpoint that does this." >&2
  exit 1
fi
echo "    unsigned request refused with 401, as Discord requires"

printf '\n==> Deployed.\n'
echo "    Interactions Endpoint URL for the Developer Portal:"
echo "      https://YOUR_EXISTING_DOMAIN/discord/interactions"
echo "    (add nginx-interactions.conf to the dash vhost first if you have not)"
