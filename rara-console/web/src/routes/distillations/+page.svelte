<script lang="ts">
	import { t } from '$lib/strings';
	import Paginator from '$lib/Paginator.svelte';

	type Distillation = {
		id: number;
		source_type: string;
		source_ref: string;
		title?: string;
		doc_context?: string;
		engine: string;
		status: string;
	};

	const STATUS_COLOR: Record<string, string> = {
		done: 'bg-green',
		failed: 'bg-red',
		filtered: 'bg-muted'
	};

	let items = $state<Distillation[]>([]);
	let loading = $state(true);
	let error = $state(false);

	$effect(() => {
		fetch('/api/distillations?limit=50')
			.then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
			.then((d) => (items = d))
			.catch(() => (error = true))
			.finally(() => (loading = false));
	});
</script>

{#if loading}
	<p class="text-muted">{t.distillations.loading}</p>
{:else if error}
	<p class="text-red">{t.distillations.error}</p>
{:else if items.length === 0}
	<p class="text-[13px] text-muted">{t.distillations.empty}</p>
{:else}
	<Paginator {items}>
		{#snippet children(page)}
			<div class="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
				{#each page as d}
					<a
						href="/distillations/{d.id}"
						class="flex flex-col gap-2 rounded-card border border-border bg-surface p-4 no-underline hover:bg-hover"
					>
						<div class="flex items-start justify-between gap-2">
							<span class="line-clamp-2 text-[13.5px] font-medium text-text">
								{d.title ?? `${d.source_type} · ${d.source_ref}`}
							</span>
							<span
								class="mt-0.5 h-[7px] w-[7px] flex-none rounded-full {STATUS_COLOR[d.status] ??
									'bg-amber'}"
							></span>
						</div>
						{#if d.doc_context}
							<p class="m-0 line-clamp-2 text-[12px] text-muted">{d.doc_context}</p>
						{/if}
						<div class="mt-auto flex items-center gap-2 text-[11px] text-muted">
							<span>{d.engine}</span>
							<span>·</span>
							<span>{d.source_type}</span>
						</div>
					</a>
				{/each}
			</div>
		{/snippet}
	</Paginator>
{/if}
