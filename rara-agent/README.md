# rara-agent

Mac launchd daemon — polls `agent_tasks`, spawns Claude Code CLI for each claimed task, writes back `status`/`result`/`session_id`.

## Prerequisites

- `claude` CLI installed (`npm install -g @anthropic-ai/claude-code`)
- `~/.rara-agent/agent.env` with values from `.env.example`
- Neon `DATABASE_URL` (same user/password as other rara workers)
- `CORE_URL` and `CORE_TOKEN` from the rara-core deployment

## Install

```bash
cd rara-agent
make build           # builds ./rara-agent
make install-local   # installs binary + launchd plist, starts daemon
```

## Verify

```bash
# Is it running?
launchctl list | grep com.rara.agent

# Recent logs
tail -f ~/Library/Logs/rara-agent/output.log
tail -f ~/Library/Logs/rara-agent/error.log
```

## First run

1. Create `~/.rara-agent/agent.env` (copy `.env.example`, fill in real values).
2. Run `make install-local`.
3. In rara-console /agents, create an agent with executor=cli (default).
4. Queue a task (free instruction).
5. Watch `output.log` for "task N done".
6. Refresh the console — task status should be `done` and result visible.

## Stop / uninstall

```bash
launchctl unload ~/Library/LaunchAgents/com.rara.agent.plist
rm -rf ~/.rara-agent ~/Library/LaunchAgents/com.rara.agent.plist ~/Library/Logs/rara-agent
```
