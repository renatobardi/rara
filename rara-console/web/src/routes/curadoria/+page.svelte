<script lang="ts">
	import { t } from '$lib/strings';
	import {
		labelDecidedBy,
		aggregatePulso,
		latestDeferReason,
		signalForKey,
		diffProfile,
		isDiffEmpty,
		sourceUrl,
		filterQuarantine,
		type FilterState,
		type Decision,
		type QuarantineItem,
		type ItemDecision,
		type ProfileDiff
	} from '$lib/curadoria';
	import MegaThumbnail from '$lib/MegaThumbnail.svelte';

	const lanesWithThumbnail = new Set(['youtube', 'news', 'email']);

	type InterestProfile = {
		version: number;
		status: string;
		narrative?: string;
		topics?: unknown;
		authors?: unknown;
		anti_topics?: unknown;
		weights?: unknown;
		created_at?: string;
	};
	type GateRule = {
		action: 'allow' | 'deny';
		match_type: 'channel' | 'title_contains';
		value: string;
		enabled: boolean;
	};
	type RecentDecision = Decision & {
		id: number;
		item_id: number;
		gate: string;
		score?: number | null;
		when: string;
		lane?: string;
		source_ref?: string;
	};

	// --- tab state ---
	let activeTab = $state<'decidir' | 'historico' | 'ajustes'>('decidir');

	// --- quarantine queue state ---
	let quarantine = $state<QuarantineItem[]>([]);
	let quarantineLoading = $state(true);
	let quarantineError = $state(false);
	let focusedIndex = $state(0);
	let focusedDecisions = $state<ItemDecision[]>([]);
	let focusedDecisionsLoading = $state(false);
	let reviewInFlight = $state(false);
	let reviewError = $state('');
	let refetchSeq = 0;

	// --- filter state (aba Decidir) ---
	let filterState = $state<FilterState>({ dateFrom: '', dateTo: '', tipo: '', canal: '', sortDir: 'newest' });

	let filteredQueue = $derived(filterQuarantine(quarantine, filterState));
	let focusedItem = $derived(filteredQueue[Math.min(focusedIndex, Math.max(0, filteredQueue.length - 1))] ?? null);
	let deferReason = $derived(latestDeferReason(focusedDecisions));

	// --- interest profile state ---
	let activeProfile = $state<InterestProfile | null>(null);
	let versions = $state<InterestProfile[]>([]);
	let profileLoading = $state(true);
	let profileError = $state(false);

	// --- decisions state (Pulso + Trilha) ---
	let decisions = $state<RecentDecision[]>([]);
	let decisionsLoading = $state(true);
	let decisionsError = $state(false);

	let pulso = $derived(aggregatePulso(decisions, versions));

	const emptyProfile = { topics: [], authors: [], anti_topics: [], weights: {} };
	let proposedProfile = $derived(versions.find((v) => v.status === 'proposed') ?? null);
	let profileDiff = $derived(
		proposedProfile ? diffProfile(activeProfile ?? emptyProfile, proposedProfile) : null
	) as unknown as ProfileDiff | null;

	// propose form
	let proposeVersion = $state('');
	let proposeNarrative = $state('');
	let proposeTopics = $state('');
	let proposeAuthors = $state('');
	let proposeAntiTopics = $state('');
	let proposeWeights = $state('');
	let proposing = $state(false);
	let proposeError = $state('');

	// nova versão form state
	let showNovaVersaoForm = $state(false);
	let novaVersaoNumber = $derived((activeProfile?.version ?? 0) + 1);

	// Pre-fill the propose form from the active profile when the user opens it.
	function openNovaVersao() {
		if (!activeProfile) return;
		proposeNarrative = activeProfile.narrative ?? '';
		proposeTopics = activeProfile.topics != null ? JSON.stringify(activeProfile.topics, null, 2) : '';
		proposeAuthors = activeProfile.authors != null ? JSON.stringify(activeProfile.authors, null, 2) : '';
		proposeAntiTopics = activeProfile.anti_topics != null ? JSON.stringify(activeProfile.anti_topics, null, 2) : '';
		proposeWeights = activeProfile.weights != null ? JSON.stringify(activeProfile.weights, null, 2) : '';
		proposeVersion = String(novaVersaoNumber);
		proposeError = '';
		showNovaVersaoForm = true;
	}

	let novaVersaoDiff = $derived.by(() => {
		if (!activeProfile || !showNovaVersaoForm) return null;
		try {
			const proposed = {
				topics: proposeTopics.trim() ? JSON.parse(proposeTopics) : activeProfile.topics,
				authors: proposeAuthors.trim() ? JSON.parse(proposeAuthors) : activeProfile.authors,
				anti_topics: proposeAntiTopics.trim() ? JSON.parse(proposeAntiTopics) : activeProfile.anti_topics,
				weights: proposeWeights.trim() ? JSON.parse(proposeWeights) : activeProfile.weights,
			};
			return diffProfile(activeProfile, proposed);
		} catch {
			return null; // JSON inválido → deixa salvar (o servidor vai rejeitar se inválido)
		}
	});
	let novaVersaoHasDiff = $derived(
		novaVersaoDiff === null ||
		!isDiffEmpty(novaVersaoDiff) ||
		proposeNarrative !== (activeProfile?.narrative ?? '')
	);

	// approve state: version number being approved, or null
	let approving = $state<number | null>(null);
	let approveError = $state('');
	let approveNotice = $state('');

	// --- gate rules state ---
	let rules = $state<GateRule[]>([]);
	let rulesLoading = $state(true);
	let rulesError = $state(false);

	// rule form (create + edit)
	let ruleAction = $state<'allow' | 'deny'>('allow');
	let ruleMatchType = $state<'channel' | 'title_contains'>('channel');
	let ruleValue = $state('');
	let ruleEnabled = $state(true);
	let savingRule = $state(false);
	let saveRuleError = $state('');

	$effect(() => {
		fetch('/api/quarantine')
			.then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
			.then((d: unknown) => {
				quarantine = Array.isArray(d) ? (d as QuarantineItem[]) : [];
				focusedIndex = 0;
			})
			.catch(() => (quarantineError = true))
			.finally(() => (quarantineLoading = false));
	});

	$effect(() => {
		const item = focusedItem;
		if (!item) {
			focusedDecisions = [];
			focusedDecisionsLoading = false;
			return;
		}
		const controller = new AbortController();
		const itemId = item.id;
		let active = true;
		focusedDecisionsLoading = true;
		focusedDecisions = [];
		fetch(`/api/items/${itemId}/decisions`, { signal: controller.signal })
			.then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
			.then((d: unknown) => { if (active) focusedDecisions = Array.isArray(d) ? (d as ItemDecision[]) : []; })
			.catch((e) => { if (active && e?.name !== 'AbortError') focusedDecisions = []; })
			.finally(() => { if (active) focusedDecisionsLoading = false; });
		return () => { active = false; controller.abort(); };
	});

	async function sendReview(signal: 'up' | 'down') {
		if (reviewInFlight || !focusedItem) return;
		reviewInFlight = true;
		reviewError = '';
		// bump seq at start so any in-flight refetch from a previous review is invalidated immediately
		const seq = ++refetchSeq;
		const item = focusedItem;
		// optimistic: remove from queue
		quarantine = quarantine.filter((q) => q.id !== item.id);
		focusedIndex = quarantine.length === 0 ? 0 : Math.min(focusedIndex, quarantine.length - 1);
		try {
			const r = await fetch('/api/quarantine/review', {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ item_id: item.id, signal })
			});
			if (!r.ok) throw new Error('review failed');
			// light refetch to stay honest — seq guard prevents stale responses from overwriting newer state
			void fetch('/api/quarantine')
				.then((r2) => (r2.ok ? r2.json() : Promise.reject(r2.status)))
				.then((d: unknown) => {
					if (seq !== refetchSeq) return;
					if (Array.isArray(d)) {
						quarantine = d as QuarantineItem[];
						focusedIndex = Math.min(focusedIndex, Math.max(0, quarantine.length - 1));
					}
				})
				.catch(() => { if (seq === refetchSeq) reviewError = t.curadoria.filaReviewError; });
		} catch {
			// restore on error
			quarantine = [item, ...quarantine];
			focusedIndex = 0;
			reviewError = t.curadoria.filaReviewError;
		} finally {
			reviewInFlight = false;
		}
	}

	function handleKeydown(e: KeyboardEvent) {
		if (activeTab !== 'decidir') return;
		const target = e.target as HTMLElement | null;
		if (
			e.altKey || e.ctrlKey || e.metaKey ||
			target?.closest('input, textarea, select, button, [contenteditable="true"]')
		) return;
		const signal = signalForKey(e.key);
		if (signal && !reviewInFlight && focusedItem) {
			e.preventDefault();
			sendReview(signal);
		}
	}

	$effect(() => {
		Promise.all([
			fetch('/api/interest-profile').then((r) =>
				r.status === 404 ? null : r.ok ? r.json() : Promise.reject(r.status)
			),
			fetch('/api/interest-profile/versions').then((r) =>
				r.ok ? r.json() : Promise.reject(r.status)
			)
		])
			.then(([active, vers]) => {
				activeProfile = active;
				versions = vers ?? [];
			})
			.catch(() => (profileError = true))
			.finally(() => (profileLoading = false));

		fetch('/api/gate-rules')
			.then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
			.then((d) => (rules = d ?? []))
			.catch(() => (rulesError = true))
			.finally(() => (rulesLoading = false));

		fetch('/api/decisions?limit=200')
			.then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
			.then((d: unknown) => {
				if (!Array.isArray(d)) throw new Error('unexpected decisions payload');
				decisions = d as RecentDecision[];
			})
			.catch(() => (decisionsError = true))
			.finally(() => (decisionsLoading = false));
	});

	function parseOptionalJSON(s: string): unknown | undefined {
		const trimmed = s.trim();
		if (!trimmed) return undefined;
		return JSON.parse(trimmed); // throws on invalid — caught by caller
	}

	async function propose() {
		const v = parseInt(proposeVersion, 10);
		if (!v || v <= 0) {
			proposeError = 'Versão deve ser um inteiro positivo.';
			return;
		}
		let body: Record<string, unknown>;
		try {
			body = { version: v, narrative: proposeNarrative };
			const top = parseOptionalJSON(proposeTopics);
			if (top !== undefined) body.topics = top;
			const auth = parseOptionalJSON(proposeAuthors);
			if (auth !== undefined) body.authors = auth;
			const anti = parseOptionalJSON(proposeAntiTopics);
			if (anti !== undefined) body.anti_topics = anti;
			const wt = parseOptionalJSON(proposeWeights);
			if (wt !== undefined) body.weights = wt;
		} catch {
			proposeError = 'JSON inválido em um dos campos opcionais.';
			return;
		}
		proposing = true;
		proposeError = '';
		try {
			const r = await fetch('/api/interest-profile', {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify(body)
			});
			if (!r.ok) {
				const data = await r.json().catch(() => ({}));
				proposeError = (data as { error?: string }).error ?? t.curadoria.profileProposeError;
			} else {
				versions = await fetch('/api/interest-profile/versions')
					.then((r2) => (r2.ok ? r2.json() : versions))
					.catch(() => versions);
				proposeVersion = '';
				proposeNarrative = '';
				proposeTopics = '';
				proposeAuthors = '';
				proposeAntiTopics = '';
				proposeWeights = '';
			}
		} catch {
			proposeError = t.curadoria.profileProposeError;
		} finally {
			proposing = false;
		}
	}

	async function approve(version: number) {
		approving = version;
		approveError = '';
		approveNotice = '';
		try {
			const r = await fetch('/api/interest-profile/approve', {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ version })
			});
			if (!r.ok) {
				const data = await r.json().catch(() => ({}));
				approveError = (data as { error?: string }).error ?? t.curadoria.profileApproveError;
			} else {
				const refreshed = await Promise.all([
					fetch('/api/interest-profile').then((r2) =>
						r2.status === 404 ? null : r2.ok ? r2.json() : Promise.reject(r2.status)
					),
					fetch('/api/interest-profile/versions').then((r2) =>
						r2.ok ? r2.json() : Promise.reject(r2.status)
					)
				]).catch(() => null);
				if (refreshed) {
					[activeProfile, versions] = refreshed as [InterestProfile | null, InterestProfile[]];
				} else {
					// Approve succeeded but refresh failed — user sees stale state
					approveNotice = t.curadoria.profileApproveRefreshNotice;
				}
			}
		} catch {
			approveError = t.curadoria.profileApproveError;
		} finally {
			approving = null;
		}
	}

	function editRule(rule: GateRule) {
		ruleAction = rule.action;
		ruleMatchType = rule.match_type;
		ruleValue = rule.value;
		ruleEnabled = rule.enabled;
	}

	async function toggleRule(rule: GateRule) {
		await saveRuleData({ ...rule, enabled: !rule.enabled });
	}

	async function saveRule() {
		if (!ruleValue.trim()) {
			saveRuleError = 'Valor é obrigatório.';
			return;
		}
		await saveRuleData({ action: ruleAction, match_type: ruleMatchType, value: ruleValue.trim(), enabled: ruleEnabled });
	}

	async function saveRuleData(rule: GateRule) {
		savingRule = true;
		saveRuleError = '';
		try {
			const r = await fetch('/api/gate-rules', {
				method: 'PUT',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify(rule)
			});
			if (!r.ok) {
				const data = await r.json().catch(() => ({}));
				saveRuleError = (data as { error?: string }).error ?? t.curadoria.gateSaveError;
			} else {
				rules = await fetch('/api/gate-rules')
					.then((r2) => (r2.ok ? r2.json() : rules))
					.catch(() => rules);
				ruleValue = ''; // action/match_type/enabled kept so the user can batch-add similar rules
			}
		} catch {
			saveRuleError = t.curadoria.gateSaveError;
		} finally {
			savingRule = false;
		}
	}

	const pulsoCards = $derived([
		{ label: t.curadoria.pulsoEntrou, color: 'text-text', value: pulso.entrou },
		{ label: t.curadoria.pulsoManteve, color: 'text-green', value: pulso.manteve },
		{ label: t.curadoria.pulsoBarrou, color: 'text-red', value: pulso.barrou },
		{ label: t.curadoria.pulsoDuvida, color: 'text-primary', value: pulso.duvida }
	]);

	const spineSteps = [
		t.curadoria.spineStep1,
		t.curadoria.spineStep2,
		t.curadoria.spineStep3,
		t.curadoria.spineStep4
	] as const;
</script>

<!-- ── 1. PULSO (~24h) ─────────────────────────────────────────────── -->
<section class="mb-6">
	<div class="mb-3 flex items-baseline gap-2">
		<h2 class="text-[15px] font-semibold">{t.curadoria.pulsoZone}</h2>
		<span class="text-[12px] text-muted">{t.curadoria.pulsoLabel}</span>
	</div>
	{#if decisionsLoading}
		<p class="text-[13px] text-muted">{t.curadoria.pulsoLoading}</p>
	{:else if decisionsError}
		<p class="text-[13px] text-red">{t.curadoria.pulsoError}</p>
	{:else}
		{#if pulso.proposedPending}
			<a href="#gosto" class="mb-3 flex items-center gap-1 text-[12px] text-primary hover:underline">
				{t.curadoria.pulsoProposedPending}
			</a>
		{/if}
		<div class="grid grid-cols-2 gap-3 sm:grid-cols-4">
			{#each pulsoCards as card}
				<div class="rounded-card border border-border bg-surface px-4 py-3">
					<div class="text-[11px] text-muted">{card.label}</div>
					<div class="mt-1 text-[22px] font-semibold {card.color}">{card.value}</div>
				</div>
			{/each}
		</div>
	{/if}
</section>

<!-- ── 2. SPINE ────────────────────────────────────────────────────── -->
<div
	class="mb-6 flex items-center overflow-hidden rounded-card border border-border bg-surface-2 px-5 py-3 text-[12px]"
	aria-hidden="true"
>
	{#each spineSteps as step, i}
		{#if i > 0}<span class="mx-3 text-border">→</span>{/if}
		<span class="text-muted">{step}</span>
	{/each}
</div>

<!-- ── TAB NAV ───────────────────────────────────────────────────────── -->
<div class="mb-6 flex gap-1 border-b border-border" role="tablist">
	{#each ([['decidir', t.curadoria.tabDecidir], ['historico', t.curadoria.tabHistorico], ['ajustes', t.curadoria.tabAjustes]] as const) as [tab, label]}
		<button
			role="tab"
			aria-selected={activeTab === tab}
			onclick={() => (activeTab = tab)}
			class="px-4 py-2 text-[13px] font-medium transition-colors
				{activeTab === tab
					? 'border-b-2 border-primary text-primary'
					: 'text-muted hover:text-text'}"
		>{label}</button>
	{/each}
</div>

{#if activeTab === 'decidir'}
<!-- ── 3. FILA DE REVISÃO (herói) ─────────────────────────────────── -->
<section class="mb-6">
	<div class="mb-3 flex items-baseline gap-2">
		<h2 class="text-[15px] font-semibold">{t.curadoria.filaZone}</h2>
		{#if !quarantineLoading && !quarantineError && filteredQueue.length > 0}
			<span class="text-[12px] text-muted">{filteredQueue.length} {t.curadoria.filaSubtitle}</span>
		{:else}
			<span class="text-[12px] text-muted">{t.curadoria.filaSubtitle}</span>
		{/if}
	</div>

	{#if quarantineLoading}
		<p class="text-[13px] text-muted">{t.curadoria.filaLoading}</p>
	{:else if quarantineError}
		<p class="text-[13px] text-red">{t.curadoria.filaError}</p>
	{:else if quarantine.length === 0}
		<div class="overflow-hidden rounded-card border border-border bg-surface">
			<div class="flex h-32 items-center justify-center">
				<p class="text-[13px] text-muted">{t.curadoria.filaEmpty}</p>
			</div>
		</div>
	{:else}
		<!-- filters -->
		<details class="mb-4 text-[13px]">
			<summary class="cursor-pointer text-muted hover:text-text mb-2">{t.curadoria.filterSortBy}</summary>
			<div class="rounded-card border border-border bg-surface p-3 space-y-2">
				<div class="flex flex-wrap gap-3 items-end">
					<div>
						<label class="mb-1 block text-[11px] text-muted" for="f-from">{t.curadoria.filterDateFrom}</label>
						<input id="f-from" type="date" bind:value={filterState.dateFrom}
							class="rounded-token border border-border bg-surface-2 px-2 py-1 text-[12px] focus:outline-none focus:ring-1 focus:ring-primary/50" />
					</div>
					<div>
						<label class="mb-1 block text-[11px] text-muted" for="f-to">{t.curadoria.filterDateTo}</label>
						<input id="f-to" type="date" bind:value={filterState.dateTo}
							class="rounded-token border border-border bg-surface-2 px-2 py-1 text-[12px] focus:outline-none focus:ring-1 focus:ring-primary/50" />
					</div>
					<div>
						<label class="mb-1 block text-[11px] text-muted" for="f-tipo">{t.curadoria.filterTipo}</label>
						<select id="f-tipo" bind:value={filterState.tipo}
							class="rounded-token border border-border bg-surface-2 px-2 py-1 text-[12px] focus:outline-none focus:ring-1 focus:ring-primary/50">
							<option value="">{t.curadoria.filterTipoTodos}</option>
							{#each [...new Set(quarantine.map(q => q.lane))].sort() as lane}
								<option value={lane}>{lane}</option>
							{/each}
						</select>
					</div>
					<div class="flex-1 min-w-[120px]">
						<label class="mb-1 block text-[11px] text-muted" for="f-canal">{t.curadoria.filterCanal}</label>
						<input id="f-canal" type="text" bind:value={filterState.canal} placeholder={t.curadoria.filterCanalPlaceholder}
							class="w-full rounded-token border border-border bg-surface-2 px-2 py-1 text-[12px] focus:outline-none focus:ring-1 focus:ring-primary/50" />
					</div>
					<div>
						<label class="mb-1 block text-[11px] text-muted" for="f-sort">{t.curadoria.filterSortBy}</label>
						<select id="f-sort" bind:value={filterState.sortDir}
							class="rounded-token border border-border bg-surface-2 px-2 py-1 text-[12px] focus:outline-none focus:ring-1 focus:ring-primary/50">
							<option value="newest">{t.curadoria.filterSortNewest}</option>
							<option value="oldest">{t.curadoria.filterSortOldest}</option>
						</select>
					</div>
					<button onclick={() => (filterState = { dateFrom: '', dateTo: '', tipo: '', canal: '', sortDir: 'newest' })}
						class="rounded-token border border-border px-3 py-1 text-[12px] text-muted hover:bg-hover">
						{t.curadoria.filterClear}
					</button>
				</div>
			</div>
		</details>

		<div class="overflow-hidden rounded-card border border-border bg-surface">
			{#if focusedItem}
				{@const itemUrl = sourceUrl(focusedItem.lane, focusedItem.source_ref ?? '')}
				<div class="decidir-grid" class:has-thumb={lanesWithThumbnail.has(focusedItem.lane)}>
				<div class="decidir-card p-5">
					<!-- item header -->
					<div class="mb-1 flex items-center gap-2">
						<span class="rounded-full border border-border px-2 py-0.5 text-[11px] text-muted">{focusedItem.lane}</span>
						{#if focusedItem.channel}
							<span class="text-[12px] text-muted">{focusedItem.channel}</span>
						{/if}
					</div>
					{#if itemUrl}
						<a href={itemUrl} target="_blank" rel="noopener noreferrer"
							class="mb-2 block text-[14px] font-medium hover:underline text-primary">
							{focusedItem.title ?? focusedItem.source_ref ?? String(focusedItem.id)}
						</a>
					{:else}
						<h3 class="mb-2 text-[14px] font-medium">{focusedItem.title ?? focusedItem.source_ref ?? String(focusedItem.id)}</h3>
					{/if}
					<div class="mb-3 flex flex-wrap gap-x-3 gap-y-1 text-[12px] text-muted">
						<span><span class="font-medium">{t.curadoria.filterTipo}:</span> {focusedItem.lane}</span>
						{#if focusedItem.published_at}
							<span>{new Date(focusedItem.published_at).toLocaleString('pt-BR')}</span>
						{/if}
					</div>
					{#if focusedItem.summary}
						<p class="mb-4 text-[13px] text-muted">{focusedItem.summary}</p>
					{/if}

					<!-- why fence panel -->
					<div class="mb-4 text-[12px]">
						<p class="mb-2 font-medium text-muted">{t.curadoria.filaWhyFence}</p>
						{#if focusedDecisionsLoading}
							<p class="text-muted">{t.curadoria.pulsoLoading}</p>
						{:else if deferReason}
							<dl class="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1">
								{#if deferReason.score != null}
									<dt class="text-muted">{t.curadoria.filaScore}</dt>
									<dd>{deferReason.score}</dd>
								{/if}
								<dt class="text-muted">{t.curadoria.filaDecidedBy}</dt>
								<dd>{labelDecidedBy(deferReason.decided_by)}</dd>
								{#if deferReason.reason}
									<dt class="text-muted">{t.curadoria.filaReason}</dt>
									<dd>{deferReason.reason}</dd>
								{/if}
							</dl>
						{:else}
							<p class="text-muted">—</p>
						{/if}
					</div>

					<!-- actions -->
					{#if reviewError}
						<p class="mb-2 text-[12px] text-red">{reviewError}</p>
					{/if}
					<div class="flex gap-2">
						<button
							onclick={() => sendReview('up')}
							disabled={reviewInFlight}
							class="rounded-token border border-green px-4 py-1.5 text-[13px] text-green hover:bg-green/10 disabled:opacity-40"
						>{t.curadoria.filaKeep} →</button>
						<button
							onclick={() => sendReview('down')}
							disabled={reviewInFlight}
							class="rounded-token border border-red px-4 py-1.5 text-[13px] text-red hover:bg-red/10 disabled:opacity-40"
						>← {t.curadoria.filaDrop}</button>
					</div>
					<!-- progress -->
					<p class="mt-3 text-[11px] text-muted">{focusedIndex + 1} / {filteredQueue.length}</p>
				</div><!-- /decidir-card -->
				{#if lanesWithThumbnail.has(focusedItem.lane)}
					<div class="decidir-thumb">
						<MegaThumbnail item={focusedItem} />
					</div>
				{/if}
				</div><!-- /decidir-grid -->
			{/if}
		</div>
	{/if}
</section>
{/if}

<svelte:window onkeydown={handleKeydown} />

{#if activeTab === 'historico'}
<!-- ── 5. TRILHA DE DECISÕES ───────────────────────────────────────── -->
<section class="mb-6">
	<h2 class="mb-3 text-[15px] font-semibold">{t.curadoria.trilhaZone}</h2>
	<div class="overflow-hidden rounded-card border border-border bg-surface">
		{#if decisionsLoading}
			<p class="px-4 py-3 text-[13px] text-muted">{t.curadoria.trilhaLoading}</p>
		{:else if decisionsError}
			<p class="px-4 py-3 text-[13px] text-red">{t.curadoria.trilhaError}</p>
		{:else if decisions.length === 0}
			<p class="px-4 py-3 text-[13px] text-muted">{t.curadoria.trilhaEmpty}</p>
		{:else}
			<ul class="divide-y divide-border">
				{#each decisions as d}
					<li class="flex items-start gap-3 px-4 py-2.5 text-[13px]">
						<span class="mt-0.5 shrink-0 rounded-full px-2 py-0.5 text-[11px] font-medium
							{d.decision === 'keep' ? 'bg-green/15 text-green' :
							 d.decision === 'drop' ? 'bg-border text-text' :
							 'bg-primary/15 text-primary'}">{d.decision}</span>
						<div class="min-w-0 flex-1">
							<div class="flex flex-wrap items-center gap-x-2 gap-y-0.5">
								{#if d.lane && d.source_ref && sourceUrl(d.lane, d.source_ref)}
									<a href={sourceUrl(d.lane, d.source_ref)} target="_blank" rel="noopener noreferrer"
									   class="truncate text-primary hover:underline">{d.source_ref}</a>
								{:else}
									<span class="truncate text-muted">{d.source_ref || `#${d.item_id}`}</span>
								{/if}
								{#if d.lane}
									<span class="rounded-full border border-border px-1.5 py-0.5 text-[11px] text-muted">{d.lane}</span>
								{/if}
								{#if d.decided_by}
									<span class="text-muted opacity-60">· {labelDecidedBy(d.decided_by)}</span>
								{/if}
								<span class="ml-auto shrink-0 text-[11px] text-muted">
									{new Date(d.when).toLocaleString('pt-BR')}
								</span>
							</div>
							{#if d.reason}
								<p class="mt-0.5 text-[12px] text-muted">{d.reason}</p>
							{/if}
						</div>
					</li>
				{/each}
			</ul>
		{/if}
	</div>
</section>
{/if}

{#if activeTab === 'ajustes'}
<!-- ── 4. O GOSTO (Interest Profile) ─────────────────────────────── -->
<section id="gosto" class="mb-6">
	<h2 class="mb-3 text-[15px] font-semibold">{t.curadoria.gostoZone}</h2>

	{#if profileLoading}
		<p class="text-[13px] text-muted">{t.curadoria.profileLoading}</p>
	{:else if profileError}
		<p class="text-[13px] text-red">{t.curadoria.profileError}</p>
	{:else}
		<!-- Proposed version card (hero) -->
		{#if proposedProfile && profileDiff}
			{@const pv = proposedProfile}
			<div class="mb-4 overflow-hidden rounded-card border border-primary/40 bg-surface">
				<div class="flex items-center justify-between border-b border-primary/20 bg-primary/5 px-4 py-2">
					<span class="text-[12px] font-medium text-primary">
						{t.curadoria.profileProposedCard} · v{pv.version}
					</span>
					{#if approving === pv.version}
						<span class="text-[12px] text-muted">{t.curadoria.profileApproving}</span>
					{:else}
						<button
							class="cursor-pointer rounded-token border border-primary bg-primary/10 px-4 py-1 text-[13px] font-semibold text-primary hover:bg-primary/20"
							onclick={() => approve(pv.version)}
						>{t.curadoria.profileApproveBtn(pv.version)}</button>
					{/if}
				</div>
				{#if approveError}
					<p class="px-4 py-2 text-[12px] text-red">{approveError}</p>
				{/if}
				{#if approveNotice}
					<p class="px-4 py-2 text-[12px] text-muted">{approveNotice}</p>
				{/if}
				<div class="divide-y divide-border/50 px-4 py-3 text-[13px]">
					{#if pv.narrative}
						<div class="pb-3">
							<div class="mb-1 text-[11px] font-medium uppercase tracking-wide text-muted">{t.curadoria.profileProposedNarrative}</div>
							<p class="text-[13px] italic text-muted">{pv.narrative}</p>
						</div>
					{/if}
					{#each ([['topics', t.curadoria.profileTopicsLabel], ['authors', t.curadoria.profileAuthorsLabel], ['anti_topics', t.curadoria.profileAntiTopicsLabel]] as const) as [field, label]}
						{@const d = profileDiff[field]}
						{#if d.fallback}
							<div class="py-2">
								<div class="mb-1 text-[11px] font-medium text-muted">{label}</div>
								<p class="text-[12px] text-muted">{t.curadoria.profileDiffFallback}</p>
							</div>
						{:else if d.added.length > 0 || d.removed.length > 0}
							<div class="py-2">
								<div class="mb-1 text-[11px] font-medium text-muted">{label}</div>
								<div class="flex flex-wrap gap-1">
									{#each d.added as item}
										<span class="rounded-full bg-green/15 px-2 py-0.5 text-[11px] text-green">+ {item}</span>
									{/each}
									{#each d.removed as item}
										<span class="rounded-full bg-red/10 px-2 py-0.5 text-[11px] text-muted line-through opacity-60">− {item}</span>
									{/each}
								</div>
							</div>
						{:else}
							<div class="py-2">
								<div class="mb-1 text-[11px] font-medium text-muted">{label}</div>
								<p class="text-[12px] text-muted">{t.curadoria.profileDiffNoChanges}</p>
							</div>
						{/if}
					{/each}
					{#if profileDiff.weights.fallback}
						<div class="py-2">
							<div class="mb-1 text-[11px] font-medium text-muted">{t.curadoria.profileWeightsLabel}</div>
							<p class="text-[12px] text-muted">{t.curadoria.profileDiffFallback}</p>
						</div>
					{:else if profileDiff.weights.added.length > 0 || profileDiff.weights.removed.length > 0 || profileDiff.weights.changed.length > 0}
						<div class="py-2">
							<div class="mb-1 text-[11px] font-medium text-muted">{t.curadoria.profileWeightsLabel}</div>
							<div class="space-y-0.5 font-mono text-[12px]">
								{#each profileDiff.weights.added as e}
									<div class="text-green">+ {e.key}: {JSON.stringify(e.value)}</div>
								{/each}
								{#each profileDiff.weights.removed as e}
									<div class="text-muted line-through opacity-60">− {e.key}: {JSON.stringify(e.value)}</div>
								{/each}
								{#each profileDiff.weights.changed as c}
									<div class="text-primary">{c.key}: {JSON.stringify(c.from)} → {JSON.stringify(c.to)}</div>
								{/each}
							</div>
						</div>
					{/if}
				</div>
			</div>
		{:else if !profileLoading}
			<p class="mb-4 text-[13px] text-muted">{t.curadoria.profileStable}</p>
		{/if}

		<!-- Active version card -->
		<div class="mb-4 overflow-hidden rounded-card border border-border bg-surface">
			<div class="border-b border-border px-4 py-2 text-[12px] font-medium text-muted">
				{t.curadoria.profileCurrent}
			</div>
			{#if activeProfile}
				<div class="space-y-3 px-4 py-3 text-[13px]">
					<div>
						<span class="text-muted">{t.curadoria.profileVersion}:</span>
						<span class="ml-1 font-semibold">v{activeProfile.version}</span>
					</div>
					{#if activeProfile.narrative}
						<div>
							<div class="mb-1 text-[11px] font-medium text-muted">{t.curadoria.profileNarrative}</div>
							<p class="text-[13px]">{activeProfile.narrative}</p>
						</div>
					{/if}
					{#each ([['topics', t.curadoria.profileTopicsLabel, activeProfile.topics], ['authors', t.curadoria.profileAuthorsLabel, activeProfile.authors], ['anti_topics', t.curadoria.profileAntiTopicsLabel, activeProfile.anti_topics]] as const) as [, label, val]}
						{#if Array.isArray(val) && val.length > 0}
							<div>
								<div class="mb-1 text-[11px] font-medium text-muted">{label}</div>
								<div class="flex flex-wrap gap-1">
									{#each val as item}
										<span class="rounded-full border border-border px-2 py-0.5 text-[11px]">{item}</span>
									{/each}
								</div>
							</div>
						{:else if val != null && !Array.isArray(val)}
							<div>
								<div class="mb-1 text-[11px] font-medium text-muted">{label}</div>
								<pre class="text-[11px] text-muted">{JSON.stringify(val)}</pre>
							</div>
						{/if}
					{/each}
					{#if activeProfile.weights && typeof activeProfile.weights === 'object' && !Array.isArray(activeProfile.weights)}
						<div>
							<div class="mb-1 text-[11px] font-medium text-muted">{t.curadoria.profileWeightsLabel}</div>
							<div class="space-y-0.5 font-mono text-[12px] text-muted">
								{#each Object.entries(activeProfile.weights as Record<string, unknown>) as [k, v]}
									<div>{k}: {JSON.stringify(v)}</div>
								{/each}
							</div>
						</div>
					{:else if activeProfile.weights != null}
						<div>
							<div class="mb-1 text-[11px] font-medium text-muted">{t.curadoria.profileWeightsLabel}</div>
							<pre class="text-[11px] text-muted">{JSON.stringify(activeProfile.weights)}</pre>
						</div>
					{/if}
				</div>
			{:else}
				<p class="px-4 py-3 text-[13px] text-muted">{t.curadoria.profileEmpty}</p>
			{/if}
		</div>

		<!-- Version history (colapsável) -->
		{#if versions.length > 0}
			<details class="group mb-4 overflow-hidden rounded-card border border-border bg-surface">
				<summary class="flex cursor-pointer list-none items-center justify-between px-4 py-2 text-[12px] font-medium text-muted hover:bg-hover">
					{t.curadoria.profileVersions}
					<span class="text-[11px] group-open:hidden">{t.curadoria.gateExpand}</span>
					<span class="hidden text-[11px] group-open:inline">{t.curadoria.gateCollapse}</span>
				</summary>
				<table class="w-full border-t border-border text-[13px]">
					<thead>
						<tr class="border-b border-border text-left text-[11px] text-muted">
							<th class="px-4 py-2 font-medium">{t.curadoria.profileVersion}</th>
							<th class="px-4 py-2 font-medium">{t.curadoria.profileStatus}</th>
							<th class="px-4 py-2 font-medium">{t.curadoria.profileCreatedAt}</th>
						</tr>
					</thead>
					<tbody>
						{#each versions as v}
							<tr class="border-b border-border last:border-b-0">
								<td class="px-4 py-2 font-medium">v{v.version}</td>
								<td class="px-4 py-2">
									<span class="rounded-full px-2 py-0.5 text-[11px] font-medium
										{v.status === 'active' ? 'bg-green/15 text-green' : v.status === 'proposed' ? 'bg-primary/15 text-primary' : 'text-muted'}"
									>{v.status}</span>
								</td>
								<td class="px-4 py-2 text-muted">
									{v.created_at ? new Date(v.created_at).toLocaleString('pt-BR') : '—'}
								</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</details>
		{/if}

		<!-- + Nova versão button / form -->
		{#if !showNovaVersaoForm}
			<button
				onclick={openNovaVersao}
				disabled={!activeProfile || !!proposedProfile}
				class="cursor-pointer rounded-token border border-border bg-transparent px-4 py-1.5 text-[13px] font-medium hover:bg-hover disabled:cursor-default disabled:opacity-50"
				title={proposedProfile ? 'Existe uma versão proposta aguardando aprovação' : ''}
			>{t.curadoria.gostoNovaVersaoBtn}</button>
		{:else}
			<!-- Nova versão form (pre-filled from active profile) -->
			<div class="overflow-hidden rounded-card border border-border bg-surface">
				<div class="flex items-center justify-between border-b border-border px-4 py-2">
					<span class="text-[12px] font-medium text-muted">v{novaVersaoNumber}</span>
					<button onclick={() => (showNovaVersaoForm = false)} class="text-[12px] text-muted hover:text-text">{t.curadoria.gostoNovaVersaoCancelar}</button>
				</div>
				<div class="space-y-3 px-4 py-3">
					<div>
						<label class="mb-1 block text-[11px] text-muted" for="prop-narrative">{t.curadoria.profileNarrativeLabel}</label>
						<textarea id="prop-narrative" rows="2" bind:value={proposeNarrative}
							class="w-full rounded-token border border-border bg-surface-2 px-2 py-1 text-[13px] focus:outline-none focus:ring-1 focus:ring-primary/50"
						></textarea>
					</div>
					<div class="grid grid-cols-2 gap-3">
						<div>
							<label class="mb-1 block text-[11px] text-muted" for="prop-topics">{t.curadoria.profileTopicsLabel}</label>
							<textarea id="prop-topics" rows="3" bind:value={proposeTopics}
								class="w-full rounded-token border border-border bg-surface-2 px-2 py-1 font-mono text-[12px] focus:outline-none focus:ring-1 focus:ring-primary/50"
							></textarea>
						</div>
						<div>
							<label class="mb-1 block text-[11px] text-muted" for="prop-authors">{t.curadoria.profileAuthorsLabel}</label>
							<textarea id="prop-authors" rows="3" bind:value={proposeAuthors}
								class="w-full rounded-token border border-border bg-surface-2 px-2 py-1 font-mono text-[12px] focus:outline-none focus:ring-1 focus:ring-primary/50"
							></textarea>
						</div>
						<div>
							<label class="mb-1 block text-[11px] text-muted" for="prop-anti">{t.curadoria.profileAntiTopicsLabel}</label>
							<textarea id="prop-anti" rows="3" bind:value={proposeAntiTopics}
								class="w-full rounded-token border border-border bg-surface-2 px-2 py-1 font-mono text-[12px] focus:outline-none focus:ring-1 focus:ring-primary/50"
							></textarea>
						</div>
						<div>
							<label class="mb-1 block text-[11px] text-muted" for="prop-weights">{t.curadoria.profileWeightsLabel}</label>
							<textarea id="prop-weights" rows="3" bind:value={proposeWeights}
								class="w-full rounded-token border border-border bg-surface-2 px-2 py-1 font-mono text-[12px] focus:outline-none focus:ring-1 focus:ring-primary/50"
							></textarea>
						</div>
					</div>
					{#if !novaVersaoHasDiff}
						<p class="text-[12px] text-muted">{t.curadoria.gostoNoDiff}</p>
					{/if}
					{#if proposeError}
						<p class="text-[12px] text-red">{proposeError}</p>
					{/if}
					<button
						disabled={proposing || !novaVersaoHasDiff}
						onclick={propose}
						class="cursor-pointer rounded-token border border-border bg-transparent px-4 py-1.5 text-[13px] font-medium hover:bg-hover disabled:cursor-default disabled:opacity-50"
					>{proposing ? t.curadoria.profileProposing : t.curadoria.profileProposeBtn}</button>
				</div>
			</div>
		{/if}
	{/if}
</section>

<!-- ── 6. REGRAS DE GATE (secundária, colapsável) ─────────────────── -->
<details class="group overflow-hidden rounded-card border border-border bg-surface">
	<summary
		class="flex cursor-pointer list-none items-center justify-between px-4 py-3 text-[13px] font-medium hover:bg-hover"
	>
		{t.curadoria.gateSection}
		<span class="text-[12px] text-muted group-open:hidden">{t.curadoria.gateExpand}</span>
		<span class="hidden text-[12px] text-muted group-open:inline">{t.curadoria.gateCollapse}</span>
	</summary>

	<div class="border-t border-border px-4 py-4">
		{#if rulesLoading}
			<p class="text-[13px] text-muted">{t.curadoria.gateLoading}</p>
		{:else if rulesError}
			<p class="text-[13px] text-red">{t.curadoria.gateError}</p>
		{:else}
			<!-- Rules table -->
			<div class="mb-4 overflow-hidden rounded-card border border-border bg-surface">
				{#if rules.length === 0}
					<p class="px-4 py-3 text-[13px] text-muted">{t.curadoria.gateEmpty}</p>
				{:else}
					<table class="w-full text-[13px]">
						<thead>
							<tr class="border-b border-border text-left text-[11px] text-muted">
								<th class="px-4 py-2 font-medium">{t.curadoria.gateAction}</th>
								<th class="px-4 py-2 font-medium">{t.curadoria.gateMatchType}</th>
								<th class="px-4 py-2 font-medium">{t.curadoria.gateValue}</th>
								<th class="px-4 py-2 font-medium">{t.curadoria.gateEnabled}</th>
								<th class="px-4 py-2"></th>
							</tr>
						</thead>
						<tbody>
							{#each rules as rule}
								<tr class="border-b border-border last:border-b-0">
									<td class="px-4 py-2">
										<span
											class="rounded-full px-2 py-0.5 text-[11px] font-medium
											{rule.action === 'allow' ? 'bg-green/15 text-green' : 'bg-red/15 text-red'}"
										>{rule.action}</span>
									</td>
									<td class="px-4 py-2 text-muted">{rule.match_type}</td>
									<td class="px-4 py-2 font-mono">{rule.value}</td>
									<td class="px-4 py-2">
										<button
											aria-label="{t.curadoria.gateToggle}: {rule.value}"
											aria-pressed={rule.enabled}
											title={t.curadoria.gateToggle}
											disabled={savingRule}
											class="h-5 w-9 cursor-pointer rounded-full border-0 transition-colors disabled:cursor-default disabled:opacity-50 {rule.enabled ? 'bg-green' : 'bg-border'}"
											onclick={() => toggleRule(rule)}
										></button>
									</td>
									<td class="px-4 py-2 text-right">
										<button
											class="cursor-pointer rounded-token border border-border bg-transparent px-2 py-0.5 text-[11px] hover:bg-hover"
											onclick={() => editRule(rule)}
										>{t.curadoria.gateEdit}</button>
									</td>
								</tr>
							{/each}
						</tbody>
					</table>
				{/if}
			</div>

			<!-- Add/edit rule form -->
			<div class="overflow-hidden rounded-card border border-border bg-surface">
				<div class="border-b border-border px-4 py-2 text-[12px] font-medium text-muted">
					{t.curadoria.gateAddSection}
				</div>
				<div class="flex flex-wrap items-end gap-3 px-4 py-3">
					<div>
						<label class="mb-1 block text-[11px] text-muted" for="rule-action">{t.curadoria.gateAction}</label>
						<select
							id="rule-action"
							bind:value={ruleAction}
							class="rounded-token border border-border bg-surface-2 px-2 py-1 text-[13px] focus:outline-none focus:ring-1 focus:ring-primary/50"
						>
							<option value="allow">allow</option>
							<option value="deny">deny</option>
						</select>
					</div>
					<div>
						<label class="mb-1 block text-[11px] text-muted" for="rule-match">{t.curadoria.gateMatchType}</label>
						<select
							id="rule-match"
							bind:value={ruleMatchType}
							class="rounded-token border border-border bg-surface-2 px-2 py-1 text-[13px] focus:outline-none focus:ring-1 focus:ring-primary/50"
						>
							<option value="channel">channel</option>
							<option value="title_contains">title_contains</option>
						</select>
					</div>
					<div class="flex-1">
						<label class="mb-1 block text-[11px] text-muted" for="rule-value">{t.curadoria.gateValue}</label>
						<input
							id="rule-value"
							type="text"
							bind:value={ruleValue}
							class="w-full rounded-token border border-border bg-surface-2 px-2 py-1 text-[13px] focus:outline-none focus:ring-1 focus:ring-primary/50"
						/>
					</div>
					<div class="flex items-center gap-2">
						<input id="rule-enabled" type="checkbox" bind:checked={ruleEnabled} class="h-4 w-4 cursor-pointer" />
						<label class="cursor-pointer text-[13px]" for="rule-enabled">{t.curadoria.gateEnabled}</label>
					</div>
					<button
						disabled={savingRule}
						onclick={saveRule}
						class="cursor-pointer rounded-token border border-border bg-transparent px-4 py-1.5 text-[13px] font-medium hover:bg-hover disabled:cursor-default disabled:opacity-50"
					>{savingRule ? t.curadoria.gateSaving : t.curadoria.gateSave}</button>
				</div>
				{#if saveRuleError}
					<p class="px-4 pb-3 text-[12px] text-red">{saveRuleError}</p>
				{/if}
			</div>
		{/if}
	</div>
</details>
{/if}

<style>
	.decidir-grid {
		display: grid;
		grid-template-columns: 1fr;
	}

	.decidir-grid.has-thumb {
		grid-template-columns: 1fr 1fr;
	}

	.decidir-card {
		min-width: 0;
	}

	.decidir-thumb {
		min-width: 0;
		padding: 1.25rem;
		padding-left: 0;
	}
</style>
