#!/bin/bash
# Sources ~/.rara-agent/agent.env and execs rara-agent.
# launchd does not expand shell variables in ProgramArguments, so this wrapper
# is the standard pattern (mirrors rara-runner/deploy/run-agent.sh).
set -euo pipefail
ENV_FILE="$HOME/.rara-agent/agent.env"
if [ ! -f "$ENV_FILE" ]; then
    echo "Missing $ENV_FILE — copy .env.example and fill in your values" >&2
    exit 1
fi
set -a
# shellcheck source=/dev/null
source "$ENV_FILE"
set +a
exec "$HOME/.rara-agent/rara-agent"
