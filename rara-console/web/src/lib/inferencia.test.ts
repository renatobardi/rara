import { describe, it, expect } from 'vitest';
import {
	asList,
	isProvider,
	isModel,
	maskKey,
	validateBaseUrl,
	costPerMillion,
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
	it('never leaks a full key (only last4 is ever an input)', () => {
		// The read DTO carries key_last4 only; assert masking output stays short.
		const out = maskKey('abcd');
		expect(out).not.toContain('secret');
		expect(out.replace(/[•\s]/g, '')).toBe('abcd');
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
			isModel({ id: 1, provider_id: 1, alias: 'fast', upstream: 'groq/llama', enabled: true })
		).toBe(true);
		expect(isModel({ id: 1 })).toBe(false);
	});
});
