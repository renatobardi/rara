import { describe, it, expect } from 'vitest';
import { labelDecidedBy, aggregatePulso, latestDeferReason, signalForKey, diffProfile, sourceUrl, filterQuarantine, isDiffEmpty, type ItemDecision, type FilterState } from './curadoria';

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

describe('diffProfile', () => {
	it('adds items present in proposed but absent in active (string array field)', () => {
		const active  = { topics: ['go', 'rust'], authors: [], anti_topics: [], weights: {} };
		const proposed = { topics: ['go', 'rust', 'python'], authors: [], anti_topics: [], weights: {} };
		const diff = diffProfile(active, proposed);
		expect(diff.topics.added).toEqual(['python']);
		expect(diff.topics.removed).toEqual([]);
		expect(diff.topics.changed).toEqual([]);
	});

	it('removes items present in active but absent in proposed (string array field)', () => {
		const active   = { topics: ['go', 'rust'], authors: [], anti_topics: [], weights: {} };
		const proposed = { topics: ['go'], authors: [], anti_topics: [], weights: {} };
		const diff = diffProfile(active, proposed);
		expect(diff.topics.removed).toEqual(['rust']);
		expect(diff.topics.added).toEqual([]);
	});

	it('reports no diff when string arrays are equal', () => {
		const profile = { topics: ['go', 'rust'], authors: [], anti_topics: [], weights: {} };
		const diff = diffProfile(profile, profile);
		expect(diff.topics.added).toEqual([]);
		expect(diff.topics.removed).toEqual([]);
		expect(diff.topics.changed).toEqual([]);
	});

	it('diffs weights object: added key', () => {
		const active   = { topics: [], authors: [], anti_topics: [], weights: { keep_threshold: 0.6 } };
		const proposed = { topics: [], authors: [], anti_topics: [], weights: { keep_threshold: 0.6, boost: 1.2 } };
		const diff = diffProfile(active, proposed);
		expect(diff.weights.added).toEqual([{ key: 'boost', value: 1.2 }]);
		expect(diff.weights.removed).toEqual([]);
		expect(diff.weights.changed).toEqual([]);
	});

	it('diffs weights object: removed key', () => {
		const active   = { topics: [], authors: [], anti_topics: [], weights: { keep_threshold: 0.6, boost: 1.2 } };
		const proposed = { topics: [], authors: [], anti_topics: [], weights: { keep_threshold: 0.6 } };
		const diff = diffProfile(active, proposed);
		expect(diff.weights.removed).toEqual([{ key: 'boost', value: 1.2 }]);
		expect(diff.weights.added).toEqual([]);
	});

	it('diffs weights object: changed value', () => {
		const active   = { topics: [], authors: [], anti_topics: [], weights: { keep_threshold: 0.6 } };
		const proposed = { topics: [], authors: [], anti_topics: [], weights: { keep_threshold: 0.8 } };
		const diff = diffProfile(active, proposed);
		expect(diff.weights.changed).toEqual([{ key: 'keep_threshold', from: 0.6, to: 0.8 }]);
	});

	it('handles null/undefined fields on either side (absent field → treat as empty)', () => {
		const active   = { topics: ['go'], authors: undefined as unknown as string[], anti_topics: [], weights: {} };
		const proposed = { topics: undefined as unknown as string[], authors: [], anti_topics: ['spam'], weights: {} };
		const diff = diffProfile(active, proposed);
		expect(diff.topics.removed).toEqual(['go']);
		expect(diff.topics.added).toEqual([]);
		expect(diff.anti_topics.added).toEqual(['spam']);
	});

	it('handles null fields (null → treat as empty, same as undefined)', () => {
		const active   = { topics: null as unknown as string[], authors: ['alice'], anti_topics: [], weights: {} };
		const proposed = { topics: ['go'], authors: null as unknown as string[], anti_topics: [], weights: {} };
		const diff = diffProfile(active, proposed);
		expect(diff.topics.added).toEqual(['go']);
		expect(diff.topics.removed).toEqual([]);
		expect(diff.authors.removed).toEqual(['alice']);
		expect(diff.authors.added).toEqual([]);
	});

	it('falls back gracefully when a field has unexpected shape (not array / not object)', () => {
		const active   = { topics: 'not-an-array' as unknown as string[], authors: [], anti_topics: [], weights: 42 as unknown as Record<string, unknown> };
		const proposed = { topics: ['go'], authors: [], anti_topics: [], weights: {} };
		const diff = diffProfile(active, proposed);
		// fallback: topics.fallback=true, no throws
		expect(diff.topics.fallback).toBe(true);
		expect(diff.weights.fallback).toBe(true);
	});

	it('falls back when array contains non-string elements (heterogeneous array)', () => {
		const active   = { topics: [42, 'go'] as unknown as string[], authors: [], anti_topics: [], weights: {} };
		const proposed = { topics: ['go', 'rust'], authors: [], anti_topics: [], weights: {} };
		const diff = diffProfile(active, proposed);
		expect(diff.topics.fallback).toBe(true);
	});

	it('falls back on sparse array (holes that every() would skip)', () => {
		// eslint-disable-next-line no-sparse-arrays
		const sparse = [, 'go'] as unknown as string[];
		const active   = { topics: sparse, authors: [], anti_topics: [], weights: {} };
		const proposed = { topics: ['go'], authors: [], anti_topics: [], weights: {} };
		const diff = diffProfile(active, proposed);
		expect(diff.topics.fallback).toBe(true);
	});

	it('diffs weights changed correctly when objects have same keys in different order', () => {
		const active   = { topics: [], authors: [], anti_topics: [], weights: { b: 2, a: 1 } };
		const proposed = { topics: [], authors: [], anti_topics: [], weights: { a: 1, b: 2 } };
		const diff = diffProfile(active, proposed);
		// same content, different key order — must NOT appear as changed
		expect(diff.weights.changed).toEqual([]);
	});
});

describe('sourceUrl', () => {
  it('youtube: constrói URL de watch a partir do video id', () => {
    expect(sourceUrl('youtube', 'dQw4w9WgXcQ')).toBe('https://www.youtube.com/watch?v=dQw4w9WgXcQ');
  });
  it('linkedin: source_ref já é a URL, retorna direto', () => {
    expect(sourceUrl('linkedin', 'https://linkedin.com/posts/foo-123')).toBe('https://linkedin.com/posts/foo-123');
  });
  it('news: source_ref já é a URL, retorna direto', () => {
    expect(sourceUrl('news', 'https://example.com/article')).toBe('https://example.com/article');
  });
  it('podcast: guid não é URL navegável, retorna null', () => {
    expect(sourceUrl('podcast', 'urn:uuid:abc-123')).toBeNull();
  });
  it('email: message-id não é URL, retorna null', () => {
    expect(sourceUrl('email', '<msg-id@mail>')).toBeNull();
  });
  it('lane desconhecido: retorna null', () => {
    expect(sourceUrl('unknown-lane', 'ref')).toBeNull();
  });
  it('linkedin: rejeita scheme javascript:', () => {
    expect(sourceUrl('linkedin', 'javascript:alert(1)')).toBeNull();
  });
  it('news: rejeita scheme data:', () => {
    expect(sourceUrl('news', 'data:text/html,<script>alert(1)</script>')).toBeNull();
  });
  it('youtube: encoda video id com caracteres especiais', () => {
    expect(sourceUrl('youtube', 'abc+def')).toBe('https://www.youtube.com/watch?v=abc%2Bdef');
  });
});

const baseItem = (id: number, overrides: Partial<{ lane: string; channel: string; published_at: string }>): import('./curadoria').QuarantineItem => ({
  id,
  lane: overrides.lane ?? 'youtube',
  source_ref: `ref${id}`,
  status: 'quarantine',
  channel: overrides.channel ?? 'ChannelA',
  published_at: overrides.published_at ?? '2026-06-01T00:00:00Z',
});

const defaultFilter = (): FilterState => ({ dateFrom: '', dateTo: '', tipo: '', canal: '', sortDir: 'newest' });

describe('filterQuarantine', () => {
  const items = [
    baseItem(1, { lane: 'youtube', channel: 'ChannelA', published_at: '2026-06-01T00:00:00Z' }),
    baseItem(2, { lane: 'podcast', channel: 'FeedB',    published_at: '2026-06-10T00:00:00Z' }),
    baseItem(3, { lane: 'youtube', channel: 'ChannelC', published_at: '2026-06-20T00:00:00Z' }),
  ];

  it('sem filtros: retorna todos em ordem mais recente (padrão newest)', () => {
    const result = filterQuarantine(items, defaultFilter());
    expect(result.map(i => i.id)).toEqual([3, 2, 1]);
  });
  it('sortDir oldest: retorna em ordem mais antigo primeiro', () => {
    const result = filterQuarantine(items, { ...defaultFilter(), sortDir: 'oldest' });
    expect(result.map(i => i.id)).toEqual([1, 2, 3]);
  });
  it('filtro tipo (lane): retorna só youtube', () => {
    const result = filterQuarantine(items, { ...defaultFilter(), tipo: 'youtube' });
    expect(result.map(i => i.id)).toEqual([3, 1]);
  });
  it('filtro canal: case-insensitive substring match', () => {
    const result = filterQuarantine(items, { ...defaultFilter(), canal: 'channela' });
    expect(result.map(i => i.id)).toEqual([1]);
  });
  it('filtro dateFrom: exclui itens anteriores', () => {
    const result = filterQuarantine(items, { ...defaultFilter(), dateFrom: '2026-06-10' });
    expect(result.map(i => i.id)).toEqual([3, 2]);
  });
  it('filtro dateTo: exclui itens posteriores', () => {
    const result = filterQuarantine(items, { ...defaultFilter(), dateTo: '2026-06-10' });
    expect(result.map(i => i.id)).toEqual([2, 1]);
  });
  it('item sem published_at: mantido quando sem filtro de data', () => {
    const noDate = { ...baseItem(4, {}), published_at: undefined };
    const result = filterQuarantine([noDate], defaultFilter());
    expect(result).toHaveLength(1);
  });
  it('item sem published_at: excluído quando há filtro de data', () => {
    const noDate = { ...baseItem(4, {}), published_at: undefined };
    const result = filterQuarantine([noDate], { ...defaultFilter(), dateFrom: '2026-06-01' });
    expect(result).toHaveLength(0);
  });
});

describe('isDiffEmpty', () => {
  it('retorna true quando nenhum campo tem diferença', () => {
    const diff = diffProfile(
      { topics: ['go'], authors: [], anti_topics: [], weights: {} },
      { topics: ['go'], authors: [], anti_topics: [], weights: {} }
    );
    expect(isDiffEmpty(diff)).toBe(true);
  });
  it('retorna false quando há item adicionado', () => {
    const diff = diffProfile(
      { topics: ['go'], authors: [], anti_topics: [], weights: {} },
      { topics: ['go', 'rust'], authors: [], anti_topics: [], weights: {} }
    );
    expect(isDiffEmpty(diff)).toBe(false);
  });
  it('retorna false quando há item removido', () => {
    const diff = diffProfile(
      { topics: ['go', 'rust'], authors: [], anti_topics: [], weights: {} },
      { topics: ['go'], authors: [], anti_topics: [], weights: {} }
    );
    expect(isDiffEmpty(diff)).toBe(false);
  });
  it('retorna false quando peso foi alterado', () => {
    const diff = diffProfile(
      { topics: [], authors: [], anti_topics: [], weights: { keep_threshold: 0.6 } },
      { topics: [], authors: [], anti_topics: [], weights: { keep_threshold: 0.8 } }
    );
    expect(isDiffEmpty(diff)).toBe(false);
  });
  it('retorna true para diff com fallback em todos os campos (não tem como confirmar)', () => {
    // fallback significa formato inesperado — não há diff computável, tratamos como "vazio"
    const diff = diffProfile(
      { topics: 'not-array' as unknown as string[], authors: [], anti_topics: [], weights: {} },
      { topics: ['go'], authors: [], anti_topics: [], weights: {} }
    );
    // topics.fallback=true, outros campos sem diff → isDiffEmpty deve retornar true
    // (conservador: não bloquear save quando diff não puder ser computado)
    expect(isDiffEmpty(diff)).toBe(true);
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
