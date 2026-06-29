// Worker → LLM model binding. The chosen model is stored as LITELLM_MODEL in the
// worker's providers.env (no new schema). These pure helpers let the Worker form
// pick the alias from a dropdown instead of hand-editing the raw env JSON, while
// preserving every other env key. Kept framework-free for vitest (mirrors inferencia.ts).
import type { LLMModel } from './inferencia';

export const MODEL_ENV_KEY = 'LITELLM_MODEL';

// Capabilities whose workers actually call an LLM (carry LITELLM_MODEL).
// Mirrors the LITELLM_MODEL placements in rara-core/seed.go. Collectors
// (coletar), transcribe and extract do deterministic work — no LLM.
export const LLM_CAPABILITIES = new Set(['destilar', 'gate_barato', 'gate_rico']);

// Whether the Model field is relevant for a worker: an LLM capability, or an
// existing binding (covers a manually-added LLM worker like reason/hone). Keeps
// the field hidden for pure collectors so they never get a stray LITELLM_MODEL.
export function usesModel(capability: string, env?: Record<string, string>): boolean {
	return LLM_CAPABILITIES.has(capability) || currentAlias(env) !== '';
}

// Aliases of enabled models — the dropdown options.
export function enabledAliases(models: LLMModel[]): string[] {
	return models.filter((m) => m.enabled).map((m) => m.alias);
}

// Alias currently bound to a worker, read from its env (empty = unbound).
export function currentAlias(env?: Record<string, string>): string {
	return env?.[MODEL_ENV_KEY] ?? '';
}

// Env minus the model key — what the raw JSON editor shows, so the dropdown is the
// sole owner of LITELLM_MODEL (no double-editing the same value in two places).
export function envWithoutModel(env?: Record<string, string>): Record<string, string> {
	const { [MODEL_ENV_KEY]: _omit, ...rest } = env ?? {};
	return rest;
}

// Merge the chosen alias back into env, preserving every other key. Empty alias
// removes the binding. Does not mutate the input.
export function withModelAlias(env: Record<string, string>, alias: string): Record<string, string> {
	const next = { ...env };
	if (alias) next[MODEL_ENV_KEY] = alias;
	else delete next[MODEL_ENV_KEY];
	return next;
}
