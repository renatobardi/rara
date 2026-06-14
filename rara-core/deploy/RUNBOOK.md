# rara-core VPC Deployment Runbook

One-time setup for the Oracle ARM VM (Ubuntu 22.04 aarch64). After this, every
`workflow_dispatch` on `deploy-core.yml` replaces the binary and restarts the service.

## Prerequisites

- Oracle ARM VM (Ampere A1, 4 OCPU / 24 GB recommended) — Ubuntu 22.04.
- Tailscale installed and authorized on both your Mac and the VM.
- SSH access to the VM's **public IP** as `ubuntu`.
- GitHub Secrets set: `SSH_PRIVATE_KEY`, `DEPLOY_HOST` (public IP), `DEPLOY_USER` (ubuntu),
  `DEPLOY_PATH` (/opt/rara-core).

---

## Step 1 — Install Tailscale on the VM

```bash
curl -fsSL https://tailscale.com/install.sh | sh
sudo tailscale up --authkey=<tskey-auth-...>
tailscale ip -4          # note this address — you need it for SURFACE_ADDR
```

The surface will bind to this Tailscale IP only. Never use `:8080` (that would
expose the surface on the Oracle public interface).

---

## Step 2 — Create directories and config

```bash
sudo mkdir -p /opt/rara-core/bin
sudo chown -R ubuntu:ubuntu /opt/rara-core

sudo mkdir -p /etc/rara-core
sudo chown ubuntu:ubuntu /etc/rara-core
sudo chmod 750 /etc/rara-core
```

Copy and fill in the env file:

```bash
# on your Mac — copy the template
scp rara-core/deploy/env.example ubuntu@<public-ip>:/etc/rara-core/env

# on the VM — edit
nano /etc/rara-core/env
```

Required fields:

| Variable | Value |
|----------|-------|
| `DATABASE_URL` | Neon connection string |
| `SURFACE_TOKEN` | `openssl rand -hex 32` |
| `SURFACE_ADDR` | `100.x.x.x:8080` — Tailscale IP from Step 1 |

Secure it:

```bash
chmod 640 /etc/rara-core/env
```

---

## Step 3 — First deploy

Trigger the workflow from GitHub:

```
Actions → Deploy rara-core → Run workflow
```

The workflow cross-compiles the ARM64 binary on `ubuntu-latest`, rsyncs it to
`/opt/rara-core/bin/core-job`, installs the systemd service, and starts it.

Verify on the VM:

```bash
sudo systemctl status rara-core
journalctl -u rara-core -f        # tail logs
```

Expected log on start:
```
rara-core reconciler: always-on, interval=30s
rara-core surface: listening on 100.x.x.x:8080
```

---

## Step 4 — Seed lane config

One-shot, from the VM:

```bash
/opt/rara-core/bin/core-job seed
```

Expected:
```
rara-core: lane config seeded (youtube, podcast, email, linkedin)
```

This is idempotent — safe to re-run after schema changes.

---

## Step 5 — Validate via Tailscale

From your Mac (on the same Tailnet):

```bash
TAILSCALE_IP=100.x.x.x

# Liveness probe (no auth required)
curl -s http://${TAILSCALE_IP}:8080/healthz

# Authenticated check
TOKEN=$(cat /etc/rara-core/env | grep SURFACE_TOKEN | cut -d= -f2)
curl -s -H "Authorization: Bearer ${TOKEN}" \
  http://${TAILSCALE_IP}:8080/capabilities | jq .
```

---

## Ongoing operations

| Task | Command |
|------|---------|
| Redeploy | `Actions → Deploy rara-core → Run workflow` |
| View logs | `journalctl -u rara-core -f` (on VM) |
| Restart service | `sudo systemctl restart rara-core` |
| Check status | `sudo systemctl status rara-core --no-pager` |
| Force reconcile pass | `sudo systemctl restart rara-core` (loop triggers on start) |
| Seed again | `/opt/rara-core/bin/core-job seed` |

---

## Notes

- The surface is **Tailscale-only**. The Oracle public IP firewall should block port 8080.
  Add an ingress rule allowing 8080 from `100.64.0.0/10` (Tailscale CGNAT range) only, or
  rely on the OS firewall (`ufw allow in on tailscale0 to any port 8080`).
- Cloud Run activation (`CLOUD_RUN_*`) and tailnet pokes (`POKE_AUTH_TOKEN`) are commented
  out in env.example — wire them in a follow-up phase (P2 activators).
- rara-scribe (local Mac) and Cloud Run workers continue running unaffected. The reconciler
  only routes steps; it does not manage those processes.
