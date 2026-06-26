<script lang="ts">
	import { onMount } from 'svelte';
	import { t } from '$lib/strings';
	import { timeAgo } from '$lib/timeAgo';
	import WorkerForm from '$lib/WorkerForm.svelte';

	const GLOBAL_SCOPE = 'global' as const;

	type Constraints = {
		requires?: string;
		accepts?: string[];
		sensitivity?: string;
	};

	type Provider = {
		name: string;
		worker?: string;
		app?: string;
		capability: string;
		runtime: string;
		activation: string;
		enabled: boolean;
		heartbeat_at?: string;
		last_collect_at?: string;
		last_error?: string;
		constraints?: Constraints;
		runner_url?: string;
		env?: Record<string, string>;
		description?: string;
	};

	type Worker = {
		name: string;
		capability: string;
		placements: Provider[];
	};

	type RoutingPolicy = {
		scope: string;
		fallback: string[];
	};

	type ByStatus = {
		pending: number;
		assigned: number;
		running: number;
		done: number;
		failed: number;
		skipped: number;
	};

	type WorkerMetric = {
		provider: string;
		total: number;
		by_status?: ByStatus;
		done: number;
		failed: number;
		success_rate: number;
		queue: number;
		avg_attempt: number;
		last_activity_at?: string;
	};

	function isProvider(v: unknown): v is Provider {
		if (typeof v !== 'object' || v === null) return false;
		const p = v as Record<string, unknown>;
		return (
			typeof p.name === 'string' && typeof p.capability === 'string' &&
			typeof p.runtime === 'string' && typeof p.activation === 'string' &&
			typeof p.enabled === 'boolean'
		);
	}

	function isWorker(v: unknown): v is Worker {
		if (typeof v !== 'object' || v === null) return false;
		const w = v as Record<string, unknown>;
		return (
			typeof w.name === 'string' && typeof w.capability === 'string' &&
			Array.isArray(w.placements) && w.placements.every(isProvider)
		);
	}

	// --- existing state ---
	let workers = $state<Worker[]>([]);
	// providers is a flat derived list used by health cards, routing editor, and route preview.
	let providers = $derived(workers.flatMap((w) => w.placements));
	let policies = $state<RoutingPolicy[]>([]);
	let loading = $state(true);
	let error = $state(false);
	let policiesLoading = $state(true);
	let policiesError = $state(false);
	let saving = $state<string | null>(null);
	let saveMsg = $state('');
	let expandedWorkers = $state<Set<string>>(new Set());

	// --- metrics state ---
	const PERIODS = [
		{ label: t.workers.period1d, days: 1 as number | null },
		{ label: t.workers.period7d, days: 7 as number | null },
		{ label: t.workers.period30d, days: 30 as number | null },
		{ label: t.workers.periodAll, days: null as number | null }
	];

	let selectedDays = $state<number | null>(7);
	let metricsAll = $state<WorkerMetric[]>([]);
	let metricsWindow = $state<WorkerMetric[]>([]);
	let metricsAllLoading = $state(true);
	let metricsAllError = $state(false);
	let metricsWindowLoading = $state(true);
	let metricsWindowError = $state(false);

	// --- CRUD form state ---
	let formMode = $state<'add' | 'edit' | null>(null);
	let formInitial = $state<Provider | null>(null);
	let formLockedWorker = $state<string | null>(null);
	let formLockedCapability = $state<string | null>(null);
	let formLockedConstraints = $state<Constraints | null>(null);
	let formLockedApp = $state<string | null>(null);

	// --- routing editor state ---
	let selectedScope = $state<string>(GLOBAL_SCOPE);
	let editFallback = $state<string[]>([]);
	let routingAddWorker = $state('');
	let routingSaving = $state(false);
	let routingMsg = $state('');

	// ponytail: 5min stale threshold mirrors defaultHealthTTL in core router.go
	const STALE_MS = 5 * 60 * 1000;

	// request counter to discard out-of-order responses on rapid period switching
	let windowReqId = 0;

	function workerStatus(p: Provider): 'alive' | 'stale' | 'asleep' {
		if (p.activation === 'on_demand') return 'alive';
		if (!p.heartbeat_at) return 'asleep';
		return Date.now() - new Date(p.heartbeat_at).getTime() > STALE_MS ? 'stale' : 'alive';
	}

	// health card (ao vivo — uses /api/workers heartbeat data + all-time last_activity_at)
	let aliveCount = $derived(providers.filter((p) => workerStatus(p) === 'alive').length);
	let staleCount = $derived(providers.filter((p) => workerStatus(p) === 'stale').length);
	let asleepCount = $derived(providers.filter((p) => workerStatus(p) === 'asleep').length);
	let lastActivity = $derived(
		metricsAll.reduce((best, m) => {
			if (!m.last_activity_at) return best;
			return !best || m.last_activity_at > best ? m.last_activity_at : best;
		}, '' as string)
	);

	// all-time last activity by placement name (item_steps workers: distill, gate, extract, …)
	let lastActivityByProvider = $derived(
		new Map(metricsAll.map((m) => [m.provider, m.last_activity_at]))
	);
	// "última execução" per placement: collectors carry last_collect_at; item_steps workers
	// fall back to last_activity_at. undefined = never ran.
	function lastRun(p: Provider): string | undefined {
		return p.last_collect_at ?? lastActivityByProvider.get(p.name);
	}

	// reliability card (windowed)
	let totalDoneW = $derived(metricsWindow.reduce((s, m) => s + m.done, 0));
	let totalFailedW = $derived(metricsWindow.reduce((s, m) => s + m.failed, 0));
	let successRate = $derived(
		totalDoneW + totalFailedW > 0
			? Math.round((totalDoneW / (totalDoneW + totalFailedW)) * 100)
			: null
	);
	let topErrorWorker = $derived(
		metricsWindow.length > 0
			? [...metricsWindow].sort((a, b) => b.failed - a.failed)[0]
			: null
	);

	// volume card (windowed)
	let volumeShares = $derived(
		totalDoneW > 0
			? [...metricsWindow]
					.filter((m) => m.done > 0)
					.sort((a, b) => b.done - a.done)
					.slice(0, 3)
					.map((m) => `${m.provider} ${Math.round((m.done / totalDoneW) * 100)}%`)
			: []
	);

	// queue card (ao vivo — uses all-time fetch for current in-flight items)
	let totalQueue = $derived(metricsAll.reduce((s, m) => s + m.queue, 0));
	let pendingTotal = $derived(metricsAll.reduce((s, m) => s + (m.by_status?.pending ?? 0), 0));
	let assignedTotal = $derived(metricsAll.reduce((s, m) => s + (m.by_status?.assigned ?? 0), 0));
	let runningTotal = $derived(metricsAll.reduce((s, m) => s + (m.by_status?.running ?? 0), 0));

	// unique capabilities for datalist
	let knownCapabilities = $derived([...new Set(providers.map((p) => p.capability))].sort());

	// routing editor derived
	let routingScopes = $derived([
		{ id: GLOBAL_SCOPE, label: t.workers.policyScopeGlobal },
		...knownCapabilities.filter((c) => c !== GLOBAL_SCOPE).map((c) => ({ id: c, label: c }))
	]);
	let fallbackAvailable = $derived(
		selectedScope === GLOBAL_SCOPE
			? providers
			: providers.filter((p) => p.capability === selectedScope)
	);
	let fallbackAddable = $derived(
		fallbackAvailable.filter((p) => !editFallback.includes(p.name))
	);

	function selectScope(scope: string) {
		selectedScope = scope;
		const pol = policies.find((p) => p.scope === scope);
		editFallback = pol ? (pol.fallback ?? []) : [];
		routingMsg = '';
		routingAddWorker = '';
	}

	function moveFallback(idx: number, dir: -1 | 1) {
		const newIdx = idx + dir;
		if (newIdx < 0 || newIdx >= editFallback.length) return;
		const arr = [...editFallback];
		[arr[idx], arr[newIdx]] = [arr[newIdx], arr[idx]];
		editFallback = arr;
	}

	function removeFallback(idx: number) {
		editFallback = editFallback.filter((_, i) => i !== idx);
	}

	function addFallback(name: string) {
		if (!name || editFallback.includes(name)) return;
		if (!fallbackAvailable.some((p) => p.name === name)) return;
		editFallback = [...editFallback, name];
	}

	async function saveRoutingPolicy() {
		routingSaving = true;
		routingMsg = '';
		const existing = policies.find((p) => p.scope === selectedScope);
		const payload: RoutingPolicy = {
			scope: selectedScope,
			fallback: editFallback
		};
		try {
			const res = await fetch('/api/routing-policies', {
				method: 'PUT',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify(payload)
			});
			if (!res.ok) throw new Error();
			const idx = policies.findIndex((p) => p.scope === selectedScope);
			if (idx >= 0) {
				policies = policies.map((p, i) => (i === idx ? payload : p));
			} else {
				policies = [...policies, payload];
			}
			routingMsg = t.workers.policySaveOk;
		} catch {
			routingMsg = t.workers.policySaveError;
		} finally {
			routingSaving = false;
		}
	}

	function fetchWorkers() {
		loading = true;
		error = false;
		fetch('/api/workers')
			.then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
			.then((d) => {
				if (!Array.isArray(d) || !d.every(isWorker)) throw new Error('unexpected payload');
				workers = d;
				loading = false;
			})
			.catch(() => {
				error = true;
				loading = false;
			});
	}

	function fetchMetricsAll() {
		metricsAllLoading = true;
		metricsAllError = false;
		fetch('/api/workers/metrics')
			.then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
			.then((d) => {
				metricsAll = Array.isArray(d) ? d : [];
				metricsAllLoading = false;
			})
			.catch(() => {
				metricsAllError = true;
				metricsAllLoading = false;
			});
	}

	function fetchMetricsWindow(days: number | null) {
		metricsWindowLoading = true;
		metricsWindowError = false;
		const reqId = ++windowReqId;
		const url = days !== null ? `/api/workers/metrics?days=${days}` : '/api/workers/metrics';
		fetch(url)
			.then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
			.then((d) => {
				if (reqId !== windowReqId) return;
				metricsWindow = Array.isArray(d) ? d : [];
				metricsWindowLoading = false;
			})
			.catch(() => {
				if (reqId !== windowReqId) return;
				metricsWindowError = true;
				metricsWindowLoading = false;
			});
	}

	function selectPeriod(days: number | null) {
		selectedDays = days;
		try {
			localStorage.setItem('workers-period', String(days));
		} catch { /* ignore */ }
		fetchMetricsWindow(days);
	}

	function fmtDate(iso: string): string {
		if (!iso) return t.workers.never;
		return new Date(iso).toLocaleString('pt-BR', {
			day: '2-digit',
			month: '2-digit',
			hour: '2-digit',
			minute: '2-digit'
		});
	}

	onMount(() => {
		// restore period preference
		try {
			const saved = localStorage.getItem('workers-period');
			if (saved === 'null') {
				selectedDays = null;
			} else if (saved) {
				const n = parseInt(saved, 10);
				if ([1, 7, 30].includes(n)) selectedDays = n;
			}
		} catch { /* ignore */ }

		fetchWorkers();

		fetch('/api/routing-policies')
			.then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
			.then((d) => {
				if (!Array.isArray(d)) throw new Error('unexpected payload');
				policies = d;
				policiesLoading = false;
				selectScope(GLOBAL_SCOPE);
			})
			.catch(() => {
				policiesError = true;
				policiesLoading = false;
			});

		fetchMetricsAll();
		fetchMetricsWindow(selectedDays);
	});

	async function toggleProvider(p: Provider, workerName: string) {
		saving = p.name;
		saveMsg = '';
		try {
			// ponytail: explicit DTO — omit heartbeat_at (core preserves it from DB on upsert).
			// description/env must stay: full-record upsert, omitting them would clear the DB columns.
			const dto = {
				worker: workerName, name: p.name, app: p.app, capability: p.capability, runtime: p.runtime,
				activation: p.activation,
				enabled: !p.enabled, constraints: p.constraints,
				runner_url: p.runner_url, env: p.env, description: p.description
			};
			const optimistic: Provider = { ...p, worker: workerName, enabled: !p.enabled };
			const res = await fetch('/api/placements', {
				method: 'PUT',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify(dto)
			});
			if (res.ok) {
				workers = workers.map((w) => ({
					...w,
					placements: w.placements.map((x) => (x.name === p.name ? optimistic : x))
				}));
				saveMsg = t.workers.saveOk;
			} else {
				saveMsg = t.workers.saveError;
			}
		} catch {
			saveMsg = t.workers.saveError;
		} finally {
			saving = null;
		}
	}

	function openAdd() {
		formInitial = null;
		formMode = 'add';
		formLockedWorker = null;
		formLockedCapability = null;
		formLockedConstraints = null;
		saveMsg = '';
	}

	function openAddPlacement(w: Worker) {
		formInitial = null;
		formMode = 'add';
		formLockedWorker = w.name;
		formLockedCapability = w.capability;
		// any placement carries the worker's constraints (per-worker invariant in the data model)
		formLockedConstraints = w.placements.find((p) => p.constraints)?.constraints ?? null;
		// inherit app from existing siblings so the new placement doesn't fall back to app=name
		formLockedApp = w.placements[0]?.app || null;
		saveMsg = '';
		// ensure worker row is expanded so the inline form is visible
		const next = new Set(expandedWorkers);
		next.add(w.name);
		expandedWorkers = next;
	}

	function openEdit(p: Provider, workerName: string) {
		formInitial = { ...p, worker: workerName };
		formMode = 'edit';
		formLockedWorker = null;
		formLockedCapability = null;
		formLockedConstraints = null;
		saveMsg = '';
	}

	function closeForm() {
		formMode = null;
		formInitial = null;
		formLockedWorker = null;
		formLockedCapability = null;
		formLockedConstraints = null;
		formLockedApp = null;
	}

	async function saveWorker(payload: Provider) {
		try {
			const res = await fetch('/api/placements', {
				method: 'PUT',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify(payload)
			});
			if (!res.ok) {
				try { const b = await res.json(); if (b?.error) console.warn('[saveWorker]', b.error); } catch { /* ignore */ }
				throw new Error(t.workers.saveError);
			}
			saveMsg = t.workers.saveOk;
			closeForm();
			fetchWorkers();
		} catch (err) {
			// Re-throw so WorkerForm's onSave catch can surface the error to the user.
			throw err instanceof Error ? err : new Error(t.workers.saveError);
		}
	}

	function toggleExpanded(workerName: string) {
		const next = new Set(expandedWorkers);
		if (next.has(workerName)) next.delete(workerName);
		else next.add(workerName);
		expandedWorkers = next;
	}
</script>

<!-- ── Metrics section ── -->
<section class="mb-8">
	<div class="mb-4 flex items-center gap-3">
		<h2 class="text-[15px] font-semibold">{t.workers.metricsSection}</h2>
		<!-- period selector -->
		<div class="ml-auto flex rounded-lg border border-border bg-surface-2 p-0.5 text-[12px]">
			{#each PERIODS as period}
				<button
					class="cursor-pointer rounded-md px-3 py-1 transition-colors {selectedDays ===
					period.days
						? 'bg-bg font-semibold text-text shadow-sm'
						: 'text-muted hover:text-text'}"
					aria-pressed={selectedDays === period.days}
					onclick={() => selectPeriod(period.days)}
				>
					{period.label}
				</button>
			{/each}
		</div>
	</div>

	{#if metricsAllLoading || metricsWindowLoading}
		<div class="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-4">
			{#each [1, 2, 3, 4] as _}
				<div class="h-[108px] animate-pulse rounded-xl border border-border bg-surface-2"></div>
			{/each}
		</div>
	{:else}
		<div class="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-4">
			<!-- Card 1: Saúde & atividade (ao vivo) -->
			<div class="rounded-xl border border-border bg-surface-2 p-4">
				<div class="mb-2 flex items-center justify-between">
					<span class="text-[11px] font-semibold uppercase tracking-wide text-muted"
						>{t.workers.cardHealth}</span
					>
					<span class="rounded-full bg-green/15 px-2 py-0.5 text-[10px] font-semibold text-green"
						>{t.workers.nowBadge}</span
					>
				</div>
				{#if metricsAllError || error}
					<p class="text-sm text-red-500">{t.workers.metricsError}</p>
				{:else if loading}
					<p class="text-sm text-muted">{t.workers.loading}</p>
				{:else if providers.length === 0}
					<p class="text-sm text-muted">{t.workers.empty}</p>
				{:else}
					<p class="text-[22px] font-bold text-text">
						{aliveCount}<span class="text-[14px] font-normal text-muted"> / {providers.length}</span>
					</p>
					<p class="mt-1 text-[11px] text-muted">
						{staleCount}
						{t.workers.stale} · {asleepCount}
						{t.workers.asleep}
					</p>
					{#if lastActivity}
						<p class="mt-1 text-[11px] text-muted">
							{t.workers.lastActivity}: {fmtDate(lastActivity)}
						</p>
					{/if}
				{/if}
			</div>

			<!-- Card 2: Confiabilidade (janela) -->
			<div class="rounded-xl border border-border bg-surface-2 p-4">
				<div class="mb-2">
					<span class="text-[11px] font-semibold uppercase tracking-wide text-muted"
						>{t.workers.cardReliability}</span
					>
				</div>
				{#if metricsWindowError}
					<p class="text-sm text-red-500">{t.workers.metricsError}</p>
				{:else if successRate === null}
					<p class="text-sm text-muted">{t.workers.metricsEmpty}</p>
				{:else}
					<p class="text-[22px] font-bold text-text">
						{successRate}<span class="text-[14px] font-normal text-muted">%</span>
					</p>
					<p class="mt-1 text-[11px] text-muted">{totalFailedW} {t.workers.failuresInPeriod}</p>
					{#if topErrorWorker && topErrorWorker.failed > 0}
						<p class="mt-1 text-[11px] text-muted">
							{t.workers.topErrorWorker}: {topErrorWorker.provider}
						</p>
					{/if}
				{/if}
			</div>

			<!-- Card 3: Volume & share (janela) -->
			<div class="rounded-xl border border-border bg-surface-2 p-4">
				<div class="mb-2">
					<span class="text-[11px] font-semibold uppercase tracking-wide text-muted"
						>{t.workers.cardVolume}</span
					>
				</div>
				{#if metricsWindowError}
					<p class="text-sm text-red-500">{t.workers.metricsError}</p>
				{:else if totalDoneW === 0}
					<p class="text-sm text-muted">{t.workers.metricsEmpty}</p>
				{:else}
					<p class="text-[22px] font-bold text-text">{totalDoneW}</p>
					<p class="mt-1 text-[11px] text-muted">{t.workers.successItems}</p>
					{#if volumeShares.length > 0}
						<p class="mt-1 text-[11px] text-muted">{volumeShares.join(' · ')}</p>
					{/if}
				{/if}
			</div>

			<!-- Card 4: Fila atual (ao vivo) -->
			<div class="rounded-xl border border-border bg-surface-2 p-4">
				<div class="mb-2 flex items-center justify-between">
					<span class="text-[11px] font-semibold uppercase tracking-wide text-muted"
						>{t.workers.cardQueue}</span
					>
					<span class="rounded-full bg-green/15 px-2 py-0.5 text-[10px] font-semibold text-green"
						>{t.workers.nowBadge}</span
					>
				</div>
				{#if metricsAllError}
					<p class="text-sm text-red-500">{t.workers.metricsError}</p>
				{:else if metricsAll.length === 0}
					<p class="text-sm text-muted">{t.workers.metricsEmpty}</p>
				{:else}
					<p class="text-[22px] font-bold text-text">{totalQueue}</p>
					<p class="mt-1 text-[11px] text-muted">
						{pendingTotal}
						{t.workers.queuePending} · {assignedTotal}
						{t.workers.queueAssigned} · {runningTotal}
						{t.workers.queueRunning}
					</p>
				{/if}
			</div>
		</div>
	{/if}
</section>

<!-- ── Workers table (grouped by worker → placements) ── -->
<section class="mb-8">
	<div class="mb-4 flex items-center gap-3">
		<h2 class="text-[15px] font-semibold">{t.workers.title}</h2>
		<button
			class="ml-auto cursor-pointer rounded-token border border-border bg-surface-2 px-3 py-1.5 text-[12px] font-semibold hover:bg-hover"
			onclick={openAdd}
		>
			+ {t.workers.addWorker}
		</button>
	</div>

	{#if formMode === 'add' && !formLockedWorker}
		<div class="mb-4">
			<WorkerForm
				initial={null}
				lockedApp={null}
				capabilities={knownCapabilities}
				onSave={saveWorker}
				onCancel={closeForm}
			/>
		</div>
	{/if}

	{#if loading}
		<p class="text-sm text-muted">{t.workers.loading}</p>
	{:else if error}
		<p class="text-sm text-red-500">{t.workers.error}</p>
	{:else if workers.length === 0}
		<p class="text-sm text-muted">{t.workers.empty}</p>
	{:else}
		{#if saveMsg}
			<p class="mb-3 text-sm text-muted">{saveMsg}</p>
		{/if}
		<div class="overflow-x-auto rounded-xl border border-border">
			<table class="w-full border-collapse text-[13px]">
				<thead>
					<tr class="border-b border-border bg-surface-2 text-left text-muted">
						<th class="w-6 px-2 py-2.5"></th>
						<th class="px-4 py-2.5 font-medium">{t.workers.colWorker}</th>
						<th class="px-4 py-2.5 font-medium">{t.workers.colCapability}</th>
						<th class="px-4 py-2.5 font-medium">{t.workers.colPlacements}</th>
					</tr>
				</thead>
				<tbody>
					{#each workers as w (w.name)}
						{@const expanded = expandedWorkers.has(w.name)}
						<tr
							class="border-b border-border hover:bg-hover {expanded ? 'bg-surface-2' : ''} cursor-pointer"
							role="button"
							tabindex="0"
							aria-expanded={expanded}
							aria-label={expanded ? t.workers.collapsePlacements + ' ' + w.name : t.workers.expandPlacements + ' ' + w.name}
							onclick={() => toggleExpanded(w.name)}
							onkeydown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); toggleExpanded(w.name); } }}
						>
							<td class="px-2 py-2.5 text-center text-[11px] text-muted select-none">
								{expanded ? '▲' : '▼'}
							</td>
							<td class="px-4 py-2.5 font-mono text-[12px] font-semibold">
								{w.name}{#if w.placements[0]?.description}<span class="font-sans font-normal text-muted"> — {w.placements[0].description}</span>{/if}
							</td>
							<td class="px-4 py-2.5 text-muted">{w.capability}</td>
							<td class="px-4 py-2.5 tabular-nums text-muted">
								{w.placements.length} {t.workers.placementsCount}
							</td>
						</tr>
						{#if expanded}
							<tr class="border-b border-border">
								<td colspan="4" class="px-0 py-0">
									<table class="w-full border-collapse text-[12px]">
										<thead>
											<tr class="border-b border-border/40 bg-surface-2/60 text-left text-[11px] text-muted">
												<th class="py-1.5 pl-10 pr-3 font-medium">{t.workers.colName}</th>
												<th class="py-1.5 pr-3 font-medium">{t.workers.colRuntime}</th>
												<th class="py-1.5 pr-3 font-medium">{t.workers.colActivation}</th>
												<th class="py-1.5 pr-3 font-medium">{t.workers.colEnabled}</th>
												<th class="py-1.5 pr-3 font-medium">{t.workers.colLastRun}</th>
												<th class="py-1.5 pr-3" aria-label={t.workers.lastErrorLabel}></th>
												<th class="py-1.5 pr-3"></th>
											</tr>
										</thead>
										<tbody>
											{#each w.placements as p (p.name)}
												<tr class="border-b border-border/30 last:border-0 hover:bg-hover/60">
													<td class="py-2 pl-10 pr-3 font-mono">{p.name}</td>
													<td class="py-2 pr-3 text-muted">{p.runtime}</td>
													<td class="py-2 pr-3 text-muted">{p.activation}</td>
													<td class="py-2 pr-3">
														<span
															class="inline-block rounded-full px-2 py-0.5 text-[10px] font-semibold {p.enabled
																? 'bg-green/20 text-green'
																: 'bg-surface-2 text-muted'}"
															aria-label={p.enabled ? t.workers.enabledStatus : t.workers.disabledStatus}
														>
															<span aria-hidden="true">{p.enabled ? '●' : '○'}</span>
														</span>
													</td>
													<td class="py-2 pr-3 text-muted tabular-nums">
														{#if lastRun(p)}
															{@const ts = lastRun(p)!}
															<span title={new Date(ts).toLocaleString('pt-BR')}>{timeAgo(ts)}</span>
														{:else}
															<span class="text-muted" aria-label={t.workers.lastRunNever}>—</span>
														{/if}
													</td>
													<td class="py-2 pr-3">
														{#if p.last_error}
															<span
																class="inline-block rounded-full bg-red-500/15 px-2 py-0.5 text-[10px] font-semibold text-red-500"
																title={p.last_error}
																aria-label="{t.workers.lastErrorLabel}: {p.last_error}"
															>{t.workers.lastError}</span>
														{/if}
													</td>
													<td class="py-2 pr-3">
														<div class="flex items-center gap-2">
															<button
																class="cursor-pointer rounded-token border border-border bg-bg px-2 py-0.5 text-[11px] hover:bg-hover disabled:opacity-40"
																onclick={(e) => { e.stopPropagation(); openEdit(p, w.name); }}
																onkeydown={(e) => e.stopPropagation()}
																aria-label="{t.workers.editWorker} {p.name}"
																title={t.workers.editWorker}
															>✏</button>
															<button
																class="cursor-pointer rounded-token border border-border bg-bg px-2 py-0.5 text-[11px] hover:bg-hover disabled:opacity-40"
																onclick={(e) => { e.stopPropagation(); toggleProvider(p, w.name); }}
																onkeydown={(e) => e.stopPropagation()}
																disabled={saving === p.name}
																aria-label="{p.enabled ? t.workers.disable : t.workers.enable} {p.name}"
															>
																{saving === p.name
																	? t.workers.saving
																	: p.enabled
																		? t.workers.disable
																		: t.workers.enable}
															</button>
														</div>
													</td>
												</tr>
												{#if formMode === 'edit' && formInitial?.name === p.name}
													<tr>
														<td colspan="7" class="px-4 py-3 pl-10">
															<WorkerForm
																initial={formInitial}
																lockedApp={null}
																capabilities={knownCapabilities}
																onSave={saveWorker}
																onCancel={closeForm}
															/>
														</td>
													</tr>
												{/if}
											{/each}
										</tbody>
									</table>
									{#if formMode === 'add' && formLockedWorker === w.name}
										<div class="px-4 py-3 pl-10">
											<WorkerForm
												lockedWorker={w.name}
												lockedCapability={w.capability}
												lockedConstraints={formLockedConstraints}
												lockedApp={formLockedApp}
												capabilities={knownCapabilities}
												onSave={saveWorker}
												onCancel={closeForm}
											/>
										</div>
									{:else}
										<div class="border-t border-border/30 px-4 py-2 pl-10">
											<button
												class="cursor-pointer rounded-token border border-border bg-bg px-3 py-1 text-[12px] font-semibold hover:bg-hover"
												onclick={() => openAddPlacement(w)}
												aria-label="{t.workers.addPlacement}: {w.name}"
											>
												+ {t.workers.addPlacement}
											</button>
										</div>
									{/if}
								</td>
							</tr>
						{/if}
					{/each}
				</tbody>
			</table>
		</div>
	{/if}
</section>

<!-- ── Routing policies editor ── -->
<section>
	<h2 class="mb-4 text-[15px] font-semibold">{t.workers.policiesSection}</h2>

	{#if policiesLoading}
		<p class="text-sm text-muted">{t.workers.policiesLoading}</p>
	{:else if policiesError}
		<p class="text-sm text-red-500">{t.workers.policiesError}</p>
	{:else}
		<div class="rounded-xl border border-border bg-surface-2 p-5">

			<!-- Scope selector -->
			<div class="mb-5">
				<label class="mb-1.5 block text-[12px] font-medium text-muted" for="routing-scope">
					{t.workers.colScope}
				</label>
				<select
					id="routing-scope"
					value={selectedScope}
					onchange={(e) => selectScope((e.target as HTMLSelectElement).value)}
					class="rounded-token border border-border bg-bg px-3 py-1.5 text-[13px] outline-none focus:border-text/40"
				>
					{#each routingScopes as scope}
						<option value={scope.id}>{scope.label}</option>
					{/each}
				</select>
			</div>

			<!-- Fallback list -->
			<div class="mb-5">
				<p class="mb-2 text-[12px] font-medium text-muted">{t.workers.policyFallbackSection}</p>
				{#if editFallback.length === 0}
					<p class="mb-2 text-[12px] text-muted">{t.workers.policyFallbackEmpty}</p>
				{:else}
					<ol class="mb-3 space-y-1">
						{#each editFallback as name, i}
							<li class="flex items-center gap-1.5">
								<span class="w-4 text-center text-[10px] text-muted">{i + 1}</span>
								<span class="flex-1 font-mono text-[12px]">{name}</span>
								<button
									class="rounded px-1.5 py-0.5 text-[11px] text-muted hover:bg-hover disabled:opacity-30"
									onclick={() => moveFallback(i, -1)}
									disabled={i === 0}
									aria-label={t.workers.fallbackMoveUp}
								>{t.workers.hostsUp}</button>
								<button
									class="rounded px-1.5 py-0.5 text-[11px] text-muted hover:bg-hover disabled:opacity-30"
									onclick={() => moveFallback(i, 1)}
									disabled={i === editFallback.length - 1}
									aria-label={t.workers.fallbackMoveDown}
								>{t.workers.hostsDown}</button>
								<button
									class="rounded px-1.5 py-0.5 text-[11px] text-muted hover:bg-hover"
									onclick={() => removeFallback(i)}
									aria-label={t.workers.fallbackRemove}
								>{t.workers.hostsRemove}</button>
							</li>
						{/each}
					</ol>
				{/if}

				<!-- Add worker to fallback -->
				{#if fallbackAddable.length > 0}
					<div class="mb-2 flex gap-1.5">
						<select
							bind:value={routingAddWorker}
							class="flex-1 rounded border border-border bg-bg px-2 py-1 text-[12px] outline-none focus:border-text/40"
							aria-label={t.workers.hostsAddPlaceholder}
						>
							<option value="">{t.workers.hostsAddPlaceholder}</option>
							{#each fallbackAddable as p}
								<option value={p.name}>{p.name}</option>
							{/each}
						</select>
						<button
							type="button"
							class="cursor-pointer rounded border border-border bg-bg px-2 py-1 text-[12px] hover:bg-hover disabled:opacity-40"
							disabled={!routingAddWorker}
							onclick={() => { addFallback(routingAddWorker); routingAddWorker = ''; }}
							aria-label={t.workers.hostsAddPlaceholder}
						>+</button>
					</div>
				{/if}
			</div>

			<!-- Save -->
			<div class="flex items-center gap-3">
				<button
					class="cursor-pointer rounded-token border border-border bg-bg px-4 py-1.5 text-[13px] font-semibold hover:bg-hover disabled:opacity-40"
					onclick={saveRoutingPolicy}
					disabled={routingSaving}
				>
					{routingSaving ? t.workers.policySaving : t.workers.policySaveBtn}
				</button>
				{#if routingMsg}
					<span class="text-[12px] text-muted" aria-live="polite" role="status">{routingMsg}</span>
				{/if}
			</div>
		</div>
	{/if}
</section>
