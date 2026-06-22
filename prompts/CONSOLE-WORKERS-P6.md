CONSOLE-WORKERS-#P6 — gate de zero-legado (limpar mockups antigos + verificação final)

- **Nome da sessão:** nomeie esta sessão do Claude Code como `CONSOLE-WORKERS-#P6`.
- **Branch dedicada:** do **`main` atualizado**, crie `chore/console-workers-p6-zero-legado`. NÃO empilhar.
- **Doc de referência:** `CONSOLE-WORKERS-PLANO-E5b.pt-BR.md` §0/§2/§6 (P6). Seguir o `CLAUDE.md`.
  **Fecha o épico E5b.**

Última fatia: garantir **zero legado** no repo + confirmar o estado de prod (GCP/DB). O sweep de
código/docs (P5) já zerou tudo, exceto **3 mockups/HTML antigos** na raiz — o único resíduo.

## Tarefa A — remover os mockups superados
`git rm` dos 3 (são protótipos pré-épico, substituídos pelo `rara-console` real + `ARCHITECTURE-2.0`):
- `plano-arquitetura-2.0.html`
- `console-mockup.html`
- `console-mockup-b-chatgpt.html`
(Se o Renato quiser manter como histórico, mover pra um `archive/` em vez de deletar — mas o default é
remover.)

## Tarefa B — gate de grep (tem que dar 0)
```
grep -rnE 'gate-barato|gate-rico|asr-youtube|asr-direct-audio|extrair-(email|news|linkedin)|distill-local|manual-inbox|rara-sift|rara-glean|rara-scribe|rara-gate-barato|rara-gate-rico|rara-asr-direct-audio|rara-extrair|GLEAN_RUNTIME_SERVICE_ACCOUNT|com\.rara\.scribe' . \
  | grep -vE '\.git/|node_modules|migrations/|CONSOLE-WORKERS|/prompts/|\.superpowers/|transcribe|archive/'
```
Resultado esperado: **vazio**. Legítimos que ficam (não contam): capabilities `gate_barato`/`gate_rico`
(underscore), `migrations/**` (WHERE histórico), docs de plano `CONSOLE-WORKERS*`, `prompts/`,
`.superpowers/` (changelog), e "tran**scribe**" (substring).

## Tarefa C — verificação GCP (Renato roda; já deve estar ok)
- Jobs antigos removidos (esperado *not found*): `rara-gate-barato`, `rara-gate-rico`,
  `rara-asr-direct-audio`, `rara-extrair-email/news/linkedin`.
- Imagens antigas removidas: `rara-sift`, `rara-glean`, `rara-scribe`.
- Jobs novos existem: `rara-gate`, `rara-extract`, `rara-transcribe`, `rara-distill`.

## Tarefa D — verificação DB (Renato roda)
- `SELECT DISTINCT app FROM providers;` → só nomes novos (gate/extract/transcribe/distill/harvest/…),
  nenhum `*-local`/`gate-barato`/etc.
- `SELECT DISTINCT assigned_provider FROM item_steps WHERE assigned_provider IS NOT NULL;` e os
  `routing_policies.fallback` → só nomes novos de placement.

## Tarefa E — smoke
Um item por lane flui via worker novo (youtube→caption, podcast→echo, news→glean, email→winnow,
linkedin→scrub, gates→sift/assay, destilar→distill); `last_error` limpo; console mostra tudo certo.

## Aceite
Tarefa B = vazio. Mockups removidos. GCP/DB sem nome antigo. Smoke ok. **Épico E5b completo.**

---

## Ao terminar — ciclo de encerramento (Definition of Done)
1. **Commit** (`chore: remove superseded HTML mockups — zero-legacy gate (P6)`; corpo ref. §0/§6).
2. **PR base `main`**.
3. **CI verde.** **CodeRabbit** → corrigir tudo → limpo.
4. **Avisar o Renato** (com o resultado do grep gate + checklist GCP/DB/smoke pra ele rodar). **PAUSAR**.
5. Após aprovar + mergear.
6. **Resumo final do épico:** P0→P6, o que mudou no total (roteamento simplificado, rename 1:1,
   multi-runtime, observabilidade), e o backlog que fica pra depois (P3 ops/runners no Mac; fase 2:
   custo \$/tempo de execução; Agents).
