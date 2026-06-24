<script lang="ts">
	import { onMount } from 'svelte';
	import { t } from '$lib/strings';
	import Paginator from '$lib/Paginator.svelte';

	type SourceItem = {
		api_id: string;
		kind: string;
		lane: string;
		display_name: string;
		tags: string[];
		status: string; // active | paused
		config_summary: string;
		created_at: string;
		updated_at: string;
	};
	type SourceKind = { kind: string; label: string; lane: string; icon: string };
	type Counts = { by_status: Record<string, number>; by_kind: Record<string, number> };
	type SourcesResult = { items: SourceItem[]; total: number; counts: Counts };

	// The read screen loads one server page (cap = surface maxSourcePageSize) and filters/paginates
	// client-side. Far above today's ~150 rows; server-side filtering lands with the wizard (#4).
	// ponytail: client-side filter over one page; add server paging when total can exceed PAGE_CAP.
	const PAGE_CAP = 200;

	// glyph per registry icon name — purely cosmetic, falls back to a neutral dot.
	const ICONS: Record<string, string> = {
		youtube: '▶',
		podcast: '🎙',
		rss: '📡',
		globe: '🌐',
		hackernews: 'Y',
		mail: '✉'
	};

	let items = $state<SourceItem[]>([]);
	let kinds = $state<SourceKind[]>([]);
	let counts = $state<Counts>({ by_status: {}, by_kind: {} });
	let total = $state(0);
	let loading = $state(true);
	let error = $state(false);

	// filters
	let fKind = $state('');
	let fStatus = $state('');
	let fTag = $state('');
	let query = $state('');

	let kindMap = $derived(new Map(kinds.map((k) => [k.kind, k])));
	let allTags = $derived([...new Set(items.flatMap((s) => s.tags))].sort());

	function kindLabel(kind: string): string {
		return kindMap.get(kind)?.label ?? kind;
	}
	function kindIcon(kind: string): string {
		return ICONS[kindMap.get(kind)?.icon ?? ''] ?? '•';
	}

	let filtered = $derived(
		items.filter((s) => {
			if (fKind && s.kind !== fKind) return false;
			if (fStatus && s.status !== fStatus) return false;
			if (fTag && !s.tags.includes(fTag)) return false;
			if (query) {
				const q = query.toLowerCase();
				const hay = `${s.display_name} ${s.config_summary} ${s.tags.join(' ')}`.toLowerCase();
				if (!hay.includes(q)) return false;
			}
			return true;
		})
	);

	// Defensive: the BFF is trusted, but a malformed row must not crash the table. Keep only
	// objects with a string api_id and coerce tags to an array (the .includes/.length paths assume it).
	function normItems(raw: unknown): SourceItem[] {
		if (!Array.isArray(raw)) return [];
		return raw
			.filter((s): s is SourceItem => !!s && typeof (s as SourceItem).api_id === 'string')
			.map((s) => ({ ...s, tags: Array.isArray(s.tags) ? s.tags : [] }));
	}

	// Coerce counts to the {by_status, by_kind} shape so the badge lookups can't crash the page
	// on a malformed payload (the template dereferences both sub-maps).
	function normCounts(raw: unknown): Counts {
		const c = (raw ?? {}) as Partial<Counts>;
		const obj = (v: unknown): Record<string, number> =>
			v && typeof v === 'object' ? (v as Record<string, number>) : {};
		return { by_status: obj(c.by_status), by_kind: obj(c.by_kind) };
	}

	function fmtDate(iso: string): string {
		if (!iso) return t.fontes.never;
		return new Date(iso).toLocaleString('pt-BR', {
			day: '2-digit',
			month: '2-digit',
			hour: '2-digit',
			minute: '2-digit'
		});
	}

	onMount(() => {
		const ctrl = new AbortController();
		Promise.all([
			fetch('/api/source-kinds', { signal: ctrl.signal }).then((r) => (r.ok ? r.json() : Promise.reject(r.status))),
			fetch(`/api/sources?page_size=${PAGE_CAP}`, { signal: ctrl.signal }).then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
		])
			.then(([k, res]: [SourceKind[], SourcesResult]) => {
				kinds = Array.isArray(k) ? k : [];
				items = normItems(res?.items);
				counts = normCounts(res?.counts);
				total = res?.total ?? items.length;
				loading = false;
			})
			.catch((e) => {
				if (e?.name === 'AbortError') return; // unmounted mid-flight; leave state untouched
				error = true;
				loading = false;
			});
		// Abort pending fetches on unmount so a hung request can't update state after navigation.
		return () => ctrl.abort();
	});
</script>

<section>
	<p class="mb-5 text-[13px] text-muted">{t.fontes.subtitle}</p>

	{#if loading}
		<p class="text-sm text-muted">{t.fontes.loading}</p>
	{:else if error}
		<p class="text-sm text-red-500">{t.fontes.error}</p>
	{:else if items.length === 0}
		<p class="text-sm text-muted">{t.fontes.empty}</p>
	{:else}
		<!-- ── Filters ── -->
		<div class="mb-4 flex flex-wrap items-center gap-2">
			<input
				type="search"
				bind:value={query}
				placeholder={t.fontes.searchPlaceholder}
				aria-label={t.fontes.searchPlaceholder}
				class="min-w-[240px] flex-1 rounded-token border border-border bg-bg px-3 py-1.5 text-[13px] outline-none focus:border-text/40"
			/>
			<select
				bind:value={fKind}
				aria-label={t.fontes.colKind}
				class="rounded-token border border-border bg-bg px-3 py-1.5 text-[13px] outline-none focus:border-text/40"
			>
				<option value="">{t.fontes.filterAllKinds}</option>
				{#each kinds as k}
					<option value={k.kind}>{k.label}{counts.by_kind[k.kind] ? ` (${counts.by_kind[k.kind]})` : ''}</option>
				{/each}
			</select>
			<select
				bind:value={fStatus}
				aria-label={t.fontes.colStatus}
				class="rounded-token border border-border bg-bg px-3 py-1.5 text-[13px] outline-none focus:border-text/40"
			>
				<option value="">{t.fontes.filterAllStatus}</option>
				<option value="active">{t.fontes.statusActive}{counts.by_status.active ? ` (${counts.by_status.active})` : ''}</option>
				<option value="paused">{t.fontes.statusPaused}{counts.by_status.paused ? ` (${counts.by_status.paused})` : ''}</option>
			</select>
			{#if allTags.length > 0}
				<select
					bind:value={fTag}
					aria-label={t.fontes.colTags}
					class="rounded-token border border-border bg-bg px-3 py-1.5 text-[13px] outline-none focus:border-text/40"
				>
					<option value="">{t.fontes.filterAllTags}</option>
					{#each allTags as tag}
						<option value={tag}>{tag}</option>
					{/each}
				</select>
			{/if}
			<span class="ml-auto text-[12px] tabular-nums text-muted">
				{filtered.length} {filtered.length === 1 ? t.fontes.count : t.fontes.countPlural}
			</span>
		</div>

		{#if total > items.length}
			<p class="mb-3 text-[12px] text-muted">{t.fontes.capNotice.replace('{n}', String(items.length))}</p>
		{/if}

		{#if filtered.length === 0}
			<p class="text-sm text-muted">{t.fontes.emptyFiltered}</p>
		{:else}
			<div class="overflow-x-auto rounded-xl border border-border">
				<Paginator items={filtered} storageKey="fontes.pageSize">
					{#snippet children(page: SourceItem[])}
						<table class="w-full border-collapse text-[13px]">
							<thead>
								<tr class="border-b border-border bg-surface-2 text-left text-muted">
									<th class="px-4 py-2.5 font-medium">{t.fontes.colName}</th>
									<th class="px-4 py-2.5 font-medium">{t.fontes.colKind}</th>
									<th class="px-4 py-2.5 font-medium">{t.fontes.colLane}</th>
									<th class="px-4 py-2.5 font-medium">{t.fontes.colStatus}</th>
									<th class="px-4 py-2.5 font-medium">{t.fontes.colTags}</th>
									<th class="px-4 py-2.5 font-medium">{t.fontes.colUpdated}</th>
								</tr>
							</thead>
							<tbody>
								{#each page as s (s.api_id)}
									<tr class="border-b border-border last:border-0 hover:bg-hover">
										<td class="px-4 py-2.5">
											<span class="font-medium text-text">{s.display_name || s.api_id}</span>
											{#if s.config_summary}
												<span class="block truncate text-[11px] text-muted" title={s.config_summary}>{s.config_summary}</span>
											{/if}
										</td>
										<td class="px-4 py-2.5 whitespace-nowrap text-muted">
											<span aria-hidden="true" class="mr-1 opacity-70">{kindIcon(s.kind)}</span>{kindLabel(s.kind)}
										</td>
										<td class="px-4 py-2.5 text-muted">{s.lane}</td>
										<td class="px-4 py-2.5 whitespace-nowrap">
											<span class="inline-flex items-center gap-1.5 text-muted">
												<span
													class="h-[7px] w-[7px] flex-none rounded-full {s.status === 'active' ? 'bg-green' : 'bg-amber'}"
												></span>
												{s.status === 'active' ? t.fontes.statusActive : t.fontes.statusPaused}
											</span>
										</td>
										<td class="px-4 py-2.5">
											{#if s.tags.length > 0}
												<div class="flex flex-wrap gap-1">
													{#each s.tags as tag}
														<span class="rounded-full border border-border bg-surface-2 px-2 py-0.5 text-[10px] text-muted">{tag}</span>
													{/each}
												</div>
											{:else}
												<span class="text-muted">{t.fontes.never}</span>
											{/if}
										</td>
										<td class="px-4 py-2.5 whitespace-nowrap tabular-nums text-muted">{fmtDate(s.updated_at)}</td>
									</tr>
								{/each}
							</tbody>
						</table>
					{/snippet}
				</Paginator>
			</div>
		{/if}
	{/if}
</section>
