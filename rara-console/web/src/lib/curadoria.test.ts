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
});

describe('aggregatePulso', () => {
	const decisions = [
		{ decision: 'keep', decided_by: 'rules' },
		{ decision: 'keep', decided_by: 'profile' },
		{ decision: 'drop', decided_by: 'llm-judge' },
		{ decision: 'defer', decided_by: 'rules' },
	];

	it('counts all decisions as entrou', () => {
		expect(aggregatePulso(decisions, []).entrou).toBe(4);
	});
	it('counts keep decisions as manteve', () => {
		expect(aggregatePulso(decisions, []).manteve).toBe(2);
	});
	it('counts drop decisions as barrou', () => {
		expect(aggregatePulso(decisions, []).barrou).toBe(1);
	});
	it('counts defer decisions as duvida', () => {
		expect(aggregatePulso(decisions, []).duvida).toBe(1);
	});
	it('proposedPending is true when a proposed version exists', () => {
		const versions = [{ status: 'active' }, { status: 'proposed' }];
		expect(aggregatePulso([], versions).proposedPending).toBe(true);
	});
	it('proposedPending is false when no proposed version exists', () => {
		const versions = [{ status: 'active' }, { status: 'superseded' }];
		expect(aggregatePulso([], versions).proposedPending).toBe(false);
	});
	it('handles empty inputs without throwing', () => {
		expect(() => aggregatePulso([], [])).not.toThrow();
	});
});
