<script lang="ts">
	import { t } from '$lib/strings';

	const STATUSES = [
		'discovered',
		'to_text',
		'distilled',
		'done',
		'filtered',
		'quarantine',
		'failed'
	] as const;
	type Status = (typeof STATUSES)[number];

	type Item = { id: number; title: string; status: string; source_type?: string };
	type Step = { id: number; capability: string; provider: string; status: string; attempts: number };
	type PipelineData = { counts: Record<Status, number>; items: Record<Status, Item[]> };

	const STATUS_COLOR: Record<Status, string> = {
		discovered: 'bg-blue',
		to_text: 'bg-amber',
		distilled: 'bg-violet',
		done: 'bg-green',
		filtered: 'bg-muted',
		quarantine: 'bg-red',
		failed: 'bg-red'
	};

	let data = $state<PipelineData | null>(null);
	let loading = $state(true);
	let error = $state(false);

	let openItemId = $state<number | null>(null);
	let steps = $state<Step[]>([]);
	let stepsLoading = $state(false);
	let stepsError = $state(false);

	$effect(() => {
		fetch('/api/pipeline')
			.then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
			.then((d) => (data = d))
			.catch(() => (error = true))
			.finally(() => (loading = false));
	});

	async function toggleItem(id: number) {
		if (openItemId === id) {
			openItemId = null;
			steps = [];
			return;
		}
		openItemId = id;
		stepsLoading = true;
		stepsError = false;
		try {
			const r = await fetch(`/api/items/${id}/steps`);
			steps = r.ok ? await r.json() : [];
			if (!r.ok) stepsError = true;
		} catch {
			stepsError = true;
			steps = [];
		} finally {
			stepsLoading = false;
		}
	}
</script>

{#if loading}
	<p class="text-muted">{t.pipeline.loading}</p>
{:else if error}
	<p class="text-red">{t.pipeline.error}</p>
{:else if data}
	<!-- Contadores por status -->
	<div class="mb-6 grid grid-cols-7 gap-3">
		{#each STATUSES as st}
			<div class="rounded-card border border-border bg-surface p-3 text-center">
				<div class="flex items-center justify-center gap-1.5 text-[11px] text-muted">
					<span class="h-[6px] w-[6px] rounded-full {STATUS_COLOR[st]} flex-none"></span>
					{t.pipeline.statusLabels[st]}
				</div>
				<div class="mt-1 text-[22px] font-bold tracking-tight">{data.counts[st] ?? 0}</div>
			</div>
		{/each}
	</div>

	<!-- Listas por status (só exibe seções com itens) -->
	{#each STATUSES as st}
		{@const items = data.items[st] ?? []}
		{#if items.length > 0}
			<div class="mb-4 overflow-hidden rounded-card border border-border bg-surface">
				<h2
					class="m-0 flex items-center gap-2 border-b border-border px-4 py-3 text-[13.5px] font-semibold"
				>
					<span class="h-[7px] w-[7px] rounded-full {STATUS_COLOR[st]} flex-none"></span>
					{t.pipeline.statusLabels[st]}
					<span class="ml-auto text-[11px] font-normal text-muted">{items.length}</span>
				</h2>
				{#each items as item}
					<div class="border-b border-border last:border-b-0">
						<button
							class="flex w-full cursor-pointer items-center gap-3 border-0 bg-transparent px-4 py-3 text-left hover:bg-hover"
							onclick={() => toggleItem(item.id)}
						>
							<span class="flex-1 text-[13.5px]">{item.title ?? `#${item.id}`}</span>
							{#if item.source_type}
								<span class="text-[11px] text-muted">{item.source_type}</span>
							{/if}
							<span class="text-[11px] text-muted opacity-50">{openItemId === item.id ? '▲' : '▼'}</span>
						</button>

						{#if openItemId === item.id}
							<div class="border-t border-border bg-bg px-4 py-3">
								{#if stepsLoading}
									<p class="text-[12px] text-muted">{t.pipeline.stepsLoading}</p>
								{:else if stepsError}
									<p class="text-[12px] text-red">{t.pipeline.stepsError}</p>
								{:else if steps.length === 0}
									<p class="text-[12px] text-muted">{t.pipeline.stepsEmpty}</p>
								{:else}
									<table class="w-full text-[12px]">
										<thead>
											<tr class="text-left text-[11px] text-muted">
												<th class="pb-2 font-medium">{t.pipeline.colCapability}</th>
												<th class="pb-2 font-medium">{t.pipeline.colProvider}</th>
												<th class="pb-2 font-medium">{t.pipeline.colStatus}</th>
												<th class="pb-2 text-right font-medium">{t.pipeline.colAttempts}</th>
											</tr>
										</thead>
										<tbody>
											{#each steps as step}
												<tr class="border-t border-border">
													<td class="py-1.5">{step.capability}</td>
													<td class="py-1.5 text-muted">{step.provider}</td>
													<td class="py-1.5">
														<span class="flex items-center gap-1.5">
															<span class="h-[6px] w-[6px] rounded-full {step.status === 'done' ? 'bg-green' : step.status === 'failed' ? 'bg-red' : 'bg-amber'} flex-none"></span>
															{step.status}
														</span>
													</td>
													<td class="py-1.5 text-right text-muted">{step.attempts}</td>
												</tr>
											{/each}
										</tbody>
									</table>
								{/if}
							</div>
						{/if}
					</div>
				{/each}
			</div>
		{/if}
	{/each}

	<!-- Empty state quando todos os status estão zerados -->
	{#if STATUSES.every((st) => (data?.counts[st] ?? 0) === 0)}
		<p class="text-[13px] text-muted">{t.overview.empty}</p>
	{/if}
{/if}
