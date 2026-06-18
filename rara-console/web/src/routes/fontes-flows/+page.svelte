<script lang="ts">
	import { t } from '$lib/strings';

	type Feed = { id: number; feed_url: string; title: string; active: boolean };
	type Flow = { id: number; name: string; source_type: string; enabled: boolean; version: number };
	type Step = { flow_id: number; seq: number; capability: string; enabled: boolean };
	type Provider = { name: string; capability: string; runtime: string; activation: string; enabled: boolean };
	type StepHostsKey = `${number}:${number}`;

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

	// Hosts editor (lazy per step).
	let openHostsKey = $state<StepHostsKey | null>(null);
	let hostsAvailable = $state<Provider[]>([]);
	let hostsPriority = $state<string[]>([]); // the edited ordered list
	let hostsLoading = $state(false);
	let hostsError = $state(false);
	let hostsSaving = $state(false);
	let hostsMsg = $state('');
	let selectedAddHost = $state('');

	function hostsKey(flowId: number, seq: number): StepHostsKey {
		return `${flowId}:${seq}`;
	}

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
			openHostsKey = null;
			return;
		}
		openFlowId = id;
		openHostsKey = null;
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

	async function toggleHosts(flowId: number, seq: number) {
		if (!Number.isInteger(flowId) || flowId <= 0 || !Number.isInteger(seq) || seq < 0) {
			hostsError = true;
			hostsLoading = false;
			return;
		}
		const key = hostsKey(flowId, seq);
		if (openHostsKey === key) {
			openHostsKey = null;
			return;
		}
		openHostsKey = key;
		hostsAvailable = [];
		hostsPriority = [];
		hostsError = false;
		hostsLoading = true;
		hostsMsg = '';
		selectedAddHost = '';
		const ctrl = new AbortController();
		const tid = setTimeout(() => ctrl.abort(), 15000);
		try {
			const r = await fetch(`/api/flows/${flowId}/steps/${seq}/hosts`, { signal: ctrl.signal });
			clearTimeout(tid);
			if (!r.ok) {
				hostsMsg = r.status >= 500 ? `${t.fontesFlows.hostsError} (${r.status})` : t.fontesFlows.hostsError;
				throw new Error();
			}
			const d = await r.json();
			hostsAvailable = d.available ?? [];
			hostsPriority = d.providers ?? [];
		} catch {
			clearTimeout(tid);
			hostsError = true;
		}
		hostsLoading = false;
	}

	function hostsAvailableToAdd(priority: string[], available: Provider[]): Provider[] {
		const inList = new Set(priority);
		return available.filter((p) => !inList.has(p.name));
	}

	function moveHost(idx: number, dir: -1 | 1) {
		const newIdx = idx + dir;
		if (newIdx < 0 || newIdx >= hostsPriority.length) return;
		const arr = [...hostsPriority];
		[arr[idx], arr[newIdx]] = [arr[newIdx], arr[idx]];
		hostsPriority = arr;
	}

	function removeHost(idx: number) {
		hostsPriority = hostsPriority.filter((_, i) => i !== idx);
	}

	function addHost(name: string) {
		if (!name || hostsPriority.includes(name)) return;
		hostsPriority = [...hostsPriority, name];
	}

	async function saveHosts(flowId: number, seq: number) {
		if (!Number.isInteger(flowId) || flowId <= 0 || !Number.isInteger(seq) || seq < 0) {
			hostsMsg = t.fontesFlows.hostsSaveError;
			return;
		}
		const validNames = new Set(hostsAvailable.map((p) => p.name));
		const invalid = hostsPriority.filter((n) => !validNames.has(n));
		if (invalid.length > 0) {
			hostsMsg = t.fontesFlows.hostsSaveError;
			return;
		}
		hostsSaving = true;
		hostsMsg = '';
		const ctrl = new AbortController();
		const tid = setTimeout(() => ctrl.abort(), 15000);
		try {
			const res = await fetch(`/api/flows/${flowId}/steps/${seq}/hosts`, {
				signal: ctrl.signal,
				method: 'PUT',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ providers: hostsPriority })
			});
			clearTimeout(tid);
			hostsMsg = res.ok
				? t.fontesFlows.hostsSaveOk
				: res.status >= 500
					? `${t.fontesFlows.hostsSaveError} (${res.status})`
					: t.fontesFlows.hostsSaveError;
		} catch {
			clearTimeout(tid);
			hostsMsg = t.fontesFlows.hostsSaveError;
		} finally {
			hostsSaving = false;
		}
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
													<th class="py-1 pr-4"></th>
												</tr>
											</thead>
											<tbody>
												{#each steps as st}
													<tr class="border-t border-border/50">
														<td class="py-1.5 pr-4">{st.seq}</td>
														<td class="py-1.5 pr-4 font-mono">{st.capability}</td>
														<td class="py-1.5 pr-4">{st.enabled ? '●' : '○'}</td>
														<td class="py-1.5 pr-4">
															<button
																class="cursor-pointer rounded border border-border bg-bg px-2 py-0.5 text-[11px] hover:bg-hover {openHostsKey === hostsKey(st.flow_id, st.seq) ? 'bg-surface-2' : ''}"
																onclick={() => toggleHosts(st.flow_id, st.seq)}
																aria-expanded={openHostsKey === hostsKey(st.flow_id, st.seq)}
																aria-label={`Editar hosts para step ${st.seq}`}
															>
																{t.fontesFlows.hostsToggle}
															</button>
														</td>
													</tr>
													{#if openHostsKey === hostsKey(st.flow_id, st.seq)}
														<tr>
															<td colspan="4" class="py-2 pl-4">
																{#if hostsLoading}
																	<p class="text-muted">{t.fontesFlows.hostsLoading}</p>
																{:else if hostsError}
																	<p class="text-red-500">{t.fontesFlows.hostsError}</p>
																{:else}
																	<div class="rounded-lg border border-border bg-bg p-3">
																		<!-- Priority list -->
																		{#if hostsPriority.length === 0}
																			<p class="mb-2 text-[11px] text-muted">{t.fontesFlows.hostsEmpty}</p>
																		{:else}
																			<ol class="mb-2 space-y-1">
																				{#each hostsPriority as name, i}
																					<li class="flex items-center gap-1.5">
																						<span class="w-4 text-center text-[10px] text-muted">{i + 1}</span>
																						<span class="flex-1 font-mono text-[11px]">{name}</span>
																						<button
																							class="rounded px-1 py-0.5 text-[10px] text-muted hover:bg-hover disabled:opacity-30"
																							onclick={() => moveHost(i, -1)}
																							disabled={i === 0}
																							aria-label="mover para cima">{t.fontesFlows.hostsUp}</button
																						>
																						<button
																							class="rounded px-1 py-0.5 text-[10px] text-muted hover:bg-hover disabled:opacity-30"
																							onclick={() => moveHost(i, 1)}
																							disabled={i === hostsPriority.length - 1}
																							aria-label="mover para baixo">{t.fontesFlows.hostsDown}</button
																						>
																						<button
																							class="rounded px-1 py-0.5 text-[10px] text-muted hover:bg-hover"
																							onclick={() => removeHost(i)}
																							aria-label="remover">{t.fontesFlows.hostsRemove}</button
																						>
																					</li>
																				{/each}
																			</ol>
																		{/if}

																		<!-- Add from available -->
																		{#if hostsAvailableToAdd(hostsPriority, hostsAvailable).length > 0}
																			<div class="mb-2 flex gap-1.5">
																				<select
																					id="add-host-{st.flow_id}-{st.seq}"
																					bind:value={selectedAddHost}
																					class="flex-1 rounded border border-border bg-surface-2 px-2 py-1 text-[11px]"
																					aria-label={t.fontesFlows.hostsAddPlaceholder}
																				>
																					<option value="">{t.fontesFlows.hostsAddPlaceholder}</option>
																					{#each hostsAvailableToAdd(hostsPriority, hostsAvailable) as p}
																						<option value={p.name}>{p.name}</option>
																					{/each}
																				</select>
																				<button
																					class="cursor-pointer rounded border border-border bg-surface-2 px-2 py-1 text-[11px] hover:bg-hover disabled:opacity-40"
																					disabled={!selectedAddHost}
																					onclick={() => { addHost(selectedAddHost); selectedAddHost = ''; }}
																				>+</button>
																			</div>
																		{/if}

																		<!-- Save -->
																		<div class="flex items-center gap-2">
																			<button
																				class="cursor-pointer rounded border border-border bg-surface-2 px-3 py-1 text-[11px] hover:bg-hover disabled:opacity-40"
																				onclick={() => saveHosts(st.flow_id, st.seq)}
																				disabled={hostsSaving}
																			>
																				{hostsSaving ? t.fontesFlows.hostsSaving : t.fontesFlows.hostsSaveBtn}
																			</button>
																			{#if hostsMsg}
																				<span class="text-[11px] text-muted" aria-live="polite" role="status">{hostsMsg}</span>
																			{/if}
																		</div>
																	</div>
																{/if}
															</td>
														</tr>
													{/if}
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
