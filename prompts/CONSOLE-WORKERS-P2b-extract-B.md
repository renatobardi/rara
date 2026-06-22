CONSOLE-WORKERS-#P2b-extract-B — flip providers.app→extract + limpeza (cutover do extract)

- **Nome da sessão:** nomeie esta sessão do Claude Code como `CONSOLE-WORKERS-#P2b-extract-B`.
- **Branch dedicada:** do **`main` atualizado**, crie `feat/console-workers-p2b-extract-b`. NÃO empilhar.
- **Doc de referência:** `CONSOLE-WORKERS-PLANO-E5b.pt-BR.md` §1.6/§3/§4 (P2b). Espelho da
  **P2b-gate-B**. Seguir o `CLAUDE.md`. Pré-requisito: P2b-extract-A mergeada (job + imagem
  `rara-extract` existem no GCP).

Fase **B (cutover + limpeza)** do app `extract`. Flipa o `app` dos 3 placements do extract
(`winnow`=extrair-email, `glean`=extrair-news, `scrub`=extrair-linkedin) → **`extract`**, fazendo o
dispatcher usar o job/imagem `rara-extract`. **Sem restart** (on_demand; dispatcher lê `app` fresh).
Extract é **cloud-only** (não há placement vpc/mac) → cutover simples, sem urgência de allowlist.

## Tarefa A — migration `021_extract_app_flip.sql` (core, idempotente)
- `UPDATE providers SET app = 'extract' WHERE worker IN ('glean','winnow','scrub');`
  (atinge os 3; re-rodar = no-op.) Valida na branch Neon do PR; aplica no merge.

## Tarefa B — seed (core)
- `seed.go`: trocar `App: "extrair-email"` / `"extrair-news"` / `"extrair-linkedin"` → **`App: "extract"`**
  nos 3 providers do extract. (Worker, Env/identidade, lane ficam como estão.)
- Testes: asserir `App == "extract"` pros 3; `make test`/`make lint` verdes.

## Ordem do cutover
Como todos os 3 são **cloud** e o job `rara-extract` já existe (fase A), o flip é imediato:
1. Merge do PR → migration flipa o `app`.
2. Verificar: próximo dispatch executa `rara-extract`; `last_error` limpo nos 3; um item de cada lane
   (email/news/linkedin) flui.
- (Allowlist: só relevante se/quando um placement **vpc/mac** de extract for adicionado — aí incluir
  `extract=<imagem rara-extract>`. Não é necessário agora.)

## Limpeza (pós-verificação, ops — §2.1 do plano)
- Remover os jobs antigos `rara-extrair-email` / `rara-extrair-news` / `rara-extrair-linkedin` no
  Cloud Run (confira os nomes reais).
- Remover a imagem antiga `rara-glean` do Artifact Registry.
- Rollback (§3): reverter o PR (app volta ao antigo) + reabilitar os jobs antigos.

## Aceite
`cd rara-core && make test && make lint` verdes. Migration 021 idempotente. Pós-merge, o extract roda
via `rara-extract`; jobs/imagem antigos removidos. Grep `extrair-email/news/linkedin` como **valor de
app** = 0 (continuam só como lane/identidade e na migration `WHERE`/comentário).

---

## Ao terminar — ciclo de encerramento (Definition of Done)
1. **Commit** (convencional; corpo referencia o plano §4/P2b).
2. **PR base `main`**.
3. **CI** (`ci-core` + `database-core`). Falhou → corrige até verde.
4. **CodeRabbit** → corrigir tudo → limpo + verde.
5. **Avisar o Renato** + **os passos de ops** (cleanup dos jobs/imagem antigos). **PAUSAR**. Não mergear.
6. Após aprovar + mergear, **acompanhar o deploy** e a verificação do roteamento via `rara-extract`.
   Falhou → corrige.
7. **Resumo final:** o que mudou, PR, CI/CodeRabbit, migration+deploy, cleanup GCP, e o follow-up
   **P2b-transcribe-A** (rara-scribe→rara-transcribe; ⚠️ caption-mac é resident no Mac → passo manual
   no Mac na fase B).
