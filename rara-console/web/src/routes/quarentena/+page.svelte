<script lang="ts">
	import { t } from '$lib/strings';

	type QuarantineItem = {
		id: number;
		lane: string;
		source_ref: string;
		status: string;
	};

	let items = $state<QuarantineItem[]>([]);
	let loading = $state(true);
	let error = $state(false);

	// per-item review state: null = idle, 'pending' = in-flight, 'done' = resolved, 'err' = failed
	let reviewState = $state<Record<number, 'pending' | 'done' | 'err'>>({});

	$effect(() => {
		fetch('/api/quarantine')
			.then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
			.then((d) => (items = d))
			.catch(() => (error = true))
			.finally(() => (loading = false));
	});

	async function review(itemId: number, signal: 'up' | 'down') {
		reviewState = { ...reviewState, [itemId]: 'pending' };
		try {
			const r = await fetch('/api/quarantine/review', {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ item_id: itemId, signal })
			});
			if (r.ok) {
				reviewState = { ...reviewState, [itemId]: 'done' };
				// optimistic: remove from list
				items = items.filter((it) => it.id !== itemId);
			} else {
				reviewState = { ...reviewState, [itemId]: 'err' };
			}
		} catch {
			reviewState = { ...reviewState, [itemId]: 'err' };
		}
	}
</script>

{#if loading}
	<p class="text-muted">{t.quarantine.loading}</p>
{:else if error}
	<p class="text-red">{t.quarantine.error}</p>
{:else if items.length === 0}
	<p class="text-[13px] text-muted">{t.quarantine.empty}</p>
{:else}
	<div class="overflow-hidden rounded-card border border-border bg-surface">
		{#each items as item}
			<div class="flex items-center gap-4 border-b border-border px-4 py-3 last:border-b-0">
				<div class="flex flex-1 flex-col gap-0.5">
					<span class="text-[13.5px] font-medium">#{item.id}</span>
					<span class="text-[11px] text-muted"
						>{item.lane} · {item.source_ref}</span
					>
				</div>

				{#if reviewState[item.id] === 'pending'}
					<span class="text-[12px] text-muted">{t.quarantine.reviewing}</span>
				{:else if reviewState[item.id] === 'err'}
					<span class="text-[12px] text-red">{t.quarantine.reviewError}</span>
				{:else}
					<div class="flex gap-2">
						<button
							class="cursor-pointer rounded-token border border-border bg-transparent px-3 py-1 text-[12px] font-medium hover:bg-hover"
							onclick={() => review(item.id, 'up')}
						>
							{t.quarantine.rescue}
						</button>
						<button
							class="cursor-pointer rounded-token border border-red/30 bg-transparent px-3 py-1 text-[12px] font-medium text-red hover:bg-red/10"
							onclick={() => review(item.id, 'down')}
						>
							{t.quarantine.confirmDrop}
						</button>
					</div>
				{/if}
			</div>
		{/each}
	</div>
{/if}
