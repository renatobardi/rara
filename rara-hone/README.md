# rara-hone — the interest_profile reviser

`rara-hone` is the **learning loop** of the rara 2.0 pipeline: a **periodic job** that turns
accumulated curation feedback into a revised **`interest_profile`** — the living preferences
document the gates read. It is the slice of the system that lets curation *improve itself* over
time, through a single closed loop over a **human-readable artifact** (no training infra).

It is **not** a claim-worker. There is no [`rara-addon`](../rara-addon) SDK, no per-item routing,
no provider to wake. hone is a **run-once-and-exit batch** fired by a **systemd timer** on the VPC:
each invocation checks whether a revision is due and, if so, proposes one and exits.

## Propose vs. approve

hone **PROPOSES**; a human **APPROVES**. The split is the whole safety story:

- **hone** appends a revision as a **new `proposed` version** (append-only). A proposal is *inert*
  — the gate cascade reads the **`active`** version, never the latest.
- **rara-core's surface** (MCP / HTTP) is where a person **approves** a proposal
  (`ActivateInterestProfile`), demoting the prior active to `superseded`. That approval stays in
  the control plane, behind a human — hone can never activate anything.

## How a revision works (hybrid: deterministic engine + LLM narrator)

The revision has **strictly separated jobs**:

- The **deterministic engine** owns the **structured** change. It aggregates feedback (distillation
  thumbs + quarantine reviews), attributes each signal to the `concepts`/`entities`/`author` of the
  rated distillation's `structured`, scores each term with a **Wilson lower bound** (small-sample
  conservative), and then — under a hard **per-revision ceiling** — promotes reliably-liked terms to
  `topics`/`authors`, demotes reliably-disliked ones to `anti_topics`, and nudges `keep_threshold`
  from the quarantine rescue/confirm rates. It **never invents** a change the counts don't justify.
- The **LLM** (via LiteLLM) owns **only** the natural-language **narrative** — the prose the gate's
  LLM-judge reads as context. It cannot touch a single structured field, and a narrator failure is
  non-fatal (the proposal falls back to a deterministic template).

## The trigger

`shouldRevise` fires when the **cadence** has elapsed *or* enough **new feedback** has accumulated —
but **never** while a proposal already awaits approval (no stacking), **never** within the
**debounce** window of the last revision, and **never** with zero new signal. So the timer can fire
as often as you like; a run with nothing to learn from is a clean no-op.

## What it reads / writes

Per invocation (all over the shared Neon control database, the 1.0 isolation convention — read a
sibling's table, never call it):

1. **Read** the `active` `interest_profile` (the base) and the `feedback` rows created since the
   last revision.
2. **Resolve** each distillation thumb's terms from `distillations.structured` (cross-agent SELECT).
3. **Run** the deterministic engine + the (best-effort) narrator.
4. **Append** the result as a new `proposed` `interest_profile` version.

The engine, the trigger, and the JSON parsing are **pure** (zero I/O), so the whole reviser is
unit-tested with an in-memory mock and fakes. The two I/O seams — the pgx distillation resolver and
the LiteLLM narrator — live in `runners.go`.

## Configuration

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `DATABASE_URL` | ✅ | — | Neon control database |
| `LITELLM_BASE_URL` | — | — | LiteLLM gateway for the narrator (unset → template narrative) |
| `LITELLM_API_KEY` | — | — | Bearer for the gateway |
| `LITELLM_MODEL` | — | `claude-sonnet-4-6` | Narrator model |
| `REVISE_CADENCE_HOURS` | — | `168` (weekly) | Revise at least this often if there's new feedback |
| `REVISE_FEEDBACK_THRESHOLD` | — | `20` | ...or sooner, once this many new signals accumulate |
| `REVISE_DEBOUNCE_HOURS` | — | `24` | Never revise within this window of the last revision |

Flags: `--force` bypasses the cadence/threshold/debounce gate (an operator forcing a revision now);
it still no-ops when there is genuinely no new feedback.

## Run

```bash
make test          # unit tests (zero I/O)
make build-linux   # linux/amd64 binary for the VPC
DATABASE_URL=... ./hone-job            # one revision pass, then exit
DATABASE_URL=... ./hone-job --force    # force the gate (still no-ops with no new feedback)
```

## Deploy

A native binary on the always-on **VPC**, driven by a **systemd timer** (weekly by default). Not a
Cloud Run Job — there is no gate/activation to wake, the timer *is* the trigger. A sketch:

```ini
# /etc/systemd/system/rara-hone.service
[Service]
Type=oneshot
EnvironmentFile=/etc/rara-hone/.env
ExecStart=/opt/rara-hone/hone-job

# /etc/systemd/system/rara-hone.timer
[Timer]
OnCalendar=weekly
Persistent=true
[Install]
WantedBy=timers.target
```

## Boundaries

- **No activation.** hone only writes `proposed`. Approval is rara-core's surface, behind a human.
- **No per-item routing.** hone is off the control plane's claim/route path entirely — it is a
  scheduled batch, not a provider.
- **Isolation is the contract.** hone shares only the Neon tables (`interest_profile`, `feedback`,
  `distillations`); it never calls rara-core or the workers.
