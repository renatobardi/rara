<script lang="ts">
	import { onMount } from 'svelte';
	import { t } from '$lib/strings';
	import {
		asList,
		isProvider,
		maskKey,
		validateBaseUrl,
		isCatalogEntry,
		catalogKinds,
		isSpendDay,
		isSpendProvider,
		formatDay,
		formatUSD,
		spendBars,
		SPEND_PERIODS,
		type LLMProvider,
		type CatalogEntry,
		type LLMSpendDay,
		type LLMSpendProvider,
		type SpendBar
	} from '$lib/inferencia';

	// ── data ──
	let providers = $state<LLMProvider[]>([]);
	let provLoading = $state(true);
	let provError = $state(false);

	// ── litellm catalog: feeds the provider "kind" combobox (distinct litellm providers) ──
	// Best-effort: a failed fetch leaves the list as just ['openai_compatible'] (BYO still works).
	let catalog = $state<CatalogEntry[]>([]);
	let kindOptions = $derived(catalogKinds(catalog));
	function fetchCatalog() {
		return fetch('/api/llm-catalog')
			.then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
			.then((d) => {
				catalog = asList<CatalogEntry>(d).filter(isCatalogEntry);
			})
			.catch(() => {
				catalog = [];
			});
	}

	// Load the provider registry from the BFF into `providers`, toggling loading/error flags.
	function fetchProviders() {
		provLoading = true;
		provError = false;
		return fetch('/api/llm-providers')
			.then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
			.then((d) => {
				providers = asList<LLMProvider>(d).filter(isProvider);
				provLoading = false;
			})
			.catch(() => {
				provError = true;
				provLoading = false;
			});
	}

	// ── Gastos (CORR-INFER-#4): spend charts off the litellm spend log ──
	let spendDays = $state<LLMSpendDay[]>([]);
	let spendProviders = $state<LLMSpendProvider[]>([]);
	let spendLoading = $state(true);
	let spendError = $state(false);
	let spendDaysSel = $state<number | null>(7); // default 7d (mirrors #9)

	const periodLabel: Record<string, string> = {
		spend24h: t.inferencia.spend24h,
		spend7d: t.inferencia.spend7d,
		spend30d: t.inferencia.spend30d,
		spendAll: t.inferencia.spendAll
	};

	// Fetch both spend aggregations for the chosen window; either failing flips the
	// error flag (distinct from "no data"). null days = all-time (no ?days).
	// A monotonic token discards out-of-order responses: a slow earlier window must
	// not overwrite a faster later one (rapid period switches).
	let spendReqSeq = 0;
	async function fetchSpend(days: number | null) {
		const seq = ++spendReqSeq;
		spendLoading = true;
		spendError = false;
		spendDaysSel = days;
		const qs = days !== null ? `?days=${days}` : '';
		try {
			const [tsRes, provRes] = await Promise.all([
				fetch(`/api/llm-spend/timeseries${qs}`),
				fetch(`/api/llm-spend/by-provider${qs}`)
			]);
			if (!tsRes.ok || !provRes.ok) throw new Error();
			const days_ = asList<LLMSpendDay>(await tsRes.json()).filter(isSpendDay);
			const provs_ = asList<LLMSpendProvider>(await provRes.json()).filter(isSpendProvider);
			if (seq !== spendReqSeq) return; // a newer request superseded this one
			spendDays = days_;
			spendProviders = provs_;
			spendLoading = false;
		} catch {
			if (seq !== spendReqSeq) return;
			spendError = true;
			spendLoading = false;
		}
	}

	// Key the daily series by the raw `day` (formatDay drops the year, so the all-time
	// window would otherwise collide same-MM-DD days across years); format at render.
	let overallBars = $derived(spendBars(spendDays.map((d) => ({ label: d.day, value: d.spend }))));
	let providerBars = $derived(spendBars(spendProviders.map((p) => ({ label: p.provider, value: p.spend }))));
	let spendTotal = $derived(spendDays.reduce((s, d) => s + d.spend, 0));
	let hasSpend = $derived(spendDays.length > 0 || spendProviders.length > 0);

	onMount(() => {
		fetchProviders();
		fetchCatalog();
		fetchSpend(spendDaysSel);
	});

	// ── toasts (mirrors workers/+page.svelte) ──
	type Toast = { id: number; kind: 'ok' | 'err'; msg: string };
	let toasts = $state<Toast[]>([]);
	let toastSeq = 0;
	const toastTimers: ReturnType<typeof setTimeout>[] = [];
	// Push a transient toast that auto-dismisses after 4s.
	function toast(kind: 'ok' | 'err', msg: string) {
		const id = ++toastSeq;
		toasts = [...toasts, { id, kind, msg }];
		toastTimers.push(setTimeout(() => (toasts = toasts.filter((x) => x.id !== id)), 4000));
	}
	$effect(() => () => toastTimers.forEach(clearTimeout));

	// ── search + sort (lightweight; registry is small) ──
	// ponytail: client-side filter + single-column sort; popover filters (workers-style) if it grows.
	let provSearch = $state('');
	let provSearchOpen = $state(false);
	let provSortDir = $state<'asc' | 'desc'>('asc');

	let filteredProviders = $derived.by(() => {
		const q = provSearch.trim().toLowerCase();
		const rows = providers.filter((p) => !q || p.name.toLowerCase().includes(q) || p.kind.includes(q));
		const dir = provSortDir === 'asc' ? 1 : -1;
		return [...rows].sort((a, b) => a.name.localeCompare(b.name) * dir);
	});

	// ── kebab (one open at a time) ──
	let activeKebab = $state<string | null>(null);
	// Close the open kebab menu when clicking anywhere outside it.
	function onWindowClick(e: MouseEvent) {
		if (!(e.target instanceof Element)) return;
		if (activeKebab && !e.target.closest('[data-kebab]')) activeKebab = null;
	}
	// Escape unwinds the topmost open layer: kebab → search → form → delete confirm.
	function closeOnEsc(e: KeyboardEvent) {
		if (e.key !== 'Escape') return;
		if (activeKebab) { activeKebab = null; return; }
		if (provSearchOpen) { provSearchOpen = false; provSearch = ''; }
		if (formOpen) closeForm();
		if (confirmDelete) confirmDelete = null;
	}

	// ── form ──
	type FormMode = 'add' | 'edit';
	let formOpen = $state<FormMode | null>(null);
	let formErrors = $state<Record<string, string>>({});
	let formServerError = $state('');
	let submitting = $state(false);

	// provider form fields
	let pName = $state('');
	let pKind = $state('');
	let pBaseUrl = $state('');
	let pApiKey = $state('');
	let pEnabled = $state(true);
	let pEditId = $state<number | null>(null);

	// Fixed kinds have a single canonical endpoint; name = kind, no base_url needed.
	// Configurable kinds allow a free name and accept a custom base_url.
	const CONFIGURABLE_KINDS = ['openai_compatible', 'azure'];
	let isConfigurable = $derived(CONFIGURABLE_KINDS.includes(pKind.trim()));
	// Auto-fill name and clear base_url for fixed providers (add mode only).
	$effect(() => {
		const kind = pKind.trim();
		if (formOpen !== 'add' || !kind) return;
		if (CONFIGURABLE_KINDS.includes(kind)) return;
		pName = kind;
		pBaseUrl = '';
	});

	// Open the form to create a new provider, clearing any prior state.
	function openAddProvider() {
		formOpen = 'add';
		formErrors = {}; formServerError = '';
		pName = ''; pKind = ''; pBaseUrl = ''; pApiKey = ''; pEnabled = true; pEditId = null;
		activeKebab = null;
	}
	// Open the form to edit an existing provider, seeding fields from the row (key stays blank).
	function openEditProvider(p: LLMProvider) {
		formOpen = 'edit';
		formErrors = {}; formServerError = '';
		pName = p.name; pKind = p.kind; pBaseUrl = p.base_url ?? ''; pApiKey = ''; pEnabled = p.enabled;
		pEditId = p.id; activeKebab = null;
	}
	// Close the form and wipe transient state, including any key material in memory.
	function closeForm() {
		formOpen = null; formErrors = {}; formServerError = ''; submitting = false;
		pApiKey = ''; // never leave key material in memory after the form closes
	}

	// Validate the provider form and PUT it to the BFF; toasts + refreshes the list on success.
	async function submitProvider() {
		const e: Record<string, string> = {};
		if (!pName.trim()) e.name = t.inferencia.errNameRequired;
		if (!pKind.trim()) e.kind = t.inferencia.errKindRequired;
		if (!isConfigurable && providers.some((p) => p.kind === pKind.trim() && p.enabled && p.id !== pEditId)) {
			e.kind = t.inferencia.errKindDuplicate;
		}
		const baseErr = validateBaseUrl(pKind, pBaseUrl);
		if (baseErr === 'required') e.baseUrl = t.inferencia.errBaseUrlRequired;
		else if (baseErr === 'invalid') e.baseUrl = t.inferencia.errBaseUrlInvalid;
		else if (baseErr === 'scheme') e.baseUrl = t.inferencia.errBaseUrlScheme;
		else if (baseErr === 'private') e.baseUrl = t.inferencia.errBaseUrlPrivate;
		// api_key is required only when creating; empty on edit = preserve existing.
		if (formOpen === 'add' && !pApiKey.trim()) e.apiKey = t.inferencia.errApiKeyRequired;
		formErrors = e;
		if (Object.keys(e).length) return;

		submitting = true; formServerError = '';
		const body = {
			name: pName.trim(),
			kind: pKind.trim(),
			base_url: pBaseUrl.trim(),
			api_key: pApiKey, // write-only; empty preserves on edit
			enabled: pEnabled
		};
		try {
			const res = await fetch('/api/llm-providers', {
				method: 'PUT',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify(body)
			});
			if (!res.ok) throw new Error(await errorMessage(res));
			toast('ok', t.inferencia.saveOk);
			closeForm();
			await fetchProviders();
		} catch (err) {
			formServerError = err instanceof Error ? err.message : t.inferencia.saveError;
			submitting = false;
		}
	}

	// Extract a human error message from a failed response, falling back to a generic save error.
	async function errorMessage(res: Response): Promise<string> {
		try {
			const b = await res.json();
			if (b?.error) return b.error;
		} catch { /* non-JSON body */ }
		return t.inferencia.saveError;
	}

	// ── toggle enabled (full-record upsert; api_key empty preserves the key) ──
	async function toggleProvider(p: LLMProvider) {
		activeKebab = null;
		try {
			const res = await fetch('/api/llm-providers', {
				method: 'PUT',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ name: p.name, kind: p.kind, base_url: p.base_url ?? '', api_key: '', enabled: !p.enabled })
			});
			if (!res.ok) throw new Error();
			toast('ok', t.inferencia.saveOk);
			await fetchProviders();
		} catch {
			toast('err', t.inferencia.saveError);
		}
	}

	// ── delete (soft) ──
	type DeleteTarget = { id: number; label: string };
	let confirmDelete = $state<DeleteTarget | null>(null);
	let deleting = $state(false);
	// Soft-delete the confirmed provider via the BFF, then refresh the list.
	async function doDelete() {
		if (!confirmDelete) return;
		deleting = true;
		try {
			const res = await fetch(`/api/llm-providers/${confirmDelete.id}`, { method: 'DELETE' });
			if (!res.ok) throw new Error();
			toast('ok', t.inferencia.deleteOk);
			confirmDelete = null;
			await fetchProviders();
		} catch {
			toast('err', t.inferencia.deleteError);
		} finally {
			deleting = false;
		}
	}

	// shared field classes (mirror WorkerForm)
	const fieldClass = 'w-full rounded-token border border-border bg-bg px-3 py-1.5 text-[13px] text-text placeholder:text-muted focus:border-text focus:outline-none';
	const readonlyFieldClass = 'w-full rounded-token border border-border bg-bg px-3 py-1.5 text-[13px] text-text cursor-not-allowed opacity-60';
	const labelClass = 'block text-[11px] font-semibold uppercase tracking-wide text-muted mb-1';
	const errorClass = 'mt-0.5 text-[11px] text-red-500';
</script>

<svelte:window onkeydown={closeOnEsc} onclick={onWindowClick} />

<!-- ══ Providers ══ -->
<section>
	<div class="mb-1 flex items-center gap-2">
		<h2 class="text-[15px] font-semibold">{t.inferencia.providersSection}</h2>
	</div>
	<p class="mb-4 text-[12px] text-muted">{t.inferencia.providersSubtitle}</p>

	<div class="mb-4 flex items-center gap-2">
		<button
			class="flex h-[34px] w-[34px] flex-none items-center justify-center rounded-token border border-border text-muted hover:bg-hover {provSearchOpen ? 'bg-hover' : ''}"
			aria-label={t.inferencia.searchToggle}
			onclick={() => { provSearchOpen = !provSearchOpen; if (!provSearchOpen) provSearch = ''; }}
		>
			<svg viewBox="0 0 20 20" width="15" height="15" fill="none" stroke="currentColor" stroke-width="1.7" aria-hidden="true"><circle cx="8.5" cy="8.5" r="5.5"/><path d="M13.5 13.5 18 18" stroke-linecap="round"/></svg>
		</button>
		{#if provSearchOpen}
			<!-- svelte-ignore a11y_autofocus -->
			<input autofocus bind:value={provSearch} placeholder={t.inferencia.searchPlaceholder} class="h-[34px] flex-1 rounded-token border border-border bg-bg px-3 text-[13px] outline-none focus:border-text/40" />
		{/if}
		{#if provSearch.trim()}
			<button class="text-[12px] text-muted hover:text-text" onclick={() => { provSearch = ''; provSearchOpen = false; }}>{t.inferencia.filterClear}</button>
		{/if}
		{#if !provLoading && !provError}
			<button class="ml-auto flex-none rounded-token bg-text px-3.5 py-1.5 text-[13px] font-medium text-bg hover:opacity-90" onclick={openAddProvider}>+ {t.inferencia.addProvider}</button>
		{/if}
	</div>

	{#if formOpen === 'add'}
		<div class="mb-4">{@render providerForm()}</div>
	{/if}

	{#if provLoading}
		<p class="text-[13px] text-muted">{t.inferencia.providersLoading}</p>
	{:else if provError}
		<p class="text-[13px] text-red-500">{t.inferencia.providersError}</p>
	{:else if providers.length === 0}
		<p class="text-[13px] text-muted">{t.inferencia.providersEmpty}</p>
	{:else if filteredProviders.length === 0}
		<p class="text-[13px] text-muted">{t.inferencia.providersEmptyFiltered}</p>
	{:else}
		<div class="overflow-x-auto rounded-xl border border-border">
			<table class="w-full border-collapse text-[13px]">
				<thead>
					<tr class="border-b border-border bg-surface-2 text-left text-muted">
						<th class="px-4 py-2.5 font-medium">
							<button class="flex items-center gap-1 hover:text-text" onclick={() => (provSortDir = provSortDir === 'asc' ? 'desc' : 'asc')}>
								{t.inferencia.colName}<span class="opacity-40">{provSortDir === 'asc' ? '▲' : '▼'}</span>
							</button>
						</th>
						<th class="px-4 py-2.5 font-medium">{t.inferencia.colKind}</th>
						<th class="px-4 py-2.5 font-medium">{t.inferencia.colBaseUrl}</th>
						<th class="px-4 py-2.5 font-medium">{t.inferencia.colKey}</th>
						<th class="px-4 py-2.5 font-medium">{t.inferencia.colEnabled}</th>
						<th class="w-10 px-2 py-2.5"><span class="sr-only">{t.inferencia.actionsLabel}</span></th>
					</tr>
				</thead>
				<tbody>
					{#each filteredProviders as p (p.id)}
						<tr class="border-b border-border last:border-0 hover:bg-hover">
							<td class="px-4 py-2.5 font-mono text-[12px] font-semibold">{p.name}</td>
							<td class="px-4 py-2.5 text-muted">{p.kind}</td>
							<td class="px-4 py-2.5 font-mono text-[12px] text-muted">{p.base_url || '—'}</td>
							<td class="px-4 py-2.5 font-mono text-[12px] text-muted">{maskKey(p.key_last4)}</td>
							<td class="px-4 py-2.5">
								<span class="inline-flex items-center gap-1.5 text-muted">
									<span class="h-[7px] w-[7px] flex-none rounded-full {p.enabled ? 'bg-green' : 'bg-surface-2 border border-border'}"></span>
									{p.enabled ? t.inferencia.enabledStatus : t.inferencia.disabledStatus}
								</span>
							</td>
							<td class="w-10 px-2 py-2.5 text-right">
								{@render kebab(`prov-${p.id}`, [
									{ label: p.enabled ? t.inferencia.disable : t.inferencia.enable, run: () => toggleProvider(p) },
									{ label: t.inferencia.edit, run: () => openEditProvider(p) },
									{ label: t.inferencia.delete, run: () => { activeKebab = null; confirmDelete = { id: p.id, label: p.name }; }, danger: true }
								])}
							</td>
						</tr>
						{#if formOpen === 'edit' && pEditId === p.id}
							<tr><td colspan="6" class="px-4 py-3">{@render providerForm()}</td></tr>
						{/if}
					{/each}
				</tbody>
			</table>
		</div>
	{/if}
</section>

<!-- ══ Gastos (CORR-INFER-#4) ══ -->
<section class="mt-8">
	<div class="mb-1 flex items-center gap-2">
		<h2 class="text-[15px] font-semibold">{t.inferencia.gastosSection}</h2>
	</div>
	<p class="mb-4 text-[12px] text-muted">{t.inferencia.gastosSubtitle}</p>

	<div class="mb-4 flex items-center gap-1.5">
		{#each SPEND_PERIODS as p (p.key)}
			<button
				class="rounded-token border px-3 py-1 text-[12px] {spendDaysSel === p.days ? 'border-text bg-text text-bg' : 'border-border text-muted hover:bg-hover'}"
				aria-pressed={spendDaysSel === p.days}
				onclick={() => fetchSpend(p.days)}
			>{periodLabel[p.key]}</button>
		{/each}
		{#if !spendLoading && !spendError && hasSpend}
			<span class="ml-auto text-[12px] text-muted">{t.inferencia.spendTotal}: <span class="font-mono font-semibold text-text">{formatUSD(spendTotal)}</span></span>
		{/if}
	</div>

	{#if spendLoading}
		<p class="text-[13px] text-muted">{t.inferencia.spendLoading}</p>
	{:else if spendError}
		<p class="text-[13px] text-red-500">{t.inferencia.spendError}</p>
	{:else if !hasSpend}
		<p class="text-[13px] text-muted">{t.inferencia.spendEmpty}</p>
	{:else}
		<div class="grid grid-cols-1 gap-4 lg:grid-cols-2">
			<div class="rounded-xl border border-border p-4">
				<h3 class="mb-3 text-[13px] font-semibold">{t.inferencia.chartOverallTitle}</h3>
				{@render columnChart(overallBars)}
			</div>
			<div class="rounded-xl border border-border p-4">
				<h3 class="mb-3 text-[13px] font-semibold">{t.inferencia.chartProviderTitle}</h3>
				{@render barChart(providerBars)}
			</div>
		</div>
	{/if}
</section>

<!-- ══ snippets ══ -->
<!-- columnChart: vertical bars (spend per day). ponytail: CSS heights, no chart lib.
     Columns are flex-1 capped at max-w-16 + justify-start so the axis grows left-to-right
     like a timeline — a single day reads as one slim bar, not a full-width block.
     With ≤3 bars the value sits above each (no hover needed); more would clutter. -->
{#snippet columnChart(bars: SpendBar[])}
	{@const showValues = bars.length <= 3}
	<div class="flex h-40 justify-start gap-1">
		{#each bars as b (b.label)}
			<div
				class="flex min-w-0 max-w-16 flex-1 flex-col items-center gap-1"
				title="{formatDay(b.label)}: {formatUSD(b.value)}"
				role="img"
				aria-label="{formatDay(b.label)}: {formatUSD(b.value)}"
			>
				{#if showValues}
					<span class="w-full truncate text-center font-mono text-[10px] text-muted" aria-hidden="true">{formatUSD(b.value)}</span>
				{/if}
				<div class="flex w-full flex-1 items-end">
					<div class="w-full rounded-t bg-text" style="height: {Math.max(b.pct, b.value > 0 ? 2 : 0)}%"></div>
				</div>
				<span class="w-full truncate text-center text-[9px] text-muted" aria-hidden="true">{formatDay(b.label)}</span>
			</div>
		{/each}
	</div>
{/snippet}

<!-- barChart: horizontal bars (spend per provider). -->
{#snippet barChart(bars: SpendBar[])}
	<div class="flex flex-col gap-2">
		{#each bars as b (b.label)}
			<div class="flex items-center gap-2 text-[12px]">
				<span class="w-24 flex-none truncate text-muted" title={b.label}>{b.label}</span>
				<div class="h-4 flex-1 rounded bg-surface-2">
					<div class="h-4 rounded bg-text" style="width: {Math.max(b.pct, b.value > 0 ? 2 : 0)}%"></div>
				</div>
				<span class="w-16 flex-none text-right font-mono text-muted">{formatUSD(b.value)}</span>
			</div>
		{/each}
	</div>
{/snippet}

{#snippet kebab(id: string, actions: { label: string; run: () => void; danger?: boolean }[])}
	<div class="relative inline-block" data-kebab>
		<button
			class="flex h-7 w-7 items-center justify-center rounded-token text-muted hover:bg-surface-2"
			aria-label={t.inferencia.actionsLabel}
			aria-haspopup="menu"
			aria-expanded={activeKebab === id}
			onclick={(e) => { e.stopPropagation(); activeKebab = activeKebab === id ? null : id; }}
			data-kebab
		>⋮</button>
		{#if activeKebab === id}
			<div role="menu" class="absolute right-0 top-full z-30 min-w-[160px] rounded-xl border border-border bg-bg py-1 shadow-xl" data-kebab>
				{#each actions as a}
					<button role="menuitem" class="w-full px-3 py-1.5 text-left text-[13px] hover:bg-hover {a.danger ? 'text-red-500' : 'text-muted'}" onclick={a.run}>{a.label}</button>
				{/each}
			</div>
		{/if}
	</div>
{/snippet}

{#snippet providerForm()}
	<div class="rounded-xl border border-border bg-surface-2 p-5">
		<h3 class="mb-4 text-[14px] font-semibold">{formOpen === 'edit' ? t.inferencia.editProvider : t.inferencia.addProvider}</h3>
		<form onsubmit={(e) => { e.preventDefault(); submitProvider(); }} novalidate>
			<div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
				<div>
					<label class={labelClass} for="p-name">{formOpen === 'edit' ? t.inferencia.formNameReadonly : t.inferencia.formName}</label>
					{#if formOpen === 'edit'}
						<input id="p-name" class={readonlyFieldClass} value={pName} readonly />
					{:else if isConfigurable}
						<input id="p-name" class={fieldClass} placeholder={t.inferencia.formNamePlaceholder} bind:value={pName} autocomplete="off" aria-describedby={formErrors.name ? 'p-name-err' : undefined} aria-invalid={formErrors.name ? 'true' : undefined} />
						{#if formErrors.name}<p id="p-name-err" class={errorClass}>{formErrors.name}</p>{/if}
					{:else}
						<input id="p-name" class={readonlyFieldClass} value={pName} readonly />
					{/if}
				</div>
				<div>
					<label class={labelClass} for="p-kind">{t.inferencia.formKind}</label>
					<input id="p-kind" class={fieldClass} list="p-kinds" maxlength="24" placeholder={t.inferencia.formKindPlaceholder} bind:value={pKind} autocomplete="off" aria-describedby="p-kind-hint{formErrors.kind ? ' p-kind-err' : ''}" aria-invalid={formErrors.kind ? 'true' : undefined} />
					<datalist id="p-kinds">
						{#each kindOptions as k (k)}<option value={k}></option>{/each}
					</datalist>
					<p id="p-kind-hint" class="mt-0.5 text-[11px] text-muted">{t.inferencia.formKindHint}</p>
					{#if formErrors.kind}<p id="p-kind-err" class={errorClass}>{formErrors.kind}</p>{/if}
				</div>
				{#if isConfigurable}
				<div class="sm:col-span-2">
					<label class={labelClass} for="p-baseurl">{t.inferencia.formBaseUrl}</label>
					<input id="p-baseurl" class={fieldClass} placeholder={t.inferencia.formBaseUrlPlaceholder} bind:value={pBaseUrl} autocomplete="off" aria-describedby={formErrors.baseUrl ? 'p-baseurl-err' : undefined} aria-invalid={formErrors.baseUrl ? 'true' : undefined} />
					{#if formErrors.baseUrl}<p id="p-baseurl-err" class={errorClass}>{formErrors.baseUrl}</p>{/if}
				</div>
				{/if}
				<div class="sm:col-span-2">
					<label class={labelClass} for="p-key">{t.inferencia.formApiKey}</label>
					<input id="p-key" type="password" class={fieldClass} placeholder={formOpen === 'edit' ? t.inferencia.formApiKeyPlaceholderEdit : t.inferencia.formApiKeyPlaceholderNew} bind:value={pApiKey} autocomplete="off" />
					<p class="mt-0.5 text-[11px] text-muted">{t.inferencia.formApiKeyHint}</p>
					{#if formErrors.apiKey}<p class={errorClass}>{formErrors.apiKey}</p>{/if}
				</div>
				<label class="flex items-center gap-2 text-[13px]">
					<input type="checkbox" bind:checked={pEnabled} />{t.inferencia.formEnabled}
				</label>
			</div>
			{#if formServerError}<p class="{errorClass} mt-3">{formServerError}</p>{/if}
			<div class="mt-4 flex gap-2">
				<button type="submit" class="rounded-token bg-text px-3.5 py-1.5 text-[13px] font-medium text-bg hover:opacity-90 disabled:opacity-50" disabled={submitting}>{t.inferencia.formSave}</button>
				<button type="button" class="rounded-token border border-border px-3.5 py-1.5 text-[13px] text-muted hover:bg-hover" onclick={closeForm}>{t.inferencia.formCancel}</button>
			</div>
		</form>
	</div>
{/snippet}

<!-- ══ delete confirm ══ -->
{#if confirmDelete}
	<!-- svelte-ignore a11y_click_events_have_key_events -->
	<div role="presentation" class="fixed inset-0 z-50 flex items-center justify-center p-4" style="background:rgba(0,0,0,0.35)" onclick={(e) => { if (e.target === e.currentTarget) confirmDelete = null; }}>
		<div class="w-full max-w-md rounded-xl border border-border bg-bg p-5 shadow-2xl" role="dialog" aria-modal="true" aria-labelledby="delete-confirm-title">
			<h3 id="delete-confirm-title" class="sr-only">{t.inferencia.deleteConfirmBtn}</h3>
			<p class="mb-4 text-[13px] text-text">
				{t.inferencia.deleteProviderConfirm.replace('{name}', confirmDelete.label)}
			</p>
			<div class="flex justify-end gap-2">
				<button class="rounded-token border border-border px-3.5 py-1.5 text-[13px] text-muted hover:bg-hover" onclick={() => (confirmDelete = null)}>{t.inferencia.formCancel}</button>
				<button class="rounded-token bg-red-500 px-3.5 py-1.5 text-[13px] font-medium text-white hover:opacity-90 disabled:opacity-50" disabled={deleting} onclick={doDelete}>{deleting ? t.inferencia.deleting : t.inferencia.deleteConfirmBtn}</button>
			</div>
		</div>
	</div>
{/if}

<!-- ══ toasts ══ -->
{#if toasts.length > 0}
	<div class="fixed bottom-4 right-4 z-[60] flex flex-col gap-2">
		{#each toasts as tst (tst.id)}
			<div class="rounded-token border px-4 py-2 text-[13px] shadow-lg {tst.kind === 'ok' ? 'border-green/40 bg-surface-2 text-text' : 'border-red-500/40 bg-surface-2 text-red-500'}" role={tst.kind === 'ok' ? 'status' : 'alert'}>{tst.msg}</div>
		{/each}
	</div>
{/if}
