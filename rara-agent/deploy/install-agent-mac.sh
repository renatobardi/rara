#!/bin/bash
# Builds rara-agent, installs binary + launchd plist, starts the daemon.
# Re-run after `make build` to update the binary in place.
set -euo pipefail

BINARY="rara-agent"
INSTALL_DIR="$HOME/.rara-agent"
PLIST_LABEL="com.rara.agent"
PLIST_DST="$HOME/Library/LaunchAgents/${PLIST_LABEL}.plist"
LOG_DIR="$HOME/Library/Logs/rara-agent"

SCRIPT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
cd "$SCRIPT_DIR"
echo "Building rara-agent..."
go build -ldflags="-w -s" -o "$BINARY" .

# Create dirs; LaunchAgents may not exist on a fresh profile.
mkdir -p "$INSTALL_DIR" "$LOG_DIR" "$(dirname "$PLIST_DST")"

# Restrict dirs and env file: credentials live here.
chmod 700 "$INSTALL_DIR" "$LOG_DIR"
if [ -f "$INSTALL_DIR/agent.env" ]; then
    chmod 600 "$INSTALL_DIR/agent.env"
fi

cp "$BINARY" "$INSTALL_DIR/$BINARY"
chmod 755 "$INSTALL_DIR/$BINARY"

WRAPPER="$INSTALL_DIR/run-agent.sh"
cp deploy/run-agent.sh "$WRAPPER"
chmod 755 "$WRAPPER"

sed "s|/Users/YOUR_USER|$HOME|g" deploy/com.rara.agent.plist > "$PLIST_DST"

launchctl unload "$PLIST_DST" 2>/dev/null || true
launchctl load -w "$PLIST_DST"

echo "rara-agent installed and started"
echo "  Logs: $LOG_DIR/"
echo "  Env:  $INSTALL_DIR/agent.env  (create if missing, copy .env.example)"
echo "  Stop: launchctl unload $PLIST_DST"
