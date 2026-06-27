import { describe, it, expect } from 'vitest';
import { labelDecidedBy, aggregatePulso } from './curadoria';

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
