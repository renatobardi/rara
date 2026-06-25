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
	type SourceField = {
		name: string;
		label: string;
		type: string; // text | url | textarea
		required?: boolean;
		placeholder?: string;
	};
	type SourceKind = {
		kind: string;
		label: string;
		lane: string;
		icon: string;
		supports_pause?: boolean;
		supports_tags?: boolean;
		fields?: SourceField[];
	};
	type Counts = { by_status: Record<string, number>; by_kind: Record<string, number> };
	type SourcesResult = { items: SourceItem[]; total: number; counts: Counts };
	type BulkResult = { applied: number; failed: number };

	// Filtering and search now run SERVER-side (the BFF forwards kind/status/tag/q to sources_v),
	// so the table only holds the rows matching the active filter — no more in-memory filtering of a
	// fixed blob (fatia #3 debt). One server page is still fetched (cap = surface maxSourcePageSize)
	// and split into client pages for display.
	// ponytail: server-side FILTER; client-side paging over the filtered set. A single filter that
	// matches >PAGE_CAP rows shows a "refine" notice; add server page fetching only if that happens.
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

	let items = $state<SourceItem[]>([]); // rows matching the active filter (one server page)
	let kinds = $state<SourceKind[]>([]);
	let counts = $state<Counts>({ by_status: {}, by_kind: {} }); // GLOBAL badge counts (unfiltered)
	let tagUniverse = $state<string[]>([]); // GLOBAL tag list (unfiltered) for the tag dropdown
	let total = $state(0); // total rows matching the active filter
	let loading = $state(true);
	let error = $state(false);
	let refetching = $state(false); // a filter/search reload in flight (table dims, no skeleton)

	// filters
	let fKind = $state('');
	let fStatus = $state('');
	let fTag = $state('');
	let query = $state('');
	let searchTimer: ReturnType<typeof setTimeout> | undefined;

	// selection (api_ids); persists only within the active filter — cleared on every refilter.
	let selectedIds = $state<string[]>([]);
	let selectedSet = $derived(new Set(selectedIds));
	let allSelected = $derived(items.length > 0 && items.every((s) => selectedSet.has(s.api_id)));
	let someSelected = $derived(selectedIds.length > 0 && !allSelected);

	// bulk
	let bulkBusy = $state(false);
	let bulkTagInput = $state('');
	let bulkDeleteOpen = $state(false);

	// toasts
	type Toast = { id: number; kind: 'ok' | 'err'; msg: string };
	let toasts = $state<Toast[]>([]);
	let toastSeq = 0;
	function toast(kind: 'ok' | 'err', msg: string) {
		const id = ++toastSeq;
		toasts = [...toasts, { id, kind, msg }];
		setTimeout(() => (toasts = toasts.filter((x) => x.id !== id)), 4000);
	}

	// wizard (create)
	let wizardOpen = $state(false);
	let wizardStep = $state<1 | 2>(1);
	let wizardKind = $state<SourceKind | null>(null);
	let formValues = $state<Record<string, string>>({});
	let creating = $state(false);
	let createError = $state('');

	// edit
	let editSource = $state<SourceItem | null>(null);
	let editDisplayName = $state('');
	let editTags = $state<string[]>([]);
	let editTagInput = $state('');
	let editSaving = $state(false);
	let editError = $state('');

	// delete (single)
	let deleteTarget = $state<SourceItem | null>(null);
	let deleting = $state(false);

	// in-flight pause/resume guard, keyed by api_id
	let toggling = $state<Record<string, boolean>>({});

	let kindMap = $derived(new Map(kinds.map((k) => [k.kind, k])));

	function kindLabel(kind: string): string {
		return kindMap.get(kind)?.label ?? kind;
	}
	function kindIcon(kind: string): string {
		return ICONS[kindMap.get(kind)?.icon ?? ''] ?? '•';
	}

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

	// Pull the {"error": "..."} message a core 4xx carries, falling back to a generic label.
	async function errMsg(res: Response, fallback: string): Promise<string> {
		try {
			const body = await res.json();
			if (body && typeof body.error === 'string') return body.error;
		} catch {
			/* non-JSON body — use the fallback */
		}
		return fallback;
	}

	// Does this kind support tags / pause? The registry flags default to true; an explicit false
	// hides the affordance and drops the field from the payload (capability model, not hardcoded).
	function supportsTags(kind: string): boolean {
		const k = kindMap.get(kind);
		return k ? k.supports_tags !== false : false;
	}
	function supportsPause(kind: string): boolean {
		const k = kindMap.get(kind);
		return k ? k.supports_pause !== false : false;
	}

	// Build a write URL with the dynamic segment percent-encoded so a stray character can't reshape
	// the route. The colon in kind:N survives as %3A and the Go mux decodes it back to the api_id.
	function apiPath(seg: string, suffix = ''): string {
		return `/api/sources/${encodeURIComponent(seg)}${suffix}`;
	}

	// Build the GET /api/sources query from the active filter. The BFF allow-lists these params.
	function sourcesQuery(): string {
		const q = new URLSearchParams();
		if (fKind) q.set('kind', fKind);
		if (fStatus) q.set('status', fStatus);
		if (fTag) q.set('tag', fTag);
		if (query.trim()) q.set('q', query.trim());
		q.set('page_size', String(PAGE_CAP));
		return q.toString();
	}

	// pageLoad fetches the rows matching the active filter (server-side). Clears the selection,
	// since the visible set just changed. Errors raise a toast but leave the prior rows in place.
	async function pageLoad() {
		refetching = true;
		try {
			const res = await fetch(`/api/sources?${sourcesQuery()}`);
			if (!res.ok) {
				toast('err', t.fontes.error);
				return;
			}
			const data: SourcesResult = await res.json();
			items = normItems(data?.items);
			total = data?.total ?? items.length;
			selectedIds = [];
		} catch {
			toast('err', t.fontes.error);
		} finally {
			refetching = false;
		}
	}

	// globalLoad fetches the UNFILTERED dataset to keep the dropdown badges (counts) and the tag
	// filter (tagUniverse) global — independent of the active filter. Reused after every mutation.
	// ponytail: tagUniverse is derived from one unfiltered page; if the dataset ever exceeds
	// PAGE_CAP, rare tags past the cap won't list — acceptable until that happens.
	async function globalLoad() {
		try {
			const res = await fetch(`/api/sources?page_size=${PAGE_CAP}`);
			if (!res.ok) return;
			const data: SourcesResult = await res.json();
			counts = normCounts(data?.counts);
			tagUniverse = [...new Set(normItems(data?.items).flatMap((s) => s.tags))].sort();
		} catch {
			/* badges degrade silently — the table itself uses pageLoad */
		}
	}

	// Refresh both the filtered table and the global badges/tags after a mutation.
	async function reloadAll() {
		await Promise.all([pageLoad(), globalLoad()]);
	}

	// Re-run the server query when a filter changes (selects fire onchange; search is debounced).
	function applyFilters() {
		clearTimeout(searchTimer);
		pageLoad();
	}
	function onSearchInput() {
		clearTimeout(searchTimer);
		searchTimer = setTimeout(pageLoad, 300);
	}

	onMount(() => {
		const ctrl = new AbortController();
		Promise.all([
			fetch('/api/source-kinds', { signal: ctrl.signal }).then((r) => (r.ok ? r.json() : Promise.reject(r.status))),
			fetch(`/api/sources?page_size=${PAGE_CAP}`, { signal: ctrl.signal }).then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
		])
			.then(([k, res]: [SourceKind[], SourcesResult]) => {
				kinds = Array.isArray(k) ? k : [];
				// First load is unfiltered, so it seeds the table AND the global badges/tags in one fetch.
				items = normItems(res?.items);
				counts = normCounts(res?.counts);
				tagUniverse = [...new Set(items.flatMap((s) => s.tags))].sort();
				total = res?.total ?? items.length;
				loading = false;
			})
			.catch((e) => {
				if (e?.name === 'AbortError') return; // unmounted mid-flight; leave state untouched
				error = true;
				loading = false;
			});
		// Abort pending fetches on unmount so a hung request can't update state after navigation.
		return () => {
			ctrl.abort();
			clearTimeout(searchTimer);
		};
	});

	// ── Selection ──
	function toggleRow(id: string) {
		selectedIds = selectedSet.has(id) ? selectedIds.filter((x) => x !== id) : [...selectedIds, id];
	}
	function toggleAll() {
		selectedIds = allSelected ? [] : items.map((s) => s.api_id);
	}
	function clearSelection() {
		selectedIds = [];
	}

	// ── Bulk actions ──
	async function bulk(action: 'pause' | 'resume' | 'tag' | 'untag' | 'delete', tag?: string) {
		if (selectedIds.length === 0 || bulkBusy) return;
		bulkBusy = true;
		try {
			const body: { action: string; ids: string[]; tag?: string } = { action, ids: selectedIds };
			if (tag) body.tag = tag;
			const res = await fetch('/api/sources/bulk', {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify(body)
			});
			if (!res.ok) {
				toast('err', await errMsg(res, t.fontes.bulkError));
				return;
			}
			const r: BulkResult = await res.json();
			const applied = r?.applied ?? 0;
			const failed = r?.failed ?? 0;
			if (failed > 0) {
				toast('err', t.fontes.bulkResultPartial.replace('{ok}', String(applied)).replace('{fail}', String(failed)));
			} else {
				toast('ok', t.fontes.bulkResultOk.replace('{ok}', String(applied)));
			}
			bulkTagInput = '';
			await reloadAll();
		} catch {
			toast('err', t.fontes.bulkError);
		} finally {
			bulkBusy = false;
			bulkDeleteOpen = false;
		}
	}
	function bulkTag(action: 'tag' | 'untag') {
		const tag = bulkTagInput.trim();
		if (!tag) return;
		bulk(action, tag);
	}

	// ── Wizard (create) ──
	function openWizard() {
		wizardStep = 1;
		wizardKind = null;
		formValues = {};
		createError = '';
		wizardOpen = true;
	}
	function pickKind(k: SourceKind) {
		wizardKind = k;
		formValues = Object.fromEntries((k.fields ?? []).map((f) => [f.name, '']));
		createError = '';
		wizardStep = 2;
	}
	async function submitCreate() {
		if (!wizardKind) return;
		const fields = wizardKind.fields ?? [];
		const body: Record<string, string> = {};
		for (const f of fields) {
			const v = (formValues[f.name] ?? '').trim();
			if (f.required && !v) {
				createError = `${f.label}: ${t.fontes.wizardRequired}`;
				return;
			}
			if (v) body[f.name] = v;
		}
		creating = true;
		createError = '';
		try {
			const res = await fetch(apiPath(wizardKind.kind), {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify(body)
			});
			if (!res.ok) {
				createError = await errMsg(res, t.fontes.wizardCreateError);
				return;
			}
			wizardOpen = false;
			toast('ok', t.fontes.wizardCreateOk);
			await reloadAll();
		} catch {
			createError = t.fontes.wizardCreateError;
		} finally {
			creating = false;
		}
	}

	// ── Edit (display_name + tags) ──
	function openEdit(s: SourceItem) {
		editSource = s;
		editDisplayName = s.display_name;
		editTags = [...s.tags];
		editTagInput = '';
		editError = '';
	}
	function addEditTag() {
		const tag = editTagInput.trim();
		if (tag && !editTags.includes(tag)) editTags = [...editTags, tag];
		editTagInput = '';
	}
	function removeEditTag(tag: string) {
		editTags = editTags.filter((x) => x !== tag);
	}
	async function submitEdit() {
		if (!editSource) return;
		editSaving = true;
		editError = '';
		// Only send tags for kinds that support them (the registry capability model).
		const payload: { display_name: string; tags?: string[] } = { display_name: editDisplayName.trim() };
		if (supportsTags(editSource.kind)) {
			addEditTag(); // commit any tag still typed in the input but not yet Enter-ed
			payload.tags = editTags;
		}
		try {
			const res = await fetch(apiPath(editSource.api_id), {
				method: 'PATCH',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify(payload)
			});
			if (!res.ok) {
				editError = await errMsg(res, t.fontes.editSaveError);
				return;
			}
			editSource = null;
			toast('ok', t.fontes.editSaveOk);
			await reloadAll();
		} catch {
			editError = t.fontes.editSaveError;
		} finally {
			editSaving = false;
		}
	}

	// ── Delete single (soft-delete; source disappears from the list) ──
	async function confirmDelete() {
		if (!deleteTarget) return;
		const target = deleteTarget;
		deleting = true;
		try {
			const res = await fetch(apiPath(target.api_id), { method: 'DELETE' });
			if (!res.ok) {
				toast('err', await errMsg(res, t.fontes.deleteError));
				return;
			}
			deleteTarget = null;
			toast('ok', t.fontes.deleteOk);
			await reloadAll();
		} catch {
			toast('err', t.fontes.deleteError);
		} finally {
			deleting = false;
		}
	}

	// ── Pause / resume (optimistic) ──
	async function togglePause(s: SourceItem) {
		if (toggling[s.api_id]) return;
		const next = s.status === 'active' ? 'paused' : 'active';
		const action = next === 'paused' ? 'pause' : 'resume';
		// Optimistic flip; revert on failure.
		items = items.map((x) => (x.api_id === s.api_id ? { ...x, status: next } : x));
		toggling = { ...toggling, [s.api_id]: true };
		try {
			const res = await fetch(apiPath(s.api_id, `/${action}`), { method: 'POST' });
			if (!res.ok) {
				items = items.map((x) => (x.api_id === s.api_id ? { ...x, status: s.status } : x));
				toast('err', t.fontes.pauseError);
				return;
			}
			// Move the badge count from the old status bucket to the new one.
			counts = {
				...counts,
				by_status: {
					...counts.by_status,
					[s.status]: Math.max(0, (counts.by_status[s.status] ?? 0) - 1),
					[next]: (counts.by_status[next] ?? 0) + 1
				}
			};
			toast('ok', action === 'pause' ? t.fontes.pauseOk : t.fontes.resumeOk);
		} catch {
			items = items.map((x) => (x.api_id === s.api_id ? { ...x, status: s.status } : x));
			toast('err', t.fontes.pauseError);
		} finally {
			const { [s.api_id]: _, ...rest } = toggling;
			toggling = rest;
		}
	}

	// A modal must not be dismissable mid-mutation — closing would hide the inline error.
	let mutating = $derived(creating || editSaving || deleting || bulkBusy);

	function closeOnEsc(e: KeyboardEvent) {
		if (e.key !== 'Escape' || mutating) return;
		if (wizardOpen) wizardOpen = false;
		else if (editSource) editSource = null;
		else if (deleteTarget) deleteTarget = null;
		else if (bulkDeleteOpen) bulkDeleteOpen = false;
	}

	// Move focus into a dialog when it opens and keep Tab cycling trapped inside it, so keyboard
	// users can't wander into the inert page behind the modal.
	function focusInto(node: HTMLElement) {
		const sel =
			'a[href], button:not([disabled]), input:not([disabled]), textarea:not([disabled]), select:not([disabled]), [tabindex]:not([tabindex="-1"])';
		const focusables = () =>
			Array.from(node.querySelectorAll<HTMLElement>(sel)).filter((el) => el.offsetParent !== null);
		const prev = document.activeElement as HTMLElement | null;
		focusables()[0]?.focus();
		function onKeydown(e: KeyboardEvent) {
			if (e.key !== 'Tab') return;
			const els = focusables();
			if (els.length === 0) return;
			const first = els[0];
			const last = els[els.length - 1];
			const active = document.activeElement;
			if (e.shiftKey && active === first) {
				e.preventDefault();
				last.focus();
			} else if (!e.shiftKey && active === last) {
				e.preventDefault();
				first.focus();
			}
		}
		node.addEventListener('keydown', onKeydown);
		return {
			destroy: () => {
				node.removeEventListener('keydown', onKeydown);
				prev?.focus?.(); // return focus to whatever opened the dialog
			}
		};
	}

	// Bind a checkbox's indeterminate property (it's not settable via an attribute).
	function indeterminate(node: HTMLInputElement, value: boolean) {
		node.indeterminate = value;
		return { update: (v: boolean) => (node.indeterminate = v) };
	}

	function selectedLabel(n: number): string {
		return (n === 1 ? t.fontes.selectedCount : t.fontes.selectedCountPlural).replace('{n}', String(n));
	}
</script>

<svelte:window onkeydown={closeOnEsc} />

<section>
	<div class="mb-5 flex items-start gap-4">
		<p class="flex-1 text-[13px] text-muted">{t.fontes.subtitle}</p>
		{#if !loading && !error}
			<button
				class="flex-none rounded-token bg-text px-3.5 py-1.5 text-[13px] font-medium text-bg hover:opacity-90"
				onclick={openWizard}>+ {t.fontes.newSource}</button
			>
		{/if}
	</div>

	{#if loading}
		<!-- Skeleton rows while the first fetch resolves; honours reduced-motion. -->
		<div class="space-y-2" aria-busy="true" aria-live="polite">
			<span class="sr-only">{t.fontes.loading}</span>
			{#each Array(6) as _, i (i)}
				<div class="h-11 animate-pulse rounded-token bg-surface-2 motion-reduce:animate-none"></div>
			{/each}
		</div>
	{:else if error}
		<p class="text-sm text-red-500">{t.fontes.error}</p>
	{:else}
		<!-- ── Filters ── -->
		<div class="mb-4 flex flex-wrap items-center gap-2">
			<input
				type="search"
				bind:value={query}
				oninput={onSearchInput}
				placeholder={t.fontes.searchPlaceholder}
				aria-label={t.fontes.searchPlaceholder}
				class="min-w-[240px] flex-1 rounded-token border border-border bg-bg px-3 py-1.5 text-[13px] outline-none focus:border-text/40"
			/>
			<select
				bind:value={fKind}
				onchange={applyFilters}
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
				onchange={applyFilters}
				aria-label={t.fontes.colStatus}
				class="rounded-token border border-border bg-bg px-3 py-1.5 text-[13px] outline-none focus:border-text/40"
			>
				<option value="">{t.fontes.filterAllStatus}</option>
				<option value="active">{t.fontes.statusActive}{counts.by_status.active ? ` (${counts.by_status.active})` : ''}</option>
				<option value="paused">{t.fontes.statusPaused}{counts.by_status.paused ? ` (${counts.by_status.paused})` : ''}</option>
			</select>
			{#if tagUniverse.length > 0}
				<select
					bind:value={fTag}
					onchange={applyFilters}
					aria-label={t.fontes.colTags}
					class="rounded-token border border-border bg-bg px-3 py-1.5 text-[13px] outline-none focus:border-text/40"
				>
					<option value="">{t.fontes.filterAllTags}</option>
					{#each tagUniverse as tag}
						<option value={tag}>{tag}</option>
					{/each}
				</select>
			{/if}
			<span class="ml-auto text-[12px] tabular-nums text-muted">
				{total} {total === 1 ? t.fontes.count : t.fontes.countPlural}
			</span>
		</div>

		<!-- ── Bulk action bar ── -->
		{#if selectedIds.length > 0}
			<div
				class="mb-3 flex flex-wrap items-center gap-2 rounded-token border border-border bg-surface-2 px-3 py-2 text-[13px]"
				role="region"
				aria-label={selectedLabel(selectedIds.length)}
			>
				<span class="font-medium text-text">{selectedLabel(selectedIds.length)}</span>
				<span class="mx-1 h-4 w-px bg-border"></span>
				<button class="rounded-token border border-border px-2 py-1 text-muted hover:bg-hover disabled:opacity-50" disabled={bulkBusy} onclick={() => bulk('pause')}>{t.fontes.bulkPause}</button>
				<button class="rounded-token border border-border px-2 py-1 text-muted hover:bg-hover disabled:opacity-50" disabled={bulkBusy} onclick={() => bulk('resume')}>{t.fontes.bulkResume}</button>
				<span class="mx-1 h-4 w-px bg-border"></span>
				<input
					bind:value={bulkTagInput}
					placeholder={t.fontes.bulkTagPlaceholder}
					aria-label={t.fontes.bulkTag}
					onkeydown={(e) => {
						if (e.key === 'Enter') {
							e.preventDefault();
							bulkTag('tag');
						}
					}}
					class="w-28 rounded-token border border-border bg-bg px-2 py-1 outline-none focus:border-text/40"
				/>
				<button class="rounded-token border border-border px-2 py-1 text-muted hover:bg-hover disabled:opacity-50" disabled={bulkBusy || !bulkTagInput.trim()} onclick={() => bulkTag('tag')}>{t.fontes.bulkTag}</button>
				<button class="rounded-token border border-border px-2 py-1 text-muted hover:bg-hover disabled:opacity-50" disabled={bulkBusy || !bulkTagInput.trim()} onclick={() => bulkTag('untag')}>{t.fontes.bulkUntag}</button>
				<span class="mx-1 h-4 w-px bg-border"></span>
				<button class="rounded-token border border-border px-2 py-1 text-red-500 hover:bg-hover disabled:opacity-50" disabled={bulkBusy} onclick={() => (bulkDeleteOpen = true)}>{t.fontes.bulkDelete}</button>
				<button class="ml-auto rounded-token px-2 py-1 text-muted hover:bg-hover disabled:opacity-50" disabled={bulkBusy} onclick={clearSelection}>{t.fontes.bulkClear}</button>
				{#if bulkBusy}<span class="text-muted">{t.fontes.bulkApplying}</span>{/if}
			</div>
		{/if}

		{#if total > items.length}
			<p class="mb-3 text-[12px] text-muted">{t.fontes.capNoticeFiltered.replace('{n}', String(items.length))}</p>
		{/if}

		{#if items.length === 0}
			<p class="text-sm text-muted">{query || fKind || fStatus || fTag ? t.fontes.emptyFiltered : t.fontes.empty}</p>
		{:else}
			<div class="overflow-x-auto rounded-xl border border-border transition-opacity {refetching ? 'opacity-60' : ''}" aria-busy={refetching}>
				<Paginator items={items} storageKey="fontes.pageSize">
					{#snippet children(page: SourceItem[])}
						<table class="w-full border-collapse text-[13px]">
							<thead>
								<tr class="border-b border-border bg-surface-2 text-left text-muted">
									<th class="w-9 px-4 py-2.5">
										<input
											type="checkbox"
											checked={allSelected}
											use:indeterminate={someSelected}
											onchange={toggleAll}
											aria-label={t.fontes.selectAll}
											class="cursor-pointer align-middle"
										/>
									</th>
									<th class="px-4 py-2.5 font-medium">{t.fontes.colName}</th>
									<th class="px-4 py-2.5 font-medium">{t.fontes.colKind}</th>
									<th class="px-4 py-2.5 font-medium">{t.fontes.colLane}</th>
									<th class="px-4 py-2.5 font-medium">{t.fontes.colStatus}</th>
									<th class="px-4 py-2.5 font-medium">{t.fontes.colTags}</th>
									<th class="px-4 py-2.5 font-medium">{t.fontes.colUpdated}</th>
									<th class="px-4 py-2.5 text-right font-medium">{t.fontes.colActions}</th>
								</tr>
							</thead>
							<tbody>
								{#each page as s (s.api_id)}
									<tr class="border-b border-border last:border-0 hover:bg-hover {selectedSet.has(s.api_id) ? 'bg-surface-2' : ''}">
										<td class="px-4 py-2.5">
											<input
												type="checkbox"
												checked={selectedSet.has(s.api_id)}
												onchange={() => toggleRow(s.api_id)}
												aria-label={`${t.fontes.selectRow}: ${s.display_name || s.api_id}`}
												class="cursor-pointer align-middle"
											/>
										</td>
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
										<td class="px-4 py-2.5 whitespace-nowrap text-right">
											<div class="inline-flex gap-1.5">
												{#if supportsPause(s.kind)}
													<button
														class="rounded-token border border-border px-2 py-1 text-[12px] text-muted hover:bg-surface-2 disabled:opacity-50"
														disabled={toggling[s.api_id]}
														aria-label={`${s.status === 'active' ? t.fontes.actionPause : t.fontes.actionResume}: ${s.display_name || s.api_id}`}
														onclick={() => togglePause(s)}
													>{s.status === 'active' ? t.fontes.actionPause : t.fontes.actionResume}</button>
												{/if}
												<button
													class="rounded-token border border-border px-2 py-1 text-[12px] text-muted hover:bg-surface-2"
													aria-label={`${t.fontes.actionEdit}: ${s.display_name || s.api_id}`}
													onclick={() => openEdit(s)}>{t.fontes.actionEdit}</button>
												<button
													class="rounded-token border border-border px-2 py-1 text-[12px] text-red-500 hover:bg-surface-2"
													aria-label={`${t.fontes.actionDelete}: ${s.display_name || s.api_id}`}
													onclick={() => (deleteTarget = s)}>{t.fontes.actionDelete}</button>
											</div>
										</td>
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

<!-- ── Wizard modal (create) ── -->
{#if wizardOpen}
	<div
		class="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4"
		role="presentation"
		onclick={(e) => e.target === e.currentTarget && !mutating && (wizardOpen = false)}
	>
		<div
			class="w-full max-w-[560px] rounded-xl border border-border bg-bg p-5 shadow-xl"
			role="dialog"
			aria-modal="true"
			aria-labelledby="fontes-wizard-title"
			use:focusInto
		>
			{#if wizardStep === 1}
				<h2 id="fontes-wizard-title" class="mb-4 text-[15px] font-semibold">{t.fontes.wizardStep1Title}</h2>
				<div class="grid grid-cols-2 gap-2 sm:grid-cols-3">
					{#each kinds as k}
						<button
							class="flex flex-col items-start gap-1 rounded-token border border-border p-3 text-left hover:border-text/40 hover:bg-hover"
							onclick={() => pickKind(k)}
						>
							<span class="text-lg" aria-hidden="true">{kindIcon(k.kind)}</span>
							<span class="text-[13px] font-medium text-text">{k.label}</span>
							<span class="text-[11px] text-muted">{k.lane}</span>
						</button>
					{/each}
				</div>
				<div class="mt-4 flex justify-end">
					<button class="rounded-token border border-border px-3 py-1.5 text-[13px] text-muted hover:bg-surface-2" onclick={() => (wizardOpen = false)}>{t.fontes.wizardCancel}</button>
				</div>
			{:else if wizardKind}
				<h2 id="fontes-wizard-title" class="mb-4 text-[15px] font-semibold">{t.fontes.wizardStep2Title.replace('{kind}', wizardKind.label)}</h2>
				<div class="flex flex-col gap-3">
					{#each wizardKind.fields ?? [] as f (f.name)}
						<label class="flex flex-col gap-1 text-[13px]">
							<span class="text-muted">{f.label}{f.required ? ' *' : ''}</span>
							{#if f.type === 'textarea'}
								<textarea
									bind:value={formValues[f.name]}
									placeholder={f.placeholder ?? ''}
									rows="3"
									class="rounded-token border border-border bg-bg px-3 py-1.5 outline-none focus:border-text/40"
								></textarea>
							{:else}
								<input
									type={f.type === 'url' ? 'url' : 'text'}
									bind:value={formValues[f.name]}
									placeholder={f.placeholder ?? ''}
									class="rounded-token border border-border bg-bg px-3 py-1.5 outline-none focus:border-text/40"
								/>
							{/if}
						</label>
					{/each}
					{#if createError}
						<p class="text-[12px] text-red-500">{createError}</p>
					{/if}
				</div>
				<div class="mt-5 flex items-center justify-between">
					<button class="rounded-token border border-border px-3 py-1.5 text-[13px] text-muted hover:bg-surface-2 disabled:opacity-50" disabled={creating} onclick={() => (wizardStep = 1)}>{t.fontes.wizardBack}</button>
					<div class="flex gap-2">
						<button class="rounded-token border border-border px-3 py-1.5 text-[13px] text-muted hover:bg-surface-2 disabled:opacity-50" disabled={creating} onclick={() => (wizardOpen = false)}>{t.fontes.wizardCancel}</button>
						<button
							class="rounded-token bg-text px-3.5 py-1.5 text-[13px] font-medium text-bg hover:opacity-90 disabled:opacity-50"
							disabled={creating}
							onclick={submitCreate}>{creating ? t.fontes.wizardCreating : t.fontes.wizardCreate}</button>
					</div>
				</div>
			{/if}
		</div>
	</div>
{/if}

<!-- ── Edit modal ── -->
{#if editSource}
	<div
		class="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4"
		role="presentation"
		onclick={(e) => e.target === e.currentTarget && !mutating && (editSource = null)}
	>
		<div
			class="w-full max-w-[480px] rounded-xl border border-border bg-bg p-5 shadow-xl"
			role="dialog"
			aria-modal="true"
			aria-labelledby="fontes-edit-title"
			use:focusInto
		>
			<h2 id="fontes-edit-title" class="mb-1 text-[15px] font-semibold">{t.fontes.editTitle}</h2>
			<p class="mb-4 text-[11px] text-muted">{editSource.api_id} · {kindLabel(editSource.kind)}</p>
			<div class="flex flex-col gap-3">
				<label class="flex flex-col gap-1 text-[13px]">
					<span class="text-muted">{t.fontes.editDisplayName}</span>
					<input
						bind:value={editDisplayName}
						class="rounded-token border border-border bg-bg px-3 py-1.5 outline-none focus:border-text/40"
					/>
				</label>
				{#if supportsTags(editSource.kind)}
				<div class="flex flex-col gap-1 text-[13px]">
					<span class="text-muted">{t.fontes.editTags}</span>
					{#if editTags.length > 0}
						<div class="flex flex-wrap gap-1">
							{#each editTags as tag}
								<span class="inline-flex items-center gap-1 rounded-full border border-border bg-surface-2 px-2 py-0.5 text-[11px] text-muted">
									{tag}
									<button class="opacity-60 hover:opacity-100" aria-label={t.fontes.editTagRemove} onclick={() => removeEditTag(tag)}>×</button>
								</span>
							{/each}
						</div>
					{/if}
					<input
						bind:value={editTagInput}
						placeholder={t.fontes.editTagAdd}
						onkeydown={(e) => {
							if (e.key === 'Enter') {
								e.preventDefault();
								addEditTag();
							}
						}}
						class="mt-1 rounded-token border border-border bg-bg px-3 py-1.5 outline-none focus:border-text/40"
					/>
				</div>
				{/if}
				<p class="text-[11px] text-muted">{t.fontes.editFieldsNote}</p>
				{#if editError}
					<p class="text-[12px] text-red-500">{editError}</p>
				{/if}
			</div>
			<div class="mt-5 flex justify-end gap-2">
				<button class="rounded-token border border-border px-3 py-1.5 text-[13px] text-muted hover:bg-surface-2 disabled:opacity-50" disabled={editSaving} onclick={() => (editSource = null)}>{t.fontes.wizardCancel}</button>
				<button
					class="rounded-token bg-text px-3.5 py-1.5 text-[13px] font-medium text-bg hover:opacity-90 disabled:opacity-50"
					disabled={editSaving}
					onclick={submitEdit}>{editSaving ? t.fontes.editSaving : t.fontes.editSave}</button>
			</div>
		</div>
	</div>
{/if}

<!-- ── Delete confirm (single) ── -->
{#if deleteTarget}
	<div
		class="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4"
		role="presentation"
		onclick={(e) => e.target === e.currentTarget && !mutating && (deleteTarget = null)}
	>
		<div
			class="w-full max-w-[420px] rounded-xl border border-border bg-bg p-5 shadow-xl"
			role="dialog"
			aria-modal="true"
			aria-labelledby="fontes-delete-title"
			use:focusInto
		>
			<h2 id="fontes-delete-title" class="mb-3 text-[15px] font-semibold">{t.fontes.deleteTitle}</h2>
			<p class="mb-5 text-[13px] text-muted">
				{t.fontes.deleteConfirm.replace('{name}', deleteTarget.display_name || deleteTarget.api_id)}
			</p>
			<div class="flex justify-end gap-2">
				<button class="rounded-token border border-border px-3 py-1.5 text-[13px] text-muted hover:bg-surface-2 disabled:opacity-50" disabled={deleting} onclick={() => (deleteTarget = null)}>{t.fontes.deleteCancel}</button>
				<button
					class="rounded-token bg-red-500 px-3.5 py-1.5 text-[13px] font-medium text-white hover:opacity-90 disabled:opacity-50"
					disabled={deleting}
					onclick={confirmDelete}>{deleting ? t.fontes.deleting : t.fontes.deleteConfirmBtn}</button>
			</div>
		</div>
	</div>
{/if}

<!-- ── Bulk delete confirm ── -->
{#if bulkDeleteOpen}
	<div
		class="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4"
		role="presentation"
		onclick={(e) => e.target === e.currentTarget && !mutating && (bulkDeleteOpen = false)}
	>
		<div
			class="w-full max-w-[420px] rounded-xl border border-border bg-bg p-5 shadow-xl"
			role="dialog"
			aria-modal="true"
			aria-labelledby="fontes-bulk-delete-title"
			use:focusInto
		>
			<h2 id="fontes-bulk-delete-title" class="mb-3 text-[15px] font-semibold">{t.fontes.bulkDeleteTitle}</h2>
			<p class="mb-5 text-[13px] text-muted">
				{t.fontes.bulkDeleteConfirm.replace('{n}', String(selectedIds.length))}
			</p>
			<div class="flex justify-end gap-2">
				<button class="rounded-token border border-border px-3 py-1.5 text-[13px] text-muted hover:bg-surface-2 disabled:opacity-50" disabled={bulkBusy} onclick={() => (bulkDeleteOpen = false)}>{t.fontes.deleteCancel}</button>
				<button
					class="rounded-token bg-red-500 px-3.5 py-1.5 text-[13px] font-medium text-white hover:opacity-90 disabled:opacity-50"
					disabled={bulkBusy}
					onclick={() => bulk('delete')}>{bulkBusy ? t.fontes.deleting : t.fontes.deleteConfirmBtn}</button>
			</div>
		</div>
	</div>
{/if}

<!-- ── Toasts ── -->
{#if toasts.length > 0}
	<div class="fixed bottom-4 right-4 z-[60] flex flex-col gap-2">
		{#each toasts as tst (tst.id)}
			<div
				class="rounded-token border px-4 py-2 text-[13px] shadow-lg {tst.kind === 'ok'
					? 'border-green/40 bg-surface-2 text-text'
					: 'border-red-500/40 bg-surface-2 text-red-500'}"
				role={tst.kind === 'ok' ? 'status' : 'alert'}
			>
				{tst.msg}
			</div>
		{/each}
	</div>
{/if}
