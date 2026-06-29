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
	type LLMProvider
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
