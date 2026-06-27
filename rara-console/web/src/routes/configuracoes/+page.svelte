<script lang="ts">
	import { t } from '$lib/strings';

	type ProviderHealth = { total: number; enabled: number; stale: number };
	type Health = { db_ok: boolean; last_reconcile_at?: string; providers: ProviderHealth };
	type ItemCount = { lane: string; status: string; count: number };
	type StepCount = { capability: string; status: string; count: number };
	type Usage = { items: ItemCount[]; item_steps: StepCount[]; distillations: number; quarantine: number };

	// ── Routing ──
	type RoutingPolicy = { scope: string; fallback: string[] };
	type Provider = {
		name: string;
		capability: string;
		runtime: string;
		activation: string;
		enabled: boolean;
		heartbeat_at?: string;
		last_collect_at?: string;
		last_error?: string;
	};
	type Worker = { name: string; capability: string; placements: Provider[] };

	const GLOBAL_SCOPE = 'global' as const;
	let workers = $state<Worker[]>([]);
	let policies = $state<RoutingPolicy[]>([]);
	let policiesLoading = $state(true);
	let policiesError = $state(false);
	let selectedScope = $state<string>(GLOBAL_SCOPE);
	let editFallback = $state<string[]>([]);
	let routingAddWorker = $state('');
	let routingSaving = $state(false);
	let routingMsg = $state('');

	let providers = $derived(workers.flatMap((w) => w.placements));
	let knownCapabilities = $derived([...new Set(providers.map((p) => p.capability))].sort());
	let routingScopes = $derived([
		{ id: GLOBAL_SCOPE, label: t.workers.policyScopeGlobal },
		...knownCapabilities.filter((c) => c !== GLOBAL_SCOPE).map((c) => ({ id: c, label: c }))
	]);
	let fallbackAvailable = $derived(
		selectedScope === GLOBAL_SCOPE
			? providers
			: providers.filter((p) => p.capability === selectedScope)
	);
	let fallbackAddable = $derived(fallbackAvailable.filter((p) => !editFallback.includes(p.name)));

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
		const payload: RoutingPolicy = { scope: selectedScope, fallback: editFallback };
		try {
			const res = await fetch('/api/routing-policies', {
				method: 'PUT',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify(payload)
			});
			if (!res.ok) throw new Error();
			const idx = policies.findIndex((p) => p.scope === selectedScope);
			policies = idx >= 0
				? policies.map((p, i) => (i === idx ? payload : p))
				: [...policies, payload];
			routingMsg = t.workers.policySaveOk;
		} catch {
			routingMsg = t.workers.policySaveError;
		} finally {
			routingSaving = false;
		}
	}

	// ── Worker Metrics ──
	type ByStatus = { pending: number; assigned: number; running: number; done: number; failed: number; skipped: number };
	type WorkerMetric = {
		provider: string; total: number; by_status?: ByStatus;
		done: number; failed: number; success_rate: number;
		queue: number; avg_attempt: number; last_activity_at?: string;
	};

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

	const STALE_MS = 5 * 60 * 1000;
	let windowReqId = 0;

	function workerStatus(p: Provider): 'alive' | 'stale' | 'asleep' {
		if (p.activation === 'on_demand') return 'alive';
		if (!p.heartbeat_at) return 'asleep';
		return Date.now() - new Date(p.heartbeat_at).getTime() > STALE_MS ? 'stale' : 'alive';
	}

	let aliveCount = $derived(providers.filter((p) => workerStatus(p) === 'alive').length);
	let staleCount = $derived(providers.filter((p) => workerStatus(p) === 'stale').length);
	let asleepCount = $derived(providers.filter((p) => workerStatus(p) === 'asleep').length);
	let lastActivity = $derived(
		metricsAll.reduce((best, m) => {
			if (!m.last_activity_at) return best;
			return !best || m.last_activity_at > best ? m.last_activity_at : best;
		}, '' as string)
	);

	let totalDoneW = $derived(metricsWindow.reduce((s, m) => s + m.done, 0));
	let totalFailedW = $derived(metricsWindow.reduce((s, m) => s + m.failed, 0));
	let successRate = $derived(
		totalDoneW + totalFailedW > 0
			? Math.round((totalDoneW / (totalDoneW + totalFailedW)) * 100)
			: null
	);
	let topErrorWorker = $derived(
		metricsWindow.length > 0 ? [...metricsWindow].sort((a, b) => b.failed - a.failed)[0] : null
	);
	let volumeShares = $derived(
		totalDoneW > 0
			? [...metricsWindow].filter((m) => m.done > 0).sort((a, b) => b.done - a.done).slice(0, 3)
				.map((m) => `${m.provider} ${Math.round((m.done / totalDoneW) * 100)}%`)
			: []
	);
	let totalQueue = $derived(metricsAll.reduce((s, m) => s + m.queue, 0));
	let pendingTotal = $derived(metricsAll.reduce((s, m) => s + (m.by_status?.pending ?? 0), 0));
	let assignedTotal = $derived(metricsAll.reduce((s, m) => s + (m.by_status?.assigned ?? 0), 0));
	let runningTotal = $derived(metricsAll.reduce((s, m) => s + (m.by_status?.running ?? 0), 0));

	function fmtDate(iso: string): string {
		if (!iso) return t.workers.never;
		return new Date(iso).toLocaleString('pt-BR', {
			day: '2-digit', month: '2-digit', hour: '2-digit', minute: '2-digit'
		});
	}

	function fetchMetricsAll() {
		metricsAllLoading = true;
		metricsAllError = false;
		fetch('/api/workers/metrics')
			.then((r) => (r.ok ? r.json() : Promise.reject()))
			.then((d) => { metricsAll = Array.isArray(d) ? d : []; metricsAllLoading = false; })
			.catch(() => { metricsAllError = true; metricsAllLoading = false; });
	}

	function fetchMetricsWindow(days: number | null) {
		metricsWindowLoading = true;
		metricsWindowError = false;
		const reqId = ++windowReqId;
		const url = days !== null ? `/api/workers/metrics?days=${days}` : '/api/workers/metrics';
		fetch(url)
			.then((r) => (r.ok ? r.json() : Promise.reject()))
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
		try { localStorage.setItem('workers-period', String(days)); } catch { /* ignore */ }
		fetchMetricsWindow(days);
	}

	let health = $state<Health | null>(null);
	let usage = $state<Usage | null>(null);
	let loadingHealth = $state(true);
	let loadingUsage = $state(true);
	let errorHealth = $state(false);
	let errorUsage = $state(false);

	$effect(() => {
		fetch('/api/health')
			.then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
			.then((d) => (health = d))
			.catch(() => (errorHealth = true))
			.finally(() => (loadingHealth = false));
	});

	$effect(() => {
		fetch('/api/usage')
			.then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
			.then((d) => (usage = d))
			.catch(() => (errorUsage = true))
			.finally(() => (loadingUsage = false));
	});

	$effect(() => {
		fetch('/api/workers')
			.then((r) => (r.ok ? r.json() : Promise.reject()))
			.then((d) => { workers = Array.isArray(d) ? d : []; });

		fetch('/api/routing-policies')
			.then((r) => (r.ok ? r.json() : Promise.reject()))
			.then((d) => {
				if (!Array.isArray(d)) throw new Error();
				policies = d;
				policiesLoading = false;
				selectScope(GLOBAL_SCOPE);
			})
			.catch(() => { policiesError = true; policiesLoading = false; });

		// restore period preference
		try {
			const saved = localStorage.getItem('workers-period');
			if (saved === 'null') selectedDays = null;
			else if (saved) {
				const n = parseInt(saved, 10);
				if ([1, 7, 30].includes(n)) selectedDays = n;
			}
		} catch { /* ignore */ }

		fetchMetricsAll();
		fetchMetricsWindow(selectedDays);
	});

	function fmtTime(iso: string | undefined): string {
		if (!iso) return t.settings.never;
		return new Date(iso).toLocaleString('pt-BR');
	}
</script>

<!-- Saúde -->
<section class="mb-8">
	<h2 class="mb-3 text-[14px] font-semibold text-muted">{t.settings.healthSection}</h2>

	{#if loadingHealth}
		<p class="text-[13px] text-muted">{t.settings.loading}</p>
	{:else if errorHealth}
		<p class="text-[13px] text-red">{t.settings.error}</p>
	{:else if health}
		<div class="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-5">
			<!-- db_ok -->
			<div class="rounded-card border border-border bg-surface p-4">
				<div class="text-[11px] text-muted">{t.settings.dbOk}</div>
				<div class="mt-1 flex items-center gap-1.5">
					<span class="h-[8px] w-[8px] rounded-full flex-none {health.db_ok ? 'bg-green' : 'bg-red'}"></span>
					<span class="text-[15px] font-semibold">{health.db_ok ? t.settings.dbOkYes : t.settings.dbOkNo}</span>
				</div>
			</div>
			<!-- last reconcile -->
			<div class="rounded-card border border-border bg-surface p-4 col-span-1 sm:col-span-2">
				<div class="text-[11px] text-muted">{t.settings.lastReconcile}</div>
				<div class="mt-1 text-[13px] font-medium">{fmtTime(health.last_reconcile_at)}</div>
			</div>
			<!-- providers total -->
			<div class="rounded-card border border-border bg-surface p-4">
				<div class="text-[11px] text-muted">{t.settings.providersTotal}</div>
				<div class="mt-1 text-[22px] font-bold tracking-tight">{health.providers.total}</div>
			</div>
			<!-- providers enabled -->
			<div class="rounded-card border border-border bg-surface p-4">
				<div class="text-[11px] text-muted">{t.settings.providersEnabled}</div>
				<div class="mt-1 text-[22px] font-bold tracking-tight">{health.providers.enabled}</div>
			</div>
			<!-- providers stale -->
			<div class="rounded-card border border-border bg-surface p-4">
				<div class="text-[11px] text-muted">{t.settings.providersStale}</div>
				<div class="mt-1 flex items-center gap-1.5">
					{#if health.providers.stale > 0}
						<span class="h-[8px] w-[8px] rounded-full bg-amber flex-none"></span>
					{/if}
					<span class="text-[22px] font-bold tracking-tight">{health.providers.stale}</span>
				</div>
			</div>
		</div>
	{/if}
</section>

<!-- Métricas Workers -->
<section class="mb-8">
	<div class="mb-4 flex items-center gap-3">
		<h2 class="text-[14px] font-semibold text-muted">{t.settings.metricsWorkersSection}</h2>
		<!-- period selector -->
		<div class="ml-auto flex rounded-lg border border-border bg-surface-2 p-0.5 text-[12px]">
			{#each PERIODS as period}
				<button
					class="cursor-pointer rounded-md px-3 py-1 transition-colors {selectedDays === period.days
						? 'bg-bg font-semibold text-text shadow-sm'
						: 'text-muted hover:text-text'}"
					aria-pressed={selectedDays === period.days}
					onclick={() => selectPeriod(period.days)}
				>{period.label}</button>
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
					<span class="text-[11px] font-semibold uppercase tracking-wide text-muted">{t.workers.cardHealth}</span>
					<span class="rounded-full bg-green/15 px-2 py-0.5 text-[10px] font-semibold text-green">{t.workers.nowBadge}</span>
				</div>
				{#if metricsAllError}
					<p class="text-sm text-red-500">{t.workers.metricsError}</p>
				{:else if providers.length === 0}
					<p class="text-sm text-muted">{t.workers.empty}</p>
				{:else}
					<p class="text-[22px] font-bold text-text">
						{aliveCount}<span class="text-[14px] font-normal text-muted"> / {providers.length}</span>
					</p>
					<p class="mt-1 text-[11px] text-muted">{staleCount} {t.workers.stale} · {asleepCount} {t.workers.asleep}</p>
					{#if lastActivity}
						<p class="mt-1 text-[11px] text-muted">{t.workers.lastActivity}: {fmtDate(lastActivity)}</p>
					{/if}
				{/if}
			</div>

			<!-- Card 2: Confiabilidade (janela) -->
			<div class="rounded-xl border border-border bg-surface-2 p-4">
				<div class="mb-2">
					<span class="text-[11px] font-semibold uppercase tracking-wide text-muted">{t.workers.cardReliability}</span>
				</div>
				{#if metricsWindowError}
					<p class="text-sm text-red-500">{t.workers.metricsError}</p>
				{:else if successRate === null}
					<p class="text-sm text-muted">{t.workers.metricsEmpty}</p>
				{:else}
					<p class="text-[22px] font-bold text-text">{successRate}<span class="text-[14px] font-normal text-muted">%</span></p>
					<p class="mt-1 text-[11px] text-muted">{totalFailedW} {t.workers.failuresInPeriod}</p>
					{#if topErrorWorker && topErrorWorker.failed > 0}
						<p class="mt-1 text-[11px] text-muted">{t.workers.topErrorWorker}: {topErrorWorker.provider}</p>
					{/if}
				{/if}
			</div>

			<!-- Card 3: Volume & share (janela) -->
			<div class="rounded-xl border border-border bg-surface-2 p-4">
				<div class="mb-2">
					<span class="text-[11px] font-semibold uppercase tracking-wide text-muted">{t.workers.cardVolume}</span>
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
					<span class="text-[11px] font-semibold uppercase tracking-wide text-muted">{t.workers.cardQueue}</span>
					<span class="rounded-full bg-green/15 px-2 py-0.5 text-[10px] font-semibold text-green">{t.workers.nowBadge}</span>
				</div>
				{#if metricsAllError}
					<p class="text-sm text-red-500">{t.workers.metricsError}</p>
				{:else if metricsAll.length === 0}
					<p class="text-sm text-muted">{t.workers.metricsEmpty}</p>
				{:else}
					<p class="text-[22px] font-bold text-text">{totalQueue}</p>
					<p class="mt-1 text-[11px] text-muted">
						{pendingTotal} {t.workers.queuePending} · {assignedTotal} {t.workers.queueAssigned} · {runningTotal} {t.workers.queueRunning}
					</p>
				{/if}
			</div>
		</div>
	{/if}
</section>

<!-- Roteamento -->
<section class="mb-8">
	<h2 class="mb-3 text-[14px] font-semibold text-muted">{t.settings.routingSection}</h2>

	{#if policiesLoading}
		<p class="text-[13px] text-muted">{t.workers.policiesLoading}</p>
	{:else if policiesError}
		<p class="text-[13px] text-red">{t.workers.policiesError}</p>
	{:else}
		<div class="rounded-card border border-border bg-surface p-5">
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

<!-- Uso -->
<section class="mb-8">
	<h2 class="mb-3 text-[14px] font-semibold text-muted">{t.settings.usageSection}</h2>

	{#if loadingUsage}
		<p class="text-[13px] text-muted">{t.settings.loading}</p>
	{:else if errorUsage}
		<p class="text-[13px] text-red">{t.settings.error}</p>
	{:else if usage}
		<!-- KPI cards -->
		<div class="mb-4 grid grid-cols-2 gap-3 sm:grid-cols-4">
			<div class="rounded-card border border-border bg-surface p-4">
				<div class="text-[11px] text-muted">{t.settings.distillationsTotal}</div>
				<div class="mt-1 text-[22px] font-bold tracking-tight">{usage.distillations}</div>
			</div>
			<div class="rounded-card border border-border bg-surface p-4">
				<div class="text-[11px] text-muted">{t.settings.quarantineTotal}</div>
				<div class="mt-1 text-[22px] font-bold tracking-tight">{usage.quarantine}</div>
			</div>
		</div>

		<!-- Items por lane + status -->
		{#if usage.items.length > 0}
			<div class="mb-4 overflow-hidden rounded-card border border-border bg-surface">
				<h3 class="m-0 border-b border-border px-4 py-3 text-[13px] font-semibold">
					{t.settings.itemsSection}
				</h3>
				<table class="w-full text-[13px]">
					<caption class="sr-only">{t.settings.itemsSection}</caption>
					<thead>
						<tr class="border-b border-border text-left text-[11px] text-muted">
							<th class="px-4 py-2 font-medium">{t.settings.colLane}</th>
							<th class="px-4 py-2 font-medium">{t.settings.colStatus}</th>
							<th class="px-4 py-2 text-right font-medium">{t.settings.colCount}</th>
						</tr>
					</thead>
					<tbody>
						{#each usage.items as row}
							<tr class="border-b border-border last:border-b-0">
								<td class="px-4 py-2 text-muted">{row.lane}</td>
								<td class="px-4 py-2">{row.status}</td>
								<td class="px-4 py-2 text-right font-medium">{row.count}</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
		{/if}

		<!-- Item steps por capability + status -->
		{#if usage.item_steps.length > 0}
			<div class="overflow-hidden rounded-card border border-border bg-surface">
				<h3 class="m-0 border-b border-border px-4 py-3 text-[13px] font-semibold">
					{t.settings.stepsSection}
				</h3>
				<table class="w-full text-[13px]">
					<caption class="sr-only">{t.settings.stepsSection}</caption>
					<thead>
						<tr class="border-b border-border text-left text-[11px] text-muted">
							<th class="px-4 py-2 font-medium">{t.settings.colCapability}</th>
							<th class="px-4 py-2 font-medium">{t.settings.colStatus}</th>
							<th class="px-4 py-2 text-right font-medium">{t.settings.colCount}</th>
						</tr>
					</thead>
					<tbody>
						{#each usage.item_steps as row}
							<tr class="border-b border-border last:border-b-0">
								<td class="px-4 py-2 text-muted">{row.capability}</td>
								<td class="px-4 py-2">{row.status}</td>
								<td class="px-4 py-2 text-right font-medium">{row.count}</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
		{/if}
	{/if}
</section>

<!-- Custos -->
<section>
	<h2 class="mb-3 text-[14px] font-semibold text-muted">{t.settings.billingSection}</h2>
	<div class="rounded-card border border-border bg-surface p-4">
		<p class="mb-3 text-[13px] text-muted">{t.settings.billingNote}</p>
		<a
			href="https://console.cloud.google.com/billing"
			target="_blank"
			rel="noopener noreferrer"
			class="inline-flex items-center gap-1.5 rounded-token border border-border bg-hover px-3 py-1.5 text-[13px] no-underline hover:opacity-80"
		>
			{t.settings.billingLink} ↗
		</a>
	</div>
</section>
