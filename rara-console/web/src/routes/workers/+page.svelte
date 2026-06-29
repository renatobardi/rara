<script lang="ts">
	import { onMount } from 'svelte';
	import { t } from '$lib/strings';
	import { timeAgo } from '$lib/timeAgo';
	import WorkerForm from '$lib/WorkerForm.svelte';
	import { isModel, type LLMModel } from '$lib/inferencia';

	type Constraints = {
		requires?: string;
		accepts?: string[];
		sensitivity?: string;
	};

	type Provider = {
		name: string;
		worker?: string;
		app?: string;
		capability: string;
		runtime: string;
		activation: string;
		enabled: boolean;
		heartbeat_at?: string;
		last_collect_at?: string;
		last_error?: string;
		constraints?: Constraints;
		runner_url?: string;
		env?: Record<string, string>;
		description?: string;
	};

	type Worker = {
		name: string;
		capability: string;
		placements: Provider[];
	};

	function isProvider(v: unknown): v is Provider {
		if (typeof v !== 'object' || v === null) return false;
		const p = v as Record<string, unknown>;
		return (
			typeof p.name === 'string' && typeof p.capability === 'string' &&
			typeof p.runtime === 'string' && typeof p.activation === 'string' &&
			typeof p.enabled === 'boolean'
		);
	}

	function isWorker(v: unknown): v is Worker {
		if (typeof v !== 'object' || v === null) return false;
		const w = v as Record<string, unknown>;
		return (
			typeof w.name === 'string' && typeof w.capability === 'string' &&
			Array.isArray(w.placements) && w.placements.every(isProvider)
		);
	}

	// --- core state ---
	let workers = $state<Worker[]>([]);
	let models = $state<LLMModel[]>([]);
	// 'loading' until the first /api/llm-models resolves, then 'ready' (≥1 model) or 'failed'
	// (fetch error, malformed body, or empty registry). WorkerForm blocks an LLM save unless
	// 'ready', so a worker never saves modelless while the list is still unknown — and shows a
	// "loading" hint vs a "failed, reload" error so the in-flight window isn't a false alarm.
	let modelsStatus = $state<'loading' | 'ready' | 'failed'>('loading');

	function loadModels() {
		modelsStatus = 'loading';
		fetch('/api/llm-models')
			.then((r) => (r.ok ? r.json() : Promise.reject()))
			.then((d) => {
				// A malformed body is a load failure, not "no models" — else WorkerForm wouldn't
				// block (modelOptions empty + status ready = silent save of an LLM worker).
				if (!Array.isArray(d) || !d.every(isModel)) throw new Error('unexpected payload');
				models = d;
				// An empty registry is still "no model to pick" — the bug covers "ou volta vazio".
				modelsStatus = d.length > 0 ? 'ready' : 'failed';
			})
			.catch(() => { models = []; modelsStatus = 'failed'; /* WorkerForm bloqueia salvar worker LLM sem Model */ });
	}
	let loading = $state(true);
	let error = $state(false);
	let saving = $state<string | null>(null);

	// --- metricsLite: lightweight fetch just for last_activity_at column ---
	type MetricLite = { provider: string; last_activity_at?: string };
	let metricsLite = $state<MetricLite[]>([]);
	let lastActivityByProvider = $derived(new Map(metricsLite.map((m) => [m.provider, m.last_activity_at])));
	const ITEM_STEP_CAPABILITIES = new Set(['destilar', 'gate_barato', 'gate_rico', 'extrair', 'transcrever']);
	function lastRun(p: Provider): string | undefined {
		if (p.last_collect_at) return p.last_collect_at;
		return ITEM_STEP_CAPABILITIES.has(p.capability) ? lastActivityByProvider.get(p.name) : undefined;
	}

	// --- CRUD form state ---
	let formMode = $state<'add' | 'edit' | null>(null);
	let formInitial = $state<Provider | null>(null);
	let formLockedWorker = $state<string | null>(null);
	let formLockedCapability = $state<string | null>(null);
	let formLockedApp = $state<string | null>(null);

	function fetchWorkers() {
		loading = true;
		error = false;
		fetch('/api/workers')
			.then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
			.then((d) => {
				if (!Array.isArray(d) || !d.every(isWorker)) throw new Error('unexpected payload');
				workers = d;
				loading = false;
			})
			.catch(() => {
				error = true;
				loading = false;
			});
	}

	onMount(() => {
		fetchWorkers();
		fetch('/api/workers/metrics')
			.then((r) => (r.ok ? r.json() : Promise.reject()))
			.then((d) => { metricsLite = Array.isArray(d) ? d : []; })
			.catch(() => { /* coluna "última execução" degrada para — */ });
		loadModels();
	});

	// ── filtros + busca (client-side, lista pequena) ──
	// ponytail: client-side filter, lista pequena; migrar p/ server-side se passar de ~centenas
	let search = $state('');
	let searchOpen = $state(false);
	let fCapability = $state('');
	let fRuntime = $state('');
	let fActivation = $state('');
	let fStatus = $state(''); // '' | 'enabled' | 'disabled'

	// ── sort ──
	type SortCol = 'capability' | 'worker' | 'name' | 'runtime' | 'activation' | 'status' | 'lastrun';
	let sortBy = $state<SortCol>('worker');
	let sortDir = $state<'asc' | 'desc'>('asc');

	// ── popover de coluna + kebab de linha (um aberto por vez) ──
	let activePopover = $state<string | null>(null);
	let activeKebab = $state<string | null>(null); // placement.name da linha com ⋮ aberto

	// ── toasts ──
	type Toast = { id: number; kind: 'ok' | 'err'; msg: string };
	let toasts = $state<Toast[]>([]);
	let toastSeq = 0;
	const toastTimers: ReturnType<typeof setTimeout>[] = [];
	function toast(kind: 'ok' | 'err', msg: string) {
		const id = ++toastSeq;
		toasts = [...toasts, { id, kind, msg }];
		toastTimers.push(setTimeout(() => (toasts = toasts.filter((x) => x.id !== id)), 4000));
	}
	$effect(() => () => toastTimers.forEach(clearTimeout));

	// ── flat rows ──
	type FlatRow = { workerName: string; capability: string; placement: Provider };
	let flatRows = $derived<FlatRow[]>(
		workers.flatMap((w) =>
			w.placements.map((p) => ({ workerName: w.name, capability: w.capability, placement: p }))
		)
	);

	let hasFilter = $derived(!!(fCapability || fRuntime || fActivation || fStatus || search.trim()));

	let filteredRows = $derived.by(() => {
		const q = search.trim().toLowerCase();
		const rows = flatRows.filter((r) => {
			if (q && !r.workerName.toLowerCase().includes(q) && !r.placement.name.toLowerCase().includes(q)) return false;
			if (fCapability && r.capability !== fCapability) return false;
			if (fRuntime && r.placement.runtime !== fRuntime) return false;
			if (fActivation && r.placement.activation !== fActivation) return false;
			if (fStatus === 'enabled' && !r.placement.enabled) return false;
			if (fStatus === 'disabled' && r.placement.enabled) return false;
			return true;
		});
		const dir = sortDir === 'asc' ? 1 : -1;
		const key = (r: FlatRow): string => {
			switch (sortBy) {
				case 'capability': return r.capability;
				case 'worker': return r.workerName;
				case 'name': return r.placement.name;
				case 'runtime': return r.placement.runtime;
				case 'activation': return r.placement.activation;
				case 'status': return r.placement.enabled ? '1' : '0';
				case 'lastrun': return lastRun(r.placement) ?? '1970-01-01';
			}
		};
		return [...rows].sort((a, b) => key(a).localeCompare(key(b)) * dir);
	});

	// capabilities e activations únicos (para o WorkerForm e filtros)
	let knownCapabilities = $derived([...new Set(flatRows.map((r) => r.capability))].sort());
	let knownActivations = $derived([...new Set(flatRows.map((r) => r.placement.activation))].sort());

	function setSort(col: SortCol, dir: 'asc' | 'desc') {
		sortBy = col;
		sortDir = dir;
		activePopover = null;
	}

	function clearFilters() {
		fCapability = '';
		fRuntime = '';
		fActivation = '';
		fStatus = '';
		search = '';
		searchOpen = false;
		activePopover = null;
	}

	// fecha popover/kebab ao clicar fora (mesma mecânica do Fontes)
	function onWindowClick(e: MouseEvent) {
		if (!(e.target instanceof Element)) return;
		const el = e.target;
		if (activeKebab && !el.closest('[data-kebab]')) activeKebab = null;
		if (activePopover && !el.closest('[data-col-popover]')) activePopover = null;
	}
	function closeOnEsc(e: KeyboardEvent) {
		if (e.key !== 'Escape') return;
		if (activeKebab) { activeKebab = null; return; }
		if (activePopover) { activePopover = null; return; }
		if (searchOpen) { searchOpen = false; search = ''; }
	}

	async function toggleProvider(p: Provider, workerName: string) {
		saving = p.name;
		try {
			// ponytail: explicit DTO — omit heartbeat_at (core preserves it from DB on upsert).
			// description/env must stay: full-record upsert, omitting them would clear the DB columns.
			const dto = {
				worker: workerName, name: p.name, app: p.app, capability: p.capability, runtime: p.runtime,
				activation: p.activation,
				enabled: !p.enabled, constraints: p.constraints,
				runner_url: p.runner_url, env: p.env, description: p.description
			};
			const optimistic: Provider = { ...p, worker: workerName, enabled: !p.enabled };
			const res = await fetch('/api/placements', {
				method: 'PUT',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify(dto)
			});
			if (res.ok) {
				workers = workers.map((w) => ({
					...w,
					placements: w.placements.map((x) => (x.name === p.name ? optimistic : x))
				}));
				toast('ok', t.workers.saveOkToast);
			} else {
				toast('err', t.workers.saveError);
			}
		} catch {
			toast('err', t.workers.saveError);
		} finally {
			saving = null;
		}
	}

	function openAdd() {
		if (modelsStatus !== 'ready') loadModels(); // recover a transient model-fetch failure on reopen
		formInitial = null;
		formMode = 'add';
		formLockedWorker = null;
		formLockedCapability = null;
		formLockedApp = null;
	}

	function openEdit(p: Provider, workerName: string) {
		if (modelsStatus !== 'ready') loadModels(); // recover a transient model-fetch failure on reopen
		formInitial = { ...p, worker: workerName };
		formMode = 'edit';
		formLockedWorker = null;
		formLockedCapability = null;
		formLockedApp = null;
	}

	function closeForm() {
		formMode = null;
		formInitial = null;
		formLockedWorker = null;
		formLockedCapability = null;
		formLockedApp = null;
	}

	async function saveWorker(payload: Provider) {
		try {
			const res = await fetch('/api/placements', {
				method: 'PUT',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify(payload)
			});
			if (!res.ok) {
				try { const b = await res.json(); if (b?.error) console.warn('[saveWorker]', b.error); } catch { /* ignore */ }
				throw new Error(t.workers.saveError);
			}
			toast('ok', t.workers.saveOkToast);
			closeForm();
			fetchWorkers();
		} catch (err) {
			// Re-throw so WorkerForm's onSave catch can surface the error to the user.
			throw err instanceof Error ? err : new Error(t.workers.saveError);
		}
	}
</script>

<svelte:window onkeydown={closeOnEsc} onclick={onWindowClick} />

<!-- ── Workers table (flat, Fontes-style) ── -->
<section>
	<!-- Barra de topo (espelha fontes/+page.svelte:726-754) -->
	<div class="mb-5 flex items-center gap-2">
		<button
			class="flex h-[34px] w-[34px] flex-none items-center justify-center rounded-token border border-border text-muted hover:bg-hover {searchOpen ? 'bg-hover' : ''}"
			aria-label={t.workers.searchToggle}
			onclick={() => { searchOpen = !searchOpen; if (!searchOpen) search = ''; }}
		>
			<svg viewBox="0 0 20 20" width="15" height="15" fill="none" stroke="currentColor" stroke-width="1.7" aria-hidden="true">
				<circle cx="8.5" cy="8.5" r="5.5"/><path d="M13.5 13.5 18 18" stroke-linecap="round"/>
			</svg>
		</button>
		{#if searchOpen}
			<!-- svelte-ignore a11y_autofocus -->
			<input
				autofocus
				bind:value={search}
				placeholder={t.workers.searchPlaceholder}
				class="h-[34px] flex-1 rounded-token border border-border bg-bg px-3 text-[13px] outline-none focus:border-text/40"
			/>
		{/if}
		{#if hasFilter}
			<button class="text-[12px] text-muted hover:text-text" onclick={clearFilters}>{t.workers.filterClear}</button>
		{/if}
		{#if !loading && !error}
			<button
				class="ml-auto flex-none rounded-token bg-text px-3.5 py-1.5 text-[13px] font-medium text-bg hover:opacity-90"
				onclick={openAdd}
			>+ {t.workers.addWorker}</button>
		{/if}
	</div>

	{#if formMode === 'add' && !formLockedWorker}
		<div class="mb-4">
			<WorkerForm initial={null} lockedApp={null} capabilities={knownCapabilities} {models} {modelsStatus} onSave={saveWorker} onCancel={closeForm} />
		</div>
	{/if}

	{#if loading}
		<p class="text-[13px] text-muted">{t.workers.loading}</p>
	{:else if error}
		<p class="text-[13px] text-red-500">{t.workers.error}</p>
	{:else if workers.length === 0}
		<p class="text-[13px] text-muted">{t.workers.empty}</p>
	{:else if filteredRows.length === 0}
		<p class="text-[13px] text-muted">{t.workers.emptyFiltered}</p>
	{:else}
		<div class="overflow-x-auto rounded-xl border border-border">
			<table class="w-full border-collapse text-[13px]">
				<thead>
					<tr class="border-b border-border bg-surface-2 text-left text-muted">
						<!-- Tipo: sort + filtro por capability -->
						<th class="relative px-4 py-2.5 font-medium" data-col-popover>
							<button class="flex items-center gap-1 hover:text-text" aria-haspopup="true" aria-expanded={activePopover === 'type'} onclick={(e) => { e.stopPropagation(); activePopover = activePopover === 'type' ? null : 'type'; }}>
								{t.workers.colType}
								{#if fCapability}<span class="h-1.5 w-1.5 rounded-full bg-text"></span>{/if}
								<span class="opacity-40">▾</span>
							</button>
							{#if activePopover === 'type'}
								<div role="menu" class="absolute left-0 top-full z-30 min-w-[200px] rounded-xl border border-border bg-bg p-3 shadow-xl" data-col-popover>
									<div class="mb-2 flex gap-1">
										<button class="flex-1 rounded-token border border-border px-2 py-1 text-[12px] {sortBy==='capability'&&sortDir==='asc'?'bg-surface-2 font-medium':''} hover:bg-hover" onclick={() => setSort('capability','asc')}>{t.workers.sortAZ}</button>
										<button class="flex-1 rounded-token border border-border px-2 py-1 text-[12px] {sortBy==='capability'&&sortDir==='desc'?'bg-surface-2 font-medium':''} hover:bg-hover" onclick={() => setSort('capability','desc')}>{t.workers.sortZA}</button>
									</div>
									<div class="flex flex-col gap-0.5">
										<button class="rounded-token px-2 py-1 text-left text-[12px] {!fCapability?'font-medium text-text':'text-muted'} hover:bg-hover" onclick={() => { fCapability=''; activePopover=null; }}>{t.workers.filterAllCapability}</button>
										{#each knownCapabilities as c}
											<button class="rounded-token px-2 py-1 text-left text-[12px] {fCapability===c?'font-medium text-text':'text-muted'} hover:bg-hover" onclick={() => { fCapability=c; activePopover=null; }}>{c}</button>
										{/each}
									</div>
								</div>
							{/if}
						</th>
						<!-- Worker: sort only -->
						<th class="relative px-4 py-2.5 font-medium" data-col-popover>
							<button class="flex items-center gap-1 hover:text-text" aria-haspopup="true" aria-expanded={activePopover === 'worker'} onclick={(e) => { e.stopPropagation(); activePopover = activePopover === 'worker' ? null : 'worker'; }}>
								{t.workers.colWorkerGroup}<span class="opacity-40">▾</span>
							</button>
							{#if activePopover === 'worker'}
								<div role="menu" class="absolute left-0 top-full z-30 min-w-[160px] rounded-xl border border-border bg-bg p-3 shadow-xl" data-col-popover>
									<div class="flex gap-1">
										<button class="flex-1 rounded-token border border-border px-2 py-1 text-[12px] {sortBy==='worker'&&sortDir==='asc'?'bg-surface-2 font-medium':''} hover:bg-hover" onclick={() => setSort('worker','asc')}>{t.workers.sortAZ}</button>
										<button class="flex-1 rounded-token border border-border px-2 py-1 text-[12px] {sortBy==='worker'&&sortDir==='desc'?'bg-surface-2 font-medium':''} hover:bg-hover" onclick={() => setSort('worker','desc')}>{t.workers.sortZA}</button>
									</div>
								</div>
							{/if}
						</th>
						<!-- Nome: sort only -->
						<th class="relative px-4 py-2.5 font-medium" data-col-popover>
							<button class="flex items-center gap-1 hover:text-text" aria-haspopup="true" aria-expanded={activePopover === 'name'} onclick={(e) => { e.stopPropagation(); activePopover = activePopover === 'name' ? null : 'name'; }}>
								{t.workers.colName}<span class="opacity-40">▾</span>
							</button>
							{#if activePopover === 'name'}
								<div role="menu" class="absolute left-0 top-full z-30 min-w-[160px] rounded-xl border border-border bg-bg p-3 shadow-xl" data-col-popover>
									<div class="flex gap-1">
										<button class="flex-1 rounded-token border border-border px-2 py-1 text-[12px] {sortBy==='name'&&sortDir==='asc'?'bg-surface-2 font-medium':''} hover:bg-hover" onclick={() => setSort('name','asc')}>{t.workers.sortAZ}</button>
										<button class="flex-1 rounded-token border border-border px-2 py-1 text-[12px] {sortBy==='name'&&sortDir==='desc'?'bg-surface-2 font-medium':''} hover:bg-hover" onclick={() => setSort('name','desc')}>{t.workers.sortZA}</button>
									</div>
								</div>
							{/if}
						</th>
						<!-- Runtime: sort + filtro -->
						<th class="relative px-4 py-2.5 font-medium" data-col-popover>
							<button class="flex items-center gap-1 hover:text-text" aria-haspopup="true" aria-expanded={activePopover === 'runtime'} onclick={(e) => { e.stopPropagation(); activePopover = activePopover === 'runtime' ? null : 'runtime'; }}>
								{t.workers.colRuntime}
								{#if fRuntime}<span class="h-1.5 w-1.5 rounded-full bg-text"></span>{/if}
								<span class="opacity-40">▾</span>
							</button>
							{#if activePopover === 'runtime'}
								<div role="menu" class="absolute left-0 top-full z-30 min-w-[180px] rounded-xl border border-border bg-bg p-3 shadow-xl" data-col-popover>
									<div class="mb-2 flex gap-1">
										<button class="flex-1 rounded-token border border-border px-2 py-1 text-[12px] {sortBy==='runtime'&&sortDir==='asc'?'bg-surface-2 font-medium':''} hover:bg-hover" onclick={() => setSort('runtime','asc')}>{t.workers.sortAZ}</button>
										<button class="flex-1 rounded-token border border-border px-2 py-1 text-[12px] {sortBy==='runtime'&&sortDir==='desc'?'bg-surface-2 font-medium':''} hover:bg-hover" onclick={() => setSort('runtime','desc')}>{t.workers.sortZA}</button>
									</div>
									<div class="flex flex-col gap-0.5">
										<button class="rounded-token px-2 py-1 text-left text-[12px] {!fRuntime?'font-medium text-text':'text-muted'} hover:bg-hover" onclick={() => { fRuntime=''; activePopover=null; }}>{t.workers.filterAllRuntime}</button>
										{#each ['local','cloudrun','vpc'] as rt}
											<button class="rounded-token px-2 py-1 text-left text-[12px] {fRuntime===rt?'font-medium text-text':'text-muted'} hover:bg-hover" onclick={() => { fRuntime=rt; activePopover=null; }}>{rt}</button>
										{/each}
									</div>
								</div>
							{/if}
						</th>
						<!-- Ativação: sort + filtro -->
						<th class="relative px-4 py-2.5 font-medium" data-col-popover>
							<button class="flex items-center gap-1 hover:text-text" aria-haspopup="true" aria-expanded={activePopover === 'activation'} onclick={(e) => { e.stopPropagation(); activePopover = activePopover === 'activation' ? null : 'activation'; }}>
								{t.workers.colActivation}
								{#if fActivation}<span class="h-1.5 w-1.5 rounded-full bg-text"></span>{/if}
								<span class="opacity-40">▾</span>
							</button>
							{#if activePopover === 'activation'}
								<div role="menu" class="absolute left-0 top-full z-30 min-w-[180px] rounded-xl border border-border bg-bg p-3 shadow-xl" data-col-popover>
									<div class="flex flex-col gap-0.5">
										<button class="rounded-token px-2 py-1 text-left text-[12px] {!fActivation?'font-medium text-text':'text-muted'} hover:bg-hover" onclick={() => { fActivation=''; activePopover=null; }}>{t.workers.filterAllActivation}</button>
										{#each knownActivations as act}
											<button class="rounded-token px-2 py-1 text-left text-[12px] {fActivation===act?'font-medium text-text':'text-muted'} hover:bg-hover" onclick={() => { fActivation=act; activePopover=null; }}>{act}</button>
										{/each}
									</div>
								</div>
							{/if}
						</th>
						<!-- Status: filtro -->
						<th class="relative px-4 py-2.5 font-medium" data-col-popover>
							<button class="flex items-center gap-1 hover:text-text" aria-haspopup="true" aria-expanded={activePopover === 'status'} onclick={(e) => { e.stopPropagation(); activePopover = activePopover === 'status' ? null : 'status'; }}>
								{t.workers.colEnabled}
								{#if fStatus}<span class="h-1.5 w-1.5 rounded-full bg-text"></span>{/if}
								<span class="opacity-40">▾</span>
							</button>
							{#if activePopover === 'status'}
								<div role="menu" class="absolute left-0 top-full z-30 min-w-[160px] rounded-xl border border-border bg-bg p-3 shadow-xl" data-col-popover>
									<div class="flex flex-col gap-0.5">
										<button class="rounded-token px-2 py-1 text-left text-[12px] {!fStatus?'font-medium text-text':'text-muted'} hover:bg-hover" onclick={() => { fStatus=''; activePopover=null; }}>{t.workers.filterAllStatus}</button>
										<button class="rounded-token px-2 py-1 text-left text-[12px] {fStatus==='enabled'?'font-medium text-text':'text-muted'} hover:bg-hover" onclick={() => { fStatus='enabled'; activePopover=null; }}>{t.workers.enabledStatus}</button>
										<button class="rounded-token px-2 py-1 text-left text-[12px] {fStatus==='disabled'?'font-medium text-text':'text-muted'} hover:bg-hover" onclick={() => { fStatus='disabled'; activePopover=null; }}>{t.workers.disabledStatus}</button>
									</div>
								</div>
							{/if}
						</th>
						<!-- Última execução: sort -->
						<th class="relative px-4 py-2.5 font-medium" data-col-popover>
							<button class="flex items-center gap-1 hover:text-text" aria-haspopup="true" aria-expanded={activePopover === 'lastrun'} onclick={(e) => { e.stopPropagation(); activePopover = activePopover === 'lastrun' ? null : 'lastrun'; }}>
								{t.workers.colLastRun}
								{#if sortBy==='lastrun'}<span class="h-1.5 w-1.5 rounded-full bg-text"></span>{/if}
								<span class="opacity-40">▾</span>
							</button>
							{#if activePopover === 'lastrun'}
								<div role="menu" class="absolute right-0 top-full z-30 min-w-[180px] rounded-xl border border-border bg-bg p-3 shadow-xl" data-col-popover>
									<div class="flex gap-1">
										<button class="flex-1 rounded-token border border-border px-2 py-1 text-[12px] {sortBy==='lastrun'&&sortDir==='desc'?'bg-surface-2 font-medium':''} hover:bg-hover" onclick={() => setSort('lastrun','desc')}>{t.workers.sortNewest}</button>
										<button class="flex-1 rounded-token border border-border px-2 py-1 text-[12px] {sortBy==='lastrun'&&sortDir==='asc'?'bg-surface-2 font-medium':''} hover:bg-hover" onclick={() => setSort('lastrun','asc')}>{t.workers.sortOldest}</button>
									</div>
								</div>
							{/if}
						</th>
						<!-- ⋮ -->
						<th class="w-10 px-2 py-2.5"><span class="sr-only">{t.workers.actionsLabel}</span></th>
					</tr>
				</thead>
				<tbody>
					{#each filteredRows as row (row.placement.name)}
						{@const p = row.placement}
						{@const lr = lastRun(p)}
						<tr class="border-b border-border last:border-0 hover:bg-hover">
							<td class="px-4 py-2.5 text-muted">{row.capability}</td>
							<td class="px-4 py-2.5 font-mono text-[12px] font-semibold">{row.workerName}</td>
							<td class="px-4 py-2.5 font-mono text-[12px]">{p.name}</td>
							<td class="px-4 py-2.5 text-muted">{p.runtime}</td>
							<td class="px-4 py-2.5 text-muted">{p.activation}</td>
							<td class="px-4 py-2.5">
								<span class="inline-flex items-center gap-1.5 text-muted">
									<span class="h-[7px] w-[7px] flex-none rounded-full {p.enabled ? 'bg-green' : 'bg-surface-2 border border-border'}"></span>
									{p.enabled ? t.workers.enabledStatus : t.workers.disabledStatus}
								</span>
							</td>
							<td class="whitespace-nowrap px-4 py-2.5 tabular-nums text-muted">
								{#if lr}
									<time datetime={lr} title={new Date(lr).toLocaleString('pt-BR')}>{timeAgo(lr)}</time>
								{:else}
									<span aria-label={t.workers.lastRunNever}>—</span>
								{/if}
							</td>
							<td class="w-10 px-2 py-2.5 text-right">
								<div class="relative inline-block" data-kebab>
									<button
										class="flex h-7 w-7 items-center justify-center rounded-token text-muted hover:bg-surface-2"
										aria-label="{t.workers.actionsLabel}: {p.name}"
										aria-haspopup="menu"
										aria-expanded={activeKebab === p.name}
										onclick={(e) => { e.stopPropagation(); activeKebab = activeKebab === p.name ? null : p.name; }}
										data-kebab
									>⋮</button>
									{#if activeKebab === p.name}
										<div role="menu" class="absolute right-0 top-full z-30 min-w-[160px] rounded-xl border border-border bg-bg py-1 shadow-xl" data-kebab>
											<button
												class="w-full px-3 py-1.5 text-left text-[13px] text-muted hover:bg-hover disabled:opacity-50"
												disabled={saving === p.name}
												onclick={() => { activeKebab = null; toggleProvider(p, row.workerName); }}
											>{p.enabled ? t.workers.disable : t.workers.enable}</button>
											<button
												class="w-full px-3 py-1.5 text-left text-[13px] text-muted hover:bg-hover"
												onclick={() => { activeKebab = null; openEdit(p, row.workerName); }}
											>{t.workers.editWorker}</button>
										</div>
									{/if}
								</div>
							</td>
						</tr>
						{#if formMode === 'edit' && formInitial?.name === p.name}
							<tr>
								<td colspan="8" class="px-4 py-3">
									<WorkerForm initial={formInitial} lockedApp={null} capabilities={knownCapabilities} {models} {modelsStatus} onSave={saveWorker} onCancel={closeForm} />
								</td>
							</tr>
						{/if}
					{/each}
				</tbody>
			</table>
		</div>
	{/if}
</section>

<!-- ── Toasts (espelha fontes/+page.svelte:1302-1316) ── -->
{#if toasts.length > 0}
	<div class="fixed bottom-4 right-4 z-[60] flex flex-col gap-2">
		{#each toasts as tst (tst.id)}
			<div
				class="rounded-token border px-4 py-2 text-[13px] shadow-lg {tst.kind === 'ok'
					? 'border-green/40 bg-surface-2 text-text'
					: 'border-red-500/40 bg-surface-2 text-red-500'}"
				role={tst.kind === 'ok' ? 'status' : 'alert'}
			>{tst.msg}</div>
		{/each}
	</div>
{/if}
