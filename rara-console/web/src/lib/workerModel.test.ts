import { describe, it, expect } from 'vitest';
import {
	MODEL_ENV_KEY,
	currentModel,
	envWithoutModel,
	withModel,
	usesModel,
	blocksOnModelLoadFailure,
	parseModel,
	composeModel,
	resolveModel,
	modelsForKind,
	modelHasInvalidChars,
	registryReady,
	BYO_KIND
} from './workerModel';
import type { CatalogEntry, LLMProvider } from './inferencia';

const entry = (upstream: string, provider: string): CatalogEntry => ({
	upstream,
	provider,
	input_cost_per_token: 0,
	output_cost_per_token: 0,
	max_tokens: 0,
	mode: 'chat'
});

const provider = (kind: string, enabled: boolean): LLMProvider => ({
	id: 1,
	name: kind + '-prod',
	kind,
	enabled
});

describe('usesModel', () => {
	it('is true for known LLM capabilities', () => {
		expect(usesModel('destilar')).toBe(true);
		expect(usesModel('gate_barato')).toBe(true);
		expect(usesModel('gate_rico')).toBe(true);
	});
	it('is true for new LLM workers not in any hardcoded list (denylist, not allowlist)', () => {
		expect(usesModel('reason')).toBe(true);
		expect(usesModel('hone')).toBe(true);
	});
	it('is false for deterministic capabilities (pure collectors)', () => {
		expect(usesModel('coletar')).toBe(false);
		expect(usesModel('transcrever')).toBe(false);
		expect(usesModel('extrair')).toBe(false);
	});
	it('is false when no capability is chosen yet', () => {
		expect(usesModel('')).toBe(false);
	});
	it('is true for a deterministic capability that already carries a binding', () => {
		expect(usesModel('coletar', { LITELLM_MODEL: 'groq/llama-3.3-70b-versatile' })).toBe(true);
	});
});

describe('blocksOnModelLoadFailure', () => {
	it('blocks an LLM-capable worker with no binding when the registry is not ready', () => {
		expect(blocksOnModelLoadFailure('destilar', undefined, true)).toBe(true);
		expect(blocksOnModelLoadFailure('reason', {}, true)).toBe(true);
	});
	it('does not block when an existing binding can be preserved', () => {
		expect(blocksOnModelLoadFailure('destilar', { LITELLM_MODEL: 'groq/llama-3.3-70b-versatile' }, true)).toBe(false);
	});
	it('does not block when the registry is ready (field stays optional)', () => {
		expect(blocksOnModelLoadFailure('destilar', undefined, false)).toBe(false);
	});
	it('never blocks a non-LLM worker, even when the registry is not ready', () => {
		expect(blocksOnModelLoadFailure('coletar', undefined, true)).toBe(false);
		expect(blocksOnModelLoadFailure('', undefined, true)).toBe(false);
	});
});

describe('currentModel', () => {
	it('reads the bound model string from env', () => {
		expect(currentModel({ LITELLM_MODEL: 'groq/llama-3.3-70b-versatile', FOO: 'bar' }))
			.toBe('groq/llama-3.3-70b-versatile');
	});
	it('returns empty when unbound or env missing', () => {
		expect(currentModel({ FOO: 'bar' })).toBe('');
		expect(currentModel(undefined)).toBe('');
	});
});

describe('envWithoutModel', () => {
	it('drops only the model key, preserves the rest', () => {
		expect(envWithoutModel({ LITELLM_MODEL: 'x', FOO: 'bar', BAZ: 'q' }))
			.toEqual({ FOO: 'bar', BAZ: 'q' });
	});
	it('is a no-op when the key is absent', () => {
		expect(envWithoutModel({ FOO: 'bar' })).toEqual({ FOO: 'bar' });
	});
	it('handles undefined env', () => {
		expect(envWithoutModel(undefined)).toEqual({});
	});
});

describe('withModel', () => {
	it('sets the full model string preserving other keys', () => {
		expect(withModel({ FOO: 'bar' }, 'groq/llama-3.3-70b-versatile'))
			.toEqual({ FOO: 'bar', [MODEL_ENV_KEY]: 'groq/llama-3.3-70b-versatile' });
	});
	it('overrides an existing model without touching siblings', () => {
		expect(withModel({ LITELLM_MODEL: 'groq/old', FOO: 'bar' }, 'gemini/new'))
			.toEqual({ LITELLM_MODEL: 'gemini/new', FOO: 'bar' });
	});
	it('removes the binding when model is empty', () => {
		expect(withModel({ LITELLM_MODEL: 'groq/old', FOO: 'bar' }, ''))
			.toEqual({ FOO: 'bar' });
	});
	it('does not mutate the input', () => {
		const env = { FOO: 'bar' };
		withModel(env, 'groq/x');
		expect(env).toEqual({ FOO: 'bar' });
	});
});

describe('parseModel', () => {
	it('splits a kind/model string on the first slash', () => {
		expect(parseModel('groq/llama-3.3-70b-versatile'))
			.toEqual({ kind: 'groq', model: 'llama-3.3-70b-versatile' });
	});
	it('keeps the remainder intact when the model contains slashes', () => {
		expect(parseModel('vertex_ai/publishers/meta/llama'))
			.toEqual({ kind: 'vertex_ai', model: 'publishers/meta/llama' });
	});
	it('treats a slashless value as a bare model with no kind (legacy alias)', () => {
		expect(parseModel('groq-llama')).toEqual({ kind: '', model: 'groq-llama' });
	});
	it('handles empty input', () => {
		expect(parseModel('')).toEqual({ kind: '', model: '' });
	});
});

describe('composeModel', () => {
	it('joins kind and model with a slash', () => {
		expect(composeModel('groq', 'llama-3.3-70b-versatile')).toBe('groq/llama-3.3-70b-versatile');
	});
	it('returns empty when either part is missing', () => {
		expect(composeModel('', 'llama')).toBe('');
		expect(composeModel('groq', '')).toBe('');
	});
	it('round-trips with parseModel', () => {
		const v = 'groq/llama-3.3-70b-versatile';
		const { kind, model } = parseModel(v);
		expect(composeModel(kind, model)).toBe(v);
	});
});

describe('resolveModel', () => {
	it('composes kind/model when both are chosen', () => {
		expect(resolveModel('groq', 'llama-3.3-70b-versatile')).toBe('groq/llama-3.3-70b-versatile');
	});
	it('preserves a legacy unscoped binding verbatim (no kind) so an unrelated save never erases it', () => {
		expect(resolveModel('', 'groq-llama')).toBe('groq-llama');
	});
	it('unbinds when a provider is chosen but no model (intentional clear)', () => {
		expect(resolveModel('groq', '')).toBe('');
	});
	it('is empty when nothing is chosen', () => {
		expect(resolveModel('', '')).toBe('');
	});
	it('trims the model part', () => {
		expect(resolveModel('groq', '  llama  ')).toBe('groq/llama');
		expect(resolveModel('', '  legacy  ')).toBe('legacy');
	});
});

describe('modelHasInvalidChars', () => {
	it('accepts a clean single-token model id', () => {
		expect(modelHasInvalidChars('llama-3.3-70b-versatile')).toBe(false);
		expect(modelHasInvalidChars('publishers/meta/llama')).toBe(false);
	});
	it('rejects whitespace and control characters (env-file injection guard)', () => {
		expect(modelHasInvalidChars('foo bar')).toBe(true);
		expect(modelHasInvalidChars('foo\nbar')).toBe(true);
		expect(modelHasInvalidChars('foo\tbar')).toBe(true);
		expect(modelHasInvalidChars('foo\u00a0bar')).toBe(true); // non-breaking space
		expect(modelHasInvalidChars('foo\u0000bar')).toBe(true); // NUL control char
	});
	it('treats empty as clean (no binding to validate)', () => {
		expect(modelHasInvalidChars('')).toBe(false);
	});
});

describe('registryReady', () => {
	const catalog = [entry('groq/llama-3.3-70b-versatile', 'groq')];
	it('is ready when an enabled provider has at least one catalog model', () => {
		expect(registryReady([provider('groq', true)], catalog)).toBe(true);
	});
	it('is ready for an enabled BYO provider even with no catalog', () => {
		expect(registryReady([provider(BYO_KIND, true)], [])).toBe(true);
	});
	it('is not ready when the only enabled provider has no catalog model (and is not BYO)', () => {
		expect(registryReady([provider('openai', true)], catalog)).toBe(false);
	});
	it('is not ready when no provider is enabled', () => {
		expect(registryReady([provider('groq', false)], catalog)).toBe(false);
	});
	it('is not ready with empty inputs', () => {
		expect(registryReady([], [])).toBe(false);
	});
});

describe('modelsForKind', () => {
	const catalog = [
		entry('groq/llama-3.3-70b-versatile', 'groq'),
		entry('groq/llama-3.1-8b-instant', 'groq'),
		entry('gemini/gemini-2.0-flash', 'gemini')
	];
	it('returns model names for the matching provider kind, stripped of the kind prefix', () => {
		expect(modelsForKind(catalog, 'groq'))
			.toEqual(['llama-3.3-70b-versatile', 'llama-3.1-8b-instant']);
	});
	it('filters by kind', () => {
		expect(modelsForKind(catalog, 'gemini')).toEqual(['gemini-2.0-flash']);
	});
	it('returns empty for an unknown or empty kind', () => {
		expect(modelsForKind(catalog, 'openai')).toEqual([]);
		expect(modelsForKind(catalog, '')).toEqual([]);
	});
});
