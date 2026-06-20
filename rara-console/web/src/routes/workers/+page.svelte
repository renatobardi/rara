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
	};

	type RoutingPolicy = {
		scope: string;
		cost_weight: number;
		quality_weight: number;
	};

	let providers = $state<Provider[]>([]);
	let policies = $state<RoutingPolicy[]>([]);
	let loading = $state(true);
	let error = $state(false);
	let policiesLoading = $state(true);
	let policiesError = $state(false);

	let saving = $state<string | null>(null); // provider name being saved
	let saveMsg = $state('');

	onMount(() => {
		fetch('/api/providers')
			.then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
			.then((d) => {
				providers = Array.isArray(d) ? d : [];
				loading = false;
			})
			.catch(() => {
				error = true;
				loading = false;
			});

		fetch('/api/routing-policies')
			.then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
			.then((d) => {
				policies = Array.isArray(d) ? d : [];
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
		try {
			const updated = { ...p, enabled: !p.enabled };
			const res = await fetch('/api/providers', {
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
									aria-label="{p.enabled ? 'Desativar' : 'Ativar'} {p.name}"
								>
									{saving === p.name ? t.workers.saving : p.enabled ? 'Desativar' : 'Ativar'}
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
