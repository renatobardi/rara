<script lang="ts">
	import { t } from '$lib/strings';

	type Flow = { id: number; name: string; source_type: string; enabled: boolean; version: number };
	type Provider = {
		name: string;
		capability: string;
		runtime: string;
		activation: string;
		enabled: boolean;
	};

	let flows = $state<Flow[]>([]);
	let providers = $state<Provider[]>([]);
	let loading = $state(true);
	let error = $state(false);

	// The "Visão geral" proves the BFF end to end: one call to the console's /api/overview, which
	// aggregates the live core surface (flows + providers) server-side.
	$effect(() => {
		fetch('/api/overview')
			.then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
			.then((d) => {
				flows = d.flows ?? [];
				providers = d.providers ?? [];
			})
			.catch(() => (error = true))
			.finally(() => (loading = false));
	});

	let enabledProviders = $derived(providers.filter((p) => p.enabled).length);
</script>

{#if loading}
	<p class="text-muted">{t.overview.loading}</p>
{:else if error}
	<p class="text-red">{t.overview.error}</p>
{:else}
	<div class="mb-6 grid grid-cols-3 gap-4">
		<div class="rounded-card border border-border bg-surface p-4">
			<div class="text-xs text-muted">{t.overview.kpiFlows}</div>
			<div class="mt-2 text-[30px] font-bold tracking-tight">{flows.length}</div>
		</div>
		<div class="rounded-card border border-border bg-surface p-4">
			<div class="text-xs text-muted">{t.overview.kpiProviders}</div>
			<div class="mt-2 text-[30px] font-bold tracking-tight">{providers.length}</div>
		</div>
		<div class="rounded-card border border-border bg-surface p-4">
			<div class="text-xs text-muted">{t.overview.kpiEnabled}</div>
			<div class="mt-2 text-[30px] font-bold tracking-tight">{enabledProviders}</div>
		</div>
	</div>

	<div class="grid grid-cols-2 gap-4">
		<div class="overflow-hidden rounded-card border border-border bg-surface">
			<h2 class="m-0 border-b border-border px-4 py-3 text-[13.5px] font-semibold">
				{t.overview.flowsPanel}
			</h2>
			{#each flows as f}
				<div class="flex items-center gap-3 border-b border-border px-4 py-3 last:border-b-0">
					<span class="h-[7px] w-[7px] rounded-full {f.enabled ? 'bg-green' : 'bg-muted'}"></span>
					<div class="flex-1 text-[13.5px]">{f.name}</div>
					<span class="text-[11px] text-muted">{f.source_type} · v{f.version}</span>
				</div>
			{:else}
				<p class="px-4 py-3 text-[13px] text-muted">{t.overview.empty}</p>
			{/each}
		</div>

		<div class="overflow-hidden rounded-card border border-border bg-surface">
			<h2 class="m-0 border-b border-border px-4 py-3 text-[13.5px] font-semibold">
				{t.overview.providersPanel}
			</h2>
			{#each providers as p}
				<div class="flex items-center gap-3 border-b border-border px-4 py-3 last:border-b-0 text-[12.5px]">
					<span class="h-[7px] w-[7px] rounded-full {p.enabled ? 'bg-green' : 'bg-muted'}"></span>
					<div class="flex-1">{p.name} <span class="text-[11px] text-muted">· {p.capability}</span></div>
					<span class="text-[11px] text-muted">{p.runtime} · {p.activation}</span>
				</div>
			{:else}
				<p class="px-4 py-3 text-[13px] text-muted">{t.overview.empty}</p>
			{/each}
		</div>
	</div>
{/if}
