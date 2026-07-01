# CONSOLE-#10c3 Surfaces Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add multi-select quick-run to `/distillations` and a live task board at `/tasks`.

**Architecture:** Pure frontend changes — all BFF endpoints already exist from #10c1 (`POST /api/agents/{id}/tasks`, `GET /api/agent-tasks`, `GET /api/agents`). No Go code changes. Three Svelte files touched/created + i18n strings + nav entry.

**Tech Stack:** SvelteKit (Svelte 5 runes), Tailwind CSS, TypeScript.

## Global Constraints

- No new BFF routes — all needed endpoints exist (10c1).
- `instruction` passed to task must be a non-empty string; fall back to `agent.instructions` when the user leaves the modal field blank.
- `context_refs` carries distillation IDs as `number[]`.
- Auto-refresh on the task board uses `setInterval` + `onDestroy`/`$effect` cleanup, same pattern as the task panel in `agents/+page.svelte`.
- All UI strings go in `$lib/strings.ts` — no hardcoded copy in components.
- Run `cd rara-console && make test && make lint` after every Go-touching step (though this plan has none). Run `cd rara-console/web && npx svelte-check` after every Svelte step.
- Commit early and often; each task is independently testable.

---

## File Structure

| File | Action | Purpose |
|------|--------|---------|
| `rara-console/web/src/lib/strings.ts` | Modify | Add `nav.tasks`, `distillations.quickRun*`, `distillations.select*`, `tasks.*` |
| `rara-console/web/src/routes/+layout.svelte` | Modify | Add Tasks link to nav |
| `rara-console/web/src/routes/distillations/+page.svelte` | Modify | Multi-select + quick-run modal |
| `rara-console/web/src/routes/tasks/+page.svelte` | Create | Task board grouped by status |

---

### Task 1: Strings

**Files:**
- Modify: `rara-console/web/src/lib/strings.ts`

**Interfaces:**
- Produces: `t.nav.tasks`, `t.distillations.quickRun*`, `t.distillations.select*`, `t.tasks.*`

- [ ] **Step 1: Add strings to strings.ts**

In `rara-console/web/src/lib/strings.ts`, make the following additions:

**In `nav` block**, after `agents: 'Agents'`:
```typescript
tasks: 'Tarefas',
```

**In `distillations` block**, after `back: '← Voltar'`:
```typescript
selectRow: 'Selecionar distillation',
selectedCount: '{n} selecionada(s)',
clearSelection: 'Limpar seleção',
quickRunBtn: 'Rodar agent',
quickRunTitle: 'Executar agent sobre seleção',
quickRunAgent: 'Agent',
quickRunAgentPlaceholder: '— escolher agent —',
quickRunInstruction: 'Instrução (opcional)',
quickRunInstructionPlaceholder: 'Deixe em branco para usar as instruções do agent',
quickRunConfirm: 'Executar',
quickRunRunning: 'Enfileirando…',
quickRunOk: 'Tarefa enfileirada.',
quickRunError: 'Não foi possível enfileirar a tarefa.',
quickRunAgentsError: 'Não foi possível carregar os agents.',
quickRunAgentRequired: 'Escolha um agent.',
```

**Add new top-level key** after the `agents` block:
```typescript
tasks: {
  title: 'Tarefas',
  loading: 'Carregando tarefas…',
  error: 'Não foi possível carregar as tarefas.',
  empty: 'Nenhuma tarefa ainda.',
  colQueued: 'Na fila',
  colRunning: 'Executando',
  colDone: 'Concluído',
  colFailed: 'Falhou',
  colCancelled: 'Cancelado',
  showResult: 'Ver resultado',
  labelInstruction: 'Instrução',
  labelContext: 'Contexto (ids)',
  labelCreated: 'Criado',
  labelCompleted: 'Concluído em',
  labelError: 'Erro',
},
```

- [ ] **Step 2: Verify svelte-check still passes**

```bash
cd rara-console/web && npx svelte-check --no-tsconfig 2>&1 | tail -5
```

Expected: `0 errors` (or same count as before if any pre-existing).

- [ ] **Step 3: Commit**

```bash
git add rara-console/web/src/lib/strings.ts
git commit -m "feat(console): 10c3 — i18n strings for quick-run + task board"
```

---

### Task 2: Nav — Tasks link

**Files:**
- Modify: `rara-console/web/src/routes/+layout.svelte`

**Interfaces:**
- Consumes: `t.nav.tasks` (Task 1)
- Produces: `/tasks` route in the sidebar nav

- [ ] **Step 1: Add Tasks to nav array**

In `rara-console/web/src/routes/+layout.svelte`, find the `nav` const and add after `{ icon: '◆', label: t.nav.agents, href: '/agents' }`:

```typescript
{ icon: '⊡', label: t.nav.tasks, href: '/tasks' },
```

The nav block should look like:
```typescript
const nav = [
  { icon: '◍', label: t.nav.overview, href: '/' },
  { icon: '▤', label: t.nav.pipeline, href: '/pipeline' },
  { icon: '✦', label: t.nav.distillations, href: '/distillations' },
  { section: t.nav.secTrain },
  { icon: '◐', label: t.nav.curation, href: '/curadoria' },
  { icon: '⛁', label: t.nav.fontes, href: '/fontes' },
  { icon: '⚡', label: t.nav.workers, href: '/workers' },
  { icon: '⊹', label: t.nav.inferencia, href: '/inferencia' },
  { icon: '◆', label: t.nav.agents, href: '/agents' },
  { icon: '⊡', label: t.nav.tasks, href: '/tasks' },
  { section: t.nav.secSystem },
  { icon: '≣', label: t.nav.audit, href: '/auditoria' },
  { icon: '⚙', label: t.nav.settings, href: '/configuracoes' }
];
```

- [ ] **Step 2: svelte-check**

```bash
cd rara-console/web && npx svelte-check --no-tsconfig 2>&1 | tail -5
```

Expected: 0 errors.

- [ ] **Step 3: Commit**

```bash
git add rara-console/web/src/routes/+layout.svelte
git commit -m "feat(console): 10c3 — add Tasks link to nav"
```

---

### Task 3: Task board page

**Files:**
- Create: `rara-console/web/src/routes/tasks/+page.svelte`

**Interfaces:**
- Consumes: `GET /api/agent-tasks` (no ?status filter — load all, group client-side), `t.tasks.*` (Task 1)
- Produces: `/tasks` route showing 5 status columns with live refresh every 5s

- [ ] **Step 1: Create the task board page**

Create `rara-console/web/src/routes/tasks/+page.svelte` with this content:

```svelte
<script lang="ts">
  import { onDestroy } from 'svelte';
  import { t } from '$lib/strings';

  type AgentTask = {
    id: number;
    agent_id: number;
    agent_name?: string;
    instruction: string;
    status: 'queued' | 'dispatched' | 'running' | 'done' | 'failed' | 'cancelled';
    context_refs?: number[];
    priority: number;
    result?: unknown;
    error?: string;
    created_at: string;
    completed_at?: string;
  };

  const COLS: { key: string; label: string; statuses: AgentTask['status'][] }[] = [
    { key: 'queued',    label: t.tasks.colQueued,    statuses: ['queued'] },
    { key: 'running',   label: t.tasks.colRunning,   statuses: ['dispatched', 'running'] },
    { key: 'done',      label: t.tasks.colDone,      statuses: ['done'] },
    { key: 'failed',    label: t.tasks.colFailed,    statuses: ['failed'] },
    { key: 'cancelled', label: t.tasks.colCancelled, statuses: ['cancelled'] },
  ];

  let tasks = $state<AgentTask[]>([]);
  let loading = $state(true);
  let error = $state(false);
  let fetchSeq = 0;
  let expanded = $state<Set<number>>(new Set());

  function tasksForCol(col: typeof COLS[number]) {
    return tasks.filter((t) => col.statuses.includes(t.status));
  }

  function toggleExpand(id: number) {
    const next = new Set(expanded);
    if (next.has(id)) next.delete(id); else next.add(id);
    expanded = next;
  }

  function statusBadgeClass(status: string): string {
    const map: Record<string, string> = {
      queued:     'bg-muted/20 text-muted',
      dispatched: 'bg-blue-500/20 text-blue-600',
      running:    'bg-yellow-500/20 text-yellow-700',
      done:       'bg-green-500/20 text-green-700',
      failed:     'bg-red-500/20 text-red-600',
      cancelled:  'bg-muted/30 text-muted',
    };
    return map[status] ?? 'bg-muted/20 text-muted';
  }

  async function fetchTasks() {
    const seq = ++fetchSeq;
    try {
      const r = await fetch('/api/agent-tasks');
      if (seq !== fetchSeq) return;
      if (r.ok) {
        const data = await r.json();
        tasks = Array.isArray(data) ? data : [];
        error = false;
      } else {
        error = true;
      }
    } catch {
      if (seq === fetchSeq) error = true;
    } finally {
      if (seq === fetchSeq) loading = false;
    }
  }

  fetchTasks();
  const poll = setInterval(fetchTasks, 5000);
  onDestroy(() => clearInterval(poll));
</script>

<section>
  <h2 class="mb-4 text-[15px] font-semibold">{t.tasks.title}</h2>

  {#if loading}
    <p class="text-[13px] text-muted">{t.tasks.loading}</p>
  {:else if error}
    <p class="text-[13px] text-red-500">{t.tasks.error}</p>
  {:else if tasks.length === 0}
    <p class="text-[13px] text-muted">{t.tasks.empty}</p>
  {:else}
    <div class="grid gap-3" style="grid-template-columns: repeat({COLS.length}, minmax(0, 1fr))">
      {#each COLS as col (col.key)}
        {@const colTasks = tasksForCol(col)}
        <div class="flex flex-col gap-2">
          <div class="flex items-center gap-1.5">
            <span class="text-[11px] font-semibold uppercase tracking-wide text-muted">{col.label}</span>
            {#if colTasks.length > 0}
              <span class="rounded-full bg-surface-2 px-1.5 py-0.5 text-[10px] text-muted">{colTasks.length}</span>
            {/if}
          </div>
          {#if colTasks.length === 0}
            <p class="text-[11px] text-muted/50">—</p>
          {:else}
            <ul class="flex flex-col gap-2">
              {#each colTasks as task (task.id)}
                <li class="rounded-xl border border-border bg-surface p-3">
                  <p class="line-clamp-2 text-[12px] text-text">{task.instruction}</p>
                  <p class="mt-1 text-[11px] text-muted">
                    #{task.id}
                    {#if task.agent_name}· {task.agent_name}{/if}
                    · {new Date(task.created_at).toLocaleString()}
                  </p>
                  {#if task.error}
                    <p class="mt-1 text-[11px] text-red-500 line-clamp-2">{task.error}</p>
                  {/if}
                  {#if task.result || (task.context_refs && task.context_refs.length > 0)}
                    <button
                      class="mt-1.5 text-[11px] text-muted underline hover:text-text"
                      onclick={() => toggleExpand(task.id)}
                    >{expanded.has(task.id) ? '▲ fechar' : t.tasks.showResult}</button>
                    {#if expanded.has(task.id)}
                      <div class="mt-2 space-y-1">
                        {#if task.context_refs && task.context_refs.length > 0}
                          <p class="text-[11px] text-muted">
                            <span class="font-semibold">{t.tasks.labelContext}:</span>
                            {task.context_refs.join(', ')}
                          </p>
                        {/if}
                        {#if task.result}
                          <pre class="max-h-40 overflow-auto rounded bg-surface-2 p-2 text-[11px] text-text">{JSON.stringify(task.result, null, 2)}</pre>
                        {/if}
                      </div>
                    {/if}
                  {/if}
                </li>
              {/each}
            </ul>
          {/if}
        </div>
      {/each}
    </div>
  {/if}
</section>
```

- [ ] **Step 2: svelte-check**

```bash
cd rara-console/web && npx svelte-check --no-tsconfig 2>&1 | tail -5
```

Expected: 0 errors.

- [ ] **Step 3: Commit**

```bash
git add rara-console/web/src/routes/tasks/+page.svelte
git commit -m "feat(console): 10c3 — task board at /tasks with live polling"
```

---

### Task 4: Distillations quick-run

**Files:**
- Modify: `rara-console/web/src/routes/distillations/+page.svelte`

**Interfaces:**
- Consumes: `GET /api/agents` (for the agent picker), `POST /api/agents/{id}/tasks` (for enqueue), `t.distillations.quickRun*`, `t.distillations.select*` (Task 1)
- Produces: checkbox per card, bulk action bar, quick-run modal

This replaces the entire 72-line file with the expanded version below.

- [ ] **Step 1: Rewrite distillations/+page.svelte**

Full content of `rara-console/web/src/routes/distillations/+page.svelte`:

```svelte
<script lang="ts">
  import { t } from '$lib/strings';
  import Paginator from '$lib/Paginator.svelte';

  type Distillation = {
    id: number;
    source_type: string;
    source_ref: string;
    title?: string;
    doc_context?: string;
    engine: string;
    status: string;
  };

  type Agent = { id: number; name: string; instructions: string };

  const STATUS_COLOR: Record<string, string> = {
    done: 'bg-green',
    failed: 'bg-red',
    filtered: 'bg-muted'
  };

  // ── list ──
  let items = $state<Distillation[]>([]);
  let loading = $state(true);
  let error = $state(false);

  $effect(() => {
    fetch('/api/distillations?limit=50')
      .then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
      .then((d) => (items = d))
      .catch(() => (error = true))
      .finally(() => (loading = false));
  });

  // ── selection ──
  let selectedIds = $state<number[]>([]);
  const isSelected = (id: number) => selectedIds.includes(id);
  function toggleRow(id: number) {
    selectedIds = isSelected(id) ? selectedIds.filter((x) => x !== id) : [...selectedIds, id];
  }
  function clearSelection() { selectedIds = []; }

  // ── toasts ──
  type Toast = { id: number; kind: 'ok' | 'err'; msg: string };
  let toasts = $state<Toast[]>([]);
  let toastSeq = 0;
  const toastTimers: ReturnType<typeof setTimeout>[] = [];
  function toast(kind: 'ok' | 'err', msg: string) {
    const id = ++toastSeq;
    toasts = [...toasts, { id, kind, msg }];
    toastTimers.push(setTimeout(() => (toasts = toasts.filter((x) => x.id !== id)), 4000));
  }
  $effect(() => () => toastTimers.forEach(clearTimeout));

  // ── quick-run modal ──
  let modalOpen = $state(false);
  let agents = $state<Agent[]>([]);
  let agentsLoading = $state(false);
  let agentsError = $state(false);
  let selectedAgentId = $state<number | null>(null);
  let quickInstruction = $state('');
  let modalErrors = $state<Record<string, string>>({});
  let submitting = $state(false);

  async function openModal() {
    modalOpen = true;
    selectedAgentId = null;
    quickInstruction = '';
    modalErrors = {};
    agentsError = false;
    agentsLoading = true;
    try {
      const r = await fetch('/api/agents');
      agents = r.ok ? (await r.json()) : [];
      if (!r.ok) agentsError = true;
    } catch {
      agents = [];
      agentsError = true;
    } finally {
      agentsLoading = false;
    }
  }

  async function submitQuickRun() {
    const errs: Record<string, string> = {};
    if (!selectedAgentId) errs.agent = t.distillations.quickRunAgentRequired;
    modalErrors = errs;
    if (Object.keys(errs).length) return;

    const agent = agents.find((a) => a.id === selectedAgentId)!;
    const instruction = quickInstruction.trim() || agent.instructions;
    submitting = true;
    try {
      const r = await fetch(`/api/agents/${selectedAgentId}/tasks`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ instruction, context_refs: selectedIds })
      });
      if (!r.ok) { toast('err', t.distillations.quickRunError); return; }
      toast('ok', t.distillations.quickRunOk);
      modalOpen = false;
      clearSelection();
    } catch {
      toast('err', t.distillations.quickRunError);
    } finally {
      submitting = false;
    }
  }

  function closeModal() { if (!submitting) modalOpen = false; }

  function onModalKeydown(e: KeyboardEvent) {
    if (e.key === 'Escape') closeModal();
  }
</script>

<svelte:window onkeydown={onModalKeydown} />

{#if loading}
  <p class="text-muted">{t.distillations.loading}</p>
{:else if error}
  <p class="text-red">{t.distillations.error}</p>
{:else if items.length === 0}
  <p class="text-[13px] text-muted">{t.distillations.empty}</p>
{:else}
  <!-- bulk action bar -->
  {#if selectedIds.length > 0}
    <div class="mb-3 flex items-center gap-3 rounded-xl border border-border bg-surface p-3">
      <span class="text-[13px] text-muted">
        {t.distillations.selectedCount.replace('{n}', String(selectedIds.length))}
      </span>
      <button
        class="rounded-token bg-text px-3 py-1.5 text-[12px] font-medium text-bg hover:opacity-90"
        onclick={openModal}
      >{t.distillations.quickRunBtn}</button>
      <button
        class="ml-auto text-[12px] text-muted underline hover:text-text"
        onclick={clearSelection}
      >{t.distillations.clearSelection}</button>
    </div>
  {/if}

  <Paginator {items}>
    {#snippet children(page)}
      <div class="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
        {#each page as d}
          <label
            class="flex cursor-pointer flex-col gap-2 rounded-card border bg-surface p-4 hover:bg-hover {isSelected(d.id) ? 'border-text' : 'border-border'}"
          >
            <div class="flex items-start justify-between gap-2">
              <input
                type="checkbox"
                class="mt-0.5 h-3.5 w-3.5 flex-none accent-green"
                checked={isSelected(d.id)}
                onchange={() => toggleRow(d.id)}
                aria-label="{t.distillations.selectRow}: {d.title ?? d.source_ref}"
              />
              <a
                href="/distillations/{d.id}"
                class="min-w-0 flex-1 no-underline"
                onclick={(e) => e.stopPropagation()}
              >
                <span class="line-clamp-2 text-[13.5px] font-medium text-text">
                  {d.title ?? `${d.source_type} · ${d.source_ref}`}
                </span>
              </a>
              <span
                class="mt-0.5 h-[7px] w-[7px] flex-none rounded-full {STATUS_COLOR[d.status] ?? 'bg-amber'}"
              ></span>
            </div>
            {#if d.doc_context}
              <p class="m-0 line-clamp-2 text-[12px] text-muted">{d.doc_context}</p>
            {/if}
            <div class="mt-auto flex items-center gap-2 text-[11px] text-muted">
              <span>{d.engine}</span>
              <span>·</span>
              <span>{d.source_type}</span>
            </div>
          </label>
        {/each}
      </div>
    {/snippet}
  </Paginator>
{/if}

<!-- quick-run modal -->
{#if modalOpen}
  <!-- svelte-ignore a11y_click_events_have_key_events -->
  <div
    role="presentation"
    class="fixed inset-0 z-50 flex items-center justify-center p-4"
    style="background:rgba(0,0,0,0.35)"
    onclick={(e) => { if (e.target === e.currentTarget) closeModal(); }}
  >
    <div
      class="w-full max-w-md rounded-xl border border-border bg-bg p-5 shadow-2xl"
      role="dialog"
      aria-modal="true"
      aria-labelledby="qr-title"
    >
      <h3 id="qr-title" class="mb-4 text-[14px] font-semibold">{t.distillations.quickRunTitle}</h3>
      <p class="mb-3 text-[12px] text-muted">
        {t.distillations.selectedCount.replace('{n}', String(selectedIds.length))}
      </p>

      <div class="grid gap-4">
        <div>
          <label class="mb-1 block text-[11px] font-semibold uppercase tracking-wide text-muted" for="qr-agent">
            {t.distillations.quickRunAgent}
          </label>
          {#if agentsLoading}
            <p class="text-[12px] text-muted">{t.agents.loading}</p>
          {:else if agentsError}
            <p class="text-[12px] text-red-500">{t.distillations.quickRunAgentsError}</p>
          {:else}
            <select
              id="qr-agent"
              class="w-full rounded-token border border-border bg-bg px-3 py-1.5 text-[13px] text-text focus:border-text focus:outline-none"
              bind:value={selectedAgentId}
            >
              <option value={null}>{t.distillations.quickRunAgentPlaceholder}</option>
              {#each agents as a (a.id)}
                <option value={a.id}>{a.name}</option>
              {/each}
            </select>
          {/if}
          {#if modalErrors.agent}<p class="mt-0.5 text-[11px] text-red-500">{modalErrors.agent}</p>{/if}
        </div>

        <div>
          <label class="mb-1 block text-[11px] font-semibold uppercase tracking-wide text-muted" for="qr-instruction">
            {t.distillations.quickRunInstruction}
          </label>
          <textarea
            id="qr-instruction"
            class="w-full resize-none rounded-token border border-border bg-bg px-3 py-1.5 text-[13px] text-text placeholder:text-muted focus:border-text focus:outline-none"
            rows="2"
            placeholder={t.distillations.quickRunInstructionPlaceholder}
            bind:value={quickInstruction}
            disabled={submitting}
          ></textarea>
        </div>

        <div class="flex gap-2">
          <button
            class="rounded-token bg-text px-3.5 py-1.5 text-[13px] font-medium text-bg hover:opacity-90 disabled:opacity-50"
            disabled={submitting || agentsLoading || agentsError}
            onclick={submitQuickRun}
          >{submitting ? t.distillations.quickRunRunning : t.distillations.quickRunConfirm}</button>
          <button
            class="rounded-token border border-border px-3.5 py-1.5 text-[13px] text-muted hover:bg-hover"
            onclick={closeModal}
            disabled={submitting}
          >{t.agents.cancel}</button>
        </div>
      </div>
    </div>
  </div>
{/if}

<!-- toasts -->
{#if toasts.length > 0}
  <div class="fixed bottom-4 right-4 z-[60] flex flex-col gap-2">
    {#each toasts as tst (tst.id)}
      <div
        class="rounded-token border px-4 py-2 text-[13px] shadow-lg {tst.kind === 'ok'
          ? 'border-green/40 bg-surface-2 text-text'
          : 'border-red-500/40 bg-surface-2 text-red-500'}"
        role={tst.kind === 'ok' ? 'status' : 'alert'}
      >{tst.msg}</div>
    {/each}
  </div>
{/if}
```

- [ ] **Step 2: svelte-check**

```bash
cd rara-console/web && npx svelte-check --no-tsconfig 2>&1 | tail -5
```

Expected: 0 errors.

- [ ] **Step 3: Verify behavior manually** (start dev server if available)

1. Open `/distillations` — cards should have checkboxes in top-left.
2. Click a checkbox — card border changes to `border-text`, bulk bar appears.
3. Click "Rodar agent" — modal opens, agents load in dropdown.
4. Pick an agent, leave instruction blank, click "Executar" — task enqueued, toast "Tarefa enfileirada.", modal closes, selection clears.
5. Open `/tasks` — task appears in "Na fila" or "Executando" column.

- [ ] **Step 4: Commit**

```bash
git add rara-console/web/src/routes/distillations/+page.svelte
git commit -m "feat(console): 10c3 — multi-select + quick-run modal in /distillations"
```

---

### Task 5: make test + make lint + final svelte-check

**Files:** none

- [ ] **Step 1: Run Go tests and lint**

```bash
cd rara-console && make test
```

Expected: all tests pass (no Go code was changed, so all prior tests should still pass).

```bash
cd rara-console && make lint
```

Expected: 0 issues.

- [ ] **Step 2: Final svelte-check**

```bash
cd rara-console/web && npx svelte-check --no-tsconfig 2>&1 | tail -10
```

Expected: 0 errors.

- [ ] **Step 3: CodeRabbit loop**

Run CodeRabbit review and fix any findings:

```
/coderabbit:review uncommitted
```

Fix all findings (security first, then quality). Re-review until clean.

---

## Self-Review

### Spec coverage

| Requirement | Task |
|------------|------|
| Multi-select in /distillations | Task 4 |
| "Rodar agent" button | Task 4 |
| Agent picker modal | Task 4 |
| Optional instruction field | Task 4 |
| `context_refs` populated from selection | Task 4 — `body: JSON.stringify({ instruction, context_refs: selectedIds })` |
| POST to `/api/agents/{id}/tasks` | Task 4 — `fetch('/api/agents/${selectedAgentId}/tasks', { method: 'POST', ... })` |
| Task board at new route | Task 3 |
| Grouped by status columns | Task 3 — `COLS` array with `tasksForCol()` |
| Auto-refresh | Task 3 — `setInterval(fetchTasks, 5000)` + `onDestroy` |
| Task detail: instruction, context, result, error | Task 3 — expandable per task |
| Nav entry | Task 2 |
| No backend changes | All tasks — no `.go` files modified |
| `make test` + `make lint` green | Task 5 |
| `svelte-check 0` | Tasks 1–4 each verify incrementally |

### Placeholder scan

None found — all code is complete.

### Type consistency

- `selectedIds: number[]` matches `Distillation.id: number` ✓
- `context_refs: selectedIds` passed as `number[]` in POST body ✓
- `AgentTask.context_refs?: number[]` in task board type ✓
- `selectedAgentId: number | null` compared with `a.id: number` ✓
- `t.distillations.quickRun*` keys match between strings.ts additions and component usage ✓
- `t.tasks.*` keys match between strings.ts and tasks page ✓
