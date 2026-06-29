import { describe, it, expect } from 'vitest';
import {
	MODEL_ENV_KEY,
	enabledAliases,
	currentAlias,
	envWithoutModel,
	withModelAlias,
	usesModel
} from './workerModel';
import type { LLMModel } from './inferencia';

const model = (alias: string, enabled: boolean): LLMModel => ({
	id: 1,
	provider_id: 1,
	alias,
	upstream: 'groq/' + alias,
	input_cost_per_token: 0,
	output_cost_per_token: 0,
	enabled
});

describe('enabledAliases', () => {
	it('returns only enabled model aliases', () => {
		expect(enabledAliases([model('groq-llama', true), model('off', false), model('gemini', true)]))
			.toEqual(['groq-llama', 'gemini']);
	});
	it('handles empty input', () => {
		expect(enabledAliases([])).toEqual([]);
	});
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
		expect(usesModel('coletar', { LITELLM_MODEL: 'groq-llama' })).toBe(true);
	});
});

describe('currentAlias', () => {
	it('reads the bound alias from env', () => {
		expect(currentAlias({ LITELLM_MODEL: 'groq-llama', FOO: 'bar' })).toBe('groq-llama');
	});
	it('returns empty when unbound or env missing', () => {
		expect(currentAlias({ FOO: 'bar' })).toBe('');
		expect(currentAlias(undefined)).toBe('');
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

describe('withModelAlias', () => {
	it('sets the alias preserving other keys', () => {
		expect(withModelAlias({ FOO: 'bar' }, 'groq-llama'))
			.toEqual({ FOO: 'bar', [MODEL_ENV_KEY]: 'groq-llama' });
	});
	it('overrides an existing alias without touching siblings', () => {
		expect(withModelAlias({ LITELLM_MODEL: 'old', FOO: 'bar' }, 'new'))
			.toEqual({ LITELLM_MODEL: 'new', FOO: 'bar' });
	});
	it('removes the binding when alias is empty', () => {
		expect(withModelAlias({ LITELLM_MODEL: 'old', FOO: 'bar' }, ''))
			.toEqual({ FOO: 'bar' });
	});
	it('does not mutate the input', () => {
		const env = { FOO: 'bar' };
		withModelAlias(env, 'x');
		expect(env).toEqual({ FOO: 'bar' });
	});
});
