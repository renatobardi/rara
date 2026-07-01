<script lang="ts">
	import { onDestroy } from 'svelte';
	import { t } from '$lib/strings';

	type AgentTask = {
		id: number;
		agent_id: number;
		agent_name?: string;
		instruction: string;
		status: 'queued' | 'dispatched' | 'running' | 'done' | 'failed' | 'cancelled';
		context_refs?: number[];
		priority: number;
		result?: unknown;
		error?: string;
		created_at: string;
		completed_at?: string;
	};

	const COLS: { key: string; label: string; statuses: AgentTask['status'][] }[] = [
		{ key: 'queued', label: t.tasks.colQueued, statuses: ['queued'] },
		{ key: 'running', label: t.tasks.colRunning, statuses: ['dispatched', 'running'] },
		{ key: 'done', label: t.tasks.colDone, statuses: ['done'] },
		{ key: 'failed', label: t.tasks.colFailed, statuses: ['failed'] },
		{ key: 'cancelled', label: t.tasks.colCancelled, statuses: ['cancelled'] }
	];

	let tasks = $state<AgentTask[]>([]);
	let loading = $state(true);
	let error = $state(false);
	let fetchSeq = 0;
	let expanded = $state<Set<number>>(new Set());

	function tasksForCol(col: (typeof COLS)[number]) {
		return tasks.filter((tk) => (col.statuses as string[]).includes(tk.status));
	}

	function toggleExpand(id: number) {
		const next = new Set(expanded);
		if (next.has(id)) next.delete(id);
		else next.add(id);
		expanded = next;
	}

	function statusBadgeClass(status: string): string {
		const map: Record<string, string> = {
			queued: 'bg-muted/20 text-muted',
			dispatched: 'bg-blue-500/20 text-blue-600',
			running: 'bg-yellow-500/20 text-yellow-700',
			done: 'bg-green-500/20 text-green-700',
			failed: 'bg-red-500/20 text-red-600',
			cancelled: 'bg-muted/30 text-muted'
		};
		return map[status] ?? 'bg-muted/20 text-muted';
	}

	async function fetchTasks() {
		const seq = ++fetchSeq;
		try {
			const r = await fetch('/api/agent-tasks');
			if (seq !== fetchSeq) return;
			if (r.ok) {
				const data = await r.json();
				tasks = Array.isArray(data) ? data : [];
				error = false;
			} else {
				error = true;
			}
		} catch {
			if (seq === fetchSeq) error = true;
		} finally {
			if (seq === fetchSeq) loading = false;
		}
	}

	fetchTasks();
	const poll = setInterval(fetchTasks, 5000);
	onDestroy(() => clearInterval(poll));
</script>

<section>
	<h2 class="mb-4 text-[15px] font-semibold">{t.tasks.title}</h2>

	{#if loading}
		<p class="text-[13px] text-muted">{t.tasks.loading}</p>
	{:else if error}
		<p class="text-[13px] text-red-500">{t.tasks.error}</p>
	{:else if tasks.length === 0}
		<p class="text-[13px] text-muted">{t.tasks.empty}</p>
	{:else}
		<div class="grid gap-3" style="grid-template-columns: repeat({COLS.length}, minmax(0, 1fr))">
			{#each COLS as col (col.key)}
				{@const colTasks = tasksForCol(col)}
				<div class="flex flex-col gap-2">
					<div class="flex items-center gap-1.5">
						<span class="text-[11px] font-semibold uppercase tracking-wide text-muted"
							>{col.label}</span
						>
						{#if colTasks.length > 0}
							<span class="rounded-full bg-surface-2 px-1.5 py-0.5 text-[10px] text-muted"
								>{colTasks.length}</span
							>
						{/if}
					</div>
					{#if colTasks.length === 0}
						<p class="text-[11px] text-muted/50">—</p>
					{:else}
						<ul class="flex flex-col gap-2">
							{#each colTasks as task (task.id)}
								<li class="rounded-xl border border-border bg-surface p-3">
									<div class="flex items-start justify-between gap-2">
										<p class="flex-1 line-clamp-2 text-[12px] text-text">{task.instruction}</p>
										<span
											class="shrink-0 rounded-full px-2 py-0.5 text-[10px] font-medium {statusBadgeClass(task.status)}"
											>{task.status}</span
										>
									</div>
									<p class="mt-1 text-[11px] text-muted">
										#{task.id}{#if task.agent_name} · {task.agent_name}{/if} ·
										{new Date(task.created_at).toLocaleString()}
									</p>
									{#if task.error}
										<p class="mt-1 line-clamp-2 text-[11px] text-red-500">{task.error}</p>
									{/if}
									{#if task.result || (task.context_refs && task.context_refs.length > 0)}
										<button
											class="mt-1.5 text-[11px] text-muted underline hover:text-text"
											onclick={() => toggleExpand(task.id)}
										>{expanded.has(task.id) ? '▲ fechar' : t.tasks.showResult}</button>
										{#if expanded.has(task.id)}
											<div class="mt-2 space-y-1">
												{#if task.context_refs && task.context_refs.length > 0}
													<p class="text-[11px] text-muted">
														<span class="font-semibold">{t.tasks.labelContext}:</span>
														{task.context_refs.join(', ')}
													</p>
												{/if}
												{#if task.result}
													<pre
														class="max-h-40 overflow-auto rounded bg-surface-2 p-2 text-[11px] text-text">{JSON.stringify(task.result, null, 2)}</pre>
												{/if}
											</div>
										{/if}
									{/if}
								</li>
							{/each}
						</ul>
					{/if}
				</div>
			{/each}
		</div>
	{/if}
</section>
