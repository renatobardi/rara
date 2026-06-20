<script lang="ts">
	import { onMount } from 'svelte';
	import { t } from '$lib/strings';

	type Provider = {
		name: string;
		capability: string;
		runtime: string;
		activation: string;
		cost: number;
		quality: number;
		enabled: boolean;
		heartbeat_at?: string;
	};

	type RoutingPolicy = {
		scope: string;
		cost_weight: number;
		quality_weight: number;
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
		by_status: ByStatus;
		done: number;
		failed: number;
		success_rate: number;
		queue: number;
		avg_attempt: number;
		last_activity_at: string;
	};

	// --- existing state ---
	let providers = $state<Provider[]>([]);
	let policies = $state<RoutingPolicy[]>([]);
	let loading = $state(true);
	let error = $state(false);
	let policiesLoading = $state(true);
	let policiesError = $state(false);
	let saving = $state<string | null>(null);
	let saveMsg = $state('');

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

	// ponytail: 5min stale threshold mirrors defaultHealthTTL in core router.go
	const STALE_MS = 5 * 60 * 1000;

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
		const url = days !== null ? `/api/workers/metrics?days=${days}` : '/api/workers/metrics';
		fetch(url)
			.then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
			.then((d) => {
				metricsWindow = Array.isArray(d) ? d : [];
				metricsWindowLoading = false;
			})
			.catch(() => {
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

		fetch('/api/workers')
			.then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
			.then((d) => {
				if (!Array.isArray(d)) throw new Error('unexpected payload');
				providers = d;
				loading = false;
			})
			.catch(() => {
				error = true;
				loading = false;
			});

		fetch('/api/routing-policies')
			.then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
			.then((d) => {
				if (!Array.isArray(d)) throw new Error('unexpected payload');
				policies = d;
				policiesLoading = false;
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
			const updated = { ...p, enabled: !p.enabled };
			const res = await fetch('/api/workers', {
				method: 'PUT',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify(updated)
			});
			if (res.ok) {
				providers = providers.map((x) => (x.name === p.name ? updated : x));
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
	{:else if metricsAllError || metricsWindowError}
		<p class="text-sm text-red-500">{t.workers.metricsError}</p>
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
				{#if loading}
					<p class="text-sm text-muted">{t.workers.loading}</p>
				{:else if error}
					<p class="text-sm text-red-500">{t.workers.error}</p>
				{:else if providers.length === 0}
					<p class="text-sm text-muted">{t.workers.empty}</p>
				{:else}
					<p class="text-[22px] font-bold text-text">
						{aliveCount}<span class="text-[14px] font-normal text-muted"> / {providers.length}</span>
					</p>
					<p class="mt-1 text-[11px] text-muted">
						{staleCount} stale · {asleepCount}
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
				{#if successRate === null}
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
				{#if totalDoneW === 0}
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
				{#if metricsAll.length === 0}
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

<!-- ── Workers table (unchanged) ── -->
<section class="mb-8">
	<h2 class="mb-4 text-[15px] font-semibold">{t.workers.title}</h2>

	{#if loading}
		<p class="text-sm text-muted">{t.workers.loading}</p>
	{:else if error}
		<p class="text-sm text-red-500">{t.workers.error}</p>
	{:else if providers.length === 0}
		<p class="text-sm text-muted">{t.workers.empty}</p>
	{:else}
		{#if saveMsg}
			<p class="mb-3 text-sm text-muted">{saveMsg}</p>
		{/if}
		<div class="overflow-x-auto rounded-xl border border-border">
			<table class="w-full border-collapse text-[13px]">
				<thead>
					<tr class="border-b border-border bg-surface-2 text-left text-muted">
						<th class="px-4 py-2.5 font-medium">{t.workers.colName}</th>
						<th class="px-4 py-2.5 font-medium">{t.workers.colCapability}</th>
						<th class="px-4 py-2.5 font-medium">{t.workers.colRuntime}</th>
						<th class="px-4 py-2.5 font-medium">{t.workers.colActivation}</th>
						<th class="px-4 py-2.5 font-medium">{t.workers.colCost}</th>
						<th class="px-4 py-2.5 font-medium">{t.workers.colQuality}</th>
						<th class="px-4 py-2.5 font-medium">{t.workers.colEnabled}</th>
						<th class="px-4 py-2.5"></th>
					</tr>
				</thead>
				<tbody>
					{#each providers as p}
						<tr class="border-b border-border last:border-0 hover:bg-hover">
							<td class="px-4 py-2.5 font-mono text-[12px]">{p.name}</td>
							<td class="px-4 py-2.5">{p.capability}</td>
							<td class="px-4 py-2.5">{p.runtime}</td>
							<td class="px-4 py-2.5">{p.activation}</td>
							<td class="px-4 py-2.5">{p.cost}</td>
							<td class="px-4 py-2.5">{p.quality}</td>
							<td class="px-4 py-2.5">
								<span
									class="inline-block rounded-full px-2 py-0.5 text-[11px] font-semibold {p.enabled
										? 'bg-green/20 text-green'
										: 'bg-surface-2 text-muted'}"
								>
									{p.enabled ? '●' : '○'}
								</span>
							</td>
							<td class="px-4 py-2.5">
								<button
									class="cursor-pointer rounded-token border border-border bg-surface-2 px-3 py-1 text-[12px] hover:bg-hover disabled:opacity-40"
									onclick={() => toggleProvider(p)}
									disabled={saving === p.name}
									aria-label="{p.enabled ? t.workers.disable : t.workers.enable} {p.name}"
								>
									{saving === p.name
										? t.workers.saving
										: p.enabled
											? t.workers.disable
											: t.workers.enable}
								</button>
							</td>
						</tr>
					{/each}
				</tbody>
			</table>
		</div>
	{/if}
</section>

<!-- ── Routing policies (unchanged) ── -->
<section>
	<h2 class="mb-4 text-[15px] font-semibold">{t.workers.policiesSection}</h2>

	{#if policiesLoading}
		<p class="text-sm text-muted">{t.workers.policiesLoading}</p>
	{:else if policiesError}
		<p class="text-sm text-red-500">{t.workers.policiesError}</p>
	{:else if policies.length === 0}
		<p class="text-sm text-muted">{t.workers.policiesEmpty}</p>
	{:else}
		<div class="overflow-x-auto rounded-xl border border-border">
			<table class="w-full border-collapse text-[13px]">
				<thead>
					<tr class="border-b border-border bg-surface-2 text-left text-muted">
						<th class="px-4 py-2.5 font-medium">{t.workers.colScope}</th>
						<th class="px-4 py-2.5 font-medium">{t.workers.colCostWeight}</th>
						<th class="px-4 py-2.5 font-medium">{t.workers.colQualityWeight}</th>
					</tr>
				</thead>
				<tbody>
					{#each policies as pol}
						<tr class="border-b border-border last:border-0 hover:bg-hover">
							<td class="px-4 py-2.5">{pol.scope}</td>
							<td class="px-4 py-2.5">{pol.cost_weight}</td>
							<td class="px-4 py-2.5">{pol.quality_weight}</td>
						</tr>
					{/each}
				</tbody>
			</table>
		</div>
	{/if}
</section>
