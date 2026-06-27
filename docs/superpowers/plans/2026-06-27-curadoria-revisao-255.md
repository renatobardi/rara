# Curadoria Revisão (Issue #255) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Reestruturar a página Curadoria em três abas (Decidir / Histórico / Ajustes) com renomeações, enriquecimento de dados e UX de "+Nova versão" para o perfil de interesse.

**Architecture:** Backend: enriquecer `ListRecentDecisions` em rara-core com `lane` e `source_ref` do item (JOIN em `items`). Frontend: estado de aba client-side (`activeTab`), filtros client-side para a fila de revisão, helper `sourceUrl(lane, source_ref)` no lib de curadoria, e formulário `+Nova versão` pre-preenchido com validação de diff.

**Tech Stack:** Go 1.26 (rara-core), SvelteKit 5 (rune-based), Vitest, Tailwind CSS, pgx v5.

## Global Constraints

- TDD obrigatório: teste falha primeiro, depois implementação mínima.
- Todo teste em `rara-core` usa `MockDatabase` (zero I/O real). Nunca tocar Neon em teste.
- Frontend: testes em `curadoria.test.ts` usando Vitest. Zero I/O real. Lógica pura no lib, apresentação no Svelte.
- Strings de UI ficam em `strings.ts`. Código, commits e PR em inglês. Conversação e doc de planejamento em pt-BR.
- `make test` deve passar em rara-core antes de qualquer commit Go.
- `cd rara-console/web && npx vitest run` deve passar antes de qualquer commit frontend.
- Sem novos pacotes npm. Sem novos pacotes Go.
- Cada commit é autoria de Renato Bardi.

---

## Mapeamento de Arquivos

| Arquivo | O que muda |
|---------|------------|
| `rara-core/main.go` | Adiciona campos `Lane`, `SourceRef` em `RecentDecision` |
| `rara-core/store_reads.go` | `ListRecentDecisions` faz JOIN em `items` para pegar `lane`, `source_ref` |
| `rara-core/main_test.go` | `MockDatabase.ListRecentDecisions` popula os novos campos; novo teste verifica `Lane`/`SourceRef` |
| `rara-console/web/src/lib/curadoria.ts` | Novo helper `sourceUrl()`, tipo `FilterState`, função `filterQuarantine()`, valida diff antes de propor |
| `rara-console/web/src/lib/curadoria.test.ts` | Testes para `sourceUrl`, `filterQuarantine`, `isDiffEmpty` |
| `rara-console/web/src/lib/strings.ts` | Renomeia rótulos: filaZone, gostoZone, trilhaZone, filaWhyFence, filaDecidedBy, filaReason, filaScore + novos para filtros e abas |
| `rara-console/web/src/routes/curadoria/+page.svelte` | Adiciona tab nav, reestrutura seções, filtros client-side, título como link, +Nova versão |

---

## Task 1: rara-core — Enrichir `RecentDecision` com `lane` e `source_ref`

**Files:**
- Modify: `rara-core/main.go` (struct `RecentDecision`)
- Modify: `rara-core/store_reads.go` (função `ListRecentDecisions`)
- Modify: `rara-core/main_test.go` (`MockDatabase.ListRecentDecisions` + novo teste)

**Interfaces:**
- Produces: `RecentDecision.Lane string`, `RecentDecision.SourceRef string` — ambos `omitempty`, vazios quando item não encontrado.
- Consumers: Task 4 (Histórico tab) lê esses campos para construir link e badge de tipo.

- [ ] **Step 1: Escrever o teste que falha**

Em `rara-core/main_test.go`, adicionar após `TestListRecentDecisionsExposesDecidedByAndReason` (linha ~2299):

```go
// ListRecentDecisions — must expose lane and source_ref from the joined item
func TestListRecentDecisionsExposesLaneAndSourceRef(t *testing.T) {
	ctx := context.Background()
	db := newMockDatabase()
	fid := seedFlow(t, db)
	itemID, err := db.UpsertItem(ctx, Item{Lane: "youtube", SourceRef: "dQw4w9WgXcQ", FlowID: fid, FlowVersion: 1, Status: itemDiscovered})
	if err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}
	if err := db.InsertGateDecision(ctx, GateDecision{ItemID: itemID, Gate: gateBarato, Decision: decisionKeep, DecidedBy: "rules"}); err != nil {
		t.Fatalf("InsertGateDecision: %v", err)
	}
	decs, err := db.ListRecentDecisions(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(decs) != 1 {
		t.Fatalf("want 1 decision, got %d", len(decs))
	}
	if decs[0].Lane != "youtube" {
		t.Errorf("Lane = %q, want %q", decs[0].Lane, "youtube")
	}
	if decs[0].SourceRef != "dQw4w9WgXcQ" {
		t.Errorf("SourceRef = %q, want %q", decs[0].SourceRef, "dQw4w9WgXcQ")
	}
}
```

- [ ] **Step 2: Rodar e confirmar que falha**

```bash
cd rara-core && go test -run TestListRecentDecisionsExposesLaneAndSourceRef -v
```
Esperado: `FAIL` com `decs[0].Lane = "", want "youtube"` (campo não existe ainda).

- [ ] **Step 3: Adicionar campos em `RecentDecision` (`main.go` ~linha 496)**

Substituir:
```go
type RecentDecision struct {
	ID        int      `json:"id"`
	ItemID    int      `json:"item_id"`
	Gate      string   `json:"gate"`
	Decision  string   `json:"decision"`
	Score     *float64 `json:"score,omitempty"`
	When      string   `json:"when"` // RFC3339
	DecidedBy string   `json:"decided_by"`
	Reason    *string  `json:"reason,omitempty"`
}
```

Por:
```go
type RecentDecision struct {
	ID        int      `json:"id"`
	ItemID    int      `json:"item_id"`
	Gate      string   `json:"gate"`
	Decision  string   `json:"decision"`
	Score     *float64 `json:"score,omitempty"`
	When      string   `json:"when"` // RFC3339
	DecidedBy string   `json:"decided_by"`
	Reason    *string  `json:"reason,omitempty"`
	Lane      string   `json:"lane,omitempty"`
	SourceRef string   `json:"source_ref,omitempty"`
}
```

- [ ] **Step 4: Atualizar `MockDatabase.ListRecentDecisions` (`main_test.go` ~linha 1325)**

Substituir o bloco `out = append(out, RecentDecision{...})`:
```go
item := m.itemByID[d.ItemID] // zero value Item{} se não encontrado — Lane/SourceRef ficam ""
out = append(out, RecentDecision{
    ID:        i + 1,
    ItemID:    d.ItemID,
    Gate:      d.Gate,
    Decision:  d.Decision,
    Score:     d.Score,
    When:      "2026-01-01T00:00:00Z",
    DecidedBy: d.DecidedBy,
    Reason:    reason,
    Lane:      item.Lane,
    SourceRef: item.SourceRef,
})
```

- [ ] **Step 5: Rodar e confirmar que o novo teste passa**

```bash
cd rara-core && go test -run TestListRecentDecisions -v
```
Esperado: todos os testes `TestListRecentDecisions*` passam.

- [ ] **Step 6: Atualizar `ListRecentDecisions` em `store_reads.go`**

Substituir a função inteira `ListRecentDecisions` (linha ~501-531) por:

```go
func (d *pgxDatabase) ListRecentDecisions(ctx context.Context, limit int) ([]RecentDecision, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	const q = `
		SELECT gd.id, gd.item_id, gd.gate, gd.decision, gd.score, gd.created_at,
		       gd.decided_by, gd.reason,
		       COALESCE(i.lane, ''), COALESCE(i.source_ref, '')
		FROM gate_decisions gd
		LEFT JOIN items i ON i.id = gd.item_id
		ORDER BY gd.id DESC
		LIMIT $1`
	rows, err := d.conn.Query(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RecentDecision
	for rows.Next() {
		var dec RecentDecision
		var when time.Time
		var reason *string
		if err := rows.Scan(&dec.ID, &dec.ItemID, &dec.Gate, &dec.Decision, &dec.Score, &when, &dec.DecidedBy, &reason, &dec.Lane, &dec.SourceRef); err != nil {
			return nil, fmt.Errorf("scan recent decisions: %w", err)
		}
		dec.When = when.UTC().Format(time.RFC3339)
		dec.Reason = reason
		out = append(out, dec)
	}
	return out, rows.Err()
}
```

- [ ] **Step 7: Rodar suite completa do rara-core**

```bash
cd rara-core && make test
```
Esperado: `PASS` sem erros. (O teste de integração real contra Neon não roda aqui — apenas unit tests com MockDatabase.)

- [ ] **Step 8: Commit**

```bash
cd rara-core
git add main.go main_test.go store_reads.go
git commit -m "feat(core): enrich RecentDecision with lane and source_ref from items join"
```

---

## Task 2: Frontend — Renomear Strings e Adicionar Strings Novas

**Files:**
- Modify: `rara-console/web/src/lib/strings.ts`

**Interfaces:**
- Produces: novos rótulos `t.curadoria.tabDecidir`, `tabHistorico`, `tabAjustes`, `gostoNovaVersaoBtn`, `filterDateFrom`, `filterDateTo`, `filterTipo`, `filterCanal`, `filterSortBy`, `filterSortNewest`, `filterSortOldest`, `filterClear`, `gostoNoDiff`, `trilhaLane`, `trilhaSourceLink`; renomeia `filaZone` → `"Revisar"`, `gostoZone` → `"Ajustes"`, `trilhaZone` → `"Histórico"`, `filaWhyFence` → `"Por que não consegui decidir:"`, `filaDecidedBy` → `"Decidido por"`, `filaReason` → `"Motivo"`, e adiciona `filaScore` → `"Score"`.

- [ ] **Step 1: Localizar o bloco `curadoria` em `strings.ts` (linha ~347) e aplicar as mudanças**

Substituir as linhas correspondentes na seção `curadoria: {`:

```typescript
// ── abas ──────────────────────────────────────────────────────────────
tabDecidir: 'Decidir',
tabHistorico: 'Histórico',
tabAjustes: 'Ajustes',
// ── zona fila (aba Decidir) ───────────────────────────────────────────
filaZone: 'Revisar',                        // era 'Fila de revisão'
filaWhyFence: 'Por que não consegui decidir:', // era 'por que ficou na cerca'
filaDecidedBy: 'Decidido por',              // era 'decidido por' (minúscula)
filaReason: 'Motivo',                       // era 'motivo'
filaScore: 'Score',                         // novo
// ── filtros (aba Decidir) ─────────────────────────────────────────────
filterDateFrom: 'De',
filterDateTo: 'Até',
filterTipo: 'Tipo',
filterCanal: 'Canal',
filterSortBy: 'Ordenar',
filterSortNewest: 'Mais recente',
filterSortOldest: 'Mais antigo',
filterClear: 'Limpar filtros',
// ── zona gosto → ajustes ─────────────────────────────────────────────
gostoZone: 'Ajustes',                       // era 'O gosto'
gostoNovaVersaoBtn: '+ Nova versão',
gostoNoDiff: 'Sem diferença em relação à versão ativa. Altere ao menos um campo para salvar.',
// ── zona trilha → histórico ───────────────────────────────────────────
trilhaZone: 'Histórico',                    // era 'Trilha de decisões'
trilhaLane: 'Tipo',
trilhaSourceLink: 'Fonte',
```

Além disso, manter todas as outras strings existentes inalteradas.

- [ ] **Step 2: Verificar que não há erros de TypeScript**

```bash
cd rara-console/web && npx tsc --noEmit
```
Esperado: sem erros. (Campos novos são adicionados, os antigos mantidos — nenhum consumidor foi ainda atualizado para usar os novos.)

- [ ] **Step 3: Commit**

```bash
cd rara-console
git add web/src/lib/strings.ts
git commit -m "feat(console/curadoria): rename labels and add strings for tabs, filters, nova-versao"
```

---

## Task 3: Frontend — Helpers de Lógica (sourceUrl, filterQuarantine, isDiffEmpty)

**Files:**
- Modify: `rara-console/web/src/lib/curadoria.ts`
- Modify: `rara-console/web/src/lib/curadoria.test.ts`

**Interfaces:**
- Produces:
  - `sourceUrl(lane: string, sourceRef: string): string | null` — retorna URL clicável ou `null` se não mapeável.
  - `type FilterState = { dateFrom: string; dateTo: string; tipo: string; canal: string; sortDir: 'newest' | 'oldest' }` — estado dos filtros.
  - `filterQuarantine(items: QuarantineItem[], f: FilterState): QuarantineItem[]` — aplica filtros e ordenação.
  - `isDiffEmpty(diff: ProfileDiff): boolean` — `true` quando não há nenhuma diferença.
  - `QuarantineItem` ganha `source_url?: string` (campo opcional derivado no componente, não do server).

- [ ] **Step 1: Escrever os testes que falham (`curadoria.test.ts`)**

Adicionar ao final do arquivo:

```typescript
import { sourceUrl, filterQuarantine, isDiffEmpty, type FilterState } from './curadoria';

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
```

- [ ] **Step 2: Rodar e confirmar que os testes falham**

```bash
cd rara-console/web && npx vitest run src/lib/curadoria.test.ts
```
Esperado: `FAIL` com erros de importação (funções não existem ainda).

- [ ] **Step 3: Implementar as funções em `curadoria.ts`**

Adicionar ao final de `curadoria.ts` (antes do `export` de `aggregatePulso`):

```typescript
// Maps (lane, source_ref) to a navigable URL, or null when no URL can be derived.
// YouTube: source_ref is the video ID. LinkedIn/news: source_ref is the URL itself.
// Podcast (GUID) and email (message-id) have no public URL.
export function sourceUrl(lane: string, sourceRef: string): string | null {
  if (lane === 'youtube') return `https://www.youtube.com/watch?v=${sourceRef}`;
  if (lane === 'linkedin' || lane === 'news') return sourceRef;
  return null;
}

export type FilterState = {
  dateFrom: string;   // ISO date string (YYYY-MM-DD) or ''
  dateTo: string;     // ISO date string (YYYY-MM-DD) or ''
  tipo: string;       // lane filter or ''
  canal: string;      // channel substring filter or ''
  sortDir: 'newest' | 'oldest';
};

// Filters and sorts quarantine items client-side. Items without published_at are
// excluded when any date filter is active (unknown age can't satisfy a date constraint).
export function filterQuarantine(items: QuarantineItem[], f: FilterState): QuarantineItem[] {
  const hasDateFilter = f.dateFrom !== '' || f.dateTo !== '';
  let result = items.filter((item) => {
    if (f.tipo && item.lane !== f.tipo) return false;
    if (f.canal && !item.channel?.toLowerCase().includes(f.canal.toLowerCase())) return false;
    if (hasDateFilter) {
      if (!item.published_at) return false;
      const d = item.published_at.slice(0, 10); // 'YYYY-MM-DD'
      if (f.dateFrom && d < f.dateFrom) return false;
      if (f.dateTo && d > f.dateTo) return false;
    }
    return true;
  });
  result = [...result].sort((a, b) => {
    const ta = a.published_at ?? '';
    const tb = b.published_at ?? '';
    return f.sortDir === 'newest' ? tb.localeCompare(ta) : ta.localeCompare(tb);
  });
  return result;
}

// Returns true when a ProfileDiff has no additions, removals, or changes in any field.
// Fallback fields (unexpected format) are treated as empty — we don't block save when
// diff can't be computed.
export function isDiffEmpty(diff: ProfileDiff): boolean {
  const stringEmpty = (d: ProfileDiff['topics']) => d.fallback || (d.added.length === 0 && d.removed.length === 0);
  const weightsEmpty = (d: ProfileDiff['weights']) => d.fallback || (d.added.length === 0 && d.removed.length === 0 && d.changed.length === 0);
  return stringEmpty(diff.topics) && stringEmpty(diff.authors) && stringEmpty(diff.anti_topics) && weightsEmpty(diff.weights);
}
```

- [ ] **Step 4: Rodar e confirmar que todos os testes passam**

```bash
cd rara-console/web && npx vitest run src/lib/curadoria.test.ts
```
Esperado: `PASS` — todos os testes em `curadoria.test.ts`.

- [ ] **Step 5: Commit**

```bash
cd rara-console
git add web/src/lib/curadoria.ts web/src/lib/curadoria.test.ts
git commit -m "feat(console/curadoria): add sourceUrl, filterQuarantine, isDiffEmpty helpers"
```

---

## Task 4: Frontend — Tab Nav + Aba Decidir (título como link, metadados, filtros)

**Files:**
- Modify: `rara-console/web/src/routes/curadoria/+page.svelte`
- Modify: `rara-console/web/src/lib/curadoria.ts` (importar `FilterState` e `filterQuarantine` — já existem após Task 3)

**Interfaces:**
- Consumes: `sourceUrl`, `filterQuarantine`, `FilterState`, `isDiffEmpty` de `curadoria.ts` (Task 3); strings renomeadas de `strings.ts` (Task 2).

**Nota de design da aba Decidir:**
- O `<details>` "por que ficou na cerca" vira uma `<div>` com rótulo sempre visível (sem `<summary>`).
- Score, Decidido por, Motivo ganham primeira letra maiúscula (via strings).
- Título do item é um `<a href={url} target="_blank">` quando `url != null`.
- Abaixo do card do item, aparece metadata: worker (badge de lane), tipo (lane), data/hora (published_at formatada em pt-BR).
- Filtros ficam acima do card herói, collapsíveis com `<details>`.
- A lista de itens filtrada é `filteredQueue` (derivada de `quarantine` + `filterState`).
- `focusedIndex` continua funcionando na lista filtrada.

- [ ] **Step 1: Adicionar imports e estado de tabs/filtros no `<script>` do componente**

No `<script lang="ts">` de `+page.svelte`, adicionar os imports:

```typescript
import { sourceUrl, filterQuarantine, isDiffEmpty, type FilterState } from '$lib/curadoria';
```

E adicionar os estados após os existentes:

```typescript
// --- tab state ---
let activeTab = $state<'decidir' | 'historico' | 'ajustes'>('decidir');

// --- filter state (aba Decidir) ---
let filterState = $state<FilterState>({ dateFrom: '', dateTo: '', tipo: '', canal: '', sortDir: 'newest' });
```

E adicionar a derivada filtrada (substituir o uso direto de `quarantine` pela lista filtrada):

```typescript
let filteredQueue = $derived(filterQuarantine(quarantine, filterState));
```

Trocar todas as referências a `quarantine[focusedIndex]` por `filteredQueue[focusedIndex]` e `quarantine.length` pela `filteredQueue.length` nas seções de fila (mas NÃO no `sendReview` — ele precisa do id do item e opera no array bruto para remover o item).

Dentro de `sendReview`, a linha `quarantine = quarantine.filter(q => q.id !== item.id)` permanece no array bruto — só o display usa `filteredQueue`.

- [ ] **Step 2: Adicionar o menu de abas no template**

Substituir a abertura do template (antes da section do Pulso) para incluir o tab nav. Adicionar ANTES da `<!-- ── 3. FILA DE REVISÃO ──` section:

```svelte
<!-- ── TAB NAV ───────────────────────────────────────────────────────── -->
<div class="mb-6 flex gap-1 border-b border-border">
  {#each ([['decidir', t.curadoria.tabDecidir], ['historico', t.curadoria.tabHistorico], ['ajustes', t.curadoria.tabAjustes]] as const) as [tab, label]}
    <button
      onclick={() => (activeTab = tab)}
      class="px-4 py-2 text-[13px] font-medium transition-colors
        {activeTab === tab
          ? 'border-b-2 border-primary text-primary'
          : 'text-muted hover:text-text'}"
    >{label}</button>
  {/each}
</div>
```

- [ ] **Step 3: Envolver as seções existentes com guards de aba**

Estruturar o template das seções 3, 4, 5, 6 com `{#if activeTab === '...'}`:

```svelte
{#if activeTab === 'decidir'}
  <!-- ── 3. FILA DE REVISÃO (herói) ─── -->
  ... (seção fila, com ajustes do Step 4 abaixo)
{/if}

{#if activeTab === 'historico'}
  <!-- ── 5. TRILHA DE DECISÕES ─── -->
  ... (Task 5, ainda não modificada aqui)
{/if}

{#if activeTab === 'ajustes'}
  <!-- ── 4. O GOSTO ─── -->
  ... (Task 6, ainda não modificada aqui)
  <!-- ── 6. REGRAS DE GATE ─── -->
  ...
{/if}
```

Pulso e Spine ficam fora das abas (acima do tab nav), visíveis sempre.

- [ ] **Step 4: Ajustar a aba Decidir — título como link, metadados, filtros, rename**

Dentro da seção da fila (`activeTab === 'decidir'`), fazer as seguintes mudanças:

**a) Trocar `t.curadoria.filaZone` pelo valor novo** — já propagado via strings (Task 2).

**b) Contador usa `filteredQueue.length` em vez de `quarantine.length`:**
```svelte
{#if !quarantineLoading && !quarantineError && filteredQueue.length > 0}
  <span class="text-[12px] text-muted">{filteredQueue.length} {t.curadoria.filaSubtitle}</span>
```

**c) Filtros collapsíveis antes do card:**
```svelte
<details class="mb-4 text-[13px]">
  <summary class="cursor-pointer text-muted hover:text-text mb-2">{t.curadoria.filterSortBy}</summary>
  <div class="rounded-card border border-border bg-surface p-3 space-y-2">
    <div class="flex flex-wrap gap-3 items-end">
      <div>
        <label class="mb-1 block text-[11px] text-muted" for="f-from">{t.curadoria.filterDateFrom}</label>
        <input id="f-from" type="date" bind:value={filterState.dateFrom}
          class="rounded-token border border-border bg-surface-2 px-2 py-1 text-[12px] focus:outline-none focus:ring-1 focus:ring-primary/50" />
      </div>
      <div>
        <label class="mb-1 block text-[11px] text-muted" for="f-to">{t.curadoria.filterDateTo}</label>
        <input id="f-to" type="date" bind:value={filterState.dateTo}
          class="rounded-token border border-border bg-surface-2 px-2 py-1 text-[12px] focus:outline-none focus:ring-1 focus:ring-primary/50" />
      </div>
      <div>
        <label class="mb-1 block text-[11px] text-muted" for="f-tipo">{t.curadoria.filterTipo}</label>
        <select id="f-tipo" bind:value={filterState.tipo}
          class="rounded-token border border-border bg-surface-2 px-2 py-1 text-[12px] focus:outline-none focus:ring-1 focus:ring-primary/50">
          <option value="">Todos</option>
          {#each [...new Set(quarantine.map(q => q.lane))].sort() as lane}
            <option value={lane}>{lane}</option>
          {/each}
        </select>
      </div>
      <div class="flex-1 min-w-[120px]">
        <label class="mb-1 block text-[11px] text-muted" for="f-canal">{t.curadoria.filterCanal}</label>
        <input id="f-canal" type="text" bind:value={filterState.canal} placeholder="ex: Lex Fridman"
          class="w-full rounded-token border border-border bg-surface-2 px-2 py-1 text-[12px] focus:outline-none focus:ring-1 focus:ring-primary/50" />
      </div>
      <div>
        <label class="mb-1 block text-[11px] text-muted" for="f-sort">{t.curadoria.filterSortBy}</label>
        <select id="f-sort" bind:value={filterState.sortDir}
          class="rounded-token border border-border bg-surface-2 px-2 py-1 text-[12px] focus:outline-none focus:ring-1 focus:ring-primary/50">
          <option value="newest">{t.curadoria.filterSortNewest}</option>
          <option value="oldest">{t.curadoria.filterSortOldest}</option>
        </select>
      </div>
      <button onclick={() => (filterState = { dateFrom: '', dateTo: '', tipo: '', canal: '', sortDir: 'newest' })}
        class="rounded-token border border-border px-3 py-1 text-[12px] text-muted hover:bg-hover">
        {t.curadoria.filterClear}
      </button>
    </div>
  </div>
</details>
```

**d) Título como link** (dentro do card do `focusedItem`):

Substituir:
```svelte
<h3 class="mb-2 text-[14px] font-medium">{focusedItem.title ?? focusedItem.source_ref ?? String(focusedItem.id)}</h3>
```

Por:
```svelte
{@const itemUrl = sourceUrl(focusedItem.lane, focusedItem.source_ref ?? '')}
{#if itemUrl}
  <a href={itemUrl} target="_blank" rel="noopener noreferrer"
     class="mb-2 block text-[14px] font-medium hover:underline text-primary">
    {focusedItem.title ?? focusedItem.source_ref ?? String(focusedItem.id)}
  </a>
{:else}
  <h3 class="mb-2 text-[14px] font-medium">{focusedItem.title ?? focusedItem.source_ref ?? String(focusedItem.id)}</h3>
{/if}
```

**e) Metadados do item** (após o título, antes do summary):
```svelte
<div class="mb-3 flex flex-wrap gap-x-3 gap-y-1 text-[12px] text-muted">
  <span><span class="font-medium">{t.curadoria.filterTipo}:</span> {focusedItem.lane}</span>
  {#if focusedItem.published_at}
    <span>{new Date(focusedItem.published_at).toLocaleString('pt-BR')}</span>
  {/if}
</div>
```

**f) Dentro do painel "por que ficou na cerca"** — trocar `<details><summary>` por bloco sempre visível e capitalizar rótulos:

Substituir o `<details class="mb-4 ...">` existente por:
```svelte
<div class="mb-4 text-[12px]">
  <p class="mb-2 font-medium text-muted">{t.curadoria.filaWhyFence}</p>
  {#if focusedDecisionsLoading}
    <p class="text-muted">{t.curadoria.pulsoLoading}</p>
  {:else if deferReason}
    <dl class="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1">
      {#if deferReason.score != null}
        <dt class="text-muted">{t.curadoria.filaScore}</dt>
        <dd>{deferReason.score}</dd>
      {/if}
      <dt class="text-muted">{t.curadoria.filaDecidedBy}</dt>
      <dd>{labelDecidedBy(deferReason.decided_by)}</dd>
      {#if deferReason.reason}
        <dt class="text-muted">{t.curadoria.filaReason}</dt>
        <dd>{deferReason.reason}</dd>
      {/if}
    </dl>
  {:else}
    <p class="text-muted">—</p>
  {/if}
</div>
```

**g) Progresso** usa `filteredQueue`:
```svelte
<p class="mt-3 text-[11px] text-muted">{focusedIndex + 1} / {filteredQueue.length}</p>
```

**h) `focusedItem` usa `filteredQueue`:**
```typescript
let focusedItem = $derived(filteredQueue[focusedIndex] ?? null);
```

- [ ] **Step 5: Verificar TypeScript**

```bash
cd rara-console/web && npx tsc --noEmit
```
Esperado: sem erros.

- [ ] **Step 6: Commit**

```bash
cd rara-console
git add web/src/routes/curadoria/+page.svelte web/src/lib/curadoria.ts
git commit -m "feat(console/curadoria): tab nav, Decidir tab with filters, title link, metadata"
```

---

## Task 5: Frontend — Aba Histórico (dados enriquecidos com lane e link de fonte)

**Files:**
- Modify: `rara-console/web/src/routes/curadoria/+page.svelte`
- Modify: `rara-console/web/src/lib/curadoria.ts` (atualizar tipo `RecentDecision`)

**Interfaces:**
- Consumes: campos `lane` e `source_ref` em `RecentDecision` (Task 1); `sourceUrl()` (Task 3).

- [ ] **Step 1: Atualizar tipo `RecentDecision` em `+page.svelte`**

No `<script>`, o tipo local `RecentDecision` precisa incluir os novos campos:

```typescript
type RecentDecision = Decision & {
  id: number;
  item_id: number;
  gate: string;
  score?: number | null;
  when: string;
  lane?: string;
  source_ref?: string;
};
```

- [ ] **Step 2: Redesenhar a seção Histórico no template**

Dentro do bloco `{#if activeTab === 'historico'}`, substituir a `<ul>` existente por uma tabela mais rica:

```svelte
{#if activeTab === 'historico'}
<section class="mb-6">
  <h2 class="mb-3 text-[15px] font-semibold">{t.curadoria.trilhaZone}</h2>
  <div class="overflow-hidden rounded-card border border-border bg-surface">
    {#if decisionsLoading}
      <p class="px-4 py-3 text-[13px] text-muted">{t.curadoria.trilhaLoading}</p>
    {:else if decisionsError}
      <p class="px-4 py-3 text-[13px] text-red">{t.curadoria.trilhaError}</p>
    {:else if decisions.length === 0}
      <p class="px-4 py-3 text-[13px] text-muted">{t.curadoria.trilhaEmpty}</p>
    {:else}
      <ul class="divide-y divide-border">
        {#each decisions as d}
          <li class="flex items-start gap-3 px-4 py-2.5 text-[13px]">
            <!-- decisão badge -->
            <span class="mt-0.5 shrink-0 rounded-full px-2 py-0.5 text-[11px] font-medium
              {d.decision === 'keep' ? 'bg-green/15 text-green' :
               d.decision === 'drop' ? 'bg-border text-text' :
               'bg-primary/15 text-primary'}">{d.decision}</span>
            <div class="min-w-0 flex-1">
              <div class="flex flex-wrap items-center gap-x-2 gap-y-0.5">
                <!-- fonte como link ou item_id -->
                {#if d.lane && d.source_ref && sourceUrl(d.lane, d.source_ref)}
                  <a href={sourceUrl(d.lane, d.source_ref)} target="_blank" rel="noopener noreferrer"
                     class="text-primary hover:underline">{t.curadoria.trilhaSourceLink} #{d.item_id}</a>
                {:else}
                  <span class="text-muted">{t.curadoria.trilhaItemRef} {d.item_id}</span>
                {/if}
                <!-- tipo (lane) -->
                {#if d.lane}
                  <span class="rounded-full border border-border px-1.5 py-0.5 text-[11px] text-muted">{d.lane}</span>
                {/if}
                <!-- worker (decided_by) -->
                {#if d.decided_by}
                  <span class="text-muted opacity-60">· {labelDecidedBy(d.decided_by)}</span>
                {/if}
                <!-- data/hora da decisão -->
                <span class="ml-auto text-[11px] text-muted shrink-0">
                  {new Date(d.when).toLocaleString('pt-BR')}
                </span>
              </div>
              {#if d.reason}
                <p class="mt-0.5 text-[12px] text-muted">{d.reason}</p>
              {/if}
            </div>
          </li>
        {/each}
      </ul>
    {/if}
  </div>
</section>
{/if}
```

- [ ] **Step 3: Verificar TypeScript**

```bash
cd rara-console/web && npx tsc --noEmit
```
Esperado: sem erros.

- [ ] **Step 4: Commit**

```bash
cd rara-console
git add web/src/routes/curadoria/+page.svelte web/src/lib/curadoria.ts
git commit -m "feat(console/curadoria): Histórico tab with lane badge, source link, decision timestamp"
```

---

## Task 6: Frontend — Aba Ajustes (+ Nova versão com pre-fill e validação de diff)

**Files:**
- Modify: `rara-console/web/src/routes/curadoria/+page.svelte`

**Comportamento esperado:**
1. A aba "Ajustes" mostra a versão ativa (como hoje).
2. Abaixo da versão ativa, um botão "+ Nova versão" — quando clicado, revela um formulário pre-preenchido com `activeProfile.version + 1` e todos os campos do perfil ativo.
3. O botão Salvar é desabilitado se `isDiffEmpty(diffProfile(activeProfile, proposedProfile))` for true (sem diferença).
4. Ao salvar, chama o endpoint `/api/interest-profile` existente (mesma lógica do `propose()` atual).
5. O formulário antigo (campo "Propor nova versão") é **removido**.
6. O card de versão proposta pendente (approve hero) permanece inalterado.
7. Gate Rules continuam na aba Ajustes (abaixo de tudo, colapsíveis).

**Novo estado necessário:**
```typescript
let showNovaVersaoForm = $state(false);
```

O `proposeVersion` não precisa mais de input manual — é derivado de `activeProfile`:
```typescript
let novaVersaoNumber = $derived((activeProfile?.version ?? 0) + 1);
```

Os demais campos (`proposeNarrative`, `proposeTopics`, etc.) continuam como estados — mas serão pre-preenchidos ao abrir o formulário.

- [ ] **Step 1: Adicionar estado e lógica de pré-preenchimento**

No `<script>` do componente, adicionar após os estados existentes do formulário de propose:

```typescript
let showNovaVersaoForm = $state(false);
let novaVersaoNumber = $derived((activeProfile?.version ?? 0) + 1);

// Pre-fill the propose form from the active profile when the user opens it.
function openNovaVersao() {
  if (!activeProfile) return;
  proposeNarrative = activeProfile.narrative ?? '';
  proposeTopics = activeProfile.topics != null ? JSON.stringify(activeProfile.topics, null, 2) : '';
  proposeAuthors = activeProfile.authors != null ? JSON.stringify(activeProfile.authors, null, 2) : '';
  proposeAntiTopics = activeProfile.anti_topics != null ? JSON.stringify(activeProfile.anti_topics, null, 2) : '';
  proposeWeights = activeProfile.weights != null ? JSON.stringify(activeProfile.weights, null, 2) : '';
  proposeVersion = String(novaVersaoNumber);
  proposeError = '';
  showNovaVersaoForm = true;
}
```

Adicionar uma derivada para detectar se o formulário atual tem diff em relação ao perfil ativo:
```typescript
let novaVersaoDiff = $derived(() => {
  if (!activeProfile || !showNovaVersaoForm) return null;
  try {
    const proposed = {
      topics: proposeTopics.trim() ? JSON.parse(proposeTopics) : activeProfile.topics,
      authors: proposeAuthors.trim() ? JSON.parse(proposeAuthors) : activeProfile.authors,
      anti_topics: proposeAntiTopics.trim() ? JSON.parse(proposeAntiTopics) : activeProfile.anti_topics,
      weights: proposeWeights.trim() ? JSON.parse(proposeWeights) : activeProfile.weights,
    };
    return diffProfile(activeProfile, proposed);
  } catch {
    return null; // JSON inválido → deixa salvar (o servidor vai rejeitar se inválido)
  }
});
let novaVersaoHasDiff = $derived(novaVersaoDiff === null || !isDiffEmpty(novaVersaoDiff));
```

- [ ] **Step 2: Redesenhar a seção Ajustes no template**

Dentro do bloco `{#if activeTab === 'ajustes'}`, substituir a seção `<!-- ── 4. O GOSTO ──` inteira por:

```svelte
{#if activeTab === 'ajustes'}
<!-- ── AJUSTES (Interest Profile) ─────────────────────────────────── -->
<section id="ajustes" class="mb-6">
  <h2 class="mb-3 text-[15px] font-semibold">{t.curadoria.gostoZone}</h2>

  {#if profileLoading}
    <p class="text-[13px] text-muted">{t.curadoria.profileLoading}</p>
  {:else if profileError}
    <p class="text-[13px] text-red">{t.curadoria.profileError}</p>
  {:else}
    <!-- Proposed version card (hero) — unchanged -->
    {#if proposedProfile && profileDiff}
      {@const pv = proposedProfile}
      <div class="mb-4 overflow-hidden rounded-card border border-primary/40 bg-surface">
        <div class="flex items-center justify-between border-b border-primary/20 bg-primary/5 px-4 py-2">
          <span class="text-[12px] font-medium text-primary">
            {t.curadoria.profileProposedCard} · v{pv.version}
          </span>
          {#if approving === pv.version}
            <span class="text-[12px] text-muted">{t.curadoria.profileApproving}</span>
          {:else}
            <button
              class="cursor-pointer rounded-token border border-primary bg-primary/10 px-4 py-1 text-[13px] font-semibold text-primary hover:bg-primary/20"
              onclick={() => approve(pv.version)}
            >{t.curadoria.profileApproveBtn(pv.version)}</button>
          {/if}
        </div>
        {#if approveError}<p class="px-4 py-2 text-[12px] text-red">{approveError}</p>{/if}
        {#if approveNotice}<p class="px-4 py-2 text-[12px] text-muted">{approveNotice}</p>{/if}
        <div class="divide-y divide-border/50 px-4 py-3 text-[13px]">
          {#if pv.narrative}
            <div class="pb-3">
              <div class="mb-1 text-[11px] font-medium uppercase tracking-wide text-muted">{t.curadoria.profileProposedNarrative}</div>
              <p class="text-[13px] italic text-muted">{pv.narrative}</p>
            </div>
          {/if}
          {#each ([['topics', t.curadoria.profileTopicsLabel], ['authors', t.curadoria.profileAuthorsLabel], ['anti_topics', t.curadoria.profileAntiTopicsLabel]] as const) as [field, label]}
            {@const d = profileDiff[field]}
            {#if d.fallback}
              <div class="py-2"><div class="mb-1 text-[11px] font-medium text-muted">{label}</div><p class="text-[12px] text-muted">{t.curadoria.profileDiffFallback}</p></div>
            {:else if d.added.length > 0 || d.removed.length > 0}
              <div class="py-2">
                <div class="mb-1 text-[11px] font-medium text-muted">{label}</div>
                <div class="flex flex-wrap gap-1">
                  {#each d.added as item}<span class="rounded-full bg-green/15 px-2 py-0.5 text-[11px] text-green">+ {item}</span>{/each}
                  {#each d.removed as item}<span class="rounded-full bg-red/10 px-2 py-0.5 text-[11px] text-muted line-through opacity-60">− {item}</span>{/each}
                </div>
              </div>
            {:else}
              <div class="py-2"><div class="mb-1 text-[11px] font-medium text-muted">{label}</div><p class="text-[12px] text-muted">{t.curadoria.profileDiffNoChanges}</p></div>
            {/if}
          {/each}
          {#if profileDiff.weights.fallback}
            <div class="py-2"><div class="mb-1 text-[11px] font-medium text-muted">{t.curadoria.profileWeightsLabel}</div><p class="text-[12px] text-muted">{t.curadoria.profileDiffFallback}</p></div>
          {:else if profileDiff.weights.added.length > 0 || profileDiff.weights.removed.length > 0 || profileDiff.weights.changed.length > 0}
            <div class="py-2">
              <div class="mb-1 text-[11px] font-medium text-muted">{t.curadoria.profileWeightsLabel}</div>
              <div class="space-y-0.5 font-mono text-[12px]">
                {#each profileDiff.weights.added as e}<div class="text-green">+ {e.key}: {JSON.stringify(e.value)}</div>{/each}
                {#each profileDiff.weights.removed as e}<div class="text-muted line-through opacity-60">− {e.key}: {JSON.stringify(e.value)}</div>{/each}
                {#each profileDiff.weights.changed as c}<div class="text-primary">{c.key}: {JSON.stringify(c.from)} → {JSON.stringify(c.to)}</div>{/each}
              </div>
            </div>
          {/if}
        </div>
      </div>
    {:else if !profileLoading}
      <p class="mb-4 text-[13px] text-muted">{t.curadoria.profileStable}</p>
    {/if}

    <!-- Active version card -->
    <div class="mb-4 overflow-hidden rounded-card border border-border bg-surface">
      <div class="border-b border-border px-4 py-2 text-[12px] font-medium text-muted">
        {t.curadoria.profileCurrent}
      </div>
      {#if activeProfile}
        <div class="space-y-3 px-4 py-3 text-[13px]">
          <div>
            <span class="text-muted">{t.curadoria.profileVersion}:</span>
            <span class="ml-1 font-semibold">v{activeProfile.version}</span>
          </div>
          {#if activeProfile.narrative}
            <div>
              <div class="mb-1 text-[11px] font-medium text-muted">{t.curadoria.profileNarrative}</div>
              <p class="text-[13px]">{activeProfile.narrative}</p>
            </div>
          {/if}
          {#each ([['topics', t.curadoria.profileTopicsLabel, activeProfile.topics], ['authors', t.curadoria.profileAuthorsLabel, activeProfile.authors], ['anti_topics', t.curadoria.profileAntiTopicsLabel, activeProfile.anti_topics]] as const) as [, label, val]}
            {#if Array.isArray(val) && val.length > 0}
              <div>
                <div class="mb-1 text-[11px] font-medium text-muted">{label}</div>
                <div class="flex flex-wrap gap-1">
                  {#each val as item}<span class="rounded-full border border-border px-2 py-0.5 text-[11px]">{item}</span>{/each}
                </div>
              </div>
            {:else if val != null && !Array.isArray(val)}
              <div>
                <div class="mb-1 text-[11px] font-medium text-muted">{label}</div>
                <pre class="text-[11px] text-muted">{JSON.stringify(val)}</pre>
              </div>
            {/if}
          {/each}
          {#if activeProfile.weights && typeof activeProfile.weights === 'object' && !Array.isArray(activeProfile.weights)}
            <div>
              <div class="mb-1 text-[11px] font-medium text-muted">{t.curadoria.profileWeightsLabel}</div>
              <div class="space-y-0.5 font-mono text-[12px] text-muted">
                {#each Object.entries(activeProfile.weights as Record<string, unknown>) as [k, v]}
                  <div>{k}: {JSON.stringify(v)}</div>
                {/each}
              </div>
            </div>
          {:else if activeProfile.weights != null}
            <div>
              <div class="mb-1 text-[11px] font-medium text-muted">{t.curadoria.profileWeightsLabel}</div>
              <pre class="text-[11px] text-muted">{JSON.stringify(activeProfile.weights)}</pre>
            </div>
          {/if}
        </div>
      {:else}
        <p class="px-4 py-3 text-[13px] text-muted">{t.curadoria.profileEmpty}</p>
      {/if}
    </div>

    <!-- Version history (colapsável) — unchanged -->
    {#if versions.length > 0}
      <details class="group mb-4 overflow-hidden rounded-card border border-border bg-surface">
        <summary class="flex cursor-pointer list-none items-center justify-between px-4 py-2 text-[12px] font-medium text-muted hover:bg-hover">
          {t.curadoria.profileVersions}
          <span class="text-[11px] group-open:hidden">{t.curadoria.gateExpand}</span>
          <span class="hidden text-[11px] group-open:inline">{t.curadoria.gateCollapse}</span>
        </summary>
        <table class="w-full border-t border-border text-[13px]">
          <thead>
            <tr class="border-b border-border text-left text-[11px] text-muted">
              <th class="px-4 py-2 font-medium">{t.curadoria.profileVersion}</th>
              <th class="px-4 py-2 font-medium">{t.curadoria.profileStatus}</th>
              <th class="px-4 py-2 font-medium">{t.curadoria.profileCreatedAt}</th>
            </tr>
          </thead>
          <tbody>
            {#each versions as v}
              <tr class="border-b border-border last:border-b-0">
                <td class="px-4 py-2 font-medium">v{v.version}</td>
                <td class="px-4 py-2">
                  <span class="rounded-full px-2 py-0.5 text-[11px] font-medium
                    {v.status === 'active' ? 'bg-green/15 text-green' : v.status === 'proposed' ? 'bg-primary/15 text-primary' : 'text-muted'}"
                  >{v.status}</span>
                </td>
                <td class="px-4 py-2 text-muted">
                  {v.created_at ? new Date(v.created_at).toLocaleString('pt-BR') : '—'}
                </td>
              </tr>
            {/each}
          </tbody>
        </table>
      </details>
    {/if}

    <!-- + Nova versão button / form -->
    {#if !showNovaVersaoForm}
      <button
        onclick={openNovaVersao}
        disabled={!activeProfile || !!proposedProfile}
        class="cursor-pointer rounded-token border border-border bg-transparent px-4 py-1.5 text-[13px] font-medium hover:bg-hover disabled:cursor-default disabled:opacity-50"
        title={proposedProfile ? 'Existe uma versão proposta aguardando aprovação' : ''}
      >{t.curadoria.gostoNovaVersaoBtn}</button>
    {:else}
      <!-- Nova versão form (pre-filled from active profile) -->
      <div class="overflow-hidden rounded-card border border-border bg-surface">
        <div class="flex items-center justify-between border-b border-border px-4 py-2">
          <span class="text-[12px] font-medium text-muted">v{novaVersaoNumber}</span>
          <button onclick={() => (showNovaVersaoForm = false)} class="text-[12px] text-muted hover:text-text">Cancelar</button>
        </div>
        <div class="space-y-3 px-4 py-3">
          <div>
            <label class="mb-1 block text-[11px] text-muted" for="prop-narrative">{t.curadoria.profileNarrativeLabel}</label>
            <textarea id="prop-narrative" rows="2" bind:value={proposeNarrative}
              class="w-full rounded-token border border-border bg-surface-2 px-2 py-1 text-[13px] focus:outline-none focus:ring-1 focus:ring-primary/50"
            ></textarea>
          </div>
          <div class="grid grid-cols-2 gap-3">
            <div>
              <label class="mb-1 block text-[11px] text-muted" for="prop-topics">{t.curadoria.profileTopicsLabel}</label>
              <textarea id="prop-topics" rows="3" bind:value={proposeTopics}
                class="w-full rounded-token border border-border bg-surface-2 px-2 py-1 font-mono text-[12px] focus:outline-none focus:ring-1 focus:ring-primary/50"
              ></textarea>
            </div>
            <div>
              <label class="mb-1 block text-[11px] text-muted" for="prop-authors">{t.curadoria.profileAuthorsLabel}</label>
              <textarea id="prop-authors" rows="3" bind:value={proposeAuthors}
                class="w-full rounded-token border border-border bg-surface-2 px-2 py-1 font-mono text-[12px] focus:outline-none focus:ring-1 focus:ring-primary/50"
              ></textarea>
            </div>
            <div>
              <label class="mb-1 block text-[11px] text-muted" for="prop-anti">{t.curadoria.profileAntiTopicsLabel}</label>
              <textarea id="prop-anti" rows="3" bind:value={proposeAntiTopics}
                class="w-full rounded-token border border-border bg-surface-2 px-2 py-1 font-mono text-[12px] focus:outline-none focus:ring-1 focus:ring-primary/50"
              ></textarea>
            </div>
            <div>
              <label class="mb-1 block text-[11px] text-muted" for="prop-weights">{t.curadoria.profileWeightsLabel}</label>
              <textarea id="prop-weights" rows="3" bind:value={proposeWeights}
                class="w-full rounded-token border border-border bg-surface-2 px-2 py-1 font-mono text-[12px] focus:outline-none focus:ring-1 focus:ring-primary/50"
              ></textarea>
            </div>
          </div>
          {#if !novaVersaoHasDiff}
            <p class="text-[12px] text-muted">{t.curadoria.gostoNoDiff}</p>
          {/if}
          {#if proposeError}
            <p class="text-[12px] text-red">{proposeError}</p>
          {/if}
          <button
            disabled={proposing || !novaVersaoHasDiff}
            onclick={propose}
            class="cursor-pointer rounded-token border border-border bg-transparent px-4 py-1.5 text-[13px] font-medium hover:bg-hover disabled:cursor-default disabled:opacity-50"
          >{proposing ? t.curadoria.profileProposing : t.curadoria.profileProposeBtn}</button>
        </div>
      </div>
    {/if}
  {/if}
</section>

<!-- ── REGRAS DE GATE (dentro da aba Ajustes) ─── -->
... (mover o <details> de gate rules aqui, sem alteração de conteúdo)
{/if}
```

Nota: o `propose()` existente usa `proposeVersion` como string. Como agora o version é derivado (`novaVersaoNumber`), antes de chamar `propose()` setar `proposeVersion = String(novaVersaoNumber)` — ou refatorar `propose()` para aceitar version como parâmetro. A forma mais simples: sincronizar `proposeVersion` em `openNovaVersao()` (já feito acima) e manter `propose()` sem alteração.

- [ ] **Step 3: Verificar TypeScript e testes**

```bash
cd rara-console/web && npx tsc --noEmit && npx vitest run
```
Esperado: sem erros, todos os testes passam.

- [ ] **Step 4: Commit**

```bash
cd rara-console
git add web/src/routes/curadoria/+page.svelte
git commit -m "feat(console/curadoria): Ajustes tab with +Nova versão pre-fill and diff validation"
```

---

## Self-Review

### Cobertura do spec

| Requisito (issue #255) | Task |
|---|---|
| Renomear "Fila de Revisão" → "Revisar" | Task 2 (strings) |
| "por que ficou na cerca" → "Por que não consegui decidir:" | Task 2 + Task 4 |
| Maiúscula: score, decidido, motivo | Task 2 (filaScore, filaDecidedBy, filaReason) |
| Título como link para documento origem | Task 4 (sourceUrl + `<a>`) |
| Worker, tipo, data/hora do documento | Task 4 (metadata block) |
| Filtros: data, tipo, canal, ordenação | Task 3 (filterQuarantine) + Task 4 (UI) |
| "O gosto" → "Ajustes" | Task 2 (strings) + Task 6 (aba) |
| Mostrar versão ativa | Task 6 (card ativo preservado) |
| Remover "Propor nova versão" antigo | Task 6 (seção removida) |
| + Nova versão com pre-fill | Task 6 (openNovaVersao) |
| Bloquear save se sem diff | Task 3 (isDiffEmpty) + Task 6 (novaVersaoHasDiff) |
| "Trilha de decisões" → "Histórico" | Task 2 (strings) |
| Trilha: worker, fonte (link), tipo, data/hora | Task 1 (backend lane+source_ref) + Task 5 (UI) |
| Menu superior: Decidir, Histórico, Ajustes | Task 4 (tab nav) |

### Checklist de Placeholders

Nenhum "TBD", "TODO", ou "similar ao Task N" sem código.

### Consistência de tipos

- `RecentDecision.Lane` / `RecentDecision.SourceRef` definidos em Task 1 e consumidos em Task 5 com os mesmos nomes.
- `FilterState` definida em Task 3 (`curadoria.ts`) e usada em Task 4 (`+page.svelte`) com os mesmos campos.
- `isDiffEmpty` importada de `curadoria.ts` em Task 6 — já exportada em Task 3.
- `novaVersaoDiff` usa `diffProfile` (já importada) e `isDiffEmpty` — ambos disponíveis após Task 3.
