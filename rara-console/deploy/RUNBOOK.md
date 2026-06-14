# rara-console VPC Deployment Runbook

The console is a second resident service on the same Oracle ARM VM as rara-core. After the
one-time setup, every `workflow_dispatch` on `deploy-console.yml` rebuilds the binary (SPA embedded)
and restarts the service.

## Prerequisites

- The rara-core VM already set up (Ubuntu 22.04 aarch64, Tailscale up, SSH as `ubuntu`).
- The rara-core surface running and reachable on the tailnet (you need its IP + `SURFACE_TOKEN`).
- GitHub Secrets (reused from rara-core): `SSH_PRIVATE_KEY`, `DEPLOY_HOST` (public IP),
  `DEPLOY_USER` (ubuntu). The console path is hard-coded to `/opt/rara-console` in the workflow.

---

## Step 1 — Create directories and config

```bash
sudo mkdir -p /opt/rara-console/bin
sudo chown -R ubuntu:ubuntu /opt/rara-console

sudo mkdir -p /etc/rara-console
sudo chown ubuntu:ubuntu /etc/rara-console
sudo chmod 750 /etc/rara-console
```

Copy and fill the env file:

```bash
# on your Mac
scp rara-console/deploy/env.example ubuntu@<public-ip>:/etc/rara-console/env
# on the VM
nano /etc/rara-console/env       # set CORE_SURFACE_URL, SURFACE_TOKEN, CONSOLE_ADDR
chmod 640 /etc/rara-console/env
```

| Variable | Value |
|----------|-------|
| `CORE_SURFACE_URL` | `http://100.x.x.x:8080` — rara-core's tailnet IP + port |
| `SURFACE_TOKEN` | Same token as rara-core's `SURFACE_TOKEN` |
| `CONSOLE_ADDR` | `100.x.x.x:8081` — THIS VM's Tailscale IP + console port |

---

## Step 2 — First deploy

```
Actions → Deploy rara-console → Run workflow
```

The workflow builds the SvelteKit SPA, embeds it into the ARM64 Go binary, rsyncs it to
`/opt/rara-console/bin/console`, installs the systemd unit, and starts it.

Verify on the VM:

```bash
sudo systemctl status rara-console --no-pager
journalctl -u rara-console -f
```

Expected log on start:

```
rara-console: listening on 100.x.x.x:8081 (core=http://100.x.x.x:8080)
```

---

## Step 3 — Validate via Tailscale

From your Mac (same tailnet):

```bash
CONSOLE_IP=100.x.x.x

# Console liveness + core reachability (no auth on this endpoint)
curl -s http://${CONSOLE_IP}:8081/healthz        # {"console":true,"core":true}

# The aggregate the Visão geral renders (flows + providers from the live core)
curl -s http://${CONSOLE_IP}:8081/api/overview | jq .
```

Then open `http://${CONSOLE_IP}:8081/` in a browser on the tailnet — the shell renders (Clean by
default) and the Visão geral shows the seeded flows/providers from the core.

---

## Ongoing operations

| Task | Command |
|------|---------|
| Redeploy | `Actions → Deploy rara-console → Run workflow` |
| View logs | `journalctl -u rara-console -f` |
| Restart | `sudo systemctl restart rara-console` |
| Status | `sudo systemctl status rara-console --no-pager` |

---

## Notes

- The console is **Tailscale-only**, same as the core surface. The Oracle public firewall should
  block 8081; allow it only from the Tailscale CGNAT range (`ufw allow in on tailscale0 to any port 8081`).
- **Trust model:** the console's own endpoints (`/api/*`, `/healthz`) are **unauthenticated** — anyone
  on the tailnet reaches core data through the console without a token. That is intentional (the
  tailnet is the trust boundary, exactly like the core surface); the surface bearer token stays
  server-side only to talk to the core. Do not expose the console beyond the tailnet without adding
  edge auth.
- No Neon access and no migrations — the console reads everything through the core surface.
- No Docker: a native ARM64 binary, like rara-core.
