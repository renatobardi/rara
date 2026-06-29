<script lang="ts">
	import { onMount } from 'svelte';
	import { t } from '$lib/strings';
	import {
		asList,
		isProvider,
		isModel,
		maskKey,
		validateBaseUrl,
		costPerMillion,
		isSpend,
		indexSpendByModel,
		formatUSD,
		formatTokens,
		SPEND_PERIODS,
		PROVIDER_KINDS,
		type LLMProvider,
		type LLMModel,
		type LLMSpend
	} from '$lib/inferencia';

	// ── data ──
	let providers = $state<LLMProvider[]>([]);
	let models = $state<LLMModel[]>([]);
	let provLoading = $state(true);
	let provError = $state(false);
	let modelLoading = $state(true);
	let modelError = $state(false);

	// ── real cost/tokens (CONSOLE-INFER-#9) ──
	let spend = $state<LLMSpend[]>([]);
	let spendPeriod = $state(2); // index into SPEND_PERIODS; default 30d
	let spendByModel = $derived(indexSpendByModel(spend));

	function fetchSpend() {
		const days = SPEND_PERIODS[spendPeriod].days;
		const url = days === null ? '/api/llm-spend' : `/api/llm-spend?days=${days}`;
		return fetch(url)
			.then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
			.then((d) => {
				spend = asList<LLMSpend>(d).filter(isSpend);
			})
			.catch(() => {
				spend = []; // degrade to "sem dados" — never blocks the registry table
			});
	}

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

	function fetchModels() {
		modelLoading = true;
		modelError = false;
		return fetch('/api/llm-models')
			.then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
			.then((d) => {
				models = asList<LLMModel>(d).filter(isModel);
				modelLoading = false;
			})
			.catch(() => {
				modelError = true;
				modelLoading = false;
			});
	}

	onMount(() => {
		fetchProviders();
		fetchModels();
		fetchSpend();
	});

	function pickSpendPeriod(i: number) {
		spendPeriod = i;
		fetchSpend();
	}

	// ── toasts (mirrors workers/+page.svelte) ──
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

	// ── search + sort (lightweight; registry is small) ──
	// ponytail: client-side filter + single-column sort; popover filters (workers-style) if it grows.
	let provSearch = $state('');
	let provSearchOpen = $state(false);
	let provSortDir = $state<'asc' | 'desc'>('asc');
	let modelSearch = $state('');
	let modelSearchOpen = $state(false);
	let modelSortDir = $state<'asc' | 'desc'>('asc');

	let filteredProviders = $derived.by(() => {
		const q = provSearch.trim().toLowerCase();
		const rows = providers.filter((p) => !q || p.name.toLowerCase().includes(q) || p.kind.includes(q));
		const dir = provSortDir === 'asc' ? 1 : -1;
		return [...rows].sort((a, b) => a.name.localeCompare(b.name) * dir);
	});
	let filteredModels = $derived.by(() => {
		const q = modelSearch.trim().toLowerCase();
		const rows = models.filter(
			(m) => !q || m.alias.toLowerCase().includes(q) || m.upstream.toLowerCase().includes(q)
		);
		const dir = modelSortDir === 'asc' ? 1 : -1;
		return [...rows].sort((a, b) => a.alias.localeCompare(b.alias) * dir);
	});

	// ── kebab (one open at a time) ──
	let activeKebab = $state<string | null>(null);
	function onWindowClick(e: MouseEvent) {
		if (!(e.target instanceof Element)) return;
		if (activeKebab && !e.target.closest('[data-kebab]')) activeKebab = null;
	}
	function closeOnEsc(e: KeyboardEvent) {
		if (e.key !== 'Escape') return;
		if (activeKebab) { activeKebab = null; return; }
		if (provSearchOpen) { provSearchOpen = false; provSearch = ''; }
		if (modelSearchOpen) { modelSearchOpen = false; modelSearch = ''; }
		if (formOpen) closeForm();
		if (confirmDelete) confirmDelete = null;
	}

	// ── forms ──
	type FormTarget = { entity: 'provider' | 'model'; mode: 'add' | 'edit' };
	let formOpen = $state<FormTarget | null>(null);
	let formErrors = $state<Record<string, string>>({});
	let formServerError = $state('');
	let submitting = $state(false);

	// provider form fields
	let pName = $state('');
	let pKind = $state<string>('groq');
	let pBaseUrl = $state('');
	let pApiKey = $state('');
	let pEnabled = $state(true);
	let pEditId = $state<number | null>(null);

	// model form fields
	let mAlias = $state('');
	let mProviderId = $state<number | ''>('');
	let mUpstream = $state('');
	let mCostIn = $state('0');
	let mCostOut = $state('0');
	let mParamsRaw = $state('');
	let mEnabled = $state(true);
	let mEditId = $state<number | null>(null);

	function openAddProvider() {
		formOpen = { entity: 'provider', mode: 'add' };
		formErrors = {}; formServerError = '';
		pName = ''; pKind = 'groq'; pBaseUrl = ''; pApiKey = ''; pEnabled = true; pEditId = null;
		activeKebab = null;
	}
	function openEditProvider(p: LLMProvider) {
		formOpen = { entity: 'provider', mode: 'edit' };
		formErrors = {}; formServerError = '';
		pName = p.name; pKind = p.kind; pBaseUrl = p.base_url ?? ''; pApiKey = ''; pEnabled = p.enabled;
		pEditId = p.id; activeKebab = null;
	}
	function openAddModel() {
		formOpen = { entity: 'model', mode: 'add' };
		formErrors = {}; formServerError = '';
		mAlias = ''; mProviderId = providers[0]?.id ?? ''; mUpstream = '';
		mCostIn = '0'; mCostOut = '0'; mParamsRaw = ''; mEnabled = true; mEditId = null;
		activeKebab = null;
	}
	function openEditModel(m: LLMModel) {
		formOpen = { entity: 'model', mode: 'edit' };
		formErrors = {}; formServerError = '';
		mAlias = m.alias; mProviderId = m.provider_id; mUpstream = m.upstream;
		mCostIn = String(m.input_cost_per_token); mCostOut = String(m.output_cost_per_token);
		mParamsRaw = m.params && Object.keys(m.params as object).length ? JSON.stringify(m.params, null, 2) : '';
		mEnabled = m.enabled; mEditId = m.id; activeKebab = null;
	}
	function closeForm() {
		formOpen = null; formErrors = {}; formServerError = ''; submitting = false;
		pApiKey = ''; // never leave key material in memory after the form closes
	}

	async function submitProvider() {
		const e: Record<string, string> = {};
		if (!pName.trim()) e.name = t.inferencia.errNameRequired;
		const baseErr = validateBaseUrl(pKind, pBaseUrl);
		if (baseErr === 'required') e.baseUrl = t.inferencia.errBaseUrlRequired;
		else if (baseErr === 'invalid') e.baseUrl = t.inferencia.errBaseUrlInvalid;
		else if (baseErr === 'scheme') e.baseUrl = t.inferencia.errBaseUrlScheme;
		else if (baseErr === 'private') e.baseUrl = t.inferencia.errBaseUrlPrivate;
		// api_key is required only when creating; empty on edit = preserve existing.
		if (formOpen?.mode === 'add' && !pApiKey.trim()) e.apiKey = t.inferencia.errApiKeyRequired;
		formErrors = e;
		if (Object.keys(e).length) return;

		submitting = true; formServerError = '';
		const body = {
			name: pName.trim(),
			kind: pKind,
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

	async function submitModel() {
		const e: Record<string, string> = {};
		if (!mAlias.trim()) e.alias = t.inferencia.errAliasRequired;
		if (mProviderId === '' || mProviderId <= 0) e.provider = t.inferencia.errProviderRequired;
		if (!mUpstream.trim()) e.upstream = t.inferencia.errUpstreamRequired;
		const ci = Number(mCostIn);
		const co = Number(mCostOut);
		// Number.isFinite rejects NaN and Infinity (e.g. "1e309" overflows to Infinity).
		if (!Number.isFinite(ci) || ci < 0) e.costIn = t.inferencia.errCostNegative;
		if (!Number.isFinite(co) || co < 0) e.costOut = t.inferencia.errCostNegative;
		let params: unknown = {};
		if (mParamsRaw.trim()) {
			try {
				params = JSON.parse(mParamsRaw.trim());
				if (typeof params !== 'object' || params === null || Array.isArray(params)) throw new Error();
			} catch {
				e.params = t.inferencia.errParamsInvalid;
			}
		}
		formErrors = e;
		if (Object.keys(e).length) return;

		submitting = true; formServerError = '';
		const body = {
			provider_id: Number(mProviderId),
			alias: mAlias.trim(),
			upstream: mUpstream.trim(),
			input_cost_per_token: ci,
			output_cost_per_token: co,
			params,
			enabled: mEnabled
		};
		try {
			const res = await fetch('/api/llm-models', {
				method: 'PUT',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify(body)
			});
			if (!res.ok) throw new Error(await errorMessage(res));
			toast('ok', t.inferencia.saveOk);
			closeForm();
			await fetchModels();
		} catch (err) {
			formServerError = err instanceof Error ? err.message : t.inferencia.saveError;
			submitting = false;
		}
	}

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
	async function toggleModel(m: LLMModel) {
		activeKebab = null;
		try {
			const res = await fetch('/api/llm-models', {
				method: 'PUT',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({
					provider_id: m.provider_id, alias: m.alias, upstream: m.upstream,
					input_cost_per_token: m.input_cost_per_token, output_cost_per_token: m.output_cost_per_token,
					params: m.params ?? {}, enabled: !m.enabled
				})
			});
			if (!res.ok) throw new Error();
			toast('ok', t.inferencia.saveOk);
			await fetchModels();
		} catch {
			toast('err', t.inferencia.saveError);
		}
	}

	// ── delete (soft) ──
	type DeleteTarget = { entity: 'provider' | 'model'; id: number; label: string };
	let confirmDelete = $state<DeleteTarget | null>(null);
	let deleting = $state(false);
	async function doDelete() {
		if (!confirmDelete) return;
		deleting = true;
		const { entity, id } = confirmDelete;
		const url = entity === 'provider' ? `/api/llm-providers/${id}` : `/api/llm-models/${id}`;
		try {
			const res = await fetch(url, { method: 'DELETE' });
			if (!res.ok) throw new Error();
			toast('ok', t.inferencia.deleteOk);
			confirmDelete = null;
			// Deleting a provider can orphan its models' provider_name join — refresh both.
			if (entity === 'provider') await Promise.all([fetchProviders(), fetchModels()]);
			else await fetchModels();
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
<section class="mb-10">
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

	{#if formOpen?.entity === 'provider' && formOpen.mode === 'add'}
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
									{ label: t.inferencia.delete, run: () => { activeKebab = null; confirmDelete = { entity: 'provider', id: p.id, label: p.name }; }, danger: true }
								])}
							</td>
						</tr>
						{#if formOpen?.entity === 'provider' && formOpen.mode === 'edit' && pEditId === p.id}
							<tr><td colspan="6" class="px-4 py-3">{@render providerForm()}</td></tr>
						{/if}
					{/each}
				</tbody>
			</table>
		</div>
	{/if}
</section>

<!-- ══ Models ══ -->
<section>
	<div class="mb-1 flex items-center gap-2">
		<h2 class="text-[15px] font-semibold">{t.inferencia.modelsSection}</h2>
	</div>
	<p class="mb-4 text-[12px] text-muted">{t.inferencia.modelsSubtitle}</p>

	<div class="mb-4 flex items-center gap-2">
		<button
			class="flex h-[34px] w-[34px] flex-none items-center justify-center rounded-token border border-border text-muted hover:bg-hover {modelSearchOpen ? 'bg-hover' : ''}"
			aria-label={t.inferencia.searchToggle}
			onclick={() => { modelSearchOpen = !modelSearchOpen; if (!modelSearchOpen) modelSearch = ''; }}
		>
			<svg viewBox="0 0 20 20" width="15" height="15" fill="none" stroke="currentColor" stroke-width="1.7" aria-hidden="true"><circle cx="8.5" cy="8.5" r="5.5"/><path d="M13.5 13.5 18 18" stroke-linecap="round"/></svg>
		</button>
		{#if modelSearchOpen}
			<!-- svelte-ignore a11y_autofocus -->
			<input autofocus bind:value={modelSearch} placeholder={t.inferencia.searchPlaceholder} class="h-[34px] flex-1 rounded-token border border-border bg-bg px-3 text-[13px] outline-none focus:border-text/40" />
		{/if}
		{#if modelSearch.trim()}
			<button class="text-[12px] text-muted hover:text-text" onclick={() => { modelSearch = ''; modelSearchOpen = false; }}>{t.inferencia.filterClear}</button>
		{/if}
		{#if !modelLoading && !modelError && providers.length > 0}
			<button class="ml-auto flex-none rounded-token bg-text px-3.5 py-1.5 text-[13px] font-medium text-bg hover:opacity-90" onclick={openAddModel}>+ {t.inferencia.addModel}</button>
		{/if}
	</div>

	<!-- real cost/tokens period selector (CONSOLE-INFER-#9) -->
	<div class="mb-4 flex items-center gap-2 text-[12px]">
		<span class="text-muted">{t.inferencia.spendPeriodLabel}</span>
		<div class="inline-flex overflow-hidden rounded-token border border-border" role="group" aria-label={t.inferencia.spendPeriodLabel}>
			{#each SPEND_PERIODS as p, i (p.key)}
				<button
					class="px-2.5 py-1 {spendPeriod === i ? 'bg-text text-bg' : 'text-muted hover:bg-hover'}"
					aria-pressed={spendPeriod === i}
					onclick={() => pickSpendPeriod(i)}
				>{t.inferencia[p.key as 'spend24h' | 'spend7d' | 'spend30d' | 'spendAll']}</button>
			{/each}
		</div>
	</div>

	{#if formOpen?.entity === 'model' && formOpen.mode === 'add'}
		<div class="mb-4">{@render modelForm()}</div>
	{/if}

	{#if modelLoading}
		<p class="text-[13px] text-muted">{t.inferencia.modelsLoading}</p>
	{:else if modelError}
		<p class="text-[13px] text-red-500">{t.inferencia.modelsError}</p>
	{:else if models.length === 0}
		<p class="text-[13px] text-muted">{providers.length === 0 ? t.inferencia.noProviders : t.inferencia.modelsEmpty}</p>
	{:else if filteredModels.length === 0}
		<p class="text-[13px] text-muted">{t.inferencia.modelsEmptyFiltered}</p>
	{:else}
		<div class="overflow-x-auto rounded-xl border border-border">
			<table class="w-full border-collapse text-[13px]">
				<thead>
					<tr class="border-b border-border bg-surface-2 text-left text-muted">
						<th class="px-4 py-2.5 font-medium">
							<button class="flex items-center gap-1 hover:text-text" onclick={() => (modelSortDir = modelSortDir === 'asc' ? 'desc' : 'asc')}>
								{t.inferencia.colAlias}<span class="opacity-40">{modelSortDir === 'asc' ? '▲' : '▼'}</span>
							</button>
						</th>
						<th class="px-4 py-2.5 font-medium">{t.inferencia.colProvider}</th>
						<th class="px-4 py-2.5 font-medium">{t.inferencia.colUpstream}</th>
						<th class="px-4 py-2.5 font-medium">{t.inferencia.colCostIn}</th>
						<th class="px-4 py-2.5 font-medium">{t.inferencia.colCostOut}</th>
						<th class="px-4 py-2.5 font-medium">{t.inferencia.colSpend}</th>
						<th class="px-4 py-2.5 font-medium">{t.inferencia.colTokens}</th>
						<th class="px-4 py-2.5 font-medium">{t.inferencia.colEnabled}</th>
						<th class="w-10 px-2 py-2.5"><span class="sr-only">{t.inferencia.actionsLabel}</span></th>
					</tr>
				</thead>
				<tbody>
					{#each filteredModels as m (m.id)}
						<tr class="border-b border-border last:border-0 hover:bg-hover">
							<td class="px-4 py-2.5 font-mono text-[12px] font-semibold">{m.alias}</td>
							<td class="px-4 py-2.5 text-muted">{m.provider_name || m.provider_id}</td>
							<td class="px-4 py-2.5 font-mono text-[12px] text-muted">{m.upstream}</td>
							<td class="whitespace-nowrap px-4 py-2.5 tabular-nums text-muted">{costPerMillion(m.input_cost_per_token)}</td>
							<td class="whitespace-nowrap px-4 py-2.5 tabular-nums text-muted">{costPerMillion(m.output_cost_per_token)}</td>
							<td class="whitespace-nowrap px-4 py-2.5 tabular-nums">{formatUSD(spendByModel.get(m.alias)?.spend ?? 0)}</td>
							<td class="whitespace-nowrap px-4 py-2.5 tabular-nums text-muted">{formatTokens(spendByModel.get(m.alias)?.total_tokens ?? 0)}</td>
							<td class="px-4 py-2.5">
								<span class="inline-flex items-center gap-1.5 text-muted">
									<span class="h-[7px] w-[7px] flex-none rounded-full {m.enabled ? 'bg-green' : 'bg-surface-2 border border-border'}"></span>
									{m.enabled ? t.inferencia.enabledStatus : t.inferencia.disabledStatus}
								</span>
							</td>
							<td class="w-10 px-2 py-2.5 text-right">
								{@render kebab(`model-${m.id}`, [
									{ label: m.enabled ? t.inferencia.disable : t.inferencia.enable, run: () => toggleModel(m) },
									{ label: t.inferencia.edit, run: () => openEditModel(m) },
									{ label: t.inferencia.delete, run: () => { activeKebab = null; confirmDelete = { entity: 'model', id: m.id, label: m.alias }; }, danger: true }
								])}
							</td>
						</tr>
						{#if formOpen?.entity === 'model' && formOpen.mode === 'edit' && mEditId === m.id}
							<tr><td colspan="9" class="px-4 py-3">{@render modelForm()}</td></tr>
						{/if}
					{/each}
				</tbody>
			</table>
		</div>
	{/if}
</section>

<!-- ══ snippets ══ -->
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
		<h3 class="mb-4 text-[14px] font-semibold">{formOpen?.mode === 'edit' ? t.inferencia.editProvider : t.inferencia.addProvider}</h3>
		<form onsubmit={(e) => { e.preventDefault(); submitProvider(); }} novalidate>
			<div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
				<div>
					<label class={labelClass} for="p-name">{formOpen?.mode === 'edit' ? t.inferencia.formNameReadonly : t.inferencia.formName}</label>
					{#if formOpen?.mode === 'edit'}
						<input id="p-name" class={readonlyFieldClass} value={pName} readonly />
					{:else}
						<input id="p-name" class={fieldClass} placeholder={t.inferencia.formNamePlaceholder} bind:value={pName} autocomplete="off" />
						{#if formErrors.name}<p class={errorClass}>{formErrors.name}</p>{/if}
					{/if}
				</div>
				<div>
					<label class={labelClass} for="p-kind">{t.inferencia.formKind}</label>
					<select id="p-kind" class={fieldClass} bind:value={pKind}>
						{#each PROVIDER_KINDS as k}<option value={k}>{k}</option>{/each}
					</select>
				</div>
				<div class="sm:col-span-2">
					<label class={labelClass} for="p-baseurl">{t.inferencia.formBaseUrl}</label>
					<input id="p-baseurl" class={fieldClass} placeholder={t.inferencia.formBaseUrlPlaceholder} bind:value={pBaseUrl} autocomplete="off" />
					{#if formErrors.baseUrl}<p class={errorClass}>{formErrors.baseUrl}</p>{/if}
				</div>
				<div class="sm:col-span-2">
					<label class={labelClass} for="p-key">{t.inferencia.formApiKey}</label>
					<input id="p-key" type="password" class={fieldClass} placeholder={formOpen?.mode === 'edit' ? t.inferencia.formApiKeyPlaceholderEdit : t.inferencia.formApiKeyPlaceholderNew} bind:value={pApiKey} autocomplete="off" />
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

{#snippet modelForm()}
	<div class="rounded-xl border border-border bg-surface-2 p-5">
		<h3 class="mb-4 text-[14px] font-semibold">{formOpen?.mode === 'edit' ? t.inferencia.editModel : t.inferencia.addModel}</h3>
		<form onsubmit={(e) => { e.preventDefault(); submitModel(); }} novalidate>
			<div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
				<div>
					<label class={labelClass} for="m-alias">{formOpen?.mode === 'edit' ? t.inferencia.formAliasReadonly : t.inferencia.formAlias}</label>
					{#if formOpen?.mode === 'edit'}
						<input id="m-alias" class={readonlyFieldClass} value={mAlias} readonly />
					{:else}
						<input id="m-alias" class={fieldClass} placeholder={t.inferencia.formAliasPlaceholder} bind:value={mAlias} autocomplete="off" />
						{#if formErrors.alias}<p class={errorClass}>{formErrors.alias}</p>{/if}
					{/if}
				</div>
				<div>
					<label class={labelClass} for="m-provider">{t.inferencia.formProvider}</label>
					<select id="m-provider" class={fieldClass} bind:value={mProviderId}>
						<option value="" disabled>{t.inferencia.formProviderPlaceholder}</option>
						{#each providers as p}<option value={p.id}>{p.name}</option>{/each}
					</select>
					{#if formErrors.provider}<p class={errorClass}>{formErrors.provider}</p>{/if}
				</div>
				<div class="sm:col-span-2">
					<label class={labelClass} for="m-upstream">{t.inferencia.formUpstream}</label>
					<input id="m-upstream" class={fieldClass} placeholder={t.inferencia.formUpstreamPlaceholder} bind:value={mUpstream} autocomplete="off" />
					{#if formErrors.upstream}<p class={errorClass}>{formErrors.upstream}</p>{/if}
				</div>
				<div>
					<label class={labelClass} for="m-costin">{t.inferencia.formCostIn}</label>
					<input id="m-costin" type="number" step="any" min="0" class={fieldClass} bind:value={mCostIn} />
					{#if formErrors.costIn}<p class={errorClass}>{formErrors.costIn}</p>{/if}
				</div>
				<div>
					<label class={labelClass} for="m-costout">{t.inferencia.formCostOut}</label>
					<input id="m-costout" type="number" step="any" min="0" class={fieldClass} bind:value={mCostOut} />
					{#if formErrors.costOut}<p class={errorClass}>{formErrors.costOut}</p>{/if}
				</div>
				<div class="sm:col-span-2">
					<label class={labelClass} for="m-params">{t.inferencia.formParams}</label>
					<textarea id="m-params" rows="3" class="{fieldClass} font-mono" placeholder={t.inferencia.formParamsPlaceholder} bind:value={mParamsRaw}></textarea>
					{#if formErrors.params}<p class={errorClass}>{formErrors.params}</p>{/if}
				</div>
				<label class="flex items-center gap-2 text-[13px]">
					<input type="checkbox" bind:checked={mEnabled} />{t.inferencia.formEnabled}
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
				{(confirmDelete.entity === 'provider' ? t.inferencia.deleteProviderConfirm.replace('{name}', confirmDelete.label) : t.inferencia.deleteModelConfirm.replace('{alias}', confirmDelete.label))}
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
