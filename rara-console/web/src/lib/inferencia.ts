// Pure logic for the /inferencia screen (Providers + Models registry).
// Kept framework-free so it's unit-testable without a DOM (mirrors curadoria.ts).

export type LLMProvider = {
	id: number;
	name: string;
	kind: string;
	base_url?: string;
	key_last4?: string;
	enabled: boolean;
	created_at?: string;
	updated_at?: string;
};

export type LLMModel = {
	id: number;
	provider_id: number;
	provider_name?: string;
	alias: string;
	upstream: string;
	input_cost_per_token: number;
	output_cost_per_token: number;
	params?: unknown;
	enabled: boolean;
	created_at?: string;
	updated_at?: string;
};

// Mirrors validLLMKinds in rara-core/llm_providers.go.
export const PROVIDER_KINDS = [
	'groq',
	'gemini',
	'anthropic',
	'openai',
	'deepseek',
	'openai_compatible'
] as const;
export type ProviderKind = (typeof PROVIDER_KINDS)[number];

// The core serializes an empty slice as null (not []); every read must coerce null → [].
export function asList<T>(d: unknown): T[] {
	return Array.isArray(d) ? (d as T[]) : [];
}

export function isProvider(v: unknown): v is LLMProvider {
	if (typeof v !== 'object' || v === null) return false;
	const p = v as Record<string, unknown>;
	return typeof p.id === 'number' && typeof p.name === 'string' &&
		typeof p.kind === 'string' && PROVIDER_KINDS.includes(p.kind as ProviderKind) &&
		typeof p.enabled === 'boolean';
}

export function isModel(v: unknown): v is LLMModel {
	if (typeof v !== 'object' || v === null) return false;
	const m = v as Record<string, unknown>;
	return typeof m.id === 'number' && typeof m.provider_id === 'number' &&
		typeof m.alias === 'string' && typeof m.upstream === 'string' &&
		typeof m.input_cost_per_token === 'number' && typeof m.output_cost_per_token === 'number' &&
		typeof m.enabled === 'boolean';
}

// Masked display of a provider key. The read DTO only ever carries key_last4, but slice
// defensively so a full key would never render in clear even if the backend regressed.
export function maskKey(last4?: string): string {
	const suffix = last4?.trim().slice(-4);
	return suffix ? `•••• ${suffix}` : '—';
}

// Mirrors the core's validateEndpointURL (surface.go): block loopback / private / link-local /
// metadata hosts so the SPA rejects an SSRF-shaped base_url before it reaches the core.
function isBlockedHost(hostname: string): boolean {
	const h = hostname.toLowerCase().replace(/\.+$/, '').replace(/^\[|\]$/g, '');
	if (h === 'localhost' || h === '0.0.0.0' || h === '::1') return true;
	if (h.endsWith('.local') || h.endsWith('.localhost')) return true;
	if (h === 'metadata.google.internal') return true;
	if (h.startsWith('169.254.')) return true; // link-local / cloud metadata
	if (/^127\./.test(h)) return true; // loopback
	if (/^10\./.test(h)) return true; // private
	if (/^192\.168\./.test(h)) return true; // private
	if (/^172\.(1[6-9]|2\d|3[01])\./.test(h)) return true; // private 172.16–31
	return false;
}

// base_url rules mirror the core: required for openai_compatible, and any non-empty value must be a
// valid http(s) URL with no embedded credentials, pointing at a public host. Returns an error code
// (mapped to a string in the component) or null.
export type BaseUrlError = 'required' | 'invalid' | 'scheme' | 'private';
export function validateBaseUrl(kind: string, baseURL: string): BaseUrlError | null {
	const v = baseURL.trim();
	if (kind === 'openai_compatible' && v === '') return 'required';
	if (v === '') return null;
	let u: URL;
	try {
		u = new URL(v);
	} catch {
		return 'invalid';
	}
	if (u.protocol !== 'http:' && u.protocol !== 'https:') return 'scheme';
	if (u.username || u.password) return 'invalid'; // no secrets embedded in the URL
	if (isBlockedHost(u.hostname)) return 'private';
	return null;
}

// Format a per-token cost for display. Costs are tiny (e.g. 1.5e-7) so plain toString is noise;
// show as "$/1M tokens" which is how vendors price.
export function costPerMillion(perToken: number): string {
	if (!perToken) return '—';
	return `$${(perToken * 1_000_000).toFixed(2)}/1M`;
}

// --- Real cost/tokens from the litellm spend log (CONSOLE-INFER-#9) ---------

// LLMSpend mirrors GET /api/llm-spend: one rollup per model alias (model = the
// alias workers send as LITELLM_MODEL = litellm's model_group).
export type LLMSpend = {
	model: string;
	spend: number;
	total_tokens: number;
	prompt_tokens: number;
	completion_tokens: number;
	requests: number;
};

export function isSpend(v: unknown): v is LLMSpend {
	if (typeof v !== 'object' || v === null) return false;
	const s = v as Record<string, unknown>;
	const nonNeg = (n: unknown): n is number => typeof n === 'number' && Number.isFinite(n) && n >= 0;
	const nonNegInt = (n: unknown): n is number => typeof n === 'number' && Number.isInteger(n) && n >= 0;
	return typeof s.model === 'string' && s.model.trim().length > 0 &&
		nonNeg(s.spend) && nonNegInt(s.total_tokens) && nonNegInt(s.prompt_tokens) &&
		nonNegInt(s.completion_tokens) && nonNegInt(s.requests);
}

// indexSpendByModel turns the spend rows into an alias → row lookup for the table.
export function indexSpendByModel(rows: LLMSpend[]): Map<string, LLMSpend> {
	return new Map(rows.map((r) => [r.model, r]));
}

// formatUSD renders a summed spend. Zero (or no data) shows a dash; tiny values keep
// 4 decimals so sub-cent spend is still legible.
export function formatUSD(usd: number): string {
	if (!usd) return '—';
	return `$${usd.toFixed(usd < 1 ? 4 : 2)}`;
}

// formatTokens renders a token count with thousands separators; zero shows a dash.
export function formatTokens(n: number): string {
	if (!n) return '—';
	return n.toLocaleString('en-US');
}

// --- litellm model catalog autocomplete (CONSOLE-INFER-#CATALOG) -----------

// CatalogEntry mirrors one slim row from GET /api/llm-catalog (the BFF strips the full litellm blob
// down to these fields). The Model form uses it to auto-fill upstream + costs.
export type CatalogEntry = {
	upstream: string;
	provider: string;
	input_cost_per_token: number;
	output_cost_per_token: number;
	max_tokens: number;
	mode: string;
};

export function isCatalogEntry(v: unknown): v is CatalogEntry {
	if (typeof v !== 'object' || v === null) return false;
	const e = v as Record<string, unknown>;
	// Costs feed the form's number inputs, so reject NaN/Infinity/negative (mirrors isSpend). Costs
	// can legitimately be 0 (free models), hence >= 0.
	const nonNegFinite = (n: unknown): n is number => typeof n === 'number' && Number.isFinite(n) && n >= 0;
	return typeof e.upstream === 'string' && e.upstream.trim().length > 0 &&
		typeof e.provider === 'string' && e.provider.trim().length > 0 &&
		nonNegFinite(e.input_cost_per_token) && nonNegFinite(e.output_cost_per_token) &&
		nonNegFinite(e.max_tokens) && e.mode === 'chat';
}

// filterCatalog returns entries whose upstream or provider contains the query (case-insensitive),
// capped at `limit` so the datalist DOM stays small even for the 2k+ row catalog.
export function filterCatalog(rows: CatalogEntry[], query: string, limit = 50): CatalogEntry[] {
	if (limit <= 0) return [];
	const q = query.trim().toLowerCase();
	const out: CatalogEntry[] = [];
	for (const r of rows) {
		if (!q || r.upstream.toLowerCase().includes(q) || r.provider.toLowerCase().includes(q)) {
			out.push(r);
			if (out.length >= limit) break;
		}
	}
	return out;
}

// catalogKindFor maps litellm's provider name onto our PROVIDER_KINDS enum, or null when there's no
// 1:1 match (the form then leaves the provider for the operator to pick).
export function catalogKindFor(provider: string): ProviderKind | null {
	return PROVIDER_KINDS.includes(provider as ProviderKind) ? (provider as ProviderKind) : null;
}

// applyCatalogPick resolves a chosen upstream against the catalog, returning the fields to auto-fill
// in the Model form — or null when the upstream isn't in the catalog (manual/BYO entry stays valid).
// provider_id is filled only when exactly one registered provider matches the mapped kind, so the
// assistant never silently picks the wrong provider when the choice is ambiguous.
export function applyCatalogPick(
	upstream: string,
	catalog: CatalogEntry[],
	providers: LLMProvider[]
): { upstream: string; input_cost_per_token: number; output_cost_per_token: number; provider_id: number | null } | null {
	const entry = catalog.find((e) => e.upstream === upstream.trim());
	if (!entry) return null;
	const kind = catalogKindFor(entry.provider);
	// Only auto-select an enabled provider — never silently point a new model at a disabled one.
	const matches = kind ? providers.filter((p) => p.kind === kind && p.enabled) : [];
	return {
		upstream: entry.upstream,
		input_cost_per_token: entry.input_cost_per_token,
		output_cost_per_token: entry.output_cost_per_token,
		provider_id: matches.length === 1 ? matches[0].id : null
	};
}

// SPEND_PERIODS is the 24h·7d·30d·Tudo selector; days feeds ?days=N (null = all-time).
export const SPEND_PERIODS: { key: string; days: number | null }[] = [
	{ key: 'spend24h', days: 1 },
	{ key: 'spend7d', days: 7 },
	{ key: 'spend30d', days: 30 },
	{ key: 'spendAll', days: null }
];
