<script lang="ts">
	import { onMount } from 'svelte';
	import { t } from '$lib/strings';

	type Skill = {
		id: number;
		name: string;
		description: string;
		content: string;
		config: unknown;
		trusted: boolean;
	};
	type SkillFile = { id: number; skill_id: number; path: string; content: string };

	const asList = <T,>(d: unknown): T[] => (Array.isArray(d) ? (d as T[]) : []);

	// ── data ──
	let skills = $state<Skill[]>([]);
	let loading = $state(true);
	let error = $state(false);

	function fetchSkills() {
		loading = true;
		error = false;
		return fetch('/api/skills')
			.then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
			.then((d) => {
				skills = asList<Skill>(d);
				loading = false;
			})
			.catch(() => {
				error = true;
				loading = false;
			});
	}
	onMount(fetchSkills);

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

	// ── selection / editor ──
	// selected: a skill id, 'new' for the create form, or null for nothing.
	let selected = $state<number | 'new' | null>(null);
	let eName = $state('');
	let eDesc = $state('');
	let eContent = $state('');
	let eTrusted = $state(false);
	let formErrors = $state<Record<string, string>>({});
	let saving = $state(false);

	let current = $derived(typeof selected === 'number' ? skills.find((s) => s.id === selected) : undefined);

	function selectSkill(s: Skill) {
		selected = s.id;
		eName = s.name;
		eDesc = s.description;
		eContent = s.content;
		eTrusted = s.trusted;
		formErrors = {};
		fileFormOpen = false; // don't carry the previous skill's file editor over
		fPath = '';
		fContent = '';
		files = [];
		fetchFiles(s.id);
	}
	function newSkill() {
		selected = 'new';
		eName = '';
		eDesc = '';
		eContent = '';
		eTrusted = false;
		formErrors = {};
		files = [];
	}

	async function saveSkill() {
		const errs: Record<string, string> = {};
		if (!eName.trim()) errs.name = t.skills.errNameRequired;
		formErrors = errs;
		if (Object.keys(errs).length) return;

		saving = true;
		try {
			const res = await fetch('/api/skills', {
				method: 'PUT',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({
					name: eName.trim(),
					description: eDesc,
					content: eContent,
					trusted: eTrusted,
					// Preserve config (e.g. an imported skill's source_url) — the upsert replaces it,
					// so omitting it would wipe provenance.
					config: current?.config ?? {}
				})
			});
			if (!res.ok) throw new Error();
			toast('ok', t.skills.saveOk);
			await fetchSkills();
			// Re-select by name (a freshly created skill gets its id on reload).
			const saved = skills.find((s) => s.name === eName.trim());
			if (saved) selectSkill(saved);
		} catch {
			toast('err', t.skills.saveError);
		} finally {
			saving = false;
		}
	}

	// Trust toggle = full-record upsert with trusted flipped (mirrors the providers enabled toggle).
	// It posts the LIVE editor state (eDesc/eContent), not the stored snapshot, so a pending edit
	// isn't silently reverted when the operator toggles trust. Only invoked for the open skill.
	async function toggleTrust(s: Skill) {
		const next = !eTrusted;
		try {
			const res = await fetch('/api/skills', {
				method: 'PUT',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({
					name: s.name,
					description: eDesc,
					content: eContent,
					trusted: next,
					config: s.config ?? {}
				})
			});
			if (!res.ok) throw new Error();
			toast('ok', t.skills.saveOk);
			eTrusted = next;
			await fetchSkills();
		} catch {
			toast('err', t.skills.saveError);
		}
	}

	// ── delete ──
	let confirmDelete = $state<Skill | null>(null);
	let deleting = $state(false);
	async function doDelete() {
		if (!confirmDelete) return;
		const target = confirmDelete; // capture before any await — the modal state can change
		deleting = true;
		try {
			const res = await fetch(`/api/skills/${target.id}`, { method: 'DELETE' });
			if (!res.ok) throw new Error();
			toast('ok', t.skills.deleteOk);
			if (selected === target.id) selected = null;
			confirmDelete = null;
			await fetchSkills();
		} catch {
			toast('err', t.skills.deleteError);
		} finally {
			deleting = false;
		}
	}

	// ── files ──
	let files = $state<SkillFile[]>([]);
	// A monotonic token discards out-of-order responses: rapidly switching skills must not let a
	// slow earlier fetch overwrite the current skill's files.
	let filesReqSeq = 0;
	function fetchFiles(skillID: number) {
		const seq = ++filesReqSeq;
		return fetch(`/api/skills/${skillID}/files`)
			.then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
			.then((d) => {
				if (seq === filesReqSeq) files = asList<SkillFile>(d);
			})
			.catch(() => {
				if (seq === filesReqSeq) files = [];
			});
	}

	let fileFormOpen = $state(false);
	let fPath = $state('');
	let fContent = $state('');
	let fEditing = $state(false); // true = editing an existing file (path is the key, so it's locked)
	let fErrors = $state<Record<string, string>>({});
	let fSaving = $state(false);

	function openAddFile() {
		fileFormOpen = true;
		fPath = '';
		fContent = '';
		fEditing = false;
		fErrors = {};
	}
	function openEditFile(f: SkillFile) {
		fileFormOpen = true;
		fPath = f.path;
		fContent = f.content;
		fEditing = true;
		fErrors = {};
	}
	async function saveFile() {
		if (typeof selected !== 'number') return;
		const sid = selected; // stable across the await
		const errs: Record<string, string> = {};
		if (!fPath.trim()) errs.path = t.skills.errPathRequired;
		fErrors = errs;
		if (Object.keys(errs).length) return;
		fSaving = true;
		try {
			const res = await fetch(`/api/skills/${sid}/files`, {
				method: 'PUT',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ path: fPath.trim(), content: fContent })
			});
			if (!res.ok) throw new Error(await errMsg(res));
			toast('ok', t.skills.saveOk);
			fileFormOpen = false;
			await fetchFiles(sid);
		} catch (err) {
			toast('err', err instanceof Error && err.message ? err.message : t.skills.saveError);
		} finally {
			fSaving = false;
		}
	}
	// File deletes go through a confirm (a bundle file may hold a script — losing it is real).
	let confirmDeleteFile = $state<SkillFile | null>(null);
	let deletingFile = $state(false);
	async function doDeleteFile() {
		if (!confirmDeleteFile || typeof selected !== 'number') return;
		const sid = selected;
		const target = confirmDeleteFile; // capture before await
		deletingFile = true;
		try {
			const res = await fetch(`/api/skills/${sid}/files`, {
				method: 'DELETE',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ path: target.path })
			});
			if (!res.ok) throw new Error();
			toast('ok', t.skills.deleteOk);
			confirmDeleteFile = null;
			await fetchFiles(sid);
		} catch {
			toast('err', t.skills.deleteError);
		} finally {
			deletingFile = false;
		}
	}

	// ── import ──
	let importOpen = $state(false);
	let importUrl = $state('');
	let importErr = $state('');
	let importing = $state(false);
	async function doImport() {
		if (!importUrl.trim()) {
			importErr = t.skills.errUrlRequired;
			return;
		}
		importing = true;
		importErr = '';
		try {
			const res = await fetch('/api/skills/import', {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ url: importUrl.trim() })
			});
			if (!res.ok) throw new Error(await errMsg(res));
			toast('ok', t.skills.importOk);
			importOpen = false;
			importUrl = '';
			await fetchSkills();
		} catch (err) {
			importErr = err instanceof Error && err.message ? err.message : t.skills.importError;
		} finally {
			importing = false;
		}
	}

	async function errMsg(res: Response): Promise<string> {
		try {
			const b = await res.json();
			if (b?.error) return b.error;
		} catch {
			/* non-JSON body */
		}
		return '';
	}

	function closeOnEsc(e: KeyboardEvent) {
		if (e.key !== 'Escape') return;
		if (importOpen) importOpen = false;
		else if (confirmDelete) confirmDelete = null;
		else if (confirmDeleteFile) confirmDeleteFile = null;
		else if (fileFormOpen) fileFormOpen = false;
	}

	// Move focus into a dialog when it opens (accessibility basic — keyboard users land inside it).
	let importInputEl = $state<HTMLInputElement>();
	let deleteCancelEl = $state<HTMLButtonElement>();
	let deleteFileCancelEl = $state<HTMLButtonElement>();
	$effect(() => {
		if (importOpen) importInputEl?.focus();
	});
	$effect(() => {
		if (confirmDelete) deleteCancelEl?.focus();
	});
	$effect(() => {
		if (confirmDeleteFile) deleteFileCancelEl?.focus();
	});

	const fieldClass =
		'w-full rounded-token border border-border bg-bg px-3 py-1.5 text-[13px] text-text placeholder:text-muted focus:border-text focus:outline-none';
	const readonlyFieldClass =
		'w-full rounded-token border border-border bg-bg px-3 py-1.5 text-[13px] text-text cursor-not-allowed opacity-60';
	const labelClass = 'block text-[11px] font-semibold uppercase tracking-wide text-muted mb-1';
	const errorClass = 'mt-0.5 text-[11px] text-red-500';
	const monoClass = 'font-mono text-[12px] leading-relaxed';
</script>

<svelte:window onkeydown={closeOnEsc} />

<section>
	<div class="mb-1 flex items-center gap-2">
		<h2 class="text-[15px] font-semibold">{t.skills.title}</h2>
	</div>
	<p class="mb-4 text-[12px] text-muted">{t.skills.subtitle}</p>

	<div class="grid gap-5 lg:grid-cols-[280px_1fr]">
		<!-- ══ list ══ -->
		<aside class="flex flex-col gap-2">
			<div class="flex gap-2">
				<button
					class="flex-1 rounded-token bg-text px-3 py-1.5 text-[13px] font-medium text-bg hover:opacity-90"
					onclick={newSkill}>+ {t.skills.add}</button
				>
				<button
					class="rounded-token border border-border px-3 py-1.5 text-[13px] text-muted hover:bg-hover"
					onclick={() => {
						importOpen = true;
						importUrl = '';
						importErr = '';
					}}>{t.skills.import}</button
				>
			</div>

			{#if loading}
				<p class="text-[13px] text-muted">{t.skills.loading}</p>
			{:else if error}
				<p class="text-[13px] text-red-500">{t.skills.error}</p>
			{:else if skills.length === 0}
				<p class="text-[13px] text-muted">{t.skills.empty}</p>
			{:else}
				<ul class="flex flex-col gap-1">
					{#each skills as s (s.id)}
						<li>
							<button
								class="flex w-full items-center gap-2 rounded-token border px-3 py-2 text-left text-[13px] hover:bg-hover {selected ===
								s.id
									? 'border-text bg-surface-2'
									: 'border-border'}"
								onclick={() => selectSkill(s)}
							>
								<span class="flex-1 truncate font-mono text-[12px] font-semibold">{s.name || t.skills.untitled}</span>
								<span
									class="h-[7px] w-[7px] flex-none rounded-full {s.trusted
										? 'bg-green'
										: 'border border-border bg-surface-2'}"
									title={s.trusted ? t.skills.trusted : t.skills.untrusted}
									aria-hidden="true"
								></span>
								<span class="sr-only">{s.trusted ? t.skills.trusted : t.skills.untrusted}</span>
							</button>
						</li>
					{/each}
				</ul>
			{/if}
		</aside>

		<!-- ══ editor ══ -->
		<div>
			{#if selected === null}
				<div class="flex min-h-[40vh] items-center justify-center rounded-xl border border-dashed border-border">
					<p class="text-[13px] text-muted">{t.skills.subtitle}</p>
				</div>
			{:else}
				<div class="rounded-xl border border-border bg-surface-2 p-5">
					<div class="grid gap-4">
						<div class="grid gap-4 sm:grid-cols-2">
							<div>
								<label class={labelClass} for="s-name">{t.skills.nameLabel}</label>
								{#if current}
									<!-- Name is the unique key; renaming would upsert a new row, so it's readonly on edit. -->
									<input id="s-name" class={readonlyFieldClass} value={eName} readonly />
								{:else}
									<input
										id="s-name"
										class={fieldClass}
										placeholder={t.skills.namePlaceholder}
										bind:value={eName}
										autocomplete="off"
									/>
									{#if formErrors.name}<p class={errorClass}>{formErrors.name}</p>{/if}
								{/if}
							</div>
							<div>
								<label class={labelClass} for="s-desc">{t.skills.descLabel}</label>
								<input
									id="s-desc"
									class={fieldClass}
									placeholder={t.skills.descPlaceholder}
									bind:value={eDesc}
									autocomplete="off"
								/>
							</div>
						</div>

						<div>
							<label class={labelClass} for="s-content">{t.skills.contentLabel}</label>
							<textarea
								id="s-content"
								rows="14"
								class="{fieldClass} {monoClass} resize-y"
								placeholder={t.skills.contentPlaceholder}
								bind:value={eContent}
							></textarea>
						</div>

						<!-- trust -->
						<div class="flex items-center gap-3 rounded-token border border-border bg-bg p-3">
							<span
								class="inline-flex items-center gap-1.5 text-[12px] {eTrusted ? 'text-green' : 'text-muted'}"
							>
								<span
									class="h-[7px] w-[7px] flex-none rounded-full {eTrusted
										? 'bg-green'
										: 'border border-border bg-surface-2'}"
								></span>
								{eTrusted ? t.skills.trusted : t.skills.untrusted}
							</span>
							{#if current}
								<button
									class="rounded-token border border-border px-2.5 py-1 text-[12px] text-muted hover:bg-hover"
									onclick={() => current && toggleTrust(current)}
									>{eTrusted ? t.skills.untrustToggle : t.skills.trustToggle}</button
								>
							{/if}
							<span class="flex-1 text-[11px] text-muted">{t.skills.trustHint}</span>
						</div>

						<div class="flex gap-2">
							<button
								class="rounded-token bg-text px-3.5 py-1.5 text-[13px] font-medium text-bg hover:opacity-90 disabled:opacity-50"
								disabled={saving}
								onclick={saveSkill}>{saving ? t.skills.saving : t.skills.save}</button
							>
							{#if current}
								<button
									class="ml-auto rounded-token border border-border px-3.5 py-1.5 text-[13px] text-red-500 hover:bg-hover"
									onclick={() => (confirmDelete = current ?? null)}>{t.skills.delete}</button
								>
							{/if}
						</div>
					</div>

					<!-- ══ files (only for a saved skill) ══ -->
					{#if current}
						<div class="mt-6 border-t border-border pt-5">
							<div class="mb-3 flex items-center gap-2">
								<h3 class="text-[13px] font-semibold">{t.skills.filesTitle}</h3>
								<button
									class="ml-auto rounded-token border border-border px-2.5 py-1 text-[12px] text-muted hover:bg-hover"
									onclick={openAddFile}>+ {t.skills.addFile}</button
								>
							</div>

							{#if files.length === 0}
								<p class="text-[12px] text-muted">{t.skills.filesEmpty}</p>
							{:else}
								<ul class="flex flex-col gap-1">
									{#each files as f (f.id)}
										<li class="flex items-center gap-2 rounded-token border border-border px-3 py-1.5 text-[12px]">
											<span class="flex-1 truncate font-mono">{f.path}</span>
											<button class="text-muted hover:text-text" onclick={() => openEditFile(f)}>{t.skills.save}</button>
											<button class="text-red-500 hover:opacity-80" onclick={() => (confirmDeleteFile = f)}>{t.skills.fileDelete}</button>
										</li>
									{/each}
								</ul>
							{/if}

							{#if fileFormOpen}
								<div class="mt-3 rounded-token border border-border bg-bg p-3">
									<div class="grid gap-3">
										<div>
											<label class={labelClass} for="f-path">{t.skills.filePathLabel}</label>
											{#if fEditing}
												<!-- Path is the key; renaming would upsert a new file and orphan the old one. -->
												<input id="f-path" class={readonlyFieldClass} value={fPath} readonly />
											{:else}
												<input
													id="f-path"
													class={fieldClass}
													placeholder={t.skills.filePathPlaceholder}
													bind:value={fPath}
													autocomplete="off"
												/>
												{#if fErrors.path}<p class={errorClass}>{fErrors.path}</p>{/if}
											{/if}
										</div>
										<div>
											<label class={labelClass} for="f-content">{t.skills.fileContentLabel}</label>
											<textarea id="f-content" rows="6" class="{fieldClass} {monoClass} resize-y" bind:value={fContent}
											></textarea>
										</div>
										<div class="flex gap-2">
											<button
												class="rounded-token bg-text px-3 py-1.5 text-[13px] font-medium text-bg hover:opacity-90 disabled:opacity-50"
												disabled={fSaving}
												onclick={saveFile}>{t.skills.fileSave}</button
											>
											<button
												class="rounded-token border border-border px-3 py-1.5 text-[13px] text-muted hover:bg-hover"
												onclick={() => (fileFormOpen = false)}>{t.skills.cancel}</button
											>
										</div>
									</div>
								</div>
							{/if}
						</div>
					{/if}
				</div>
			{/if}
		</div>
	</div>
</section>

<!-- ══ import dialog ══ -->
{#if importOpen}
	<!-- svelte-ignore a11y_click_events_have_key_events -->
	<div
		role="presentation"
		class="fixed inset-0 z-50 flex items-center justify-center p-4"
		style="background:rgba(0,0,0,0.35)"
		onclick={(e) => {
			if (e.target === e.currentTarget) importOpen = false;
		}}
	>
		<div
			class="w-full max-w-lg rounded-xl border border-border bg-bg p-5 shadow-2xl"
			role="dialog"
			aria-modal="true"
			aria-labelledby="import-title"
		>
			<h3 id="import-title" class="mb-3 text-[14px] font-semibold">{t.skills.importTitle}</h3>
			<label class={labelClass} for="import-url">{t.skills.importUrlLabel}</label>
			<input
				id="import-url"
				class={fieldClass}
				placeholder={t.skills.importUrlPlaceholder}
				bind:value={importUrl}
				bind:this={importInputEl}
				autocomplete="off"
			/>
			{#if importErr}<p class={errorClass}>{importErr}</p>{/if}
			<p class="mt-2 text-[11px] text-muted">{t.skills.importHint}</p>
			<div class="mt-4 flex justify-end gap-2">
				<button
					class="rounded-token border border-border px-3.5 py-1.5 text-[13px] text-muted hover:bg-hover"
					onclick={() => (importOpen = false)}>{t.skills.cancel}</button
				>
				<button
					class="rounded-token bg-text px-3.5 py-1.5 text-[13px] font-medium text-bg hover:opacity-90 disabled:opacity-50"
					disabled={importing}
					onclick={doImport}>{importing ? t.skills.importing : t.skills.importBtn}</button
				>
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
		<div
			class="w-full max-w-md rounded-xl border border-border bg-bg p-5 shadow-2xl"
			role="dialog"
			aria-modal="true"
			aria-labelledby="del-title"
		>
			<h3 id="del-title" class="sr-only">{t.skills.delete}</h3>
			<p class="mb-4 text-[13px] text-text">{t.skills.deleteConfirm.replace('{name}', confirmDelete.name)}</p>
			<div class="flex justify-end gap-2">
				<button
					bind:this={deleteCancelEl}
					class="rounded-token border border-border px-3.5 py-1.5 text-[13px] text-muted hover:bg-hover"
					onclick={() => (confirmDelete = null)}>{t.skills.cancel}</button
				>
				<button
					class="rounded-token bg-red-500 px-3.5 py-1.5 text-[13px] font-medium text-white hover:opacity-90 disabled:opacity-50"
					disabled={deleting}
					onclick={doDelete}>{deleting ? t.skills.deleting : t.skills.delete}</button
				>
			</div>
		</div>
	</div>
{/if}

<!-- ══ delete file confirm ══ -->
{#if confirmDeleteFile}
	<!-- svelte-ignore a11y_click_events_have_key_events -->
	<div
		role="presentation"
		class="fixed inset-0 z-50 flex items-center justify-center p-4"
		style="background:rgba(0,0,0,0.35)"
		onclick={(e) => {
			if (e.target === e.currentTarget) confirmDeleteFile = null;
		}}
	>
		<div
			class="w-full max-w-md rounded-xl border border-border bg-bg p-5 shadow-2xl"
			role="dialog"
			aria-modal="true"
			aria-labelledby="del-file-title"
		>
			<h3 id="del-file-title" class="sr-only">{t.skills.fileDelete}</h3>
			<p class="mb-4 text-[13px] text-text">
				{t.skills.deleteFileConfirm.replace('{path}', confirmDeleteFile.path)}
			</p>
			<div class="flex justify-end gap-2">
				<button
					bind:this={deleteFileCancelEl}
					class="rounded-token border border-border px-3.5 py-1.5 text-[13px] text-muted hover:bg-hover"
					onclick={() => (confirmDeleteFile = null)}>{t.skills.cancel}</button
				>
				<button
					class="rounded-token bg-red-500 px-3.5 py-1.5 text-[13px] font-medium text-white hover:opacity-90 disabled:opacity-50"
					disabled={deletingFile}
					onclick={doDeleteFile}>{deletingFile ? t.skills.deleting : t.skills.fileDelete}</button
				>
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
