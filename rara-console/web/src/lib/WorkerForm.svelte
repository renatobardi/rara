<script lang="ts">
	import { untrack } from 'svelte';
	import { t } from '$lib/strings';
	import type { LLMModel } from '$lib/inferencia';
	import { enabledAliases, currentAlias, envWithoutModel, withModelAlias, usesModel, blocksOnModelLoadFailure } from '$lib/workerModel';

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
		last_error?: string;
		constraints?: Constraints;
		runner_url?: string;
		env?: Record<string, string>;
		description?: string;
	};

	type Props = {
		initial?: Provider | null;
		capabilities: string[];
		/** Enabled LLM models for the Model dropdown (from /api/llm-models). */
		models?: LLMModel[];
		/** Whether the model registry is usable: 'loading' (fetch in flight), 'ready' (≥1 model),
		 * or 'failed' (fetch error, malformed body, or empty registry). Required (no default): an
		 * LLM worker must not save modelless unless this is 'ready', and the form distinguishes a
		 * still-loading hint from a "failed — reload" error so the in-flight window isn't a false
		 * alarm. */
		modelsStatus: 'loading' | 'ready' | 'failed';
		/** Pre-fill worker and make it read-only (add-placement mode). */
		lockedWorker?: string;
		/** Pre-fill capability and make it read-only (add-placement mode). */
		lockedCapability?: string;
		/** Constraints of the worker being extended (add-placement mode — restricts runtime options). */
		lockedConstraints?: Constraints | null;
		/** App binary inherited from sibling placements (add-placement mode — sent as-is, not shown). */
		lockedApp: string | null;
		onSave: (p: Provider) => Promise<void>;
		onCancel: () => void;
	};

	let {
		initial = null,
		capabilities,
		models = [],
		modelsStatus,
		lockedWorker,
		lockedCapability,
		lockedConstraints = null,
		lockedApp = null,
		onSave,
		onCancel
	}: Props = $props();

	const isEdit = $derived(initial !== null);
	// worker is read-only when editing (can't regroup) or when adding placement to an existing worker
	const workerReadonly = $derived(isEdit || !!lockedWorker);
	// capability and runtime are identity fields — read-only in edit mode
	const capabilityReadonly = $derived(!!lockedCapability || isEdit);
	const runtimeReadonly = $derived(isEdit);
	// allowed runtimes: restricted to ['local'] when worker requires residential IP
	const VALID_RUNTIMES = ['local', 'cloudrun', 'vpc'];
	const allowedRuntimes = $derived(
		lockedConstraints?.requires === 'residential' ? ['local'] : VALID_RUNTIMES
	);

	// untrack: form mounts fresh for each add/edit open — capturing initial value once is correct
	let worker = $state(untrack(() => lockedWorker ?? initial?.worker ?? ''));
	let name = $state(untrack(() => initial?.name ?? ''));
	let capability = $state(untrack(() => lockedCapability ?? initial?.capability ?? ''));
	let runtime = $state(untrack(() => initial?.runtime ?? 'local'));
	let activation = $state(untrack(() => initial?.activation ?? 'on_demand'));
	let enabled = $state(untrack(() => initial?.enabled ?? true));
	let runnerUrl = $state(untrack(() => initial?.runner_url ?? ''));
	// LITELLM_MODEL is owned by the dropdown; the raw env editor shows everything else.
	let model = $state(untrack(() => currentAlias(initial?.env)));
	let envRaw = $state(
		untrack(() => {
			const rest = envWithoutModel(initial?.env);
			return Object.keys(rest).length ? JSON.stringify(rest, null, 2) : '';
		})
	);

	// Dropdown options: enabled aliases, plus the current one if it's stale/disabled
	// so an existing binding still shows instead of silently blanking.
	const modelOptions = $derived(
		[...new Set([...(model ? [model] : []), ...enabledAliases(models)])]
	);
	// Show only for LLM-capable workers AND when there's actually something to pick —
	// if /api/llm-models fails/returns empty (and there's no existing binding to keep),
	// modelOptions is empty and the field degrades to hidden. Reactive on capability
	// (editable in add mode). Optional — never required.
	const showModel = $derived(usesModel(capability, initial?.env) && modelOptions.length > 0);
	// If the worker needs a Model but the registry isn't ready (loading, failed, or empty) and
	// there's no binding to keep, block the save instead of silently writing no model.
	const modelBlocked = $derived(blocksOnModelLoadFailure(capability, initial?.env, modelsStatus !== 'ready'));

	// Clear a stale selection if capability switches away from LLM, so submit never
	// writes LITELLM_MODEL onto a non-LLM worker.
	$effect(() => {
		if (!showModel) model = '';
	});

	// constraints fields
	let cRequires = $state(untrack(() => initial?.constraints?.requires ?? ''));
	let cAccepts = $state(untrack(() => initial?.constraints?.accepts?.join(', ') ?? ''));
	let cSensitivity = $state(untrack(() => initial?.constraints?.sensitivity ?? ''));

	// track whether user has manually edited the name field (stops auto-suggestion)
	let nameEdited = $state(untrack(() => isEdit || !!initial?.name));

	// auto-suggest name = <worker>-<runtime> while user hasn't touched the name field
	$effect(() => {
		if (!isEdit && !nameEdited && worker.trim() && runtime) {
			name = `${worker.trim()}-${runtime}`;
		}
	});

	// validation errors
	let errors = $state<Record<string, string>>({});
	let submitting = $state(false);
	let serverError = $state('');

	const formTitle = $derived(
		isEdit
			? t.workers.editWorker
			: lockedWorker
				? t.workers.formTitleAddPlacement
				: t.workers.addWorker
	);

	const VALID_ACTIVATIONS = ['resident', 'on_demand'];
	// ponytail: loopback only — private ranges (10.x, 172.16-31.x, 192.168.x, 100.x tailnet)
	// are intentionally allowed; rara-runner lives on those networks by design.
	const LOOPBACK = /^(localhost|127\.\d+\.\d+\.\d+|::1)$/i;

	function validate(): boolean {
		const e: Record<string, string> = {};
		if (!worker.trim()) e.worker = t.workers.formWorkerRequired;
		if (!name.trim()) e.name = t.workers.formNameRequired;
		if (!capability.trim()) e.capability = t.workers.formCapabilityRequired;
		if (!VALID_RUNTIMES.includes(runtime)) e.runtime = t.workers.formRuntimeInvalid;
		else if (!allowedRuntimes.includes(runtime)) e.runtime = t.workers.formRuntimeConstraint;
		if (!VALID_ACTIVATIONS.includes(activation)) e.activation = t.workers.formActivationInvalid;
		if (runtime !== 'cloudrun' && runnerUrl.trim()) {
			try {
				const u = new URL(runnerUrl.trim());
				if (!['http:', 'https:'].includes(u.protocol)) {
					e.runnerUrl = t.workers.formRunnerUrlInvalidScheme;
				} else if (LOOPBACK.test(u.hostname)) {
					e.runnerUrl = t.workers.formRunnerUrlInvalidHost;
				}
			} catch {
				e.runnerUrl = t.workers.formRunnerUrlInvalidFormat;
			}
		}
		if (envRaw.trim()) {
			try {
				const parsed = JSON.parse(envRaw.trim());
				if (typeof parsed !== 'object' || Array.isArray(parsed) || parsed === null) {
					e.env = t.workers.formEnvInvalid;
				} else if (!Object.values(parsed).every((v) => typeof v === 'string')) {
					e.env = t.workers.formEnvValuesInvalid;
				}
			} catch {
				e.env = t.workers.formEnvInvalid;
			}
		}
		if (modelBlocked) {
			e.model = modelsStatus === 'loading' ? t.workers.formModelLoading : t.workers.formModelLoadFailed;
		}
		errors = e;
		return Object.keys(e).length === 0;
	}

	function buildConstraints(): Constraints | undefined {
		const c: Constraints = {};
		if (cRequires) c.requires = cRequires;
		if (cAccepts.trim()) {
			c.accepts = cAccepts
				.split(',')
				.map((s) => s.trim())
				.filter(Boolean);
		}
		if (cSensitivity) c.sensitivity = cSensitivity;
		return Object.keys(c).length > 0 ? c : undefined;
	}

	async function handleSubmit() {
		if (!validate()) return;
		submitting = true;
		serverError = '';

		// Inherit app: locked value (add-placement) takes precedence, then existing record (edit)
		const appValue = lockedApp != null ? lockedApp : initial?.app;
		const appField = appValue ? { app: appValue } : {};

		const payload: Provider = {
			worker: worker.trim(),
			name: name.trim(),
			...appField,
			capability: capability.trim(),
			runtime,
			activation,
			enabled,
			// preserve description on edit (not editable in form; upsert would clear it otherwise)
			...(initial?.description ? { description: initial.description } : {})
		};

		if (runtime !== 'cloudrun' && runnerUrl.trim()) payload.runner_url = runnerUrl.trim();

		// ponytail: LITELLM_BASE_URL not written here — the runner host injects it
		// (RUNNER_WORKER_ENV_FILE on VPC/Mac). Add a gateway source if that stops holding.
		const envObj = withModelAlias(envRaw.trim() ? JSON.parse(envRaw.trim()) : {}, model);
		if (Object.keys(envObj).length) payload.env = envObj;

		const constraints = buildConstraints();
		if (constraints) payload.constraints = constraints;

		try {
			await onSave(payload);
		} catch (err: unknown) {
			serverError = err instanceof Error ? err.message : t.workers.saveError;
			submitting = false;
		}
	}

	const fieldClass =
		'w-full rounded-token border border-border bg-bg px-3 py-1.5 text-[13px] text-text placeholder:text-muted focus:border-text focus:outline-none';
	const readonlyFieldClass =
		'w-full rounded-token border border-border bg-bg px-3 py-1.5 text-[13px] text-text cursor-not-allowed opacity-60';
	const labelClass = 'block text-[11px] font-semibold uppercase tracking-wide text-muted mb-1';
	const errorClass = 'mt-0.5 text-[11px] text-red-500';
</script>

<div class="rounded-xl border border-border bg-surface-2 p-5">
	<h3 class="mb-4 text-[14px] font-semibold">
		{formTitle}
	</h3>

	<form onsubmit={(e) => { e.preventDefault(); handleSubmit(); }} novalidate>
		<div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
			<!-- Worker -->
			<div>
				<label class={labelClass} for="wf-worker">
					{workerReadonly ? t.workers.formWorkerReadonly : t.workers.formWorker}
				</label>
				{#if workerReadonly}
					<input id="wf-worker" class={readonlyFieldClass} value={worker} readonly />
				{:else}
					<input
						id="wf-worker"
						class={fieldClass}
						placeholder={t.workers.formWorkerPlaceholder}
						bind:value={worker}
						autocomplete="off"
					/>
					{#if errors.worker}<p class={errorClass}>{errors.worker}</p>{/if}
				{/if}
			</div>

			<!-- Name -->
			<div>
				<label class={labelClass} for="wf-name">
					{isEdit ? t.workers.formNameReadonly : t.workers.formName}
				</label>
				{#if isEdit}
					<input
						id="wf-name"
						class={readonlyFieldClass}
						value={name}
						readonly
					/>
				{:else}
					<input
						id="wf-name"
						class={fieldClass}
						placeholder={t.workers.formNamePlaceholder}
						bind:value={name}
						autocomplete="off"
						oninput={() => { nameEdited = true; }}
					/>
					{#if errors.name}<p class={errorClass}>{errors.name}</p>{/if}
				{/if}
			</div>

			<!-- Capability -->
			<div>
				<label class={labelClass} for="wf-cap">
					{capabilityReadonly ? t.workers.formCapabilityReadonly : t.workers.formCapability}
				</label>
				{#if capabilityReadonly}
					<input id="wf-cap" class={readonlyFieldClass} value={capability} readonly />
				{:else}
					<input
						id="wf-cap"
						class={fieldClass}
						list="wf-cap-list"
						placeholder={t.workers.formCapabilityPlaceholder}
						bind:value={capability}
						autocomplete="off"
					/>
					<datalist id="wf-cap-list">
						{#each capabilities as cap}
							<option value={cap}></option>
						{/each}
					</datalist>
					{#if errors.capability}<p class={errorClass}>{errors.capability}</p>{/if}
				{/if}
			</div>

			<!-- Runtime -->
			<div>
				<label class={labelClass} for="wf-runtime">
					{runtimeReadonly ? t.workers.formRuntimeReadonly : t.workers.formRuntime}
				</label>
				{#if runtimeReadonly}
					<input id="wf-runtime" class={readonlyFieldClass} value={runtime} readonly />
				{:else}
					<select id="wf-runtime" class={fieldClass} bind:value={runtime}>
						{#each allowedRuntimes as rt}
							<option value={rt}>{rt}</option>
						{/each}
					</select>
					{#if errors.runtime}<p class={errorClass}>{errors.runtime}</p>{/if}
				{/if}
			</div>

			<!-- Activation -->
			<div>
				<label class={labelClass} for="wf-activation">{t.workers.formActivation}</label>
				<select id="wf-activation" class={fieldClass} bind:value={activation}>
					<option value="resident">resident</option>
					<option value="on_demand">on_demand</option>
				</select>
				{#if errors.activation}<p class={errorClass}>{errors.activation}</p>{/if}
			</div>

			<!-- Model (LLM) — writes LITELLM_MODEL into env; optional, hidden when no models -->
			{#if showModel}
				<div>
					<label class={labelClass} for="wf-model">{t.workers.formModel}</label>
					<select
						id="wf-model"
						class={fieldClass}
						bind:value={model}
						aria-describedby={runtime === 'cloudrun' && model ? 'wf-model-cloudrun-hint' : undefined}
					>
						<option value="">{t.workers.formModelNone}</option>
						{#each modelOptions as alias}
							<option value={alias}>{alias}</option>
						{/each}
					</select>
					{#if runtime === 'cloudrun' && model}
						<p id="wf-model-cloudrun-hint" class="mt-0.5 text-[11px] text-muted">{t.workers.formModelCloudrunHint}</p>
					{/if}
				</div>
			{:else if modelBlocked}
				<div>
					<span class={labelClass}>{t.workers.formModel}</span>
					{#if modelsStatus === 'loading'}
						<p class="mt-0.5 text-[11px] text-muted" role="status">{t.workers.formModelLoading}</p>
					{:else}
						<p class={errorClass} role="alert">{t.workers.formModelLoadFailed}</p>
					{/if}
				</div>
			{/if}

			<!-- Enabled toggle -->
			<div class="flex items-center gap-3 pt-1">
				<label class="flex cursor-pointer items-center gap-2 text-[13px]">
					<input type="checkbox" class="h-4 w-4 accent-green" bind:checked={enabled} />
					{t.workers.formEnabled}
				</label>
			</div>

			<!-- Runner URL — only relevant for vpc/local; cloudrun has no runner -->
			{#if runtime !== 'cloudrun'}
				<div>
					<label class={labelClass} for="wf-runner">{t.workers.formRunnerUrl}</label>
					<input
						id="wf-runner"
						class={fieldClass}
						placeholder={t.workers.formRunnerUrlPlaceholder}
						bind:value={runnerUrl}
					/>
					{#if errors.runnerUrl}<p class={errorClass}>{errors.runnerUrl}</p>{/if}
				</div>
			{/if}
		</div>

		<!-- Constraints (structured) -->
		<div class="mt-4 rounded-lg border border-border bg-bg p-4">
			<p class="mb-3 text-[11px] font-semibold uppercase tracking-wide text-muted">
				{t.workers.formConstraints}
			</p>
			<div class="grid grid-cols-1 gap-4 sm:grid-cols-3">
				<div>
					<label class={labelClass} for="wf-c-requires">{t.workers.formConstraintsRequires}</label>
					<select id="wf-c-requires" class={fieldClass} bind:value={cRequires}>
						<option value="">{t.workers.formConstraintsRequiresNone}</option>
						<option value="residential">{t.workers.formConstraintsRequiresResidential}</option>
					</select>
				</div>
				<div>
					<label class={labelClass} for="wf-c-accepts">{t.workers.formConstraintsAccepts}</label>
					<input
						id="wf-c-accepts"
						class={fieldClass}
						placeholder={t.workers.formConstraintsAcceptsPlaceholder}
						bind:value={cAccepts}
					/>
				</div>
				<div>
					<label class={labelClass} for="wf-c-sensitivity"
						>{t.workers.formConstraintsSensitivity}</label
					>
					<select id="wf-c-sensitivity" class={fieldClass} bind:value={cSensitivity}>
						<option value="">{t.workers.formConstraintsSensitivityNone}</option>
						<option value="third_party">{t.workers.formConstraintsSensitivityThirdParty}</option>
					</select>
				</div>
			</div>
		</div>

		<!-- Env JSON -->
		<div class="mt-4">
			<label class={labelClass} for="wf-env">{t.workers.formEnv}</label>
			<textarea
				id="wf-env"
				class="{fieldClass} font-mono text-[12px]"
				rows="3"
				placeholder={t.workers.formEnvPlaceholder}
				bind:value={envRaw}
			></textarea>
			{#if errors.env}<p class={errorClass}>{errors.env}</p>{/if}
		</div>

		{#if serverError}
			<p class="mt-3 text-sm text-red-500">{serverError}</p>
		{/if}

		<div class="mt-5 flex gap-2">
			<button
				type="submit"
				class="cursor-pointer rounded-token bg-text px-4 py-1.5 text-[13px] font-semibold text-bg hover:opacity-90 disabled:opacity-40"
				disabled={submitting}
			>
				{submitting ? t.workers.saving : t.workers.formSave}
			</button>
			<button
				type="button"
				class="cursor-pointer rounded-token border border-border bg-surface-2 px-4 py-1.5 text-[13px] hover:bg-hover"
				onclick={onCancel}
				disabled={submitting}
			>
				{t.workers.formCancel}
			</button>
		</div>
	</form>
</div>
