<script lang="ts">
	import { t } from '$lib/strings';

	type InterestProfile = {
		version: number;
		status: string;
		narrative?: string;
		topics?: unknown;
		authors?: unknown;
		anti_topics?: unknown;
		weights?: unknown;
		created_at?: string;
	};
	type GateRule = {
		action: 'allow' | 'deny';
		match_type: 'channel' | 'title_contains';
		value: string;
		enabled: boolean;
	};

	// --- interest profile state ---
	let activeProfile = $state<InterestProfile | null>(null);
	let versions = $state<InterestProfile[]>([]);
	let profileLoading = $state(true);
	let profileError = $state(false);

	// propose form
	let proposeVersion = $state('');
	let proposeNarrative = $state('');
	let proposeTopics = $state('');
	let proposeAuthors = $state('');
	let proposeAntiTopics = $state('');
	let proposeWeights = $state('');
	let proposing = $state(false);
	let proposeError = $state('');

	// approve state: version number being approved, or null
	let approving = $state<number | null>(null);
	let approveError = $state('');

	// --- gate rules state ---
	let rules = $state<GateRule[]>([]);
	let rulesLoading = $state(true);
	let rulesError = $state(false);

	// rule form (create + edit)
	let ruleAction = $state<'allow' | 'deny'>('allow');
	let ruleMatchType = $state<'channel' | 'title_contains'>('channel');
	let ruleValue = $state('');
	let ruleEnabled = $state(true);
	let savingRule = $state(false);
	let saveRuleError = $state('');

	$effect(() => {
		Promise.all([
			fetch('/api/interest-profile').then((r) =>
				r.status === 404 ? null : r.ok ? r.json() : Promise.reject(r.status)
			),
			fetch('/api/interest-profile/versions').then((r) =>
				r.ok ? r.json() : Promise.reject(r.status)
			)
		])
			.then(([active, vers]) => {
				activeProfile = active;
				versions = vers ?? [];
			})
			.catch(() => (profileError = true))
			.finally(() => (profileLoading = false));

		fetch('/api/gate-rules')
			.then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
			.then((d) => (rules = d ?? []))
			.catch(() => (rulesError = true))
			.finally(() => (rulesLoading = false));
	});

	function parseOptionalJSON(s: string): unknown | undefined {
		const trimmed = s.trim();
		if (!trimmed) return undefined;
		return JSON.parse(trimmed); // throws on invalid — caught by caller
	}

	async function propose() {
		const v = parseInt(proposeVersion, 10);
		if (!v || v <= 0) {
			proposeError = 'Versão deve ser um inteiro positivo.';
			return;
		}
		let body: Record<string, unknown>;
		try {
			body = { version: v, narrative: proposeNarrative };
			const top = parseOptionalJSON(proposeTopics);
			if (top !== undefined) body.topics = top;
			const auth = parseOptionalJSON(proposeAuthors);
			if (auth !== undefined) body.authors = auth;
			const anti = parseOptionalJSON(proposeAntiTopics);
			if (anti !== undefined) body.anti_topics = anti;
			const wt = parseOptionalJSON(proposeWeights);
			if (wt !== undefined) body.weights = wt;
		} catch {
			proposeError = 'JSON inválido em um dos campos opcionais.';
			return;
		}
		proposing = true;
		proposeError = '';
		try {
			const r = await fetch('/api/interest-profile', {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify(body)
			});
			if (!r.ok) {
				const data = await r.json().catch(() => ({}));
				proposeError = (data as { error?: string }).error ?? t.curadoria.profileProposeError;
			} else {
				versions = await fetch('/api/interest-profile/versions')
					.then((r2) => (r2.ok ? r2.json() : versions))
					.catch(() => versions);
				proposeVersion = '';
				proposeNarrative = '';
				proposeTopics = '';
				proposeAuthors = '';
				proposeAntiTopics = '';
				proposeWeights = '';
			}
		} catch {
			proposeError = t.curadoria.profileProposeError;
		} finally {
			proposing = false;
		}
	}

	async function approve(version: number) {
		approving = version;
		approveError = '';
		try {
			const r = await fetch('/api/interest-profile/approve', {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ version })
			});
			if (!r.ok) {
				const data = await r.json().catch(() => ({}));
				approveError = (data as { error?: string }).error ?? t.curadoria.profileApproveError;
			} else {
				const refreshed = await Promise.all([
					fetch('/api/interest-profile').then((r2) =>
						r2.status === 404 ? null : r2.ok ? r2.json() : Promise.reject(r2.status)
					),
					fetch('/api/interest-profile/versions').then((r2) =>
						r2.ok ? r2.json() : Promise.reject(r2.status)
					)
				]).catch(() => null);
				if (refreshed) {
					[activeProfile, versions] = refreshed as [InterestProfile | null, InterestProfile[]];
				} else {
					// Approve succeeded but refresh failed — user sees stale state
					approveError = 'Aprovado! Recarregue a página para ver o histórico atualizado.';
				}
			}
		} catch {
			approveError = t.curadoria.profileApproveError;
		} finally {
			approving = null;
		}
	}

	function editRule(rule: GateRule) {
		ruleAction = rule.action;
		ruleMatchType = rule.match_type;
		ruleValue = rule.value;
		ruleEnabled = rule.enabled;
	}

	async function toggleRule(rule: GateRule) {
		await saveRuleData({ ...rule, enabled: !rule.enabled });
	}

	async function saveRule() {
		if (!ruleValue.trim()) {
			saveRuleError = 'Valor é obrigatório.';
			return;
		}
		await saveRuleData({ action: ruleAction, match_type: ruleMatchType, value: ruleValue.trim(), enabled: ruleEnabled });
	}

	async function saveRuleData(rule: GateRule) {
		savingRule = true;
		saveRuleError = '';
		try {
			const r = await fetch('/api/gate-rules', {
				method: 'PUT',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify(rule)
			});
			if (!r.ok) {
				const data = await r.json().catch(() => ({}));
				saveRuleError = (data as { error?: string }).error ?? t.curadoria.gateSaveError;
			} else {
				rules = await fetch('/api/gate-rules')
					.then((r2) => (r2.ok ? r2.json() : rules))
					.catch(() => rules);
				ruleValue = ''; // action/match_type/enabled kept so the user can batch-add similar rules
			}
		} catch {
			saveRuleError = t.curadoria.gateSaveError;
		} finally {
			savingRule = false;
		}
	}

	function fmt(v: unknown): string {
		return v == null ? '' : JSON.stringify(v, null, 2);
	}

	const pulsoCards = [
		{ label: t.curadoria.pulsoEntrou, color: 'text-text' },
		{ label: t.curadoria.pulsoManteve, color: 'text-green' },
		{ label: t.curadoria.pulsoBarrou, color: 'text-red' },
		{ label: t.curadoria.pulsoDuvida, color: 'text-primary' }
	] as const;

	const spineSteps = [
		t.curadoria.spineStep1,
		t.curadoria.spineStep2,
		t.curadoria.spineStep3,
		t.curadoria.spineStep4
	] as const;
</script>

<!-- ── 1. PULSO (~24h) ─────────────────────────────────────────────── -->
<section class="mb-6">
	<div class="mb-3 flex items-baseline gap-2">
		<h2 class="text-[15px] font-semibold">{t.curadoria.pulsoZone}</h2>
		<span class="text-[12px] text-muted">{t.curadoria.pulsoLabel}</span>
	</div>
	<div class="grid grid-cols-4 gap-3">
		{#each pulsoCards as card}
			<div class="rounded-card border border-border bg-surface px-4 py-3">
				<div class="text-[11px] text-muted">{card.label}</div>
				<div class="mt-1 text-[22px] font-semibold {card.color}">—</div>
			</div>
		{/each}
	</div>
</section>

<!-- ── 2. SPINE ────────────────────────────────────────────────────── -->
<div
	class="mb-6 flex items-center overflow-hidden rounded-card border border-border bg-surface-2 px-5 py-3 text-[12px]"
	aria-hidden="true"
>
	{#each spineSteps as step, i}
		{#if i > 0}<span class="mx-3 text-border">→</span>{/if}
		<span class="text-muted">{step}</span>
	{/each}
</div>

<!-- ── 3. FILA DE REVISÃO (herói, shell) ──────────────────────────── -->
<section class="mb-6">
	<div class="mb-3 flex items-baseline gap-2">
		<h2 class="text-[15px] font-semibold">{t.curadoria.filaZone}</h2>
		<span class="text-[12px] text-muted">{t.curadoria.filaSubtitle}</span>
	</div>
	<div class="overflow-hidden rounded-card border border-border bg-surface">
		<div class="flex h-32 items-center justify-center">
			<div class="text-center">
				<div class="text-[13px] text-muted">{t.curadoria.filaEmpty}</div>
				<div class="mt-2 flex justify-center gap-2">
					<button
						disabled
						class="cursor-default rounded-token border border-border bg-transparent px-4 py-1.5 text-[13px] text-muted opacity-40"
					>{t.curadoria.filaKeep}</button>
					<button
						disabled
						class="cursor-default rounded-token border border-border bg-transparent px-4 py-1.5 text-[13px] text-muted opacity-40"
					>{t.curadoria.filaDrop}</button>
				</div>
				<div class="mt-2 text-[11px] text-muted opacity-60">{t.curadoria.filaComingSoon}</div>
			</div>
		</div>
	</div>
</section>

<!-- ── 4. O GOSTO (funcional — Interest Profile) ──────────────────── -->
<section class="mb-6">
	<h2 class="mb-3 text-[15px] font-semibold">{t.curadoria.gostoZone}</h2>

	{#if profileLoading}
		<p class="text-[13px] text-muted">{t.curadoria.profileLoading}</p>
	{:else if profileError}
		<p class="text-[13px] text-red">{t.curadoria.profileError}</p>
	{:else}
		<!-- Active version card -->
		<div class="mb-4 overflow-hidden rounded-card border border-border bg-surface">
			<div class="border-b border-border px-4 py-2 text-[12px] font-medium text-muted">
				{t.curadoria.profileCurrent}
			</div>
			{#if activeProfile}
				<div class="space-y-2 px-4 py-3 text-[13px]">
					<div>
						<span class="text-muted">{t.curadoria.profileVersion}:</span>
						<span class="ml-1 font-semibold">v{activeProfile.version}</span>
					</div>
					{#if activeProfile.narrative}
						<div>
							<div class="text-muted">{t.curadoria.profileNarrative}:</div>
							<p class="mt-1 text-[13px]">{activeProfile.narrative}</p>
						</div>
					{/if}
					{#if activeProfile.topics}
						<div>
							<div class="text-muted">Topics:</div>
							<pre class="mt-1 overflow-x-auto rounded-token bg-surface-2 px-3 py-2 text-[12px]">{fmt(activeProfile.topics)}</pre>
						</div>
					{/if}
					{#if activeProfile.authors}
						<div>
							<div class="text-muted">Authors:</div>
							<pre class="mt-1 overflow-x-auto rounded-token bg-surface-2 px-3 py-2 text-[12px]">{fmt(activeProfile.authors)}</pre>
						</div>
					{/if}
					{#if activeProfile.anti_topics}
						<div>
							<div class="text-muted">Anti-topics:</div>
							<pre class="mt-1 overflow-x-auto rounded-token bg-surface-2 px-3 py-2 text-[12px]">{fmt(activeProfile.anti_topics)}</pre>
						</div>
					{/if}
					{#if activeProfile.weights}
						<div>
							<div class="text-muted">Weights:</div>
							<pre class="mt-1 overflow-x-auto rounded-token bg-surface-2 px-3 py-2 text-[12px]">{fmt(activeProfile.weights)}</pre>
						</div>
					{/if}
				</div>
			{:else}
				<p class="px-4 py-3 text-[13px] text-muted">{t.curadoria.profileEmpty}</p>
			{/if}
		</div>

		<!-- Version history -->
		{#if versions.length > 0}
			<div class="mb-4 overflow-hidden rounded-card border border-border bg-surface">
				<div class="border-b border-border px-4 py-2 text-[12px] font-medium text-muted">
					{t.curadoria.profileVersions}
				</div>
				<table class="w-full text-[13px]">
					<thead>
						<tr class="border-b border-border text-left text-[11px] text-muted">
							<th class="px-4 py-2 font-medium">{t.curadoria.profileVersion}</th>
							<th class="px-4 py-2 font-medium">{t.curadoria.profileStatus}</th>
							<th class="px-4 py-2 font-medium">{t.curadoria.profileCreatedAt}</th>
							<th class="px-4 py-2"></th>
						</tr>
					</thead>
					<tbody>
						{#each versions as v}
							<tr class="border-b border-border last:border-b-0">
								<td class="px-4 py-2 font-medium">v{v.version}</td>
								<td class="px-4 py-2">
									<span
										class="rounded-full px-2 py-0.5 text-[11px] font-medium
										{v.status === 'active' ? 'bg-green/15 text-green' : v.status === 'proposed' ? 'bg-primary/15 text-primary' : 'text-muted'}"
									>{v.status}</span>
								</td>
								<td class="px-4 py-2 text-muted">
									{v.created_at ? new Date(v.created_at).toLocaleString('pt-BR') : '—'}
								</td>
								<td class="px-4 py-2 text-right">
									{#if v.status === 'proposed'}
										{#if approving === v.version}
											<span class="text-[12px] text-muted">{t.curadoria.profileApproving}</span>
										{:else}
											<button
												class="cursor-pointer rounded-token border border-primary/40 bg-transparent px-3 py-1 text-[12px] font-medium text-primary hover:bg-primary/10"
												onclick={() => approve(v.version)}
											>{t.curadoria.profileApprove}</button>
										{/if}
									{/if}
								</td>
							</tr>
						{/each}
					</tbody>
				</table>
				{#if approveError}
					<p class="px-4 py-2 text-[12px] text-red">{approveError}</p>
				{/if}
			</div>
		{/if}

		<!-- Propose new version form -->
		<div class="overflow-hidden rounded-card border border-border bg-surface">
			<div class="border-b border-border px-4 py-2 text-[12px] font-medium text-muted">
				{t.curadoria.profileProposeSection}
			</div>
			<div class="space-y-3 px-4 py-3">
				<div class="flex gap-3">
					<div class="w-32">
						<label class="mb-1 block text-[11px] text-muted" for="prop-version">{t.curadoria.profileVersionLabel}</label>
						<input
							id="prop-version"
							type="number"
							min="1"
							bind:value={proposeVersion}
							class="w-full rounded-token border border-border bg-surface-2 px-2 py-1 text-[13px] focus:outline-none focus:ring-1 focus:ring-primary/50"
						/>
					</div>
					<div class="flex-1">
						<label class="mb-1 block text-[11px] text-muted" for="prop-narrative">{t.curadoria.profileNarrativeLabel}</label>
						<textarea
							id="prop-narrative"
							rows="2"
							bind:value={proposeNarrative}
							class="w-full rounded-token border border-border bg-surface-2 px-2 py-1 text-[13px] focus:outline-none focus:ring-1 focus:ring-primary/50"
						></textarea>
					</div>
				</div>
				<div class="grid grid-cols-2 gap-3">
					<div>
						<label class="mb-1 block text-[11px] text-muted" for="prop-topics">{t.curadoria.profileTopicsLabel}</label>
						<textarea id="prop-topics" rows="2" placeholder={t.curadoria.profileJsonHint} bind:value={proposeTopics}
							class="w-full rounded-token border border-border bg-surface-2 px-2 py-1 font-mono text-[12px] focus:outline-none focus:ring-1 focus:ring-primary/50"
						></textarea>
					</div>
					<div>
						<label class="mb-1 block text-[11px] text-muted" for="prop-authors">{t.curadoria.profileAuthorsLabel}</label>
						<textarea id="prop-authors" rows="2" placeholder={t.curadoria.profileJsonHint} bind:value={proposeAuthors}
							class="w-full rounded-token border border-border bg-surface-2 px-2 py-1 font-mono text-[12px] focus:outline-none focus:ring-1 focus:ring-primary/50"
						></textarea>
					</div>
					<div>
						<label class="mb-1 block text-[11px] text-muted" for="prop-anti">{t.curadoria.profileAntiTopicsLabel}</label>
						<textarea id="prop-anti" rows="2" placeholder={t.curadoria.profileJsonHint} bind:value={proposeAntiTopics}
							class="w-full rounded-token border border-border bg-surface-2 px-2 py-1 font-mono text-[12px] focus:outline-none focus:ring-1 focus:ring-primary/50"
						></textarea>
					</div>
					<div>
						<label class="mb-1 block text-[11px] text-muted" for="prop-weights">{t.curadoria.profileWeightsLabel}</label>
						<textarea id="prop-weights" rows="2" placeholder={t.curadoria.profileJsonHint} bind:value={proposeWeights}
							class="w-full rounded-token border border-border bg-surface-2 px-2 py-1 font-mono text-[12px] focus:outline-none focus:ring-1 focus:ring-primary/50"
						></textarea>
					</div>
				</div>
				{#if proposeError}
					<p class="text-[12px] text-red">{proposeError}</p>
				{/if}
				<button
					disabled={proposing}
					onclick={propose}
					class="cursor-pointer rounded-token border border-border bg-transparent px-4 py-1.5 text-[13px] font-medium hover:bg-hover disabled:cursor-default disabled:opacity-50"
				>{proposing ? t.curadoria.profileProposing : t.curadoria.profileProposeBtn}</button>
			</div>
		</div>
	{/if}
</section>

<!-- ── 5. TRILHA DE DECISÕES (shell) ──────────────────────────────── -->
<section class="mb-6">
	<h2 class="mb-3 text-[15px] font-semibold">{t.curadoria.trilhaZone}</h2>
	<div class="overflow-hidden rounded-card border border-border bg-surface">
		<div class="flex h-24 items-center justify-center">
			<div class="text-center">
				<div class="text-[13px] text-muted">{t.curadoria.trilhaEmpty}</div>
				<div class="mt-1 text-[11px] text-muted opacity-60">{t.curadoria.trilhaComingSoon}</div>
			</div>
		</div>
	</div>
</section>

<!-- ── 6. REGRAS DE GATE (secundária, colapsável) ─────────────────── -->
<details class="group overflow-hidden rounded-card border border-border bg-surface">
	<summary
		class="flex cursor-pointer list-none items-center justify-between px-4 py-3 text-[13px] font-medium hover:bg-hover"
	>
		{t.curadoria.gateSection}
		<span class="text-[12px] text-muted group-open:hidden">{t.curadoria.gateExpand}</span>
		<span class="hidden text-[12px] text-muted group-open:inline">{t.curadoria.gateCollapse}</span>
	</summary>

	<div class="border-t border-border px-4 py-4">
		{#if rulesLoading}
			<p class="text-[13px] text-muted">{t.curadoria.gateLoading}</p>
		{:else if rulesError}
			<p class="text-[13px] text-red">{t.curadoria.gateError}</p>
		{:else}
			<!-- Rules table -->
			<div class="mb-4 overflow-hidden rounded-card border border-border bg-surface">
				{#if rules.length === 0}
					<p class="px-4 py-3 text-[13px] text-muted">{t.curadoria.gateEmpty}</p>
				{:else}
					<table class="w-full text-[13px]">
						<thead>
							<tr class="border-b border-border text-left text-[11px] text-muted">
								<th class="px-4 py-2 font-medium">{t.curadoria.gateAction}</th>
								<th class="px-4 py-2 font-medium">{t.curadoria.gateMatchType}</th>
								<th class="px-4 py-2 font-medium">{t.curadoria.gateValue}</th>
								<th class="px-4 py-2 font-medium">{t.curadoria.gateEnabled}</th>
								<th class="px-4 py-2"></th>
							</tr>
						</thead>
						<tbody>
							{#each rules as rule}
								<tr class="border-b border-border last:border-b-0">
									<td class="px-4 py-2">
										<span
											class="rounded-full px-2 py-0.5 text-[11px] font-medium
											{rule.action === 'allow' ? 'bg-green/15 text-green' : 'bg-red/15 text-red'}"
										>{rule.action}</span>
									</td>
									<td class="px-4 py-2 text-muted">{rule.match_type}</td>
									<td class="px-4 py-2 font-mono">{rule.value}</td>
									<td class="px-4 py-2">
										<button
											title={t.curadoria.gateToggle}
											disabled={savingRule}
											class="h-5 w-9 cursor-pointer rounded-full border-0 transition-colors disabled:cursor-default disabled:opacity-50 {rule.enabled ? 'bg-green' : 'bg-border'}"
											onclick={() => toggleRule(rule)}
										>
											<span class="sr-only">{t.curadoria.gateToggle}</span>
										</button>
									</td>
									<td class="px-4 py-2 text-right">
										<button
											class="cursor-pointer rounded-token border border-border bg-transparent px-2 py-0.5 text-[11px] hover:bg-hover"
											onclick={() => editRule(rule)}
										>{t.curadoria.gateEdit}</button>
									</td>
								</tr>
							{/each}
						</tbody>
					</table>
				{/if}
			</div>

			<!-- Add/edit rule form -->
			<div class="overflow-hidden rounded-card border border-border bg-surface">
				<div class="border-b border-border px-4 py-2 text-[12px] font-medium text-muted">
					{t.curadoria.gateAddSection}
				</div>
				<div class="flex flex-wrap items-end gap-3 px-4 py-3">
					<div>
						<label class="mb-1 block text-[11px] text-muted" for="rule-action">{t.curadoria.gateAction}</label>
						<select
							id="rule-action"
							bind:value={ruleAction}
							class="rounded-token border border-border bg-surface-2 px-2 py-1 text-[13px] focus:outline-none focus:ring-1 focus:ring-primary/50"
						>
							<option value="allow">allow</option>
							<option value="deny">deny</option>
						</select>
					</div>
					<div>
						<label class="mb-1 block text-[11px] text-muted" for="rule-match">{t.curadoria.gateMatchType}</label>
						<select
							id="rule-match"
							bind:value={ruleMatchType}
							class="rounded-token border border-border bg-surface-2 px-2 py-1 text-[13px] focus:outline-none focus:ring-1 focus:ring-primary/50"
						>
							<option value="channel">channel</option>
							<option value="title_contains">title_contains</option>
						</select>
					</div>
					<div class="flex-1">
						<label class="mb-1 block text-[11px] text-muted" for="rule-value">{t.curadoria.gateValue}</label>
						<input
							id="rule-value"
							type="text"
							bind:value={ruleValue}
							class="w-full rounded-token border border-border bg-surface-2 px-2 py-1 text-[13px] focus:outline-none focus:ring-1 focus:ring-primary/50"
						/>
					</div>
					<div class="flex items-center gap-2">
						<input id="rule-enabled" type="checkbox" bind:checked={ruleEnabled} class="h-4 w-4 cursor-pointer" />
						<label class="cursor-pointer text-[13px]" for="rule-enabled">{t.curadoria.gateEnabled}</label>
					</div>
					<button
						disabled={savingRule}
						onclick={saveRule}
						class="cursor-pointer rounded-token border border-border bg-transparent px-4 py-1.5 text-[13px] font-medium hover:bg-hover disabled:cursor-default disabled:opacity-50"
					>{savingRule ? t.curadoria.gateSaving : t.curadoria.gateSave}</button>
				</div>
				{#if saveRuleError}
					<p class="px-4 pb-3 text-[12px] text-red">{saveRuleError}</p>
				{/if}
			</div>
		{/if}
	</div>
</details>
