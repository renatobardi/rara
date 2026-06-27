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
	const latest = defers.reduce((a, b) => ((b.id ?? 0) > (a.id ?? 0) ? b : a), defers[0]);
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
