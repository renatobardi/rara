// Worker → LLM model binding. The chosen model is stored as LITELLM_MODEL in the
// worker's providers.env (no new schema). These pure helpers let the Worker form
// pick the alias from a dropdown instead of hand-editing the raw env JSON, while
// preserving every other env key. Kept framework-free for vitest (mirrors inferencia.ts).
import type { LLMModel } from './inferencia';

export const MODEL_ENV_KEY = 'LITELLM_MODEL';

// Capabilities that do deterministic work and never call an LLM. Mirrors
// rara-core/seed.go (collectors, transcribe, extract carry no LITELLM_MODEL).
// A denylist, not an LLM allowlist: any new LLM worker (reason/hone/…) gets the
// Model field automatically, while pure collectors stay clean. An existing
// binding always wins regardless of capability.
export const NON_LLM_CAPABILITIES = new Set(['coletar', 'transcrever', 'extrair']);

// Whether the Model field is relevant for a worker: it already has a binding, or
// its capability isn't a known deterministic (non-LLM) one. Empty capability =
// nothing chosen yet, so hidden.
export function usesModel(capability: string, env?: Record<string, string>): boolean {
	if (currentAlias(env) !== '') return true;
	return capability !== '' && !NON_LLM_CAPABILITIES.has(capability);
}

// Whether a save must be blocked because the operator can't pick a Model they need.
// True only when the worker is LLM-capable *by capability*, the /api/llm-models fetch
// failed (so the dropdown can't render), and there's no existing binding to preserve.
// This is the one case where an empty Model is a silent data-loss bug rather than a
// deliberate choice: a non-LLM worker, an already-bound worker, or a successful (even if
// empty) models load all return false and keep the field optional as before. Pairs with
// [[usesModel]] — distinguishes "fetch failed" from "no models exist".
export function blocksOnModelLoadFailure(
	capability: string,
	env: Record<string, string> | undefined,
	modelsLoadFailed: boolean
): boolean {
	return modelsLoadFailed && currentAlias(env) === '' && usesModel(capability);
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
