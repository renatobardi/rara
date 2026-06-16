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
				[activeProfile, versions] = await Promise.all([
					fetch('/api/interest-profile').then((r2) =>
						r2.status === 404 ? null : r2.ok ? r2.json() : activeProfile
					),
					fetch('/api/interest-profile/versions').then((r2) =>
						r2.ok ? r2.json() : versions
					)
				]).catch(() => [activeProfile, versions]);
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
				ruleValue = '';
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
</script>

<!-- Interest Profile section -->
<section class="mb-8">
	<h2 class="mb-3 text-[15px] font-semibold">{t.curadoria.profileSection}</h2>

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
					{#each [
						{ id: 'prop-topics', label: t.curadoria.profileTopicsLabel, bind: proposeTopics, set: (v: string) => (proposeTopics = v) },
						{ id: 'prop-authors', label: t.curadoria.profileAuthorsLabel, bind: proposeAuthors, set: (v: string) => (proposeAuthors = v) },
						{ id: 'prop-anti', label: t.curadoria.profileAntiTopicsLabel, bind: proposeAntiTopics, set: (v: string) => (proposeAntiTopics = v) },
						{ id: 'prop-weights', label: t.curadoria.profileWeightsLabel, bind: proposeWeights, set: (v: string) => (proposeWeights = v) }
					] as field}
						<div>
							<label class="mb-1 block text-[11px] text-muted" for={field.id}>{field.label}</label>
							<textarea
								id={field.id}
								rows="2"
								placeholder={t.curadoria.profileJsonHint}
								value={field.bind}
								oninput={(e) => field.set((e.target as HTMLTextAreaElement).value)}
								class="w-full rounded-token border border-border bg-surface-2 px-2 py-1 font-mono text-[12px] focus:outline-none focus:ring-1 focus:ring-primary/50"
							></textarea>
						</div>
					{/each}
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

<!-- Gate Rules section -->
<section>
	<h2 class="mb-3 text-[15px] font-semibold">{t.curadoria.gateSection}</h2>

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
										class="h-5 w-9 cursor-pointer rounded-full border-0 transition-colors {rule.enabled ? 'bg-green' : 'bg-border'}"
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
</section>
