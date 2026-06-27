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
