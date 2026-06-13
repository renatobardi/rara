# rara-mail

The 2.0 **Email lane** collector. An isolated agent that collects messages from Gmail (OAuth
refresh-token auth, the same pattern as `rara-shelf`) and catalogs each into its own domain
table, `emails`.

Like every rara agent it shares nothing but the Neon database and never calls another agent. The
control plane (`rara-core`) reads `emails` to build the items spine
(`lane=email`, `source_ref=message_id`, **`sensitivity=private`**), and the `extrair` worker
reads `body` to clean it (strip HTML/signature/quoted-reply) into a to-text artifact — both
cross-agent SELECTs, never writes.

**Privacy.** Email content is private. This agent only *stores* it; the guarantee that private
content is processed *only* by local/self-host models is enforced by `rara-core`'s router
(items tagged `private` exclude any provider tagged `third_party`).

## Table (`migrations/001_initial_schema.sql`)

- `emails` — one row per message (`message_id` UNIQUE = the spine's `source_ref`, `sender`,
  `subject`, `body` raw text/HTML, `received_at`).

## Run

```bash
export DATABASE_URL=postgresql://...
export GOOGLE_OAUTH_CLIENT_ID=... GOOGLE_OAUTH_CLIENT_SECRET=... GOOGLE_OAUTH_REFRESH_TOKEN=...
export GMAIL_QUERY="newer_than:30d"   # optional; which messages to collect
export MAIL_MAX=100                    # optional; cap per run
go run .
```

The OAuth refresh token needs the `gmail.readonly` scope. Re-running converges: emails upsert on
`message_id`, so a re-collect never duplicates and refreshes edited metadata.

## Design

The OAuth exchange and Gmail HTTP calls live behind a `GmailAPI` seam; the Neon write behind a
`Database` seam. The tricky parsing — headers, the recursive MIME tree, URL-safe base64 bodies,
`internalDate` — is in pure functions (`parseMessageJSON`, `parseMessageListJSON`,
`decodeB64URL`), so the whole logic is unit-tested with zero I/O (`make test`). Downstream
cleaning, gating, and distillation are driven entirely by `rara-core`.
