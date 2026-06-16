<script lang="ts">
	import { t } from '$lib/strings';

	type Decision = {
		id: number;
		item_id: number;
		gate: string;
		decision: string;
		score?: number;
		when: string;
	};

	let decisions = $state<Decision[]>([]);
	let loading = $state(true);
	let error = $state(false);
	let limit = $state(50);

	async function load() {
		loading = true;
		error = false;
		try {
			const r = await fetch(`/api/decisions?limit=${limit}`);
			if (!r.ok) throw r.status;
			decisions = (await r.json()) ?? [];
		} catch {
			error = true;
		} finally {
			loading = false;
		}
	}

	$effect(() => {
		load();
	});

	function decisionClass(d: string) {
		if (d === 'keep') return 'bg-green/20 text-green';
		if (d === 'drop') return 'bg-red-500/20 text-red-400';
		return 'bg-surface-2 text-muted';
	}

	function formatWhen(iso: string) {
		const d = new Date(iso);
		return d.toLocaleString('pt-BR', { dateStyle: 'short', timeStyle: 'short' });
	}
</script>

<div class="mb-4 flex items-center gap-4">
	<label class="flex items-center gap-2 text-[13px] text-muted">
		{t.auditoria.limitLabel}
		<select
			bind:value={limit}
			onchange={load}
			class="rounded-token border border-border bg-surface-2 px-2 py-1 text-[13px] text-text"
		>
			<option value={20}>20</option>
			<option value={50}>50</option>
			<option value={100}>100</option>
			<option value={200}>200</option>
		</select>
	</label>
</div>

{#if loading}
	<p class="text-sm text-muted">{t.auditoria.loading}</p>
{:else if error}
	<p class="text-sm text-red-500">{t.auditoria.error}</p>
{:else if decisions.length === 0}
	<p class="text-sm text-muted">{t.auditoria.empty}</p>
{:else}
	<div class="overflow-x-auto rounded-xl border border-border">
		<table class="w-full border-collapse text-[13px]">
			<thead>
				<tr class="border-b border-border bg-surface-2 text-left text-muted">
					<th class="px-4 py-2.5 font-medium">{t.auditoria.colID}</th>
					<th class="px-4 py-2.5 font-medium">{t.auditoria.colItem}</th>
					<th class="px-4 py-2.5 font-medium">{t.auditoria.colGate}</th>
					<th class="px-4 py-2.5 font-medium">{t.auditoria.colDecision}</th>
					<th class="px-4 py-2.5 font-medium">{t.auditoria.colScore}</th>
					<th class="px-4 py-2.5 font-medium">{t.auditoria.colWhen}</th>
					<th class="px-4 py-2.5"></th>
				</tr>
			</thead>
			<tbody>
				{#each decisions as d}
					<tr class="border-b border-border last:border-0 hover:bg-hover">
						<td class="px-4 py-2.5 tabular-nums text-muted">{d.id}</td>
						<td class="px-4 py-2.5 tabular-nums">{d.item_id}</td>
						<td class="px-4 py-2.5 font-mono text-[12px]">{d.gate}</td>
						<td class="px-4 py-2.5">
							<span
								class="inline-block rounded-full px-2 py-0.5 text-[11px] font-semibold {decisionClass(
									d.decision
								)}"
							>
								{d.decision}
							</span>
						</td>
						<td class="px-4 py-2.5 tabular-nums">{d.score != null ? d.score.toFixed(3) : '—'}</td>
						<td class="px-4 py-2.5 text-muted">{formatWhen(d.when)}</td>
						<td class="px-4 py-2.5">
							<a
								href="/pipeline?item={d.item_id}"
								class="text-[12px] text-muted no-underline hover:text-text"
							>
								{t.auditoria.itemLink}
							</a>
						</td>
					</tr>
				{/each}
			</tbody>
		</table>
	</div>
{/if}
