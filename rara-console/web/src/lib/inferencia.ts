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
