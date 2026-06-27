export type StringDiff = { added: string[]; removed: string[]; changed: never[]; fallback?: false };
export type StringDiffFallback = { added: []; removed: []; changed: never[]; fallback: true };
export type WeightEntry = { key: string; value: unknown };
export type WeightChange = { key: string; from: unknown; to: unknown };
export type WeightDiff = { added: WeightEntry[]; removed: WeightEntry[]; changed: WeightChange[]; fallback?: false };
export type WeightDiffFallback = { added: []; removed: []; changed: []; fallback: true };

export type ProfileDiff = {
	topics: StringDiff | StringDiffFallback;
	authors: StringDiff | StringDiffFallback;
	anti_topics: StringDiff | StringDiffFallback;
	weights: WeightDiff | WeightDiffFallback;
};

type ProfileLike = {
	topics?: unknown;
	authors?: unknown;
	anti_topics?: unknown;
	weights?: unknown;
};

const allStrings = (arr: unknown[]): arr is string[] => arr.every((x) => typeof x === 'string');

function diffStringArray(a: unknown, b: unknown): StringDiff | StringDiffFallback {
	const fb = { added: [] as [], removed: [] as [], changed: [] as never[], fallback: true as const };
	if (!Array.isArray(a) && a != null) return fb;
	if (!Array.isArray(b) && b != null) return fb;
	const arrA = Array.isArray(a) ? a : [];
	const arrB = Array.isArray(b) ? b : [];
	if (!allStrings(arrA) || !allStrings(arrB)) return fb;
	const setA = new Set<string>(arrA);
	const setB = new Set<string>(arrB);
	return {
		added: [...setB].filter((x) => !setA.has(x)).sort((a, b) => a.localeCompare(b)),
		removed: [...setA].filter((x) => !setB.has(x)).sort((a, b) => a.localeCompare(b)),
		changed: [],
	};
}

function stableStringify(v: unknown): string {
	if (v === null || typeof v !== 'object' || Array.isArray(v)) return JSON.stringify(v);
	const obj = v as Record<string, unknown>;
	return '{' + Object.keys(obj).sort().map((k) => JSON.stringify(k) + ':' + stableStringify(obj[k])).join(',') + '}';
}

function diffWeights(a: unknown, b: unknown): WeightDiff | WeightDiffFallback {
	const isObj = (v: unknown): v is Record<string, unknown> =>
		v != null && typeof v === 'object' && !Array.isArray(v);
	if (!isObj(a) && a != null) return { added: [], removed: [], changed: [], fallback: true };
	if (!isObj(b) && b != null) return { added: [], removed: [], changed: [], fallback: true };
	const objA = isObj(a) ? a : {};
	const objB = isObj(b) ? b : {};
	const keysA = new Set(Object.keys(objA));
	const keysB = new Set(Object.keys(objB));
	return {
		added: [...keysB].filter((k) => !keysA.has(k)).sort((a, b) => a.localeCompare(b)).map((k) => ({ key: k, value: objB[k] })),
		removed: [...keysA].filter((k) => !keysB.has(k)).sort((a, b) => a.localeCompare(b)).map((k) => ({ key: k, value: objA[k] })),
		changed: [...keysA].filter((k) => keysB.has(k) && stableStringify(objA[k]) !== stableStringify(objB[k]))
			.sort((a, b) => a.localeCompare(b))
			.map((k) => ({ key: k, from: objA[k], to: objB[k] })),
	};
}

export function diffProfile(active: ProfileLike, proposed: ProfileLike): ProfileDiff {
	return {
		topics: diffStringArray(active.topics, proposed.topics),
		authors: diffStringArray(active.authors, proposed.authors),
		anti_topics: diffStringArray(active.anti_topics, proposed.anti_topics),
		weights: diffWeights(active.weights, proposed.weights),
	};
}

export type Decision = { decision: string; decided_by?: string | null; reason?: string | null; when?: string };
export type ItemDecision = { id?: number; decision: 'keep' | 'drop' | 'defer'; decided_by?: string | null; reason?: string | null; score?: number | null };
export type DeferReason = { score?: number | null; decided_by?: string | null; reason?: string | null };
export type QuarantineItem = { id: number; lane: string; source_ref?: string; status: string; title?: string; channel?: string; summary?: string; published_at?: string };
export type ProfileVersion = { status: string };
export type PulsoCounts = {
	entrou: number;
	manteve: number;
	barrou: number;
	duvida: number;
	proposedPending: boolean;
};

const DECIDED_BY_LABELS: Record<string, string> = {
	rules: 'regras',
	profile: 'perfil',
	'llm-judge': 'llm',
};

// Returns a PT-BR label for a decided_by value. Treats the set as open — unknown
// or absent values become "outro" so new engine strategies don't break the UI.
export function labelDecidedBy(decidedBy?: string | null): string {
	return DECIDED_BY_LABELS[decidedBy ?? ''] ?? 'outro';
}

// Returns score/decided_by/reason from the most recent defer decision (highest id), or null.
export function latestDeferReason(decisions: ItemDecision[]): DeferReason | null {
	const defers = decisions.filter((d) => d.decision === 'defer');
	if (!defers.length) return null;
	const latest = defers.reduce((a, b) => (b.id !== undefined && (a.id === undefined || b.id > a.id) ? b : a), defers[0]);
	return { score: latest.score, decided_by: latest.decided_by, reason: latest.reason };
}

// Maps keyboard key to quarantine review signal, or null for unhandled keys.
export function signalForKey(key: string): 'up' | 'down' | null {
	if (key === 'ArrowRight') return 'up';
	if (key === 'ArrowLeft') return 'down';
	return null;
}

// Aggregates decisions within the last 24h + profile versions into Pulso counts.
// Pass `now` (ms epoch) in tests for determinism; defaults to Date.now() in production.
// Decisions without a `when` timestamp are excluded (defensive: unknown age).
export function aggregatePulso(
	decisions: Decision[],
	versions: ProfileVersion[],
	now = Date.now()
): PulsoCounts {
	const cutoff = now - 24 * 60 * 60 * 1000;
	const recent = decisions.filter((d) => d.when != null && Date.parse(d.when) >= cutoff);
	return {
		entrou: recent.length,
		manteve: recent.filter((d) => d.decision === 'keep').length,
		barrou: recent.filter((d) => d.decision === 'drop').length,
		duvida: recent.filter((d) => d.decision === 'defer').length,
		proposedPending: versions.some((v) => v.status === 'proposed'),
	};
}
