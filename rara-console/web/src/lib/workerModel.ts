// Worker → LLM model binding. The chosen model is stored as LITELLM_MODEL in the
// worker's providers.env (no new schema) as the full "kind/model" upstream string
// (e.g. "groq/llama-3.3-70b-versatile") — no alias indirection. These pure helpers let
// the Worker form pick Provider + Model from two dropdowns instead of hand-editing the
// raw env JSON, while preserving every other env key. Kept framework-free for vitest
// (mirrors inferencia.ts).
import type { CatalogEntry, LLMProvider } from './inferencia';

export const MODEL_ENV_KEY = 'LITELLM_MODEL';

// BYO provider kind: its models aren't in the litellm catalog, so the Model field is free text.
export const BYO_KIND = 'openai_compatible';

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
	if (currentModel(env) !== '') return true;
	return capability !== '' && !NON_LLM_CAPABILITIES.has(capability);
}

// Whether a save must be blocked because the operator can't pick a Model they need.
// True only when the worker is LLM-capable *by capability*, the provider registry isn't
// ready (the providers/catalog fetch failed, so the dropdowns can't render), and there's
// no existing binding to preserve. This is the one case where an empty Model is a silent
// data-loss bug rather than a deliberate choice: a non-LLM worker, an already-bound worker,
// or a ready registry all return false and keep the field optional. Pairs with [[usesModel]].
export function blocksOnModelLoadFailure(
	capability: string,
	env: Record<string, string> | undefined,
	registryNotReady: boolean
): boolean {
	return registryNotReady && currentModel(env) === '' && usesModel(capability);
}

// Full "kind/model" string currently bound to a worker, read from its env (empty = unbound).
export function currentModel(env?: Record<string, string>): string {
	return env?.[MODEL_ENV_KEY] ?? '';
}

// Env minus the model key — what the raw JSON editor shows, so the dropdowns are the
// sole owner of LITELLM_MODEL (no double-editing the same value in two places).
export function envWithoutModel(env?: Record<string, string>): Record<string, string> {
	const { [MODEL_ENV_KEY]: _omit, ...rest } = env ?? {};
	return rest;
}

// Merge the chosen model string back into env, preserving every other key. Empty model
// removes the binding. Does not mutate the input.
export function withModel(env: Record<string, string>, model: string): Record<string, string> {
	const next = { ...env };
	if (model) next[MODEL_ENV_KEY] = model;
	else delete next[MODEL_ENV_KEY];
	return next;
}

// Split a stored LITELLM_MODEL ("kind/model") into its provider kind and model parts so the
// two dropdowns can show the current binding. Splits on the FIRST slash only — model names
// can contain slashes (e.g. "vertex_ai/publishers/meta/llama"). A slashless value (a legacy
// alias) is treated as a bare model with no kind.
export function parseModel(value: string): { kind: string; model: string } {
	const i = value.indexOf('/');
	if (i < 0) return { kind: '', model: value };
	return { kind: value.slice(0, i), model: value.slice(i + 1) };
}

// Compose the stored value from provider kind + model part. Empty when either is missing,
// so an incomplete selection writes no binding. Round-trips with parseModel.
export function composeModel(kind: string, model: string): string {
	return kind && model ? `${kind}/${model}` : '';
}

// Resolve the LITELLM_MODEL string to store from the two selects. Normally "kind/model"; but a
// legacy unscoped binding (parsed kind is empty because the old value had no slash, so `model`
// carries the raw alias) is preserved verbatim so an unrelated save never silently erases it —
// CORR-#5 migrates those properly. Empty when nothing is chosen, or when a provider is picked with
// no model (an intentional unbind).
export function resolveModel(kind: string, model: string): string {
	const m = model.trim();
	return composeModel(kind, m) || (kind === '' ? m : '');
}

// A model identifier (BYO free text or a catalog value) must be a single clean token — no
// whitespace or control chars — so it can't corrupt the generated env file (e.g. a newline
// injecting another variable). Empty is clean (nothing to bind).
export function modelHasInvalidChars(model: string): boolean {
	// Reject any whitespace (incl. newline/CR/tab + unicode spaces) and C0/DEL control chars — the
	// chars that would corrupt the generated env file. A model id is always one contiguous token.
	// Checked by codepoint so no literal control bytes live in the source regex.
	return [...model].some((ch) => {
		const c = ch.codePointAt(0) ?? 0;
		return c <= 0x20 || c === 0x7f || /\s/.test(ch);
	});
}

// Whether the picker can actually produce a saveable model: at least one enabled provider that is
// either BYO (free-text, no catalog needed) or has ≥1 catalog model. A non-empty catalog alone
// isn't enough — its models must belong to an enabled provider's kind, else the only Model dropdown
// the operator sees is empty and an LLM worker would save modelless. Gates registryStatus='ready'.
export function registryReady(providers: LLMProvider[], catalog: CatalogEntry[]): boolean {
	return providers.some(
		(p) => p.enabled && (p.kind === BYO_KIND || catalog.some((c) => c.provider === p.kind))
	);
}

// Model names available for a provider kind: the catalog upstreams whose litellm provider
// matches, with the redundant "kind/" prefix stripped for display (composeModel re-prefixes
// on save). Empty kind → no options.
export function modelsForKind(catalog: CatalogEntry[], kind: string): string[] {
	if (!kind) return [];
	const prefix = `${kind}/`;
	return catalog
		.filter((e) => e.provider === kind)
		.map((e) => (e.upstream.startsWith(prefix) ? e.upstream.slice(prefix.length) : e.upstream));
}
