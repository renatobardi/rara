<script lang="ts">
	import { t } from '$lib/strings';

	type Feed = { id: number; feed_url: string; title: string; active: boolean };
	type Flow = { id: number; name: string; source_type: string; enabled: boolean; version: number };
	type Step = { flow_id: number; seq: number; capability: string; enabled: boolean };

	// --- Fontes (podcast feeds) ---
	let feeds = $state<Feed[]>([]);
	let feedsLoading = $state(true);
	let feedsError = $state(false);
	let newURL = $state('');
	let adding = $state(false);
	let feedMsg = $state('');
	let savingFeed = $state<number | null>(null);

	// --- Flows ---
	let flows = $state<Flow[]>([]);
	let flowsLoading = $state(true);
	let flowsError = $state(false);
	let savingFlow = $state<number | null>(null);
	let flowMsg = $state('');

	// Steps drill-down (lazy per flow).
	let openFlowId = $state<number | null>(null);
	let steps = $state<Step[]>([]);
	let stepsLoading = $state(false);
	let stepsError = $state(false);

	function loadFeeds() {
		feedsLoading = true;
		feedsError = false;
		fetch('/api/sources/podcast')
			.then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
			.then((d) => {
				feeds = d ?? [];
				feedsLoading = false;
			})
			.catch(() => {
				feedsError = true;
				feedsLoading = false;
			});
	}

	function loadFlows() {
		flowsLoading = true;
		flowsError = false;
		fetch('/api/flows')
			.then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
			.then((d) => {
				flows = d ?? [];
				flowsLoading = false;
			})
			.catch(() => {
				flowsError = true;
				flowsLoading = false;
			});
	}

	$effect(() => {
		loadFeeds();
		loadFlows();
	});

	async function addFeed(e: Event) {
		e.preventDefault();
		const url = newURL.trim();
		if (!url) return;
		adding = true;
		feedMsg = '';
		const res = await fetch('/api/sources/podcast', {
			method: 'POST',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify({ feed_url: url })
		});
		adding = false;
		if (res.ok) {
			newURL = '';
			loadFeeds();
		} else {
			feedMsg = t.fontesFlows.addError;
		}
	}

	async function toggleFeed(f: Feed) {
		savingFeed = f.id;
		feedMsg = '';
		const res = await fetch('/api/sources/podcast', {
			method: 'PUT',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify({ id: f.id, active: !f.active })
		});
		if (res.ok) {
			feeds = feeds.map((x) => (x.id === f.id ? { ...x, active: !x.active } : x));
		} else {
			feedMsg = t.fontesFlows.saveError;
		}
		savingFeed = null;
	}

	async function toggleFlow(fl: Flow) {
		savingFlow = fl.id;
		flowMsg = '';
		const updated = { ...fl, enabled: !fl.enabled };
		const res = await fetch('/api/flows', {
			method: 'PUT',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify(updated)
		});
		if (res.ok) {
			flows = flows.map((x) => (x.id === fl.id ? updated : x));
		} else {
			flowMsg = t.fontesFlows.saveError;
		}
		savingFlow = null;
	}

	async function toggleSteps(id: number) {
		if (openFlowId === id) {
			openFlowId = null;
			return;
		}
		openFlowId = id;
		steps = [];
		stepsError = false;
		stepsLoading = true;
		try {
			const r = await fetch(`/api/flows/${id}/steps`);
			if (!r.ok) throw new Error();
			steps = (await r.json()) ?? [];
		} catch {
			stepsError = true;
		}
		stepsLoading = false;
	}
</script>

<!-- Fontes -->
<section class="mb-8">
	<h2 class="mb-4 text-[15px] font-semibold">{t.fontesFlows.sourcesSection}</h2>

	<form class="mb-4 flex gap-2" onsubmit={addFeed}>
		<input
			type="url"
			required
			bind:value={newURL}
			aria-label={t.fontesFlows.colURL}
			placeholder={t.fontesFlows.addPlaceholder}
			class="flex-1 rounded-token border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-text/40"
		/>
		<button
			type="submit"
			disabled={adding || newURL.trim() === ''}
			class="cursor-pointer rounded-token border border-border bg-surface-2 px-4 py-2 text-[13px] hover:bg-hover disabled:opacity-40"
		>
			{adding ? t.fontesFlows.adding : t.fontesFlows.addBtn}
		</button>
	</form>

	{#if feedMsg}
		<p class="mb-3 text-sm text-red-500">{feedMsg}</p>
	{/if}

	{#if feedsLoading}
		<p class="text-sm text-muted">{t.fontesFlows.sourcesLoading}</p>
	{:else if feedsError}
		<p class="text-sm text-red-500">{t.fontesFlows.sourcesError}</p>
	{:else if feeds.length === 0}
		<p class="text-sm text-muted">{t.fontesFlows.sourcesEmpty}</p>
	{:else}
		<div class="overflow-x-auto rounded-xl border border-border">
			<table class="w-full border-collapse text-[13px]">
				<thead>
					<tr class="border-b border-border bg-surface-2 text-left text-muted">
						<th class="px-4 py-2.5 font-medium">{t.fontesFlows.colURL}</th>
						<th class="px-4 py-2.5 font-medium">{t.fontesFlows.colTitle}</th>
						<th class="px-4 py-2.5 font-medium">{t.fontesFlows.colActive}</th>
						<th class="px-4 py-2.5"></th>
					</tr>
				</thead>
				<tbody>
					{#each feeds as f}
						<tr class="border-b border-border last:border-0 hover:bg-hover">
							<td class="px-4 py-2.5 font-mono text-[12px]">{f.feed_url}</td>
							<td class="px-4 py-2.5 {f.title ? '' : 'text-muted'}">{f.title || t.fontesFlows.titlePending}</td>
							<td class="px-4 py-2.5">
								<span
									class="inline-block rounded-full px-2 py-0.5 text-[11px] font-semibold {f.active
										? 'bg-green/20 text-green'
										: 'bg-surface-2 text-muted'}"
								>
									{f.active ? '●' : '○'}
								</span>
							</td>
							<td class="px-4 py-2.5">
								<button
									class="cursor-pointer rounded-token border border-border bg-surface-2 px-3 py-1 text-[12px] hover:bg-hover disabled:opacity-40"
									onclick={() => toggleFeed(f)}
									disabled={savingFeed === f.id}
								>
									{savingFeed === f.id
										? t.fontesFlows.saving
										: f.active
											? t.fontesFlows.deactivate
											: t.fontesFlows.activate}
								</button>
							</td>
						</tr>
					{/each}
				</tbody>
			</table>
		</div>
	{/if}
</section>

<!-- Flows -->
<section>
	<h2 class="mb-4 text-[15px] font-semibold">{t.fontesFlows.flowsSection}</h2>

	{#if flowMsg}
		<p class="mb-3 text-sm text-red-500">{flowMsg}</p>
	{/if}

	{#if flowsLoading}
		<p class="text-sm text-muted">{t.fontesFlows.flowsLoading}</p>
	{:else if flowsError}
		<p class="text-sm text-red-500">{t.fontesFlows.flowsError}</p>
	{:else if flows.length === 0}
		<p class="text-sm text-muted">{t.fontesFlows.flowsEmpty}</p>
	{:else}
		<div class="overflow-x-auto rounded-xl border border-border">
			<table class="w-full border-collapse text-[13px]">
				<thead>
					<tr class="border-b border-border bg-surface-2 text-left text-muted">
						<th class="px-4 py-2.5 font-medium">{t.fontesFlows.colName}</th>
						<th class="px-4 py-2.5 font-medium">{t.fontesFlows.colSourceType}</th>
						<th class="px-4 py-2.5 font-medium">{t.fontesFlows.colVersion}</th>
						<th class="px-4 py-2.5 font-medium">{t.fontesFlows.colEnabled}</th>
						<th class="px-4 py-2.5"></th>
						<th class="px-4 py-2.5"></th>
					</tr>
				</thead>
				<tbody>
					{#each flows as fl}
						<tr class="border-b border-border last:border-0 hover:bg-hover">
							<td class="px-4 py-2.5 font-mono text-[12px]">{fl.name}</td>
							<td class="px-4 py-2.5">{fl.source_type}</td>
							<td class="px-4 py-2.5">{fl.version}</td>
							<td class="px-4 py-2.5">
								<span
									class="inline-block rounded-full px-2 py-0.5 text-[11px] font-semibold {fl.enabled
										? 'bg-green/20 text-green'
										: 'bg-surface-2 text-muted'}"
								>
									{fl.enabled ? '●' : '○'}
								</span>
							</td>
							<td class="px-4 py-2.5">
								<button
									class="cursor-pointer rounded-token border border-border bg-surface-2 px-3 py-1 text-[12px] hover:bg-hover disabled:opacity-40"
									onclick={() => toggleFlow(fl)}
									disabled={savingFlow === fl.id}
								>
									{savingFlow === fl.id
										? t.fontesFlows.saving
										: fl.enabled
											? t.fontesFlows.deactivate
											: t.fontesFlows.activate}
								</button>
							</td>
							<td class="px-4 py-2.5">
								<button
									class="cursor-pointer rounded-token border border-border bg-surface-2 px-3 py-1 text-[12px] hover:bg-hover"
									onclick={() => toggleSteps(fl.id)}
									aria-expanded={openFlowId === fl.id}
								>
									{t.fontesFlows.stepsToggle}
								</button>
							</td>
						</tr>
						{#if openFlowId === fl.id}
							<tr class="border-b border-border bg-surface-2/40">
								<td colspan="6" class="px-4 py-3">
									{#if stepsLoading}
										<p class="text-sm text-muted">{t.fontesFlows.stepsLoading}</p>
									{:else if stepsError}
										<p class="text-sm text-red-500">{t.fontesFlows.stepsError}</p>
									{:else if steps.length === 0}
										<p class="text-sm text-muted">{t.fontesFlows.stepsEmpty}</p>
									{:else}
										<table class="w-full border-collapse text-[12px]">
											<thead>
												<tr class="text-left text-muted">
													<th class="py-1 pr-4 font-medium">{t.fontesFlows.colSeq}</th>
													<th class="py-1 pr-4 font-medium">{t.fontesFlows.colCapability}</th>
													<th class="py-1 pr-4 font-medium">{t.fontesFlows.colEnabled}</th>
												</tr>
											</thead>
											<tbody>
												{#each steps as st}
													<tr>
														<td class="py-1 pr-4">{st.seq}</td>
														<td class="py-1 pr-4 font-mono">{st.capability}</td>
														<td class="py-1 pr-4">{st.enabled ? '●' : '○'}</td>
													</tr>
												{/each}
											</tbody>
										</table>
									{/if}
								</td>
							</tr>
						{/if}
					{/each}
				</tbody>
			</table>
		</div>
	{/if}
</section>
