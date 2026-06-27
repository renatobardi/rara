export type Decision = { decision: string; decided_by?: string | null; reason?: string | null; when?: string };
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
