<script lang="ts">
	import { onMount } from 'svelte';
	import { t } from '$lib/strings';
	import { asList, isProvider, isCatalogEntry, type LLMProvider, type CatalogEntry } from '$lib/inferencia';
	import {
		parseModel,
		resolveModel,
		modelsForKind,
		modelHasInvalidChars,
		registryReady,
		BYO_KIND
	} from '$lib/workerModel';

	type Agent = {
		id: number;
		name: string;
		description: string;
		avatar_url: string;
		visibility: 'workspace' | 'private';
		instructions: string;
		model: string;
		skill_ids?: number[];
	};
	type Skill = { id: number; name: string; trusted: boolean };

	// ── roster ──
	let agents = $state<Agent[]>([]);
	let loading = $state(true);
	let error = $state(false);
	let agentsReqSeq = 0;
	function fetchAgents() {
		const seq = ++agentsReqSeq;
		loading = true;
		error = false;
		return fetch('/api/agents')
			.then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
			.then((d) => {
				if (seq !== agentsReqSeq) return; // superseded by a newer fetch
				agents = asList<Agent>(d);
				loading = false;
			})
			.catch(() => {
				if (seq !== agentsReqSeq) return;
				error = true;
				loading = false;
			});
	}

	// ── skills (for the multi-select) ──
	let skills = $state<Skill[]>([]);
	function fetchSkills() {
		return fetch('/api/skills')
			.then((r) => (r.ok ? r.json() : Promise.reject()))
			.then((d) => (skills = asList<Skill>(d)))
			.catch(() => (skills = []));
	}

	// ── provider+model picker registry (mirrors the workers page) ──
	let providers = $state<LLMProvider[]>([]);
	let catalog = $state<CatalogEntry[]>([]);
	let registryStatus = $state<'loading' | 'ready' | 'failed'>('loading');
	function loadRegistry() {
		registryStatus = 'loading';
		const asJson = (r: Response) => (r.ok ? r.json() : Promise.reject());
		return Promise.all([
			fetch('/api/llm-providers').then(asJson),
			fetch('/api/llm-catalog').then(asJson)
		])
			.then(([provData, catData]) => {
				providers = asList<unknown>(provData).filter(isProvider);
				catalog = asList<unknown>(catData).filter(isCatalogEntry);
				registryStatus = registryReady(providers, catalog) ? 'ready' : 'failed';
			})
			.catch(() => {
				providers = [];
				catalog = [];
				registryStatus = 'failed';
			});
	}

	onMount(() => {
		fetchAgents();
		fetchSkills();
		loadRegistry();
	});

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

	// ── editor (create/edit share one modal) ──
	let formOpen = $state(false);
	let editingId = $state<number | null>(null); // null = create
	let eName = $state('');
	let eDesc = $state('');
	let eAvatar = $state('');
	let eVisibility = $state<'workspace' | 'private'>('workspace');
	let eInstructions = $state('');
	let providerKind = $state('');
	let model = $state('');
	let eSkillIds = $state<number[]>([]);
	let formErrors = $state<Record<string, string>>({});
	let saving = $state(false);

	const isByo = $derived(providerKind === BYO_KIND);
	// Provider options: distinct kinds of enabled providers, plus the current binding's kind if its
	// provider is gone/disabled so an existing binding still shows.
	const providerOptions = $derived.by(() => {
		const seen = new Set<string>();
		const opts: { kind: string; label: string }[] = [];
		for (const p of providers) {
			if (!p.enabled || seen.has(p.kind)) continue;
			seen.add(p.kind);
			opts.push({ kind: p.kind, label: p.name });
		}
		if (providerKind && !seen.has(providerKind)) opts.unshift({ kind: providerKind, label: providerKind });
		return opts;
	});
	const modelOptions = $derived([...new Set([...(model ? [model] : []), ...modelsForKind(catalog, providerKind)])]);

	function openCreate() {
		editingId = null;
		eName = '';
		eDesc = '';
		eAvatar = '';
		eVisibility = 'workspace';
		eInstructions = '';
		providerKind = '';
		model = '';
		eSkillIds = [];
		formErrors = {};
		formOpen = true;
	}
	async function openEdit(a: Agent) {
		editingId = a.id;
		eName = a.name;
		eDesc = a.description;
		eAvatar = a.avatar_url;
		eVisibility = a.visibility;
		eInstructions = a.instructions;
		const parsed = parseModel(a.model);
		providerKind = parsed.kind;
		model = parsed.model;
		eSkillIds = a.skill_ids ?? [];
		formErrors = {};
		formOpen = true;
		// The roster list omits skill_ids — fetch the detail to pre-check the right boxes.
		try {
			const res = await fetch(`/api/agents/${a.id}`);
			if (res.ok) {
				const full = (await res.json()) as Agent;
				if (editingId === a.id) eSkillIds = full.skill_ids ?? [];
			}
		} catch {
			/* keep whatever the roster had */
		}
	}

	function toggleSkill(id: number) {
		eSkillIds = eSkillIds.includes(id) ? eSkillIds.filter((x) => x !== id) : [...eSkillIds, id];
	}

	async function save() {
		const errs: Record<string, string> = {};
		if (!eName.trim()) errs.name = t.agents.errNameRequired;
		if (isByo && modelHasInvalidChars(model)) errs.model = t.agents.modelInvalid;
		formErrors = errs;
		if (Object.keys(errs).length) return;

		saving = true;
		try {
			const res = await fetch('/api/agents', {
				method: 'PUT',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({
					name: eName.trim(),
					description: eDesc,
					avatar_url: eAvatar.trim(),
					visibility: eVisibility,
					instructions: eInstructions,
					model: resolveModel(providerKind, model)
				})
			});
			if (!res.ok) throw new Error();
			const { id } = (await res.json()) as { id: number };
			// Set the skill list on the freshly-upserted agent (the upsert returns its id either way).
			// The two writes aren't atomic: if the agent saved but skills didn't, say so explicitly
			// (the agent exists now) instead of a generic "save failed", and refresh the roster + keep
			// the form open so the operator can retry just the skills.
			const sres = await fetch(`/api/agents/${id}/skills`, {
				method: 'PUT',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ skill_ids: eSkillIds })
			});
			if (!sres.ok) {
				editingId = id; // the agent now exists — a retry edits it, not creates a duplicate
				toast('err', t.agents.skillsSaveError);
				await fetchAgents();
				return;
			}
			toast('ok', t.agents.saveOk);
			formOpen = false;
			await fetchAgents();
		} catch {
			toast('err', t.agents.saveError);
		} finally {
			saving = false;
		}
	}

	// ── delete ──
	let confirmDelete = $state<Agent | null>(null);
	let deleting = $state(false);
	async function doDelete() {
		if (!confirmDelete) return;
		const target = confirmDelete; // capture before await
		deleting = true;
		try {
			const res = await fetch(`/api/agents/${target.id}`, { method: 'DELETE' });
			if (!res.ok) throw new Error();
			toast('ok', t.agents.deleteOk);
			confirmDelete = null;
			if (editingId === target.id) formOpen = false;
			await fetchAgents();
		} catch {
			toast('err', t.agents.deleteError);
		} finally {
			deleting = false;
		}
	}

	function closeOnEsc(e: KeyboardEvent) {
		if (e.key !== 'Escape') return;
		if (confirmDelete) confirmDelete = null;
		else if (formOpen) formOpen = false;
	}

	// Move focus into a dialog when it opens and keep Tab cycling trapped inside it, so keyboard
	// users can't wander into the page behind the modal; restores focus to the opener on close.
	// (Mirrors the fontes page action.)
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

	const fieldClass =
		'w-full rounded-token border border-border bg-bg px-3 py-1.5 text-[13px] text-text placeholder:text-muted focus:border-text focus:outline-none';
	const labelClass = 'block text-[11px] font-semibold uppercase tracking-wide text-muted mb-1';
	const errorClass = 'mt-0.5 text-[11px] text-red-500';
	const skillName = (id: number) => skills.find((s) => s.id === id)?.name ?? `#${id}`;
	// Only render an avatar from an http(s) URL — a stored value could otherwise be a data:/blob:/
	// other-scheme URL the operator pasted. Empty/invalid falls back to the placeholder glyph.
	function safeAvatar(url: string): string {
		try {
			const u = new URL(url);
			return u.protocol === 'http:' || u.protocol === 'https:' ? url : '';
		} catch {
			return '';
		}
	}
</script>

<svelte:window onkeydown={closeOnEsc} />

<section>
	<div class="mb-1 flex items-center gap-2">
		<h2 class="text-[15px] font-semibold">{t.agents.title}</h2>
		<button
			class="ml-auto rounded-token bg-text px-3 py-1.5 text-[13px] font-medium text-bg hover:opacity-90"
			onclick={openCreate}>+ {t.agents.add}</button
		>
	</div>
	<p class="mb-4 text-[12px] text-muted">{t.agents.subtitle}</p>

	{#if loading}
		<p class="text-[13px] text-muted">{t.agents.loading}</p>
	{:else if error}
		<p class="text-[13px] text-red-500">{t.agents.error}</p>
	{:else if agents.length === 0}
		<div class="flex min-h-[40vh] items-center justify-center rounded-xl border border-dashed border-border">
			<p class="text-[13px] text-muted">{t.agents.empty}</p>
		</div>
	{:else}
		<ul class="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
			{#each agents as a (a.id)}
				<li class="flex flex-col gap-2 rounded-xl border border-border bg-surface p-4">
					<div class="flex items-center gap-3">
						{#if safeAvatar(a.avatar_url)}
							<img src={safeAvatar(a.avatar_url)} alt="" class="h-9 w-9 flex-none rounded-full object-cover" />
						{:else}
							<div class="flex h-9 w-9 flex-none items-center justify-center rounded-full bg-surface-2 text-[14px] opacity-40" aria-hidden="true">◆</div>
						{/if}
						<div class="min-w-0 flex-1">
							<p class="truncate text-[13.5px] font-semibold">{a.name || t.agents.untitled}</p>
							<p class="truncate text-[11px] text-muted">
								{a.visibility === 'private' ? t.agents.visibilityPrivate : t.agents.visibilityWorkspace}
								{#if a.model}· <span class="font-mono">{a.model}</span>{/if}
							</p>
						</div>
					</div>
					{#if a.description}
						<p class="line-clamp-2 text-[12px] text-muted">{a.description}</p>
					{/if}
					<div class="mt-auto flex gap-2 pt-1">
						<button class="rounded-token border border-border px-2.5 py-1 text-[12px] text-muted hover:bg-hover" aria-label="{t.agents.edit}: {a.name || t.agents.untitled}" onclick={() => openEdit(a)}>{t.agents.edit}</button>
						<button class="rounded-token border border-border px-2.5 py-1 text-[12px] text-red-500 hover:bg-hover" aria-label="{t.agents.delete}: {a.name || t.agents.untitled}" onclick={() => (confirmDelete = a)}>{t.agents.delete}</button>
					</div>
				</li>
			{/each}
		</ul>
	{/if}
</section>

<!-- ══ create/edit modal ══ -->
{#if formOpen}
	<!-- svelte-ignore a11y_click_events_have_key_events -->
	<div
		role="presentation"
		class="fixed inset-0 z-50 flex items-center justify-center p-4"
		style="background:rgba(0,0,0,0.35)"
		onclick={(e) => {
			if (e.target === e.currentTarget) formOpen = false;
		}}
	>
		<div
			class="max-h-[90vh] w-full max-w-lg overflow-y-auto rounded-xl border border-border bg-bg p-5 shadow-2xl"
			role="dialog"
			aria-modal="true"
			aria-labelledby="agent-form-title"
			use:focusInto
		>
			<h3 id="agent-form-title" class="mb-4 text-[14px] font-semibold">
				{editingId === null ? t.agents.create : t.agents.edit}
			</h3>

			<div class="grid gap-4">
				<div class="grid gap-4 sm:grid-cols-2">
					<div>
						<label class={labelClass} for="a-name">{t.agents.nameLabel}</label>
						<input id="a-name" class={fieldClass} placeholder={t.agents.namePlaceholder} bind:value={eName} autocomplete="off" />
						{#if formErrors.name}<p class={errorClass}>{formErrors.name}</p>{/if}
					</div>
					<div>
						<label class={labelClass} for="a-visibility">{t.agents.visibilityLabel}</label>
						<select id="a-visibility" class={fieldClass} bind:value={eVisibility}>
							<option value="workspace">{t.agents.visibilityWorkspace}</option>
							<option value="private">{t.agents.visibilityPrivate}</option>
						</select>
					</div>
				</div>

				<div>
					<label class={labelClass} for="a-desc">{t.agents.descLabel}</label>
					<input id="a-desc" class={fieldClass} placeholder={t.agents.descPlaceholder} bind:value={eDesc} autocomplete="off" />
				</div>

				<div>
					<label class={labelClass} for="a-avatar">{t.agents.avatarLabel}</label>
					<input id="a-avatar" class={fieldClass} placeholder={t.agents.avatarPlaceholder} bind:value={eAvatar} autocomplete="off" />
				</div>

				<!-- Provider + Model picker (writes "kind/model" into agent.model). -->
				<div class="grid gap-4 sm:grid-cols-2">
					<div>
						<label class={labelClass} for="a-provider">{t.agents.providerLabel}</label>
						{#if registryStatus === 'loading'}
							<p class="mt-0.5 text-[11px] text-muted" role="status">{t.agents.modelLoading}</p>
						{:else if registryStatus === 'failed' && providerOptions.length === 0}
							<p class={errorClass} role="alert">{t.agents.modelLoadFailed}</p>
						{:else}
							<select id="a-provider" class={fieldClass} bind:value={providerKind} onchange={() => (model = '')}>
								<option value="">{t.agents.providerNone}</option>
								{#each providerOptions as opt}
									<option value={opt.kind}>{opt.label}</option>
								{/each}
							</select>
						{/if}
					</div>
					<div>
						<label class={labelClass} for="a-model">{t.agents.modelLabel}</label>
						{#if isByo}
							<input id="a-model" class={fieldClass} placeholder={t.agents.modelManualPlaceholder} bind:value={model} autocomplete="off" aria-invalid={!!formErrors.model} />
						{:else}
							<select id="a-model" class={fieldClass} bind:value={model} disabled={!providerKind}>
								<option value="">{t.agents.modelNone}</option>
								{#each modelOptions as m}
									<option value={m}>{m}</option>
								{/each}
							</select>
						{/if}
						{#if formErrors.model}<p class={errorClass}>{formErrors.model}</p>{/if}
					</div>
				</div>

				<div>
					<label class={labelClass} for="a-instructions">{t.agents.instructionsLabel}</label>
					<textarea id="a-instructions" rows="6" class="{fieldClass} resize-y" placeholder={t.agents.instructionsPlaceholder} bind:value={eInstructions}></textarea>
				</div>

				<!-- Skills multi-select -->
				<div>
					<span class={labelClass}>{t.agents.skillsLabel}</span>
					{#if skills.length === 0}
						<p class="text-[12px] text-muted">{t.agents.skillsEmpty}</p>
					{:else}
						<div class="flex flex-wrap gap-2">
							{#each skills as s (s.id)}
								<label class="flex cursor-pointer items-center gap-1.5 rounded-token border px-2.5 py-1 text-[12px] {eSkillIds.includes(s.id) ? 'border-text bg-surface-2' : 'border-border'}">
									<input type="checkbox" class="h-3.5 w-3.5 accent-green" checked={eSkillIds.includes(s.id)} onchange={() => toggleSkill(s.id)} />
									<span class="font-mono">{skillName(s.id)}</span>
								</label>
							{/each}
						</div>
					{/if}
				</div>

				<div class="mt-1 flex gap-2">
					<button
						class="rounded-token bg-text px-3.5 py-1.5 text-[13px] font-medium text-bg hover:opacity-90 disabled:opacity-50"
						disabled={saving}
						onclick={save}>{saving ? t.agents.saving : t.agents.save}</button
					>
					<button class="rounded-token border border-border px-3.5 py-1.5 text-[13px] text-muted hover:bg-hover" onclick={() => (formOpen = false)}>{t.agents.cancel}</button>
				</div>
			</div>
		</div>
	</div>
{/if}

<!-- ══ delete confirm ══ -->
{#if confirmDelete}
	<!-- svelte-ignore a11y_click_events_have_key_events -->
	<div
		role="presentation"
		class="fixed inset-0 z-50 flex items-center justify-center p-4"
		style="background:rgba(0,0,0,0.35)"
		onclick={(e) => {
			if (e.target === e.currentTarget) confirmDelete = null;
		}}
	>
		<div class="w-full max-w-md rounded-xl border border-border bg-bg p-5 shadow-2xl" role="dialog" aria-modal="true" aria-labelledby="del-title" use:focusInto>
			<h3 id="del-title" class="sr-only">{t.agents.delete}</h3>
			<p class="mb-4 text-[13px] text-text">{t.agents.deleteConfirm.replace('{name}', confirmDelete.name)}</p>
			<div class="flex justify-end gap-2">
				<button class="rounded-token border border-border px-3.5 py-1.5 text-[13px] text-muted hover:bg-hover" onclick={() => (confirmDelete = null)}>{t.agents.cancel}</button>
				<button class="rounded-token bg-red-500 px-3.5 py-1.5 text-[13px] font-medium text-white hover:opacity-90 disabled:opacity-50" disabled={deleting} onclick={doDelete}>{deleting ? t.agents.deleting : t.agents.delete}</button>
			</div>
		</div>
	</div>
{/if}

<!-- ══ toasts ══ -->
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
