<script lang="ts">
	import { t } from '$lib/strings';

	type ProviderHealth = { total: number; enabled: number; stale: number };
	type Health = { db_ok: boolean; last_reconcile_at?: string; providers: ProviderHealth };
	type ItemCount = { lane: string; status: string; count: number };
	type StepCount = { capability: string; status: string; count: number };
	type Usage = { items: ItemCount[]; item_steps: StepCount[]; distillations: number; quarantine: number };

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
