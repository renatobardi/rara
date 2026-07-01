<script lang="ts">
	import { t } from '$lib/strings';
	import Paginator from '$lib/Paginator.svelte';

	type Distillation = {
		id: number;
		source_type: string;
		source_ref: string;
		title?: string;
		doc_context?: string;
		engine: string;
		status: string;
	};

	type Agent = { id: number; name: string };

	const STATUS_COLOR: Record<string, string> = {
		done: 'bg-green',
		failed: 'bg-red',
		filtered: 'bg-muted'
	};

	// ── list ──
	let items = $state<Distillation[]>([]);
	let loading = $state(true);
	let error = $state(false);

	$effect(() => {
		fetch('/api/distillations?limit=50')
			.then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
			.then((d) => (items = d))
			.catch(() => (error = true))
			.finally(() => (loading = false));
	});

	// ── selection ──
	let selectedIds = $state<number[]>([]);
	const isSelected = (id: number) => selectedIds.includes(id);
	function toggleRow(id: number) {
		selectedIds = isSelected(id) ? selectedIds.filter((x) => x !== id) : [...selectedIds, id];
	}
	function clearSelection() {
		selectedIds = [];
	}

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

	// ── quick-run modal ──
	let modalOpen = $state(false);
	let agents = $state<Agent[]>([]);
	let agentsLoading = $state(false);
	let agentsError = $state(false);
	let agentsRequestSeq = 0;
	let selectedAgentId = $state<number | null>(null);
	let quickInstruction = $state('');
	let modalErrors = $state<Record<string, string>>({});
	let submitting = $state(false);

	async function openModal() {
		const req = ++agentsRequestSeq;
		modalOpen = true;
		selectedAgentId = null;
		quickInstruction = '';
		modalErrors = {};
		agentsError = false;
		agentsLoading = true;
		try {
			const r = await fetch('/api/agents');
			if (req !== agentsRequestSeq) return;
			agents = r.ok ? await r.json() : [];
			if (!r.ok) agentsError = true;
		} catch {
			if (req === agentsRequestSeq) { agents = []; agentsError = true; }
		} finally {
			if (req === agentsRequestSeq) agentsLoading = false;
		}
	}

	async function submitQuickRun() {
		const errs: Record<string, string> = {};
		if (!selectedAgentId) errs.agent = t.distillations.quickRunAgentRequired;
		if (!quickInstruction.trim()) errs.instruction = t.distillations.quickRunInstructionRequired;
		modalErrors = errs;
		if (Object.keys(errs).length) return;

		const agent = agents.find((a) => a.id === selectedAgentId);
		if (!agent) { modalErrors = { agent: t.distillations.quickRunAgentRequired }; return; }

		submitting = true;
		try {
			const r = await fetch(`/api/agents/${selectedAgentId}/tasks`, {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ instruction: quickInstruction.trim(), context_refs: selectedIds })
			});
			if (!r.ok) {
				toast('err', t.distillations.quickRunError);
				return;
			}
			toast('ok', t.distillations.quickRunOk);
			modalOpen = false;
			clearSelection();
		} catch {
			toast('err', t.distillations.quickRunError);
		} finally {
			submitting = false;
		}
	}

	function closeModal() {
		if (!submitting) modalOpen = false;
	}

	function onWindowKeydown(e: KeyboardEvent) {
		if (e.key === 'Escape') closeModal();
	}

	// Focus trap for the quick-run modal (mirrors agents/+page.svelte pattern).
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
				prev?.focus?.();
			}
		};
	}

	const fieldClass =
		'w-full rounded-token border border-border bg-bg px-3 py-1.5 text-[13px] text-text placeholder:text-muted focus:border-text focus:outline-none';
	const labelClass = 'block text-[11px] font-semibold uppercase tracking-wide text-muted mb-1';
	const errorClass = 'mt-0.5 text-[11px] text-red-500';
</script>

<svelte:window onkeydown={onWindowKeydown} />

{#if loading}
	<p class="text-muted">{t.distillations.loading}</p>
{:else if error}
	<p class="text-red">{t.distillations.error}</p>
{:else if items.length === 0}
	<p class="text-[13px] text-muted">{t.distillations.empty}</p>
{:else}
	<!-- bulk action bar -->
	{#if selectedIds.length > 0}
		<div class="mb-3 flex items-center gap-3 rounded-xl border border-border bg-surface p-3">
			<span class="text-[13px] text-muted">
				{t.distillations.selectedCount.replace('{n}', String(selectedIds.length))}
			</span>
			<button
				class="rounded-token bg-text px-3 py-1.5 text-[12px] font-medium text-bg hover:opacity-90"
				onclick={openModal}
			>{t.distillations.quickRunBtn}</button>
			<button
				class="ml-auto text-[12px] text-muted underline hover:text-text"
				onclick={clearSelection}
			>{t.distillations.clearSelection}</button>
		</div>
	{/if}

	<Paginator {items}>
		{#snippet children(page)}
			<div class="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
				{#each page as d}
					<div
						class="flex flex-col gap-2 rounded-card border bg-surface p-4 hover:bg-hover {isSelected(d.id) ? 'border-text' : 'border-border'}"
					>
						<div class="flex items-start gap-2">
							<input
								type="checkbox"
								class="mt-0.5 h-3.5 w-3.5 flex-none cursor-pointer accent-green"
								checked={isSelected(d.id)}
								onchange={() => toggleRow(d.id)}
								aria-label="{t.distillations.selectRow}: {d.title ?? d.source_ref}"
							/>
							<a
								href="/distillations/{d.id}"
								class="min-w-0 flex-1 no-underline"
							>
								<span class="line-clamp-2 text-[13.5px] font-medium text-text">
									{d.title ?? `${d.source_type} · ${d.source_ref}`}
								</span>
							</a>
							<span
								class="mt-0.5 h-[7px] w-[7px] flex-none rounded-full {STATUS_COLOR[d.status] ?? 'bg-amber'}"
							></span>
						</div>
						{#if d.doc_context}
							<p class="m-0 line-clamp-2 text-[12px] text-muted">{d.doc_context}</p>
						{/if}
						<div class="mt-auto flex items-center gap-2 text-[11px] text-muted">
							<span>{d.engine}</span>
							<span>·</span>
							<span>{d.source_type}</span>
						</div>
					</div>
				{/each}
			</div>
		{/snippet}
	</Paginator>
{/if}

<!-- quick-run modal -->
{#if modalOpen}
	<!-- svelte-ignore a11y_click_events_have_key_events -->
	<div
		role="presentation"
		class="fixed inset-0 z-50 flex items-center justify-center p-4"
		style="background:rgba(0,0,0,0.35)"
		onclick={(e) => {
			if (e.target === e.currentTarget) closeModal();
		}}
	>
		<div
			class="w-full max-w-md rounded-xl border border-border bg-bg p-5 shadow-2xl"
			role="dialog"
			aria-modal="true"
			aria-labelledby="qr-title"
			use:focusInto
		>
			<h3 id="qr-title" class="mb-4 text-[14px] font-semibold">{t.distillations.quickRunTitle}</h3>
			<p class="mb-3 text-[12px] text-muted">
				{t.distillations.selectedCount.replace('{n}', String(selectedIds.length))}
			</p>

			<div class="grid gap-4">
				<div>
					<label class={labelClass} for="qr-agent">{t.distillations.quickRunAgent}</label>
					{#if agentsLoading}
						<p class="text-[12px] text-muted">{t.agents.loading}</p>
					{:else if agentsError}
						<p class="text-[12px] text-red-500">{t.distillations.quickRunAgentsError}</p>
					{:else}
						<select id="qr-agent" class={fieldClass} bind:value={selectedAgentId}>
							<option value={null}>{t.distillations.quickRunAgentPlaceholder}</option>
							{#each agents as a (a.id)}
								<option value={a.id}>{a.name}</option>
							{/each}
						</select>
					{/if}
					{#if modalErrors.agent}<p class={errorClass}>{modalErrors.agent}</p>{/if}
				</div>

				<div>
					<label class={labelClass} for="qr-instruction">{t.distillations.quickRunInstruction}</label>
					<textarea
						id="qr-instruction"
						class="{fieldClass} resize-none"
						rows="2"
						placeholder={t.distillations.quickRunInstructionPlaceholder}
						bind:value={quickInstruction}
						disabled={submitting}
					></textarea>
					{#if modalErrors.instruction}<p class={errorClass}>{modalErrors.instruction}</p>{/if}
				</div>

				<div class="flex gap-2">
					<button
						class="rounded-token bg-text px-3.5 py-1.5 text-[13px] font-medium text-bg hover:opacity-90 disabled:opacity-50"
						disabled={submitting || agentsLoading || agentsError}
						onclick={submitQuickRun}
					>{submitting ? t.distillations.quickRunRunning : t.distillations.quickRunConfirm}</button>
					<button
						class="rounded-token border border-border px-3.5 py-1.5 text-[13px] text-muted hover:bg-hover"
						onclick={closeModal}
						disabled={submitting}
					>{t.agents.cancel}</button>
				</div>
			</div>
		</div>
	</div>
{/if}

<!-- toasts -->
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
