<script lang="ts">
	import { untrack } from 'svelte';
	import { t } from '$lib/strings';

	type Constraints = {
		requires?: string;
		accepts?: string[];
		sensitivity?: string;
	};

	type Provider = {
		name: string;
		capability: string;
		runtime: string;
		activation: string;
		cost: number;
		quality: number;
		enabled: boolean;
		heartbeat_at?: string;
		constraints?: Constraints;
		runner_url?: string;
		env?: Record<string, string>;
	};

	type Props = {
		initial?: Provider | null;
		capabilities: string[];
		onSave: (p: Provider) => Promise<void>;
		onCancel: () => void;
	};

	let { initial = null, capabilities, onSave, onCancel }: Props = $props();

	const isEdit = $derived(initial !== null);

	// untrack: form mounts fresh for each add/edit open — capturing initial value once is correct
	let name = $state(untrack(() => initial?.name ?? ''));
	let capability = $state(untrack(() => initial?.capability ?? ''));
	let runtime = $state(untrack(() => initial?.runtime ?? 'local'));
	let activation = $state(untrack(() => initial?.activation ?? 'on_demand'));
	let cost = $state(untrack(() => String(initial?.cost ?? '0')));
	let quality = $state(untrack(() => String(initial?.quality ?? '1')));
	let enabled = $state(untrack(() => initial?.enabled ?? true));
	let runnerUrl = $state(untrack(() => initial?.runner_url ?? ''));
	let envRaw = $state(untrack(() => (initial?.env ? JSON.stringify(initial.env, null, 2) : '')));

	// constraints fields
	let cRequires = $state(untrack(() => initial?.constraints?.requires ?? ''));
	let cAccepts = $state(untrack(() => initial?.constraints?.accepts?.join(', ') ?? ''));
	let cSensitivity = $state(untrack(() => initial?.constraints?.sensitivity ?? ''));

	// validation errors
	let errors = $state<Record<string, string>>({});
	let submitting = $state(false);
	let serverError = $state('');

	function validate(): boolean {
		const e: Record<string, string> = {};
		if (!name.trim()) e.name = t.workers.formNameRequired;
		if (!capability.trim()) e.capability = t.workers.formCapabilityRequired;
		const c = parseFloat(cost);
		if (isNaN(c) || c < 0) e.cost = t.workers.formCostInvalid;
		const q = parseFloat(quality);
		if (isNaN(q) || q < 0 || q > 1) e.quality = t.workers.formQualityInvalid;
		if (envRaw.trim()) {
			try {
				const parsed = JSON.parse(envRaw.trim());
				if (typeof parsed !== 'object' || Array.isArray(parsed) || parsed === null) {
					e.env = t.workers.formEnvInvalid;
				}
			} catch {
				e.env = t.workers.formEnvInvalid;
			}
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

		const payload: Provider = {
			// preserve all original fields when editing so upsert doesn't wipe them
			...(initial ?? {}),
			name: name.trim(),
			capability: capability.trim(),
			runtime,
			activation,
			cost: parseFloat(cost),
			quality: parseFloat(quality),
			enabled
		};

		// omit runner_url when empty
		if (runnerUrl.trim()) payload.runner_url = runnerUrl.trim();
		else delete payload.runner_url;

		// omit env when empty
		if (envRaw.trim()) {
			payload.env = JSON.parse(envRaw.trim());
		} else {
			delete payload.env;
		}

		const constraints = buildConstraints();
		if (constraints) payload.constraints = constraints;
		else delete payload.constraints;

		// latency_ms is a dead field — never send it
		// ponytail: delete even if it came from GET (server ignores it but keeps payload clean)
		delete (payload as Record<string, unknown>).latency_ms;

		try {
			await onSave(payload);
		} catch (err: unknown) {
			serverError = err instanceof Error ? err.message : t.workers.saveError;
			submitting = false;
		}
	}

	const fieldClass =
		'w-full rounded-token border border-border bg-bg px-3 py-1.5 text-[13px] text-text placeholder:text-muted focus:border-text focus:outline-none';
	const labelClass = 'block text-[11px] font-semibold uppercase tracking-wide text-muted mb-1';
	const errorClass = 'mt-0.5 text-[11px] text-red-500';
</script>

<div class="rounded-xl border border-border bg-surface-2 p-5">
	<h3 class="mb-4 text-[14px] font-semibold">
		{isEdit ? t.workers.editWorker : t.workers.addWorker}
	</h3>

	<form onsubmit={(e) => { e.preventDefault(); handleSubmit(); }} novalidate>
		<div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
			<!-- Name -->
			<div>
				<label class={labelClass} for="wf-name">
					{isEdit ? t.workers.formNameReadonly : t.workers.formName}
				</label>
				{#if isEdit}
					<input
						id="wf-name"
						class="{fieldClass} cursor-not-allowed opacity-60"
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
					/>
					{#if errors.name}<p class={errorClass}>{errors.name}</p>{/if}
				{/if}
			</div>

			<!-- Capability -->
			<div>
				<label class={labelClass} for="wf-cap">{t.workers.formCapability}</label>
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
			</div>

			<!-- Runtime -->
			<div>
				<label class={labelClass} for="wf-runtime">{t.workers.formRuntime}</label>
				<select id="wf-runtime" class={fieldClass} bind:value={runtime}>
					<option value="local">local</option>
					<option value="cloudrun">cloudrun</option>
					<option value="vpc">vpc</option>
				</select>
			</div>

			<!-- Activation -->
			<div>
				<label class={labelClass} for="wf-activation">{t.workers.formActivation}</label>
				<select id="wf-activation" class={fieldClass} bind:value={activation}>
					<option value="resident">resident</option>
					<option value="on_demand">on_demand</option>
				</select>
			</div>

			<!-- Cost -->
			<div>
				<label class={labelClass} for="wf-cost">{t.workers.formCost}</label>
				<input
					id="wf-cost"
					type="number"
					min="0"
					step="0.1"
					class={fieldClass}
					bind:value={cost}
				/>
				{#if errors.cost}<p class={errorClass}>{errors.cost}</p>{/if}
			</div>

			<!-- Quality -->
			<div>
				<label class={labelClass} for="wf-quality">{t.workers.formQuality}</label>
				<input
					id="wf-quality"
					type="number"
					min="0"
					max="1"
					step="0.05"
					class={fieldClass}
					bind:value={quality}
				/>
				{#if errors.quality}<p class={errorClass}>{errors.quality}</p>{/if}
			</div>

			<!-- Enabled toggle -->
			<div class="flex items-center gap-3 pt-1">
				<label class="flex cursor-pointer items-center gap-2 text-[13px]">
					<input type="checkbox" class="h-4 w-4 accent-green" bind:checked={enabled} />
					{t.workers.formEnabled}
				</label>
			</div>

			<!-- Runner URL -->
			<div>
				<label class={labelClass} for="wf-runner">{t.workers.formRunnerUrl}</label>
				<input
					id="wf-runner"
					class={fieldClass}
					placeholder={t.workers.formRunnerUrlPlaceholder}
					bind:value={runnerUrl}
				/>
			</div>
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
