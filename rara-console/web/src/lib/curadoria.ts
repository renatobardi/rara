export type Decision = { decision: string; decided_by?: string; reason?: string | null };
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
// values become "outro" so new engine strategies don't break the UI.
export function labelDecidedBy(decidedBy: string): string {
	return DECIDED_BY_LABELS[decidedBy] ?? 'outro';
}

// Aggregates a decisions array + profile versions into Pulso counts.
// entrou = all decisions in the window (up to the cap forwarded to /api/decisions).
export function aggregatePulso(decisions: Decision[], versions: ProfileVersion[]): PulsoCounts {
	return {
		entrou: decisions.length,
		manteve: decisions.filter((d) => d.decision === 'keep').length,
		barrou: decisions.filter((d) => d.decision === 'drop').length,
		duvida: decisions.filter((d) => d.decision === 'defer').length,
		proposedPending: versions.some((v) => v.status === 'proposed'),
	};
}
