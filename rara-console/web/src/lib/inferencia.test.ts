import { describe, it, expect } from 'vitest';
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
	isCatalogEntry,
	filterCatalog,
	catalogKinds,
	catalogKindFor,
	applyCatalogPick,
	type LLMProvider,
	type CatalogEntry
} from './inferencia';

describe('asList', () => {
	it('coerces the core null-for-empty-slice into []', () => {
		expect(asList(null)).toEqual([]);
		expect(asList(undefined)).toEqual([]);
	});
	it('passes arrays through', () => {
		expect(asList([1, 2])).toEqual([1, 2]);
	});
	it('rejects non-array junk', () => {
		expect(asList({ a: 1 })).toEqual([]);
	});
});

describe('maskKey', () => {
	it('shows bullets + last4, never the key', () => {
		expect(maskKey('4f2a')).toBe('•••• 4f2a');
	});
	it('renders a dash when no key is set', () => {
		expect(maskKey(undefined)).toBe('—');
		expect(maskKey('')).toBe('—');
	});
	it('never leaks a full secret even if the backend regresses and sends the whole key', () => {
		const out = maskKey('sk-live-super-secret-1234');
		expect(out).toBe('•••• 1234');
		expect(out).not.toContain('super-secret');
	});
});

describe('validateBaseUrl', () => {
	it('requires base_url for openai_compatible', () => {
		expect(validateBaseUrl('openai_compatible', '')).toBe('required');
		expect(validateBaseUrl('openai_compatible', '   ')).toBe('required');
	});
	it('accepts a valid http(s) url for openai_compatible', () => {
		expect(validateBaseUrl('openai_compatible', 'https://api.x.ai/v1')).toBeNull();
	});
	it('allows empty base_url for built-in kinds', () => {
		expect(validateBaseUrl('groq', '')).toBeNull();
		expect(validateBaseUrl('anthropic', '')).toBeNull();
	});
	it('rejects malformed urls', () => {
		expect(validateBaseUrl('openai_compatible', 'not a url')).toBe('invalid');
	});
	it('rejects non-http schemes', () => {
		expect(validateBaseUrl('openai_compatible', 'ftp://x.ai')).toBe('scheme');
	});
	it('validates a non-empty url even for built-in kinds', () => {
		expect(validateBaseUrl('groq', 'ftp://x')).toBe('scheme');
	});
	it('rejects urls with embedded credentials', () => {
		expect(validateBaseUrl('openai_compatible', 'https://user:pass@api.x.ai/v1')).toBe('invalid');
	});
	it('rejects SSRF-shaped hosts (loopback / private / link-local / metadata)', () => {
		for (const url of [
			'http://localhost/v1',
			'http://127.0.0.1/v1',
			'http://0.0.0.0/v1',
			'http://[::1]/v1',
			'http://10.0.0.5/v1',
			'http://192.168.1.1/v1',
			'http://172.16.0.1/v1',
			'http://172.31.255.255/v1',
			'http://169.254.169.254/latest/meta-data',
			'http://metadata.google.internal/v1',
			'http://api.local/v1'
		]) {
			expect(validateBaseUrl('openai_compatible', url)).toBe('private');
		}
	});
	it('allows a public host that merely starts with allowed octets', () => {
		// 172.32.x is outside the private 172.16–31 range — must NOT be blocked.
		expect(validateBaseUrl('openai_compatible', 'http://172.32.0.1/v1')).toBeNull();
		expect(validateBaseUrl('openai_compatible', 'https://api.x.ai/v1')).toBeNull();
	});
});

describe('costPerMillion', () => {
	it('formats per-token cost as $/1M tokens', () => {
		expect(costPerMillion(0.0000015)).toBe('$1.50/1M');
	});
	it('renders a dash for zero/unset', () => {
		expect(costPerMillion(0)).toBe('—');
	});
});

describe('type guards', () => {
	const provider: LLMProvider = { id: 1, name: 'groq-main', kind: 'groq', enabled: true };
	it('isProvider accepts a real row and rejects junk', () => {
		expect(isProvider(provider)).toBe(true);
		expect(isProvider(null)).toBe(false);
		expect(isProvider({ name: 'x' })).toBe(false);
		// kind is any litellm provider now, not the fixed six — a catalog-only kind is accepted,
		// but an empty kind is still junk.
		expect(isProvider({ ...provider, kind: 'vertex_ai' })).toBe(true);
		expect(isProvider({ ...provider, kind: '' })).toBe(false);
		expect(isProvider({ ...provider, kind: '   ' })).toBe(false);
	});
	it('isModel accepts a real row and rejects junk', () => {
		expect(
			isModel({
				id: 1, provider_id: 1, alias: 'fast', upstream: 'groq/llama',
				input_cost_per_token: 0, output_cost_per_token: 0, enabled: true
			})
		).toBe(true);
		expect(isModel({ id: 1 })).toBe(false);
		// missing cost fields → rejected
		expect(isModel({ id: 1, provider_id: 1, alias: 'x', upstream: 'y', enabled: true })).toBe(false);
	});
});

describe('spend (CONSOLE-INFER-#9)', () => {
	it('isSpend accepts a real row and rejects junk', () => {
		expect(
			isSpend({
				model: 'groq-llama', spend: 0.004, total_tokens: 420,
				prompt_tokens: 300, completion_tokens: 120, requests: 2
			})
		).toBe(true);
		expect(isSpend(null)).toBe(false);
		expect(isSpend({ model: 'x' })).toBe(false);
		// empty alias and non-finite numbers must be rejected at the trust boundary
		const base = { spend: 0.004, total_tokens: 420, prompt_tokens: 300, completion_tokens: 120, requests: 2 };
		expect(isSpend({ ...base, model: '' })).toBe(false);
		expect(isSpend({ ...base, model: '  ' })).toBe(false);
		expect(isSpend({ ...base, model: 'groq', spend: Number.NaN })).toBe(false);
		expect(isSpend({ ...base, model: 'groq', total_tokens: Number.POSITIVE_INFINITY })).toBe(false);
		// negative metrics and fractional counts are malformed too
		expect(isSpend({ ...base, model: 'groq', spend: -0.01 })).toBe(false);
		expect(isSpend({ ...base, model: 'groq', total_tokens: -1 })).toBe(false);
		expect(isSpend({ ...base, model: 'groq', prompt_tokens: 1.5 })).toBe(false);
		expect(isSpend({ ...base, model: 'groq', requests: 0.5 })).toBe(false);
	});

	it('indexSpendByModel keys rows by alias', () => {
		const idx = indexSpendByModel([
			{ model: 'groq-llama', spend: 0.004, total_tokens: 420, prompt_tokens: 300, completion_tokens: 120, requests: 2 }
		]);
		expect(idx.get('groq-llama')?.requests).toBe(2);
		expect(idx.get('absent')).toBeUndefined();
	});

	it('formatUSD shows a dash for zero and dollars otherwise', () => {
		expect(formatUSD(0)).toBe('—');
		expect(formatUSD(0.004)).toBe('$0.0040');
		expect(formatUSD(12.5)).toBe('$12.50');
	});

	it('formatTokens groups thousands and dashes zero', () => {
		expect(formatTokens(0)).toBe('—');
		expect(formatTokens(420)).toBe('420');
		expect(formatTokens(1234567)).toBe('1,234,567');
	});

	it('SPEND_PERIODS offers 24h/7d/30d/Tudo, with Tudo meaning all-time', () => {
		expect(SPEND_PERIODS.map((p) => p.days)).toEqual([1, 7, 30, null]);
	});
});

describe('llm catalog', () => {
	const groq: CatalogEntry = {
		upstream: 'groq/llama-3.3-70b-versatile', provider: 'groq',
		input_cost_per_token: 5.9e-7, output_cost_per_token: 7.9e-7, max_tokens: 32768, mode: 'chat'
	};
	const gemini: CatalogEntry = {
		upstream: 'gemini/gemini-2.0-flash', provider: 'gemini',
		input_cost_per_token: 1e-7, output_cost_per_token: 4e-7, max_tokens: 8192, mode: 'chat'
	};
	const catalog = [gemini, groq];

	it('isCatalogEntry accepts a well-formed row and rejects junk', () => {
		expect(isCatalogEntry(groq)).toBe(true);
		expect(isCatalogEntry({ upstream: 'x' })).toBe(false);
		expect(isCatalogEntry(null)).toBe(false);
		expect(isCatalogEntry({ ...groq, input_cost_per_token: 'free' })).toBe(false);
	});

	it('isCatalogEntry rejects missing mandatory fields and non-finite/negative numerics', () => {
		const { max_tokens, mode, ...noMaxNoMode } = groq;
		void max_tokens; void mode;
		expect(isCatalogEntry(noMaxNoMode)).toBe(false); // max_tokens + mode are mandatory
		expect(isCatalogEntry({ ...groq, mode: 'embedding' })).toBe(false); // only chat is valid
		expect(isCatalogEntry({ ...groq, input_cost_per_token: NaN })).toBe(false);
		expect(isCatalogEntry({ ...groq, output_cost_per_token: Infinity })).toBe(false);
		expect(isCatalogEntry({ ...groq, input_cost_per_token: -1e-7 })).toBe(false);
		expect(isCatalogEntry({ ...groq, max_tokens: -1 })).toBe(false);
		expect(isCatalogEntry({ ...groq, upstream: '   ' })).toBe(false); // whitespace-only
		expect(isCatalogEntry({ ...groq, provider: '  ' })).toBe(false);
	});

	it('filterCatalog matches upstream or provider, case-insensitively', () => {
		expect(filterCatalog(catalog, 'GROQ').map((e) => e.upstream)).toEqual([groq.upstream]);
		expect(filterCatalog(catalog, 'flash').map((e) => e.upstream)).toEqual([gemini.upstream]);
		expect(filterCatalog(catalog, 'llama').map((e) => e.upstream)).toEqual([groq.upstream]);
	});

	it('filterCatalog caps the list when the query is empty', () => {
		const many = Array.from({ length: 200 }, (_, i) => ({ ...groq, upstream: `m/${i}` }));
		expect(filterCatalog(many, '', 50)).toHaveLength(50);
	});

	it('catalogKinds lists distinct providers sorted, with openai_compatible last and de-duped', () => {
		expect(catalogKinds(catalog)).toEqual(['gemini', 'groq', 'openai_compatible']);
		// empty catalog still offers the BYO option
		expect(catalogKinds([])).toEqual(['openai_compatible']);
		// a catalog that already carries openai_compatible must not duplicate it
		const withCompat = [...catalog, { ...groq, provider: 'openai_compatible' }];
		expect(catalogKinds(withCompat)).toEqual(['gemini', 'groq', 'openai_compatible']);
	});

	it('catalogKindFor maps a known litellm provider to our enum, else null', () => {
		expect(catalogKindFor('groq')).toBe('groq');
		expect(catalogKindFor('anthropic')).toBe('anthropic');
		expect(catalogKindFor('bedrock')).toBeNull();
		expect(catalogKindFor('')).toBeNull();
	});

	it('applyCatalogPick fills upstream + costs and auto-selects the unique matching provider', () => {
		const providers: LLMProvider[] = [
			{ id: 1, name: 'my-groq', kind: 'groq', enabled: true },
			{ id: 2, name: 'my-gemini', kind: 'gemini', enabled: true }
		];
		const hit = applyCatalogPick(groq.upstream, catalog, providers);
		expect(hit).toEqual({
			upstream: groq.upstream,
			input_cost_per_token: 5.9e-7,
			output_cost_per_token: 7.9e-7,
			provider_id: 1
		});
	});

	it('applyCatalogPick leaves provider_id null when the kind is ambiguous or absent', () => {
		const two: LLMProvider[] = [
			{ id: 1, name: 'groq-a', kind: 'groq', enabled: true },
			{ id: 2, name: 'groq-b', kind: 'groq', enabled: true }
		];
		expect(applyCatalogPick(groq.upstream, catalog, two)?.provider_id).toBeNull();
		expect(applyCatalogPick(groq.upstream, catalog, [])?.provider_id).toBeNull();
	});

	it('applyCatalogPick does not auto-select a disabled provider even when it is the only match', () => {
		const disabled: LLMProvider[] = [{ id: 1, name: 'my-groq', kind: 'groq', enabled: false }];
		const hit = applyCatalogPick(groq.upstream, catalog, disabled);
		expect(hit?.provider_id).toBeNull(); // still fills costs, just not the disabled provider
		expect(hit?.input_cost_per_token).toBe(5.9e-7);
	});

	it('applyCatalogPick returns null for a manual upstream not in the catalog (manual entry stays valid)', () => {
		expect(applyCatalogPick('my/custom-model', catalog, [])).toBeNull();
		expect(applyCatalogPick('', catalog, [])).toBeNull();
	});
});
