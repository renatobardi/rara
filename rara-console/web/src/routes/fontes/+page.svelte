<script lang="ts">
	import { onMount } from 'svelte';
	import { t } from '$lib/strings';

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

	// Filtering, search AND pagination all run SERVER-side: the BFF forwards
	// kind/status/tag/q/page/page_size to sources_v, so the table only ever holds the current page of
	// rows matching the active filter — no in-memory filtering or slicing (fatia #3 debt retired).
	const PAGE_SIZES = [20, 50, 100] as const;
	const PAGE_SIZE_KEY = 'fontes.pageSize';
	// Unfiltered fetches (global counts + tag universe) ask for the surface's max page in one shot.
	const GLOBAL_CAP = 200;
	function loadPageSize(): number {
		try {
			const v = Number(localStorage.getItem(PAGE_SIZE_KEY));
			return (PAGE_SIZES as readonly number[]).includes(v) ? v : 20;
		} catch {
			return 20;
		}
	}

	// glyph per registry icon name — purely cosmetic, falls back to a neutral dot.
	const ICONS: Record<string, string> = {
		youtube: '▶',
		podcast: '🎙',
		rss: '📡',
		globe: '🌐',
		hackernews: 'Y',
		mail: '✉',
		linkedin: 'in'
	};

	let items = $state<SourceItem[]>([]); // current server page of rows matching the active filter
	let kinds = $state<SourceKind[]>([]);
	let counts = $state<Counts>({ by_status: {}, by_kind: {} }); // GLOBAL badge counts (unfiltered)
	let tagUniverse = $state<string[]>([]); // GLOBAL tag list (unfiltered) for the tag dropdown
	let total = $state(0); // total rows matching the active filter (across all pages)
	let loading = $state(true);
	let error = $state(false);
	let refetching = $state(false); // a filter/search/page reload in flight (table dims, no skeleton)

	// filters
	let fKind = $state('');
	let fStatus = $state('');
	let fTag = $state('');
	let query = $state('');
	let searchTimer: ReturnType<typeof setTimeout> | undefined;

	// sort
	let sortBy = $state('display_name');
	let sortDir = $state<'asc' | 'desc'>('asc');

	// column header popovers + global search toggle + row kebab menu
	let activePopover = $state<string | null>(null);
	let activeKebab = $state<string | null>(null); // api_id of row with open ⋮ menu
	let searchOpen = $state(false);

	// pagination (server-side)
	let page = $state(1);
	let pageSize = $state(20);
	let totalPages = $derived(Math.max(1, Math.ceil(total / pageSize)));
	let pageFrom = $derived(total === 0 ? 0 : (page - 1) * pageSize + 1);
	let pageTo = $derived(Math.min(page * pageSize, total));

	// selection (api_ids); persists across pages of the same filter, cleared on every refilter.
	// "Select all" toggles the current page; a banner offers selecting every match across pages.
	let selectedIds = $state<string[]>([]);
	let selectedSet = $derived(new Set(selectedIds));
	let pageAllSelected = $derived(items.length > 0 && items.every((s) => selectedSet.has(s.api_id)));
	let pageSomeSelected = $derived(items.some((s) => selectedSet.has(s.api_id)) && !pageAllSelected);

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
	let editConfig = $state<Record<string, string>>({});
	let editConfigLoading = $state(false);
	let editSeq = 0;

	// delete (single)
	let deleteTarget = $state<SourceItem | null>(null);
	let deleting = $state(false);

	// in-flight pause/resume guard, keyed by api_id
	let toggling = $state<Record<string, boolean>>({});

	let kindMap = $derived(new Map(kinds.map((k) => [k.kind, k])));
	// Fields to render in the Edit modal for the current source's kind (same registry the wizard uses).
	const editKind = $derived(editSource ? kindMap.get(editSource.kind) : undefined);

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

	// Build the filter portion of a GET /api/sources query. The BFF allow-lists these params.
	function filterParams(): URLSearchParams {
		const q = new URLSearchParams();
		if (fKind) q.set('kind', fKind);
		if (fStatus) q.set('status', fStatus);
		if (fTag) q.set('tag', fTag);
		if (query.trim()) q.set('q', query.trim());
		if (sortBy !== 'display_name' || sortDir !== 'asc') {
			q.set('sort_by', sortBy);
			q.set('sort_dir', sortDir);
		}
		return q;
	}
	let hasFilter = $derived(!!(fKind || fStatus || fTag || query.trim()));

	function clearFilters() {
		fKind = '';
		fStatus = '';
		fTag = '';
		query = '';
		searchOpen = false;
		activePopover = null;
		applyFilters();
	}

	function setSort(col: string, dir: 'asc' | 'desc') {
		sortBy = col;
		sortDir = dir;
		activePopover = null;
		applyFilters();
	}

	// A monotonic token + AbortController so a slow page request can't overwrite a newer one and a
	// superseded fetch is cancelled (race guard demanded by the filter/page churn).
	let pageLoadSeq = 0;
	let pageLoadCtrl: AbortController | null = null;

	// pageLoad fetches the current server page of rows matching the active filter. The selection is
	// cleared by the callers that change the result set (refilters), not here — page nav keeps it so
	// a multi-page bulk selection survives. Errors keep the prior rows in place.
	async function pageLoad() {
		const seq = ++pageLoadSeq;
		pageLoadCtrl?.abort();
		const ctrl = new AbortController();
		pageLoadCtrl = ctrl;
		refetching = true;
		try {
			const q = filterParams();
			q.set('page', String(page));
			q.set('page_size', String(pageSize));
			const res = await fetch(`/api/sources?${q.toString()}`, { signal: ctrl.signal });
			if (seq !== pageLoadSeq) return; // a newer load superseded this one — drop the stale result
			if (!res.ok) {
				toast('err', t.fontes.error);
				return;
			}
			const data: SourcesResult = await res.json();
			if (seq !== pageLoadSeq) return;
			items = normItems(data?.items);
			total = data?.total ?? items.length;
			// Clamp to the last page if a deletion shrank the result set below the current page.
			const last = Math.max(1, Math.ceil(total / pageSize));
			if (page > last) {
				page = last;
				await pageLoad();
				return;
			}
		} catch (e) {
			if ((e as DOMException)?.name === 'AbortError') return; // superseded — newer load owns state
			toast('err', t.fontes.error);
		} finally {
			if (seq === pageLoadSeq) refetching = false; // a newer load manages its own flag
		}
	}

	// globalLoad keeps the dropdown badges (counts) and the tag filter (tagUniverse) GLOBAL —
	// independent of the active filter. Counts come back global from the surface (computed over the
	// whole set, not the page), but tagUniverse must see every row, so this pages through the full
	// unfiltered dataset accumulating tags (bounded by MAX_PAGES as a runaway guard).
	async function globalLoad() {
		try {
			const tags = new Set<string>();
			let p = 1;
			let totalRows = Infinity;
			let seen = 0;
			const MAX_PAGES = 50; // 50 × GLOBAL_CAP = 10k sources — far past any realistic count
			while (p <= MAX_PAGES) {
				const res = await fetch(`/api/sources?page=${p}&page_size=${GLOBAL_CAP}`);
				if (!res.ok) return;
				const data: SourcesResult = await res.json();
				if (p === 1) {
					counts = normCounts(data?.counts);
					totalRows = data?.total ?? 0;
				}
				const rows = normItems(data?.items);
				for (const s of rows) for (const tag of s.tags) tags.add(tag);
				seen += rows.length;
				if (rows.length === 0 || seen >= totalRows) break;
				p++;
			}
			tagUniverse = [...tags].sort();
		} catch {
			/* badges degrade silently — the table itself uses pageLoad */
		}
	}

	// Refresh both the filtered table and the global badges/tags after a mutation. Keeps the
	// selection (pageLoad clamps the page if a bulk delete shrank the result set).
	async function reloadAll() {
		await Promise.all([pageLoad(), globalLoad()]);
	}

	// A filter change resets to page 1 and clears the selection IMMEDIATELY (not after the fetch), so
	// the bulk bar can't act on ids from the previous filter while the new page loads.
	function resetSelectionForRefilter() {
		selectedIds = [];
		bulkDeleteOpen = false;
	}
	function applyFilters() {
		clearTimeout(searchTimer);
		resetSelectionForRefilter();
		page = 1;
		pageLoad();
	}
	function onSearchInput() {
		clearTimeout(searchTimer);
		resetSelectionForRefilter();
		searchTimer = setTimeout(() => {
			page = 1;
			pageLoad();
		}, 300);
	}

	// ── Pagination nav (page nav keeps the selection across pages) ──
	function goToPage(p: number) {
		const target = Math.min(Math.max(1, p), totalPages);
		if (target === page) return;
		page = target;
		pageLoad();
	}
	function setPageSize(n: number) {
		if (n === pageSize) return;
		pageSize = n;
		page = 1;
		try {
			localStorage.setItem(PAGE_SIZE_KEY, String(n));
		} catch {
			/* private mode — size just won't persist */
		}
		pageLoad();
	}

	onMount(() => {
		pageSize = loadPageSize();
		const ctrl = new AbortController();
		// First load is unfiltered: one page for the table (respecting the saved page size).
		Promise.all([
			fetch('/api/source-kinds', { signal: ctrl.signal }).then((r) => (r.ok ? r.json() : Promise.reject(r.status))),
			fetch(`/api/sources?page=1&page_size=${pageSize}`, { signal: ctrl.signal }).then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
		])
			.then(([k, first]: [SourceKind[], SourcesResult]) => {
				kinds = Array.isArray(k) ? k : [];
				items = normItems(first?.items);
				total = first?.total ?? items.length;
				loading = false;
				globalLoad(); // seed global counts + tag universe (may page through the full set)
			})
			.catch((e) => {
				if (e?.name === 'AbortError') return; // unmounted mid-flight; leave state untouched
				error = true;
				loading = false;
			});
		// Abort pending fetches on unmount so a hung request can't update state after navigation.
		return () => {
			ctrl.abort();
			pageLoadCtrl?.abort();
			clearTimeout(searchTimer);
		};
	});

	// ── Selection ──
	function toggleRow(id: string) {
		selectedIds = selectedSet.has(id) ? selectedIds.filter((x) => x !== id) : [...selectedIds, id];
	}
	// Header checkbox toggles every row on the CURRENT page (others, on other pages, are untouched).
	function togglePage() {
		const pageIds = items.map((s) => s.api_id);
		if (pageAllSelected) {
			const drop = new Set(pageIds);
			selectedIds = selectedIds.filter((id) => !drop.has(id));
		} else {
			selectedIds = [...new Set([...selectedIds, ...pageIds])];
		}
	}
	function clearSelection() {
		selectedIds = [];
	}

	// Select EVERY source matching the active filter across all pages — one fetch of all ids (capped
	// at GLOBAL_CAP, which equals the surface max page). If the filter matches more than the cap, the
	// banner says so honestly and selects the first GLOBAL_CAP.
	let selectingAll = $state(false);
	async function selectAllFilter() {
		if (selectingAll) return;
		selectingAll = true;
		try {
			const q = filterParams();
			q.set('page', '1');
			q.set('page_size', String(GLOBAL_CAP));
			const res = await fetch(`/api/sources?${q.toString()}`);
			if (!res.ok) {
				toast('err', t.fontes.error);
				return;
			}
			const data: SourcesResult = await res.json();
			selectedIds = normItems(data?.items).map((s) => s.api_id);
		} catch {
			toast('err', t.fontes.error);
		} finally {
			selectingAll = false;
		}
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
			selectedIds = []; // the acted-on rows may be gone/changed — drop the stale selection
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

	// ── Edit (display_name + tags + config) ──
	async function openEdit(s: SourceItem) {
		editSource = s;
		editDisplayName = s.display_name;
		editTags = [...s.tags];
		editTagInput = '';
		editError = '';
		editConfig = {};
		// Pre-fill the per-kind config fields (URL/handle/name/title) from the backend.
		editConfigLoading = true;
		const seq = ++editSeq;
		try {
			const res = await fetch(apiPath(s.api_id, '/config'));
			if (seq !== editSeq) return; // stale: a newer openEdit already ran
			if (res.ok) editConfig = await res.json();
		} catch {
			/* leave fields blank; operator can still edit name/tags */
		} finally {
			if (seq === editSeq) editConfigLoading = false;
		}
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
		if (editConfigLoading) return;
		editSaving = true;
		editError = '';
		const payload: { display_name: string; tags?: string[]; config?: Record<string, string> } = {
			display_name: editDisplayName.trim()
		};
		if (supportsTags(editSource.kind)) {
			addEditTag(); // commit any tag still typed in the input but not yet Enter-ed
			payload.tags = editTags;
		}
		// Collect required-field validation + non-empty config (skip email, which has no editable fields here).
		const srcKind = editSource.kind;
		const cfgFields = (editKind?.fields ?? []).filter(
			(f) =>
				f.name !== 'display_name' &&
				!(f.name === 'name' && ['rss', 'html', 'hn'].includes(srcKind))
		);
		if (cfgFields.length > 0) {
			const cfg: Record<string, string> = {};
			for (const f of cfgFields) {
				const v = (editConfig[f.name] ?? '').trim();
				if (f.required && !v) {
					editError = `${f.label}: ${t.fontes.wizardRequired}`;
					editSaving = false;
					return;
				}
				cfg[f.name] = v; // include empty strings so optional fields can be cleared
			}
			payload.config = cfg;
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
		if (e.key !== 'Escape') return;
		if (activeKebab) { activeKebab = null; return; }
		if (activePopover) { activePopover = null; return; }
		if (searchOpen) { searchOpen = false; query = ''; applyFilters(); return; }
		if (mutating) return;
		if (wizardOpen) wizardOpen = false;
		else if (editSource) editSource = null;
		else if (deleteTarget) deleteTarget = null;
		else if (bulkDeleteOpen) bulkDeleteOpen = false;
	}

	function onWindowClick(e: MouseEvent) {
		if (!(e.target instanceof Element)) return;
		const t = e.target;
		if (activeKebab && !t.closest('[data-kebab]')) activeKebab = null;
		if (activePopover && !t.closest('[data-col-popover]')) activePopover = null;
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

<svelte:window onkeydown={closeOnEsc} onclick={onWindowClick} />

<section>
	<div class="mb-5 flex items-center gap-2">
		<button
			class="flex h-[34px] w-[34px] flex-none items-center justify-center rounded-token border border-border text-muted hover:bg-hover {searchOpen ? 'bg-hover' : ''}"
			aria-label={t.fontes.searchPlaceholder}
			onclick={() => { searchOpen = !searchOpen; if (!searchOpen) { query = ''; applyFilters(); } }}
		>
			<svg viewBox="0 0 20 20" width="15" height="15" fill="none" stroke="currentColor" stroke-width="1.7" aria-hidden="true">
				<circle cx="8.5" cy="8.5" r="5.5"/><path d="M13.5 13.5 18 18" stroke-linecap="round"/>
			</svg>
		</button>
		{#if searchOpen}
			<input
				autofocus
				bind:value={query}
				placeholder={t.fontes.searchPlaceholder}
				oninput={onSearchInput}
				class="h-[34px] flex-1 rounded-token border border-border bg-bg px-3 text-[13px] outline-none focus:border-text/40"
			/>
		{/if}
		{#if hasFilter}
			<button class="text-[12px] text-muted hover:text-text" onclick={clearFilters}>{t.fontes.filterClear} ✕</button>
		{/if}
		{#if !loading && !error}
			<button
				class="ml-auto flex-none rounded-token bg-text px-3.5 py-1.5 text-[13px] font-medium text-bg hover:opacity-90"
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

		<!-- Select-all-across-pages: shown once the whole page is ticked and more matches exist. -->
		{#if pageAllSelected && selectedIds.length < total}
			<div class="mb-3 flex flex-wrap items-center gap-2 rounded-token bg-surface-2 px-3 py-2 text-[12px] text-muted">
				<span>{t.fontes.selectAllPageBanner.replace('{n}', String(items.length))}</span>
				<button
					class="font-medium text-text underline underline-offset-2 hover:opacity-80 disabled:opacity-50"
					disabled={selectingAll}
					onclick={selectAllFilter}
				>{selectingAll ? t.fontes.selectingAll : t.fontes.selectAllFilterBtn.replace('{total}', String(total))}</button>
				{#if total > GLOBAL_CAP}<span>{t.fontes.selectAllCapNote.replace('{cap}', String(GLOBAL_CAP))}</span>{/if}
			</div>
		{/if}

		{#if items.length === 0}
			<p class="text-sm text-muted">{hasFilter ? t.fontes.emptyFiltered : t.fontes.empty}</p>
		{:else}
			<datalist id="tag-universe">
				{#each tagUniverse as tag}<option value={tag}></option>{/each}
			</datalist>
			<div class="overflow-x-auto rounded-xl border border-border transition-opacity {refetching ? 'opacity-60' : ''}" aria-busy={refetching}>
						<table class="w-full border-collapse text-[13px]">
							<thead>

						<tr class="border-b border-border bg-surface-2 text-left text-muted">
									<th class="w-9 px-4 py-2.5">
										<input
											type="checkbox"
											checked={pageAllSelected}
											use:indeterminate={pageSomeSelected}
											onchange={togglePage}
											aria-label={t.fontes.selectAllPage}
											class="cursor-pointer align-middle"
										/>
									</th>
									<!-- Nome — sort + text search (q) -->
									<th class="relative px-4 py-2.5 font-medium" data-col-popover>
										<button class="flex items-center gap-1 hover:text-text" aria-haspopup="true" aria-expanded={activePopover === 'name'} aria-controls="popover-name" onclick={(e) => { e.stopPropagation(); activePopover = activePopover === 'name' ? null : 'name'; }}>
											{t.fontes.colName}
											{#if query.trim()}<span class="h-1.5 w-1.5 rounded-full bg-text"></span>{/if}
											<span class="opacity-40">▾</span>
										</button>
										{#if activePopover === 'name'}
											<div id="popover-name" role="menu" class="absolute left-0 top-full z-30 min-w-[220px] rounded-xl border border-border bg-bg p-3 shadow-xl" data-col-popover>
												<div class="mb-2 flex gap-1">
													<button class="flex-1 rounded-token border border-border px-2 py-1 text-[12px] {sortBy==='display_name'&&sortDir==='asc'?'bg-surface-2 font-medium':''} hover:bg-hover" onclick={() => setSort('display_name','asc')}>{t.fontes.colSortAZ}</button>
													<button class="flex-1 rounded-token border border-border px-2 py-1 text-[12px] {sortBy==='display_name'&&sortDir==='desc'?'bg-surface-2 font-medium':''} hover:bg-hover" onclick={() => setSort('display_name','desc')}>{t.fontes.colSortZA}</button>
												</div>
												<input
													bind:value={query}
													placeholder={t.fontes.searchPlaceholder}
													oninput={onSearchInput}
													class="w-full rounded-token border border-border bg-bg px-2 py-1.5 text-[12px] outline-none focus:border-text/40"
												/>
											</div>
										{/if}
									</th>
									<!-- Tipo — sort + kind filter -->
									<th class="relative px-4 py-2.5 font-medium" data-col-popover>
										<button class="flex items-center gap-1 hover:text-text" aria-haspopup="true" aria-expanded={activePopover === 'kind'} aria-controls="popover-kind" onclick={(e) => { e.stopPropagation(); activePopover = activePopover === 'kind' ? null : 'kind'; }}>
											{t.fontes.colKind}
											{#if fKind}<span class="h-1.5 w-1.5 rounded-full bg-text"></span>{/if}
											<span class="opacity-40">▾</span>
										</button>
										{#if activePopover === 'kind'}
											<div id="popover-kind" role="menu" class="absolute left-0 top-full z-30 min-w-[200px] rounded-xl border border-border bg-bg p-3 shadow-xl" data-col-popover>
												<div class="mb-2 flex gap-1">
													<button class="flex-1 rounded-token border border-border px-2 py-1 text-[12px] {sortBy==='kind'&&sortDir==='asc'?'bg-surface-2 font-medium':''} hover:bg-hover" onclick={() => setSort('kind','asc')}>{t.fontes.colSortAZ}</button>
													<button class="flex-1 rounded-token border border-border px-2 py-1 text-[12px] {sortBy==='kind'&&sortDir==='desc'?'bg-surface-2 font-medium':''} hover:bg-hover" onclick={() => setSort('kind','desc')}>{t.fontes.colSortZA}</button>
												</div>
												<div class="flex flex-col gap-0.5">
													<button class="rounded-token px-2 py-1 text-left text-[12px] {!fKind?'font-medium text-text':'text-muted'} hover:bg-hover" onclick={() => { fKind=''; activePopover=null; applyFilters(); }}>{t.fontes.filterAllKinds}</button>
													{#each kinds as k}
														<button class="rounded-token px-2 py-1 text-left text-[12px] {fKind===k.kind?'font-medium text-text':'text-muted'} hover:bg-hover" onclick={() => { fKind=k.kind; activePopover=null; applyFilters(); }}>
															{k.label}{counts.by_kind[k.kind] ? ` (${counts.by_kind[k.kind]})` : ''}
														</button>
													{/each}
												</div>
											</div>
										{/if}
									</th>
									<!-- Status — filter only (só 2 valores, sort irrelevante) -->
									<th class="relative px-4 py-2.5 font-medium" data-col-popover>
										<button class="flex items-center gap-1 hover:text-text" aria-haspopup="true" aria-expanded={activePopover === 'status'} aria-controls="popover-status" onclick={(e) => { e.stopPropagation(); activePopover = activePopover === 'status' ? null : 'status'; }}>
											{t.fontes.colStatus}
											{#if fStatus}<span class="h-1.5 w-1.5 rounded-full bg-text"></span>{/if}
											<span class="opacity-40">▾</span>
										</button>
										{#if activePopover === 'status'}
											<div id="popover-status" role="menu" class="absolute left-0 top-full z-30 min-w-[160px] rounded-xl border border-border bg-bg p-3 shadow-xl" data-col-popover>
												<div class="flex flex-col gap-0.5">
													<button class="rounded-token px-2 py-1 text-left text-[12px] {!fStatus?'font-medium text-text':'text-muted'} hover:bg-hover" onclick={() => { fStatus=''; activePopover=null; applyFilters(); }}>{t.fontes.filterAllStatus}</button>
													<button class="rounded-token px-2 py-1 text-left text-[12px] {fStatus==='active'?'font-medium text-text':'text-muted'} hover:bg-hover" onclick={() => { fStatus='active'; activePopover=null; applyFilters(); }}>
														{t.fontes.statusActive}{counts.by_status.active ? ` (${counts.by_status.active})` : ''}
													</button>
													<button class="rounded-token px-2 py-1 text-left text-[12px] {fStatus==='paused'?'font-medium text-text':'text-muted'} hover:bg-hover" onclick={() => { fStatus='paused'; activePopover=null; applyFilters(); }}>
														{t.fontes.statusPaused}{counts.by_status.paused ? ` (${counts.by_status.paused})` : ''}
													</button>
												</div>
											</div>
										{/if}
									</th>
									<!-- Atualizado — sort only -->
									<th class="relative px-4 py-2.5 font-medium" data-col-popover>
										<button class="flex items-center gap-1 hover:text-text" aria-haspopup="true" aria-expanded={activePopover === 'updated'} aria-controls="popover-updated" onclick={(e) => { e.stopPropagation(); activePopover = activePopover === 'updated' ? null : 'updated'; }}>
											{t.fontes.colUpdated}
											{#if sortBy==='updated_at'}<span class="h-1.5 w-1.5 rounded-full bg-text"></span>{/if}
											<span class="opacity-40">▾</span>
										</button>
										{#if activePopover === 'updated'}
											<div id="popover-updated" role="menu" class="absolute left-0 top-full z-30 min-w-[180px] rounded-xl border border-border bg-bg p-3 shadow-xl" data-col-popover>
												<div class="flex gap-1">
													<button class="flex-1 rounded-token border border-border px-2 py-1 text-[12px] {sortBy==='updated_at'&&sortDir==='desc'?'bg-surface-2 font-medium':''} hover:bg-hover" onclick={() => setSort('updated_at','desc')}>{t.fontes.colSortNewest}</button>
													<button class="flex-1 rounded-token border border-border px-2 py-1 text-[12px] {sortBy==='updated_at'&&sortDir==='asc'?'bg-surface-2 font-medium':''} hover:bg-hover" onclick={() => setSort('updated_at','asc')}>{t.fontes.colSortOldest}</button>
												</div>
											</div>
										{/if}
									</th>
									<!-- Tags — sort + tag filter com datalist (penúltima) -->
									<th class="relative px-4 py-2.5 font-medium" data-col-popover>
										<button class="flex items-center gap-1 hover:text-text" aria-haspopup="true" aria-expanded={activePopover === 'tags'} aria-controls="popover-tags" onclick={(e) => { e.stopPropagation(); activePopover = activePopover === 'tags' ? null : 'tags'; }}>
											{t.fontes.colTags}
											{#if fTag}<span class="h-1.5 w-1.5 rounded-full bg-text"></span>{/if}
											<span class="opacity-40">▾</span>
										</button>
										{#if activePopover === 'tags'}
											<div id="popover-tags" role="menu" class="absolute right-0 top-full z-30 min-w-[200px] rounded-xl border border-border bg-bg p-3 shadow-xl" data-col-popover>
												<div class="mb-2 flex gap-1">
													<button class="flex-1 rounded-token border border-border px-2 py-1 text-[12px] {sortBy==='display_name'&&sortDir==='asc'?'bg-surface-2 font-medium':''} hover:bg-hover" onclick={() => setSort('display_name','asc')}>{t.fontes.colSortAZ}</button>
													<button class="flex-1 rounded-token border border-border px-2 py-1 text-[12px] {sortBy==='display_name'&&sortDir==='desc'?'bg-surface-2 font-medium':''} hover:bg-hover" onclick={() => setSort('display_name','desc')}>{t.fontes.colSortZA}</button>
												</div>
												<input
													bind:value={fTag}
													list="tag-universe"
													placeholder={t.fontes.filterAllTags}
													oninput={() => { resetSelectionForRefilter(); page=1; pageLoad(); }}
													onchange={() => { activePopover=null; applyFilters(); }}
													class="w-full rounded-token border border-border bg-bg px-2 py-1.5 text-[12px] outline-none focus:border-text/40"
												/>
											</div>
										{/if}
									</th>
									<!-- Lane — sort only (última coluna de dados) -->
									<th class="relative px-4 py-2.5 font-medium" data-col-popover>
										<button class="flex items-center gap-1 hover:text-text" aria-haspopup="true" aria-expanded={activePopover === 'lane'} aria-controls="popover-lane" onclick={(e) => { e.stopPropagation(); activePopover = activePopover === 'lane' ? null : 'lane'; }}>
											{t.fontes.colLane}
											<span class="opacity-40">▾</span>
										</button>
										{#if activePopover === 'lane'}
											<div id="popover-lane" role="menu" class="absolute right-0 top-full z-30 min-w-[160px] rounded-xl border border-border bg-bg p-3 shadow-xl" data-col-popover>
												<div class="flex gap-1">
													<button class="flex-1 rounded-token border border-border px-2 py-1 text-[12px] {sortBy==='lane'&&sortDir==='asc'?'bg-surface-2 font-medium':''} hover:bg-hover" onclick={() => setSort('lane','asc')}>{t.fontes.colSortAZ}</button>
													<button class="flex-1 rounded-token border border-border px-2 py-1 text-[12px] {sortBy==='lane'&&sortDir==='desc'?'bg-surface-2 font-medium':''} hover:bg-hover" onclick={() => setSort('lane','desc')}>{t.fontes.colSortZA}</button>
												</div>
											</div>
										{/if}
									</th>
									<!-- ⋮ kebab -->
									<th class="w-10 px-2 py-2.5" scope="col"><span class="sr-only">Ações</span></th>
								</tr>
							</thead>
							<tbody>
								{#each items as s (s.api_id)}
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
										<td class="px-4 py-2.5 whitespace-nowrap">
											<span class="inline-flex items-center gap-1.5 text-muted">
												<span
													class="h-[7px] w-[7px] flex-none rounded-full {s.status === 'active' ? 'bg-green' : 'bg-amber'}"
												></span>
												{s.status === 'active' ? t.fontes.statusActive : t.fontes.statusPaused}
											</span>
										</td>
										<td class="px-4 py-2.5 whitespace-nowrap tabular-nums text-muted">{fmtDate(s.updated_at)}</td>
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
										<td class="px-4 py-2.5 text-muted">{s.lane}</td>
										<td class="w-10 px-2 py-2.5 text-right">
											<div class="relative inline-block" data-kebab>
												<button
													class="flex h-7 w-7 items-center justify-center rounded-token text-muted hover:bg-surface-2"
													aria-label={`Ações: ${s.display_name || s.api_id}`}
													aria-haspopup="menu"
													aria-expanded={activeKebab === s.api_id}
													aria-controls={`kebab-menu-${s.api_id}`}
													onclick={(e) => { e.stopPropagation(); activeKebab = activeKebab === s.api_id ? null : s.api_id; }}
													data-kebab
												>⋮</button>
												{#if activeKebab === s.api_id}
													<div id={`kebab-menu-${s.api_id}`} role="menu" class="absolute right-0 top-full z-30 min-w-[160px] rounded-xl border border-border bg-bg py-1 shadow-xl" data-kebab>
														{#if supportsPause(s.kind)}
															<button
																class="w-full px-3 py-1.5 text-left text-[13px] text-muted hover:bg-hover disabled:opacity-50"
																disabled={toggling[s.api_id]}
																onclick={() => { activeKebab = null; togglePause(s); }}
															>{s.status === 'active' ? t.fontes.actionPause : t.fontes.actionResume}</button>
														{/if}
														<button
															class="w-full px-3 py-1.5 text-left text-[13px] text-muted hover:bg-hover"
															onclick={() => { activeKebab = null; openEdit(s); }}
														>{t.fontes.actionEdit}</button>
														<div class="my-1 border-t border-border"></div>
														<button
															class="w-full px-3 py-1.5 text-left text-[13px] text-red-500 hover:bg-hover"
															onclick={() => { activeKebab = null; deleteTarget = s; }}
														>{t.fontes.actionDelete}</button>
													</div>
												{/if}
											</div>
										</td>
									</tr>
								{/each}
							</tbody>
						</table>
				<div class="flex items-center justify-between px-4 py-2 text-[11px] text-muted">
					<span class="tabular-nums">
						{#if total > PAGE_SIZES[0]}
							{t.fontes.pageRange.replace('{from}', String(pageFrom)).replace('{to}', String(pageTo)).replace('{total}', String(total))}
						{:else}
							{total} {total === 1 ? t.fontes.count : t.fontes.countPlural}
						{/if}
					</span>
				{#if total > PAGE_SIZES[0]}
						<div class="flex items-center gap-3">
							<span class="flex gap-0.5">
								{#each PAGE_SIZES as sz}
									<button
										class="cursor-pointer rounded px-1.5 py-0.5 {pageSize === sz ? 'font-medium text-text' : 'text-muted hover:bg-hover'}"
										aria-pressed={pageSize === sz}
										disabled={refetching}
										onclick={() => setPageSize(sz)}
									>{sz}</button>
								{/each}
							</span>
							<span class="flex items-center gap-1 tabular-nums">
								<button
									class="cursor-pointer rounded px-1 py-0.5 hover:bg-hover disabled:cursor-default disabled:opacity-30"
									disabled={page <= 1 || refetching}
									aria-label={t.fontes.pagePrev}
									onclick={() => goToPage(page - 1)}
								>‹</button>
								<span>{t.fontes.pageOf.replace('{page}', String(page)).replace('{pages}', String(totalPages))}</span>
								<button
									class="cursor-pointer rounded px-1 py-0.5 hover:bg-hover disabled:cursor-default disabled:opacity-30"
									disabled={page >= totalPages || refetching}
									aria-label={t.fontes.pageNext}
									onclick={() => goToPage(page + 1)}
								>›</button>
							</span>
						</div>
					{/if}
				</div>
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
			<!-- form wrapper: Enter submits (#234) -->
			<form onsubmit={(e) => { e.preventDefault(); submitEdit(); }}>
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
									<button type="button" class="opacity-60 hover:opacity-100" aria-label={t.fontes.editTagRemove} onclick={() => removeEditTag(tag)}>×</button>
								</span>
							{/each}
						</div>
					{/if}
					<!-- #239 datalist para sugestão de tags -->
					<input
						bind:value={editTagInput}
						list="tag-universe"
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
				{#if editConfigLoading}
					<p class="text-[12px] text-muted">{t.fontes.editLoading}</p>
				{:else if editKind}
					{#each editKind.fields ?? [] as f (f.name)}
						<!-- #237: 'name' is an immutable DB key for rss/html/hn — only display_name is editable -->
						{#if f.name !== 'display_name' && !(f.name === 'name' && ['rss','html','hn'].includes(editSource.kind))}
							<label class="flex flex-col gap-1 text-[13px]">
								<span class="text-muted">{f.label}{f.required ? ' *' : ''}</span>
								<input
									type={f.type === 'url' ? 'url' : 'text'}
									bind:value={editConfig[f.name]}
									placeholder={f.placeholder ?? ''}
									class="rounded-token border border-border bg-bg px-3 py-1.5 outline-none focus:border-text/40"
								/>
							</label>
						{/if}
					{/each}
				{/if}
				{#if editError}
					<p class="text-[12px] text-red-500">{editError}</p>
				{/if}
			</div>
			<div class="mt-5 flex justify-end gap-2">
				<button type="button" class="rounded-token border border-border px-3 py-1.5 text-[13px] text-muted hover:bg-surface-2 disabled:opacity-50" disabled={editSaving || editConfigLoading} onclick={() => (editSource = null)}>{t.fontes.wizardCancel}</button>
				<button
					type="submit"
					class="rounded-token bg-text px-3.5 py-1.5 text-[13px] font-medium text-bg hover:opacity-90 disabled:opacity-50"
					disabled={editSaving || editConfigLoading}>{editSaving ? t.fontes.editSaving : t.fontes.editSave}</button>
			</div>
			</form>
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
