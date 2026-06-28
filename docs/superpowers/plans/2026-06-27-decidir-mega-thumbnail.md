# Decidir — Mega Thumbnail Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Exibir um "mega thumbnail" no lado direito da tela Decidir (layout 50/50) que carrega automaticamente ao focar um item e mostra o conteúdo original — YouTube player, corpo do artigo, ou email completo — com shimmer durante o fetch.

**Architecture:** A tela Decidir ganha um CSS grid 50/50: coluna esquerda = card atual (título, summary, botões), coluna direita = `MegaThumbnail.svelte`. Quando `focusedItem` muda, o thumbnail faz fetch para `/api/items/{id}/content` (rara-console proxia para rara-core `GET /v1/items/{id}/content`). rara-core consulta a tabela correta por lane (todos no mesmo Neon, sem schema prefix). YouTube é caso especial: iframe puro no frontend, sem backend. Se a coluna direita não tiver conteúdo (linkedin/podcast ignorados por ora, ou fetch falhou), o grid colapsa para 100% e o card ocupa a tela toda.

**Tech Stack:** Svelte 5, Go 1.26, Neon PostgreSQL (queries cross-agent diretas do pool do rara-core — mesmo padrão de `GetDistillation`/`ListRecentDistillations`), CSS grid, `@keyframes shimmer`.

## Global Constraints

- TDD obrigatório: teste falha antes da implementação, `make test` deve passar ao final de cada task.
- Zero I/O em testes unitários: MockDatabase para Go, `vi.fn()` mockando `fetch` para TypeScript.
- Nenhum dado é persistido no cliente (sem localStorage, sem cache).
- Auth: o endpoint `/v1/items/{id}/content` fica atrás do `authMiddleware` existente — zero mudança de auth.
- Shimmer desaparece quando conteúdo chega OU quando fetch falha.
- YouTube: nunca chamar o backend — o `source_ref` já é o video ID.
- LinkedIn e podcast: ignorados neste MVP (componente colapsa silenciosamente).

---

## Lanes implementadas neste MVP

| Lane | O que mostrar | Backend? | Esforço |
|------|--------------|----------|---------|
| `youtube` | `<iframe src="https://www.youtube.com/embed/{source_ref}">` | Não | XS |
| `news` | `news_items.body` (ou `excerpt` se body NULL) | Sim | S |
| `email` | `emails.sender` + `emails.body` (até 10 000 chars) | Sim | S |
| `linkedin` | — colapsado — | — | ignorado |
| `podcast` | — colapsado — | — | ignorado |

---

## File Structure

| Arquivo | Status | Responsabilidade |
|---------|--------|-----------------|
| `rara-core/main.go` | Modify | Struct `ItemContentResult` + `Database.ItemContent()` interface |
| `rara-core/store_reads.go` | Modify | `pgxDatabase.ItemContent()` — queries cross-agent |
| `rara-core/main_test.go` | Modify | `MockDatabase.ItemContent()` + campo `itemContents` |
| `rara-core/surface.go` | Modify | `Core.ItemContent()` + handler + registro da rota |
| `rara-core/surface_test.go` | Modify | Testes do handler HTTP |
| `rara-console/main.go` | Modify | `handleItemContent` proxy + registro da rota |
| `rara-console/main_test.go` | Modify | Teste do proxy |
| `rara-console/web/src/lib/curadoria.ts` | Modify | Tipo `ItemContent` + `fetchItemContent()` |
| `rara-console/web/src/lib/curadoria.test.ts` | Modify | Testes unitários |
| `rara-console/web/src/lib/MegaThumbnail.svelte` | Create | Shimmer + renderização por lane |
| `rara-console/web/src/routes/curadoria/+page.svelte` | Modify | Grid 50/50 + integrar MegaThumbnail |

---

## Task 1: rara-core — struct + Database interface + pgxDatabase + MockDatabase

**Files:**
- Modify: `rara-core/main.go` (struct + interface method)
- Modify: `rara-core/store_reads.go` (pgxDatabase impl)
- Modify: `rara-core/main_test.go` (MockDatabase impl)

**Interfaces:**
- Produces: `ItemContentResult` struct + `Database.ItemContent(ctx, itemID int)` method

- [ ] **Step 1: Escrever o teste falho em `rara-core/main_test.go`**

  Adicionar após `seedDistillation` (linha ~1588):
  ```go
  func TestMockItemContentEmail(t *testing.T) {
      ctx := context.Background()
      db := newMockDatabase()
      fid, _ := db.UpsertFlow(ctx, Flow{Name: "f", SourceType: "email", Enabled: true, Version: 1})
      id, _ := db.UpsertItem(ctx, Item{Lane: "email", SourceRef: "msg-1@mail", FlowID: fid, FlowVersion: 1, Status: "discovered"})
      db.itemContents[id] = ItemContentResult{Lane: "email", Body: "hello world", Sender: "alice@example.com"}

      got, found, err := db.ItemContent(ctx, id)
      if err != nil || !found {
          t.Fatalf("ItemContent(%d): found=%v err=%v", id, found, err)
      }
      if got.Body != "hello world" || got.Sender != "alice@example.com" {
          t.Errorf("got %+v, want body=hello world sender=alice@example.com", got)
      }
  }
  ```

- [ ] **Step 2: Verificar que o teste falha**
  ```bash
  cd rara-core && go test -run TestMockItemContentEmail -v
  ```
  Esperado: FAIL (campo/método inexistente).

- [ ] **Step 3: Adicionar o struct em `rara-core/main.go`**

  Após `DistillationSummary` ou próximo dos outros tipos de leitura (procure `type Distillation struct`):
  ```go
  // ItemContentResult holds the rich content for an item's original source,
  // returned by GET /v1/items/{id}/content for the mega-thumbnail panel.
  type ItemContentResult struct {
      Lane   string `json:"lane"`
      Body   string `json:"body,omitempty"`
      Sender string `json:"sender,omitempty"` // email only
  }
  ```

  Adicionar o método à interface `Database` (logo após `GetDistillation`, ~linha 772):
  ```go
  // ItemContent returns the rich source content for the mega-thumbnail panel.
  // found=false when the lane has no content record (e.g. linkedin, podcast).
  // Body is capped at 10 000 chars in the pgxDatabase implementation to avoid
  // transferring large email blobs over the network.
  ItemContent(ctx context.Context, itemID int) (ItemContentResult, bool, error)
  ```

- [ ] **Step 4: Adicionar campo + implementação ao MockDatabase em `rara-core/main_test.go`**

  Encontre o struct `MockDatabase` (procure `type MockDatabase struct`). Adicionar campo:
  ```go
  itemContents map[int]ItemContentResult
  ```

  Encontre `newMockDatabase()` e inicializar o campo:
  ```go
  itemContents: map[int]ItemContentResult{},
  ```

  Adicionar método ao MockDatabase (junto com os outros `func (db *MockDatabase)`):
  ```go
  func (db *MockDatabase) ItemContent(_ context.Context, itemID int) (ItemContentResult, bool, error) {
      c, ok := db.itemContents[itemID]
      return c, ok, nil
  }
  ```

- [ ] **Step 5: Verificar que o teste passa**
  ```bash
  cd rara-core && go test -run TestMockItemContentEmail -v
  ```
  Esperado: PASS.

- [ ] **Step 6: Adicionar implementação real em `rara-core/store_reads.go`**

  Adicionar após `GetDistillation`:
  ```go
  // ItemContent returns rich content for the mega-thumbnail panel.
  // It first resolves lane+source_ref from items, then queries the agent-owned table.
  // Body is capped at 10 000 chars. found=false for unimplemented lanes (linkedin, podcast).
  func (d *pgxDatabase) ItemContent(ctx context.Context, itemID int) (ItemContentResult, bool, error) {
      var lane, sourceRef string
      err := d.conn.QueryRow(ctx,
          `SELECT lane, source_ref FROM items WHERE id = $1`, itemID,
      ).Scan(&lane, &sourceRef)
      if errors.Is(err, pgx.ErrNoRows) {
          return ItemContentResult{}, false, nil
      }
      if err != nil {
          return ItemContentResult{}, false, err
      }

      switch lane {
      case "email":
          var sender, body string
          err = d.conn.QueryRow(ctx,
              `SELECT COALESCE(sender,''), SUBSTR(COALESCE(body,''), 1, 10000)
               FROM emails WHERE message_id = $1`, sourceRef,
          ).Scan(&sender, &body)
          if errors.Is(err, pgx.ErrNoRows) {
              return ItemContentResult{Lane: lane}, true, nil
          }
          if err != nil {
              return ItemContentResult{}, false, err
          }
          return ItemContentResult{Lane: lane, Body: body, Sender: sender}, true, nil

      case "news":
          var body string
          err = d.conn.QueryRow(ctx,
              `SELECT SUBSTR(COALESCE(body, excerpt, ''), 1, 10000)
               FROM news_items WHERE url = $1`, sourceRef,
          ).Scan(&body)
          if errors.Is(err, pgx.ErrNoRows) {
              return ItemContentResult{Lane: lane}, true, nil
          }
          if err != nil {
              return ItemContentResult{}, false, err
          }
          return ItemContentResult{Lane: lane, Body: body}, true, nil

      default:
          // youtube: caller uses source_ref directly; linkedin/podcast: not implemented yet.
          return ItemContentResult{Lane: lane}, true, nil
      }
  }
  ```

- [ ] **Step 7: Lint**
  ```bash
  cd rara-core && make lint
  ```
  Esperado: zero warnings.

- [ ] **Step 8: Commit**
  ```bash
  git add rara-core/main.go rara-core/store_reads.go rara-core/main_test.go
  git commit -m "feat(core): add Database.ItemContent + pgxDatabase + mock impl for mega-thumbnail"
  ```

---

## Task 2: rara-core surface — Core method + HTTP handler + rota

**Files:**
- Modify: `rara-core/surface.go`
- Modify: `rara-core/surface_test.go`

**Interfaces:**
- Consumes: `Database.ItemContent()` (Task 1)
- Produces: `GET /v1/items/{id}/content` → JSON `ItemContentResult`

- [ ] **Step 1: Escrever testes falhos em `surface_test.go`**

  Adicionar após `TestHTTPGetDistillationNotFoundIs400` (~linha 708):
  ```go
  func TestCoreItemContentEmail(t *testing.T) {
      ctx := context.Background()
      core, db, _ := newTestCore(t)
      fid := seedFlow(t, db)
      id, _ := db.UpsertItem(ctx, Item{Lane: "email", SourceRef: "msg@mail", FlowID: fid, FlowVersion: 1, Status: "discovered"})
      db.itemContents[id] = ItemContentResult{Lane: "email", Body: "hello", Sender: "a@b.com"}

      got, err := core.ItemContent(ctx, id)
      if err != nil {
          t.Fatal(err)
      }
      if got.Body != "hello" || got.Sender != "a@b.com" {
          t.Errorf("got %+v", got)
      }
  }

  func TestCoreItemContentInvalidIDIsBadInput(t *testing.T) {
      ctx := context.Background()
      core, _, _ := newTestCore(t)
      _, err := core.ItemContent(ctx, 0)
      var bad badInputError
      if !errors.As(err, &bad) {
          t.Errorf("want badInputError for id=0, got %T: %v", err, err)
      }
  }

  func TestHTTPItemContentReturnsJSON(t *testing.T) {
      ctx := context.Background()
      core, db, _ := newTestCore(t)
      fid := seedFlow(t, db)
      id, _ := db.UpsertItem(ctx, Item{Lane: "news", SourceRef: "https://ex.com/a", FlowID: fid, FlowVersion: 1, Status: "discovered"})
      db.itemContents[id] = ItemContentResult{Lane: "news", Body: "article body"}

      h := NewSurfaceMux(core, testToken)
      rec := do(t, h, http.MethodGet, fmt.Sprintf("/v1/items/%d/content", id), "")
      if rec.Code != http.StatusOK {
          t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
      }
      var got ItemContentResult
      if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
          t.Fatal(err)
      }
      if got.Lane != "news" || got.Body != "article body" {
          t.Errorf("got %+v", got)
      }
  }

  func TestHTTPItemContentInvalidIDIs400(t *testing.T) {
      core, _, _ := newTestCore(t)
      h := NewSurfaceMux(core, testToken)
      rec := do(t, h, http.MethodGet, "/v1/items/abc/content", "")
      if rec.Code != http.StatusBadRequest {
          t.Errorf("want 400, got %d", rec.Code)
      }
  }
  ```

- [ ] **Step 2: Verificar que os testes falham**
  ```bash
  cd rara-core && go test -run "TestCoreItemContent|TestHTTPItemContent" -v
  ```
  Esperado: FAIL (método inexistente).

- [ ] **Step 3: Adicionar `Core.ItemContent()` em `surface.go`**

  Após `Core.ItemDecisions()` (~linha 158):
  ```go
  // ItemContent returns rich source content for the mega-thumbnail panel.
  // Returns badInputError for id <= 0 or item not found.
  func (c *Core) ItemContent(ctx context.Context, itemID int) (ItemContentResult, error) {
      if itemID <= 0 {
          return ItemContentResult{}, badInput("item id must be positive, got %d", itemID)
      }
      result, found, err := c.db.ItemContent(ctx, itemID)
      if err != nil {
          return ItemContentResult{}, err
      }
      if !found {
          return ItemContentResult{}, badInput("item %d not found", itemID)
      }
      return result, nil
  }
  ```

- [ ] **Step 4: Adicionar o HTTP handler e registrar a rota em `surface.go`**

  Adicionar handler junto com os outros `func (h *httpSurface)` (após `itemDecisions`):
  ```go
  func (h *httpSurface) itemContent(w http.ResponseWriter, r *http.Request) {
      id, ok := pathID(w, r)
      if !ok {
          return
      }
      content, err := h.core.ItemContent(r.Context(), id)
      writeResult(w, content, err)
  }
  ```

  Registrar em `NewSurfaceMux` após a linha do `itemDecisions`:
  ```go
  mux.HandleFunc("GET /v1/items/{id}/content", h.itemContent)
  ```

- [ ] **Step 5: Verificar que os testes passam**
  ```bash
  cd rara-core && go test -run "TestCoreItemContent|TestHTTPItemContent" -v
  ```
  Esperado: PASS.

- [ ] **Step 6: Full test suite**
  ```bash
  cd rara-core && make test
  ```
  Esperado: PASS (zero regressões).

- [ ] **Step 7: Commit**
  ```bash
  git add rara-core/surface.go rara-core/surface_test.go
  git commit -m "feat(core/surface): add GET /v1/items/{id}/content for mega-thumbnail"
  ```

---

## Task 3: rara-console — proxy handler

**Files:**
- Modify: `rara-console/main.go`
- Modify: `rara-console/main_test.go`

**Interfaces:**
- Consumes: `GET /v1/items/{id}/content` (Task 2)
- Produces: `GET /api/items/{id}/content` → mesmo JSON

- [ ] **Step 1: Escrever o teste falho em `rara-console/main_test.go`**

  Siga o padrão existente (procure `TestHandleItemSteps` ou similar para ver como o httptest é montado):
  ```go
  func TestHandleItemContent(t *testing.T) {
      core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
          if r.URL.Path != "/v1/items/42/content" {
              http.NotFound(w, r)
              return
          }
          w.Header().Set("Content-Type", "application/json")
          _, _ = w.Write([]byte(`{"lane":"news","body":"article"}`))
      }))
      defer core.Close()

      s := &server{coreURL: core.URL, token: "tok", client: core.Client()}
      req := httptest.NewRequest("GET", "/api/items/42/content", nil)
      rec := httptest.NewRecorder()
      s.handleItemContent(rec, req)

      if rec.Code != http.StatusOK {
          t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
      }
      if !strings.Contains(rec.Body.String(), "article") {
          t.Errorf("body missing 'article': %s", rec.Body)
      }
  }

  func TestHandleItemContentRejectsNonNumeric(t *testing.T) {
      s := &server{coreURL: "http://unused", token: "tok", client: http.DefaultClient}
      req := httptest.NewRequest("GET", "/api/items/abc/content", nil)
      // Injetar path value manualmente (Go 1.22+):
      req.SetPathValue("id", "abc")
      rec := httptest.NewRecorder()
      s.handleItemContent(rec, req)
      if rec.Code != http.StatusBadRequest {
          t.Errorf("want 400, got %d", rec.Code)
      }
  }
  ```

- [ ] **Step 2: Verificar que o teste falha**
  ```bash
  cd rara-console && go test -run TestHandleItemContent -v
  ```
  Esperado: FAIL (método inexistente).

- [ ] **Step 3: Adicionar handler em `rara-console/main.go`**

  Após `handleItemSteps` (~linha 170):
  ```go
  // handleItemContent proxies GET /v1/items/{id}/content — the mega-thumbnail content
  // endpoint. Rejects non-numeric ids before forwarding to core.
  func (s *server) handleItemContent(w http.ResponseWriter, r *http.Request) {
      id := r.PathValue("id")
      if !isNumericID(id) {
          writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid item id"})
          return
      }
      body, err := s.fetchCore(r.Context(), "/v1/items/"+id+"/content")
      if err != nil {
          badGateway(w, err)
          return
      }
      writeJSON(w, http.StatusOK, json.RawMessage(body))
  }
  ```

  Registrar a rota (procure o bloco de rotas em `main()` ou onde `handleItemSteps` está registrado):
  ```go
  mux.HandleFunc("GET /api/items/{id}/content", s.handleItemContent)
  ```

- [ ] **Step 4: Verificar que os testes passam**
  ```bash
  cd rara-console && go test -run TestHandleItemContent -v
  ```
  Esperado: PASS.

- [ ] **Step 5: Full test suite**
  ```bash
  cd rara-console && make test
  ```
  Esperado: PASS.

- [ ] **Step 6: Commit**
  ```bash
  git add rara-console/main.go rara-console/main_test.go
  git commit -m "feat(console): proxy GET /api/items/{id}/content for mega-thumbnail"
  ```

---

## Task 4: Frontend — tipo `ItemContent` e `fetchItemContent()`

**Files:**
- Modify: `rara-console/web/src/lib/curadoria.ts`
- Modify: `rara-console/web/src/lib/curadoria.test.ts`

**Interfaces:**
- Produces:
  ```typescript
  export type ItemContent = {
    lane: string;
    body?: string;
    sender?: string;
  };
  export async function fetchItemContent(id: number): Promise<ItemContent | null>
  ```

- [ ] **Step 1: Escrever testes falhos em `curadoria.test.ts`**

  Adicionar após os testes existentes:
  ```typescript
  describe('fetchItemContent', () => {
    it('returns parsed content on 200', async () => {
      const mock: ItemContent = { lane: 'news', body: 'hello' };
      global.fetch = vi.fn().mockResolvedValue({
        ok: true,
        json: async () => mock,
      } as unknown as Response);

      const result = await fetchItemContent(42);
      expect(result).toEqual(mock);
      expect(fetch).toHaveBeenCalledWith('/api/items/42/content');
    });

    it('returns null on non-200', async () => {
      global.fetch = vi.fn().mockResolvedValue({
        ok: false,
      } as unknown as Response);

      expect(await fetchItemContent(99)).toBeNull();
    });
  });
  ```

- [ ] **Step 2: Verificar que os testes falham**
  ```bash
  cd rara-console/web && npm test -- --run curadoria
  ```
  Esperado: FAIL.

- [ ] **Step 3: Adicionar tipo e função em `curadoria.ts`**

  Após os tipos existentes:
  ```typescript
  export type ItemContent = {
    lane: string;
    body?: string;
    sender?: string; // email only
  };

  export async function fetchItemContent(id: number): Promise<ItemContent | null> {
    const res = await fetch(`/api/items/${id}/content`);
    if (!res.ok) return null;
    return res.json() as Promise<ItemContent>;
  }
  ```

- [ ] **Step 4: Verificar que os testes passam**
  ```bash
  cd rara-console/web && npm test -- --run curadoria
  ```
  Esperado: PASS.

- [ ] **Step 5: Commit**
  ```bash
  git add rara-console/web/src/lib/curadoria.ts rara-console/web/src/lib/curadoria.test.ts
  git commit -m "feat(curadoria/ts): add ItemContent type and fetchItemContent()"
  ```

---

## Task 5: `MegaThumbnail.svelte`

**Files:**
- Create: `rara-console/web/src/lib/MegaThumbnail.svelte`

**Interfaces:**
- Consumes: `item: QuarantineItem`, `fetchItemContent`, `ItemContent`

- [ ] **Step 1: Criar o componente**

  ```svelte
  <script lang="ts">
    import type { QuarantineItem, ItemContent } from './curadoria.js';
    import { fetchItemContent } from './curadoria.js';

    export let item: QuarantineItem;

    let loading = false;
    let content: ItemContent | null = null;

    $: onItemChange(item);

    async function onItemChange(i: QuarantineItem) {
      content = null;
      if (i.lane === 'youtube') {
        content = { lane: 'youtube' };
        return;
      }
      loading = true;
      content = await fetchItemContent(i.id);
      loading = false;
    }

    function truncate(text: string, max = 800): string {
      return text.length > max ? text.slice(0, max) + '…' : text;
    }
  </script>

  {#if loading}
    <div class="shimmer-wrap" aria-busy="true" aria-label="Carregando prévia…">
      <div class="shimmer tall"></div>
      <div class="shimmer medium"></div>
      <div class="shimmer short"></div>
    </div>

  {:else if content?.lane === 'youtube' && item.source_ref}
    <iframe
      src="https://www.youtube.com/embed/{item.source_ref}"
      title={item.title ?? 'YouTube'}
      allow="accelerometer; autoplay; clipboard-write; encrypted-media; gyroscope; picture-in-picture"
      allowfullscreen
      loading="lazy"
    ></iframe>

  {:else if content?.lane === 'email' && (content.body || content.sender)}
    <div class="content-wrap">
      {#if content.sender}
        <div class="badge">De: {content.sender}</div>
      {/if}
      {#if content.body}
        <p class="body">{truncate(content.body)}</p>
      {/if}
    </div>

  {:else if content?.lane === 'news' && content.body}
    <div class="content-wrap">
      <p class="body">{truncate(content.body)}</p>
    </div>
  {/if}
  <!-- else: colapsa — sem conteúdo, sem erro visível -->

  <style>
    iframe {
      display: block;
      width: 100%;
      aspect-ratio: 16 / 9;
      border: none;
      border-radius: 0.5rem;
    }

    .content-wrap {
      height: 100%;
      overflow-y: auto;
      border: 1px solid var(--color-border, #e2e8f0);
      border-radius: 0.5rem;
      background: var(--color-surface, #fff);
    }

    .badge {
      font-size: 0.75rem;
      font-weight: 600;
      padding: 0.4rem 0.75rem;
      background: var(--color-surface-alt, #f8fafc);
      border-bottom: 1px solid var(--color-border, #e2e8f0);
      color: var(--color-text-muted, #64748b);
    }

    .body {
      margin: 0;
      padding: 0.75rem;
      font-size: 0.875rem;
      line-height: 1.6;
      white-space: pre-wrap;
      word-break: break-word;
      color: var(--color-text, #1e293b);
    }

    /* Shimmer */
    .shimmer-wrap {
      display: flex;
      flex-direction: column;
      gap: 0.5rem;
      padding: 0.75rem;
      border: 1px solid var(--color-border, #e2e8f0);
      border-radius: 0.5rem;
    }

    .shimmer {
      border-radius: 0.25rem;
      background: linear-gradient(
        90deg,
        var(--color-shimmer-base, #e2e8f0) 25%,
        var(--color-shimmer-hi, #f8fafc) 50%,
        var(--color-shimmer-base, #e2e8f0) 75%
      );
      background-size: 200% 100%;
      animation: shimmer 1.4s infinite;
    }

    .shimmer.tall   { height: 10rem; }
    .shimmer.medium { height: 1rem; width: 75%; }
    .shimmer.short  { height: 1rem; width: 50%; }

    @keyframes shimmer {
      0%   { background-position: 200% 0; }
      100% { background-position: -200% 0; }
    }
  </style>
  ```

- [ ] **Step 2: Verificar tipos**
  ```bash
  cd rara-console/web && npm run check
  ```
  Esperado: zero erros.

- [ ] **Step 3: Commit**
  ```bash
  git add rara-console/web/src/lib/MegaThumbnail.svelte
  git commit -m "feat(curadoria): add MegaThumbnail component with shimmer + per-lane rendering"
  ```

---

## Task 6: Wire no +page.svelte — layout 50/50

**Files:**
- Modify: `rara-console/web/src/routes/curadoria/+page.svelte`

**O que mudar:** O painel do item focado (dentro de `{#if focusedItem}`) precisa:
1. Importar `MegaThumbnail`
2. Envolver o conteúdo existente em um grid 50/50
3. A coluna direita só existe se o lane tem thumbnail (youtube, news, email)

**Lanes com thumbnail:** `['youtube', 'news', 'email']`

- [ ] **Step 1: Adicionar o import no `<script>` de +page.svelte**

  Após os imports existentes:
  ```typescript
  import MegaThumbnail from '$lib/MegaThumbnail.svelte';

  const lanesWithThumbnail = new Set(['youtube', 'news', 'email']);
  ```

- [ ] **Step 2: Envolver o painel do focusedItem em grid**

  Localize o bloco `{#if focusedItem}` na aba Decidir. O conteúdo atual (título, summary, botões) deve ser envolvido assim:

  **Antes (estrutura atual):**
  ```svelte
  {#if focusedItem}
    <!-- ... título, summary, botões ... -->
  {/if}
  ```

  **Depois:**
  ```svelte
  {#if focusedItem}
    <div class="decidir-grid" class:has-thumb={lanesWithThumbnail.has(focusedItem.lane)}>
      <div class="decidir-card">
        <!-- ... todo o conteúdo atual de título, summary, botões sem alteração ... -->
      </div>
      {#if lanesWithThumbnail.has(focusedItem.lane)}
        <div class="decidir-thumb">
          <MegaThumbnail item={focusedItem} />
        </div>
      {/if}
    </div>
  {/if}
  ```

  Adicionar no `<style>` de +page.svelte:
  ```css
  .decidir-grid {
    display: grid;
    grid-template-columns: 1fr;
    gap: 1.5rem;
  }

  .decidir-grid.has-thumb {
    grid-template-columns: 1fr 1fr;
  }

  .decidir-card {
    min-width: 0; /* evita overflow no grid */
  }

  .decidir-thumb {
    min-width: 0;
    display: flex;
    flex-direction: column;
  }
  ```

- [ ] **Step 3: Verificar build**
  ```bash
  cd rara-console/web && npm run build
  ```
  Esperado: build sem erros.

- [ ] **Step 4: Verificar tipos**
  ```bash
  cd rara-console/web && npm run check
  ```
  Esperado: zero erros.

- [ ] **Step 5: Commit final**
  ```bash
  git add rara-console/web/src/routes/curadoria/+page.svelte
  git commit -m "feat(curadoria/decidir): 50/50 grid layout with MegaThumbnail on right column"
  ```

---

## Self-Review

### Spec Coverage

| Requisito | Task |
|-----------|------|
| Auto-load quando item muda | Task 5 — `$: onItemChange(item)` |
| Shimmer durante fetch | Task 5 — `.shimmer-wrap` + `@keyframes shimmer` |
| YouTube player | Task 5 — iframe `/embed/{source_ref}`, sem backend |
| News body | Tasks 1+2+3+5 — `news_items.body` via rara-core |
| Email preview | Tasks 1+2+3+5 — `emails.sender + body` via rara-core |
| LinkedIn ignorado | Task 5 — `lanesWithThumbnail` não inclui `linkedin` |
| Podcast ignorado | Task 5 — `lanesWithThumbnail` não inclui `podcast` |
| Layout 50/50 | Task 6 — `grid-template-columns: 1fr 1fr` |
| Colapsa para 100% sem thumbnail | Task 6 — grid sem `.has-thumb` = `1fr` só |
| Clique no título abre original | Preservado — `<a href>` em +page.svelte não é alterado |
| Nada salvo no cliente | Sem localStorage/cache em nenhuma task |

### Placeholder Scan

Nenhum TBD ou TODO. Todo code block é código concreto.

### Type Consistency

- `ItemContentResult.Lane/Body/Sender` em Go → `ItemContent.lane/body/sender` em TypeScript — nomes consistentes (snake_case no JSON, camelCase no TS via tag `json:"lane"`).
- `QuarantineItem.source_ref` e `QuarantineItem.lane` usados na Task 5 — campos existentes confirmados.
- `pathID(w, r)` usado na Task 2 (surface.go) — helper já existe no arquivo.

### Gaps Residuais

- **`pathID` lê `{id}`**: confirme que o path value se chama `id` e não `itemID` (deve ser `{id}` igual aos outros handlers do surface.go — confirmado no código lido).
- **news_items.url = source_ref**: confirmado na exploração que `source_ref` para news é a URL completa, que é a PK de `news_items`.
- **emails.message_id = source_ref**: confirmado — o spine usa `message_id` como `source_ref` para emails.
- **`source_ref` para youtube**: é o video ID cru (`dQw4w9WgXcQ`), não a URL completa — o iframe monta `https://www.youtube.com/embed/{source_ref}` corretamente.
