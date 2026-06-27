import { describe, it, expect } from 'vitest';
import { labelDecidedBy, aggregatePulso, latestDeferReason, signalForKey, type ItemDecision } from './curadoria';

describe('labelDecidedBy', () => {
	it('maps known values to PT labels', () => {
		expect(labelDecidedBy('rules')).toBe('regras');
		expect(labelDecidedBy('profile')).toBe('perfil');
		expect(labelDecidedBy('llm-judge')).toBe('llm');
	});
	it('returns "outro" for quarantine_review (open set)', () => {
		expect(labelDecidedBy('quarantine_review')).toBe('outro');
	});
	it('returns "outro" for any unknown future value', () => {
		expect(labelDecidedBy('future-engine')).toBe('outro');
	});
	it('returns "outro" for undefined (decided_by absent)', () => {
		expect(labelDecidedBy(undefined)).toBe('outro');
	});
	it('returns "outro" for null', () => {
		expect(labelDecidedBy(null)).toBe('outro');
	});
});

describe('aggregatePulso', () => {
	// Use a fixed "now" so tests are deterministic — 2026-06-27T20:00:00Z
	const NOW = 1751054400000;
	const recent = (h: number) => new Date(NOW - h * 3600_000).toISOString();

	const decisions = [
		{ decision: 'keep', decided_by: 'rules', when: recent(1) },
		{ decision: 'keep', decided_by: 'profile', when: recent(12) },
		{ decision: 'drop', decided_by: 'llm-judge', when: recent(23) },
		{ decision: 'defer', decided_by: 'rules', when: recent(2) },
	];

	it('counts all recent decisions as entrou', () => {
		expect(aggregatePulso(decisions, [], NOW).entrou).toBe(4);
	});
	it('counts keep decisions as manteve', () => {
		expect(aggregatePulso(decisions, [], NOW).manteve).toBe(2);
	});
	it('counts drop decisions as barrou', () => {
		expect(aggregatePulso(decisions, [], NOW).barrou).toBe(1);
	});
	it('counts defer decisions as duvida', () => {
		expect(aggregatePulso(decisions, [], NOW).duvida).toBe(1);
	});
	it('excludes decisions older than 24h', () => {
		const old = [
			{ decision: 'keep', decided_by: 'rules', when: recent(25) },
			{ decision: 'keep', decided_by: 'rules', when: recent(1) },
		];
		expect(aggregatePulso(old, [], NOW).entrou).toBe(1);
		expect(aggregatePulso(old, [], NOW).manteve).toBe(1);
	});
	it('proposedPending is true when a proposed version exists', () => {
		const versions = [{ status: 'active' }, { status: 'proposed' }];
		expect(aggregatePulso([], versions, NOW).proposedPending).toBe(true);
	});
	it('proposedPending is false when no proposed version exists', () => {
		const versions = [{ status: 'active' }, { status: 'superseded' }];
		expect(aggregatePulso([], versions, NOW).proposedPending).toBe(false);
	});
	it('handles empty inputs without throwing', () => {
		expect(() => aggregatePulso([], [], NOW)).not.toThrow();
	});
});

describe('latestDeferReason', () => {
	it('returns the defer decision when it is the only one', () => {
		const decisions: ItemDecision[] = [{ id: 1, decision: 'defer', decided_by: 'rules', reason: 'low score', score: 0.3 }];
		const result = latestDeferReason(decisions);
		expect(result).toEqual({ score: 0.3, decided_by: 'rules', reason: 'low score' });
	});

	it('picks the defer decision ignoring keep/drop', () => {
		const decisions: ItemDecision[] = [
			{ id: 1, decision: 'keep', decided_by: 'rules', reason: null, score: null },
			{ id: 2, decision: 'defer', decided_by: 'profile', reason: 'borderline', score: 0.5 },
			{ id: 3, decision: 'drop', decided_by: 'llm-judge', reason: 'off-topic', score: 0.1 }
		];
		const result = latestDeferReason(decisions);
		expect(result?.decided_by).toBe('profile');
	});

	it('picks the most recent defer when multiple exist (highest id)', () => {
		const decisions: ItemDecision[] = [
			{ id: 1, decision: 'defer', decided_by: 'rules', reason: 'old', score: 0.4 },
			{ id: 5, decision: 'defer', decided_by: 'profile', reason: 'latest', score: 0.6 }
		];
		const result = latestDeferReason(decisions);
		expect(result?.reason).toBe('latest');
	});

	it('returns null when there is no defer decision', () => {
		const decisions: ItemDecision[] = [
			{ id: 1, decision: 'keep', decided_by: 'rules', reason: null, score: null }
		];
		expect(latestDeferReason(decisions)).toBeNull();
	});

	it('returns null for empty array', () => {
		expect(latestDeferReason([])).toBeNull();
	});

	it('handles absent reason gracefully', () => {
		const decisions = [{ id: 1, decision: 'defer' as const, decided_by: 'rules', score: 0.3 }];
		const result = latestDeferReason(decisions);
		expect(result?.reason).toBeUndefined();
	});

	it('handles decisions without id — falls back to id=0 for all, returns one of them', () => {
		const decisions = [
			{ decision: 'defer' as const, decided_by: 'rules', reason: 'a', score: 0.3 },
			{ decision: 'defer' as const, decided_by: 'profile', reason: 'b', score: 0.5 }
		];
		const result = latestDeferReason(decisions);
		// Both have id=0 via ??, reduce keeps the initial (first), which is 'a'
		expect(result).not.toBeNull();
		expect(result?.reason).toBe('a');
	});
});

describe('signalForKey', () => {
	it('maps ArrowRight to up (Manter)', () => {
		expect(signalForKey('ArrowRight')).toBe('up');
	});

	it('maps ArrowLeft to down (Descartar)', () => {
		expect(signalForKey('ArrowLeft')).toBe('down');
	});

	it('returns null for other keys', () => {
		expect(signalForKey('ArrowUp')).toBeNull();
		expect(signalForKey('Enter')).toBeNull();
		expect(signalForKey('')).toBeNull();
	});
});
