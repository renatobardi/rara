<script lang="ts">
	import { t } from '$lib/strings';

	type Provider = {
		name: string;
		capability: string;
		runtime: string;
		activation: string;
		cost: number;
		quality: number;
		latency_ms: number;
		enabled: boolean;
	};

	type RoutingPolicy = {
		scope: string;
		cost_weight: number;
		quality_weight: number;
		latency_weight: number;
	};

	let providers = $state<Provider[]>([]);
	let policies = $state<RoutingPolicy[]>([]);
	let loading = $state(true);
	let error = $state(false);
	let policiesLoading = $state(true);
	let policiesError = $state(false);

	let saving = $state<string | null>(null); // provider name being saved
	let saveMsg = $state('');

	$effect(() => {
		fetch('/api/providers')
			.then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
			.then((d) => {
				providers = d ?? [];
				loading = false;
			})
			.catch(() => {
				error = true;
				loading = false;
			});

		fetch('/api/routing-policies')
			.then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
			.then((d) => {
				policies = d ?? [];
				policiesLoading = false;
			})
			.catch(() => {
				policiesError = true;
				policiesLoading = false;
			});
	});

	async function toggleProvider(p: Provider) {
		saving = p.name;
		saveMsg = '';
		const updated = { ...p, enabled: !p.enabled };
		const res = await fetch('/api/providers', {
			method: 'PUT',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify(updated)
		});
		if (res.ok) {
			providers = providers.map((x) => (x.name === p.name ? updated : x));
			saveMsg = t.providers.saveOk;
		} else {
			saveMsg = t.providers.saveError;
		}
		saving = null;
	}
</script>

<section class="mb-8">
	<h2 class="mb-4 text-[15px] font-semibold">{t.providers.title}</h2>

	{#if loading}
		<p class="text-sm text-muted">{t.providers.loading}</p>
	{:else if error}
		<p class="text-sm text-red-500">{t.providers.error}</p>
	{:else if providers.length === 0}
		<p class="text-sm text-muted">{t.providers.empty}</p>
	{:else}
		{#if saveMsg}
			<p class="mb-3 text-sm text-muted">{saveMsg}</p>
		{/if}
		<div class="overflow-x-auto rounded-xl border border-border">
			<table class="w-full border-collapse text-[13px]">
				<thead>
					<tr class="border-b border-border bg-surface-2 text-left text-muted">
						<th class="px-4 py-2.5 font-medium">{t.providers.colName}</th>
						<th class="px-4 py-2.5 font-medium">{t.providers.colCapability}</th>
						<th class="px-4 py-2.5 font-medium">{t.providers.colRuntime}</th>
						<th class="px-4 py-2.5 font-medium">{t.providers.colActivation}</th>
						<th class="px-4 py-2.5 font-medium">{t.providers.colCost}</th>
						<th class="px-4 py-2.5 font-medium">{t.providers.colQuality}</th>
						<th class="px-4 py-2.5 font-medium">{t.providers.colLatency}</th>
						<th class="px-4 py-2.5 font-medium">{t.providers.colEnabled}</th>
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
							<td class="px-4 py-2.5">{p.latency_ms}</td>
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
								>
									{saving === p.name ? t.providers.saving : p.enabled ? 'Desativar' : 'Ativar'}
								</button>
							</td>
						</tr>
					{/each}
				</tbody>
			</table>
		</div>
	{/if}
</section>

<section>
	<h2 class="mb-4 text-[15px] font-semibold">{t.providers.policiesSection}</h2>

	{#if policiesLoading}
		<p class="text-sm text-muted">{t.providers.policiesLoading}</p>
	{:else if policiesError}
		<p class="text-sm text-red-500">{t.providers.policiesError}</p>
	{:else if policies.length === 0}
		<p class="text-sm text-muted">{t.providers.policiesEmpty}</p>
	{:else}
		<div class="overflow-x-auto rounded-xl border border-border">
			<table class="w-full border-collapse text-[13px]">
				<thead>
					<tr class="border-b border-border bg-surface-2 text-left text-muted">
						<th class="px-4 py-2.5 font-medium">{t.providers.colScope}</th>
						<th class="px-4 py-2.5 font-medium">{t.providers.colCostWeight}</th>
						<th class="px-4 py-2.5 font-medium">{t.providers.colQualityWeight}</th>
						<th class="px-4 py-2.5 font-medium">{t.providers.colLatencyWeight}</th>
					</tr>
				</thead>
				<tbody>
					{#each policies as pol}
						<tr class="border-b border-border last:border-0 hover:bg-hover">
							<td class="px-4 py-2.5">{pol.scope}</td>
							<td class="px-4 py-2.5">{pol.cost_weight}</td>
							<td class="px-4 py-2.5">{pol.quality_weight}</td>
							<td class="px-4 py-2.5">{pol.latency_weight ?? '—'}</td>
						</tr>
					{/each}
				</tbody>
			</table>
		</div>
	{/if}
</section>
