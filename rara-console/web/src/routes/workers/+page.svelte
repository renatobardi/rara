<script lang="ts">
	import { onMount, onDestroy } from 'svelte';
	import { t } from '$lib/strings';
	import WorkerForm from '$lib/WorkerForm.svelte';

	const GLOBAL_SCOPE = 'global' as const;

	type Constraints = {
		requires?: string;
		accepts?: string[];
		sensitivity?: string;
	};

	type Provider = {
		name: string;
		capability: string;
		runtime: string;
		activation: string;
		cost: number;
		quality: number;
		enabled: boolean;
		heartbeat_at?: string;
		constraints?: Constraints;
		runner_url?: string;
		env?: Record<string, string>;
	};

	type Worker = {
		name: string;
		capability: string;
		placements: Provider[];
	};

	type RoutingPolicy = {
		scope: string;
		cost_weight: number;
		quality_weight: number;
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

	type Candidate = {
		name: string;
		eligible: boolean;
		healthy: boolean;
		reason: string;
		cost_credit: number;
		quality: number;
		score: number;
		fallback_pos: number;
		selected: boolean;
	};

	type RoutePreview = {
		capability: string;
		winner: string;
		candidates: Candidate[];
	};

	function isWorker(v: unknown): v is Worker {
		if (typeof v !== 'object' || v === null) return false;
		const w = v as Record<string, unknown>;
		return typeof w.name === 'string' && typeof w.capability === 'string' && Array.isArray(w.placements);
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

	// --- routing editor state ---
	let selectedScope = $state<string>(GLOBAL_SCOPE);
	let editCostWeight = $state(0.5);
	let editFallback = $state<string[]>([]);
	let routingAddWorker = $state('');
	let routingSaving = $state(false);
	let routingMsg = $state('');

	// --- route preview state ---
	let previewCapability = $state('');
	let previewLane = $state('');
	let previewSensitivity = $state('public');
	let previewExcludes = $state<Set<string>>(new Set());
	let previewResult = $state<RoutePreview | null>(null);
	let previewLoading = $state(false);
	let previewError = $state(false);
	let previewDebounceTimer: ReturnType<typeof setTimeout> | null = null;
	let previewAbortController: AbortController | null = null;

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
		editCostWeight = pol ? pol.cost_weight : 0.5;
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
		const payload = {
			scope: selectedScope,
			cost_weight: editCostWeight,
			quality_weight: parseFloat((1 - editCostWeight).toFixed(2)),
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

	function fetchPreview() {
		if (!previewCapability) return;
		previewAbortController?.abort();
		previewAbortController = new AbortController();
		const signal = previewAbortController.signal;
		previewLoading = true;
		previewError = false;
		const params = new URLSearchParams({ capability: previewCapability });
		if (previewLane) params.set('lane', previewLane);
		params.set('sensitivity', previewSensitivity);
		for (const name of previewExcludes) params.append('exclude', name);
		fetch(`/api/route/preview?${params}`, { signal })
			.then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
			.then((d) => {
				previewResult = d;
				previewLoading = false;
			})
			.catch((err) => {
				if (err instanceof DOMException && err.name === 'AbortError') return;
				previewError = true;
				previewLoading = false;
			});
	}

	function debouncedPreview() {
		if (previewDebounceTimer) clearTimeout(previewDebounceTimer);
		previewDebounceTimer = setTimeout(fetchPreview, 300);
	}

	onDestroy(() => {
		if (previewDebounceTimer) clearTimeout(previewDebounceTimer);
		previewAbortController?.abort();
	});

	function togglePreviewExclude(name: string) {
		const next = new Set(previewExcludes);
		if (next.has(name)) next.delete(name);
		else next.add(name);
		previewExcludes = next;
		if (previewCapability) fetchPreview();
	}

	function candidateHealth(c: Candidate): 'fresh' | 'exempt' | 'stale' {
		if (!c.healthy) return 'stale';
		const prov = providers.find((p) => p.name === c.name);
		if (!prov) return 'stale'; // ponytail: unknown provider treated conservatively
		if (prov.activation === 'on_demand') return 'exempt';
		return 'fresh';
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

	async function toggleProvider(p: Provider) {
		saving = p.name;
		saveMsg = '';
		try {
			// ponytail: explicit DTO — omit heartbeat_at (core preserves it from DB on upsert).
			// env must stay: full-record upsert, omitting it would set env={} in the DB.
			// ponytail: explicit DTO — omit heartbeat_at (core preserves it from DB on upsert).
			// env must stay: full-record upsert, omitting it would set env={} in the DB.
			const dto = {
				name: p.name, capability: p.capability, runtime: p.runtime,
				activation: p.activation, cost: p.cost, quality: p.quality,
				enabled: !p.enabled, constraints: p.constraints,
				runner_url: p.runner_url, env: p.env
			};
			const optimistic: Provider = { ...p, enabled: !p.enabled };
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
		saveMsg = '';
	}

	function openEdit(p: Provider) {
		formInitial = p;
		formMode = 'edit';
		saveMsg = '';
	}

	function closeForm() {
		formMode = null;
		formInitial = null;
	}

	async function saveWorker(payload: Provider) {
		try {
			const res = await fetch('/api/placements', {
				method: 'PUT',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify(payload)
			});
			if (!res.ok) throw new Error(t.workers.saveError);
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

	{#if formMode === 'add'}
		<div class="mb-4">
			<WorkerForm
				initial={null}
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
							<td class="px-4 py-2.5 font-mono text-[12px] font-semibold">{w.name}</td>
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
												<th class="py-1.5 pr-3 font-medium">{t.workers.colCost}</th>
												<th class="py-1.5 pr-3 font-medium">{t.workers.colQuality}</th>
												<th class="py-1.5 pr-3 font-medium">{t.workers.colEnabled}</th>
												<th class="py-1.5 pr-3"></th>
											</tr>
										</thead>
										<tbody>
											{#each w.placements as p (p.name)}
												<tr class="border-b border-border/30 last:border-0 hover:bg-hover/60">
													<td class="py-2 pl-10 pr-3 font-mono">{p.name}</td>
													<td class="py-2 pr-3 text-muted">{p.runtime}</td>
													<td class="py-2 pr-3 text-muted">{p.activation}</td>
													<td class="py-2 pr-3 tabular-nums">{p.cost}</td>
													<td class="py-2 pr-3 tabular-nums">{p.quality}</td>
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
													<td class="py-2 pr-3">
														<div class="flex items-center gap-2">
															<button
																class="cursor-pointer rounded-token border border-border bg-bg px-2 py-0.5 text-[11px] hover:bg-hover disabled:opacity-40"
																onclick={(e) => { e.stopPropagation(); openEdit(p); }}
																aria-label="{t.workers.editWorker} {p.name}"
																title={t.workers.editWorker}
															>✏</button>
															<button
																class="cursor-pointer rounded-token border border-border bg-bg px-2 py-0.5 text-[11px] hover:bg-hover disabled:opacity-40"
																onclick={(e) => { e.stopPropagation(); toggleProvider(p); }}
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
								</td>
							</tr>
						{/if}
					{/each}
				</tbody>
			</table>
		</div>
	{/if}
</section>

<!-- ── Roteamento + Simular rota (side-by-side grid) ── -->
<div class="grid grid-cols-1 gap-6 lg:grid-cols-2">

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

			<!-- Slider cost↔quality -->
			<div class="mb-5">
				<div class="mb-1.5 flex justify-between text-[12px] font-medium text-muted">
					<span>{t.workers.colCostWeight}: <strong class="text-text">{editCostWeight.toFixed(2)}</strong></span>
					<span>{t.workers.colQualityWeight}: <strong class="text-text">{(1 - editCostWeight).toFixed(2)}</strong></span>
				</div>
				<input
					id="routing-slider"
					type="range"
					min="0"
					max="1"
					step="0.01"
					value={editCostWeight}
					oninput={(e) => { editCostWeight = parseFloat((e.target as HTMLInputElement).value); }}
					class="w-full cursor-pointer accent-text"
					aria-label="{t.workers.colCostWeight} ↔ {t.workers.colQualityWeight}"
				/>
				<p class="mt-1 text-[11px] text-muted">{t.workers.policyWeightsHint}</p>
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
								>{t.fontesFlows.hostsUp}</button>
								<button
									class="rounded px-1.5 py-0.5 text-[11px] text-muted hover:bg-hover disabled:opacity-30"
									onclick={() => moveFallback(i, 1)}
									disabled={i === editFallback.length - 1}
									aria-label={t.workers.fallbackMoveDown}
								>{t.fontesFlows.hostsDown}</button>
								<button
									class="rounded px-1.5 py-0.5 text-[11px] text-muted hover:bg-hover"
									onclick={() => removeFallback(i)}
									aria-label={t.workers.fallbackRemove}
								>{t.fontesFlows.hostsRemove}</button>
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
							aria-label={t.fontesFlows.hostsAddPlaceholder}
						>
							<option value="">{t.fontesFlows.hostsAddPlaceholder}</option>
							{#each fallbackAddable as p}
								<option value={p.name}>{p.name}</option>
							{/each}
						</select>
						<button
							type="button"
							class="cursor-pointer rounded border border-border bg-bg px-2 py-1 text-[12px] hover:bg-hover disabled:opacity-40"
							disabled={!routingAddWorker}
							onclick={() => { addFallback(routingAddWorker); routingAddWorker = ''; }}
							aria-label={t.fontesFlows.hostsAddPlaceholder}
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

<!-- ── Simular rota ── -->
<section>
	<h2 class="mb-4 text-[15px] font-semibold">{t.workers.previewSection}</h2>
	<div class="rounded-xl border border-border bg-surface-2 p-5">

		<!-- Selectors -->
		<div class="mb-4 grid grid-cols-1 gap-3 sm:grid-cols-3">
			<!-- Capability (required) -->
			<div>
				<label class="mb-1 block text-[11px] font-medium text-muted" for="preview-capability">
					{t.workers.colCapability}
				</label>
				<select
					id="preview-capability"
					bind:value={previewCapability}
					onchange={debouncedPreview}
					class="w-full rounded-token border border-border bg-bg px-2 py-1.5 text-[12px] outline-none focus:border-text/40"
				>
					<option value="">{t.workers.previewCapabilityPlaceholder}</option>
					{#each knownCapabilities as cap}
						<option value={cap}>{cap}</option>
					{/each}
				</select>
			</div>

			<!-- Lane (optional) — datalist allows predefined values + free text -->
			<div>
				<label class="mb-1 block text-[11px] font-medium text-muted" for="preview-lane">
					{t.workers.previewLaneLabel}
				</label>
				<input
					id="preview-lane"
					list="preview-lane-list"
					bind:value={previewLane}
					oninput={debouncedPreview}
					placeholder={t.workers.previewLanePlaceholder}
					class="w-full rounded-token border border-border bg-bg px-2 py-1.5 text-[12px] outline-none focus:border-text/40"
				/>
				<datalist id="preview-lane-list">
					{#each ['youtube', 'podcast', 'email', 'linkedin', 'url'] as lane}
						<option value={lane}></option>
					{/each}
				</datalist>
			</div>

			<!-- Sensitivity (optional) -->
			<div>
				<label class="mb-1 block text-[11px] font-medium text-muted" for="preview-sensitivity">
					{t.workers.previewSensitivityLabel}
				</label>
				<select
					id="preview-sensitivity"
					bind:value={previewSensitivity}
					onchange={debouncedPreview}
					class="w-full rounded-token border border-border bg-bg px-2 py-1.5 text-[12px] outline-none focus:border-text/40"
				>
					<option value="public">{t.workers.previewSensitivityPublic}</option>
					<option value="private">{t.workers.previewSensitivityPrivate}</option>
				</select>
			</div>
		</div>

		<button
			class="mb-4 cursor-pointer rounded-token border border-border bg-bg px-4 py-1.5 text-[12px] font-semibold hover:bg-hover disabled:opacity-40"
			onclick={fetchPreview}
			disabled={!previewCapability || previewLoading}
		>
			{previewLoading ? t.workers.previewSimulating : t.workers.previewSimulateBtn}
		</button>

		<!-- Results -->
		{#if !previewCapability}
			<p class="text-[12px] text-muted">{t.workers.previewNoCapability}</p>
		{:else if previewLoading}
			<p class="text-[12px] text-muted">{t.workers.previewSimulating}</p>
		{:else if previewError}
			<p class="text-[12px] text-red-500">{t.workers.previewError}</p>
		{:else if previewResult}
			{@const eligibleCandidates = (previewResult.candidates ?? []).filter((c) => c.eligible)}

			{#if previewExcludes.size > 0}
				<p class="mb-3 text-[11px] text-muted" aria-live="polite">
					{t.workers.previewWhatIfNote}: {[...previewExcludes].join(', ')}{t.workers.previewWhatIfSuffix}
				</p>
			{/if}

			{#if !previewResult.winner && eligibleCandidates.length === 0}
				<p class="text-[12px] text-muted">{t.workers.previewEmpty}</p>
			{:else}
				<!-- Winner badge -->
				{#if previewResult.winner}
					<div class="mb-4 flex items-center gap-2">
						<span class="text-[11px] font-semibold uppercase tracking-wide text-muted"
							>{t.workers.previewWinner}</span
						>
						<span
							class="rounded-full bg-green/20 px-3 py-0.5 font-mono text-[12px] font-semibold text-green"
						>
							{previewResult.winner}
						</span>
					</div>
				{/if}

				<!-- Candidates table -->
				<div class="overflow-x-auto">
					<table class="w-full border-collapse text-[12px]">
						<thead>
							<tr class="border-b border-border text-left text-[11px] text-muted">
								<th class="pb-1.5 pr-3 font-medium">{t.workers.previewColWorker}</th>
								<th class="pb-1.5 pr-3 font-medium">{t.workers.previewColHealth}</th>
								<th class="pb-1.5 pr-3 font-medium">{t.workers.previewColCost}</th>
								<th class="pb-1.5 pr-3 font-medium">{t.workers.previewColQuality}</th>
								<th class="pb-1.5 pr-3 font-medium">{t.workers.previewColScore}</th>
								<th class="pb-1.5 pr-3 font-medium">{t.workers.previewColPos}</th>
								<th class="pb-1.5 pr-3 font-medium">{t.workers.previewColReason}</th>
								<th class="pb-1.5 font-medium">{t.workers.previewColExclude}</th>
							</tr>
						</thead>
						<tbody>
							{#each (previewResult.candidates ?? []) as c}
								{@const health = candidateHealth(c)}
								<tr
									class="border-b border-border/50 last:border-0 {c.selected
										? 'bg-green/5'
										: ''} {previewExcludes.has(c.name) ? 'opacity-40' : ''}"
								>
									<td class="py-1.5 pr-3 font-mono text-[11px]">{c.name}</td>
									<td class="py-1.5 pr-3">
										<span
											class={health === 'fresh'
												? 'text-green'
												: health === 'exempt'
													? 'text-muted'
													: 'text-orange-400'}
										>
											{health === 'fresh'
												? t.workers.previewHealthFresh
												: health === 'exempt'
													? t.workers.previewHealthExempt
													: t.workers.previewHealthStale}
										</span>
									</td>
									<td class="py-1.5 pr-3 tabular-nums">{Number(c.cost_credit ?? 0).toFixed(2)}</td>
									<td class="py-1.5 pr-3 tabular-nums">{Number(c.quality ?? 0).toFixed(2)}</td>
									<td class="py-1.5 pr-3 tabular-nums font-semibold"
										>{c.eligible ? Number(c.score ?? 0).toFixed(2) : '—'}</td
									>
									<td class="py-1.5 pr-3 tabular-nums"
										>{c.fallback_pos > 0 ? c.fallback_pos : '—'}</td
									>
									<td class="py-1.5 pr-3 text-[11px] text-muted">{c.reason || '—'}</td>
									<td class="py-1.5">
										<input
											type="checkbox"
											checked={previewExcludes.has(c.name)}
											onchange={() => togglePreviewExclude(c.name)}
											aria-label="{t.workers.previewExcludeAriaLabel} {c.name}"
											class="cursor-pointer"
										/>
									</td>
								</tr>
							{/each}
						</tbody>
					</table>
				</div>
			{/if}
		{/if}
	</div>
</section>

</div><!-- end grid -->
