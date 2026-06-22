CONSOLE-WORKERS-#P2b-gate-B — flip providers.app→gate + allowlist + limpeza (cutover do gate)

- **Nome da sessão:** nomeie esta sessão do Claude Code como `CONSOLE-WORKERS-#P2b-gate-B`.
- **Branch dedicada:** do **`main` atualizado**, crie `feat/console-workers-p2b-gate-b`. NÃO empilhar.
- **Doc de referência:** `CONSOLE-WORKERS-PLANO-E5b.pt-BR.md` §1.6/§3/§4 (P2b). Seguir o `CLAUDE.md`.
  Pré-requisito: P2b-gate-A mergeada (job `rara-gate` + imagem `rara-gate` existem no GCP).

Fase **B (cutover + limpeza)** do app `gate`. Flipa o targeting dos 4 placements do gate
(`sift-cloud`, `sift-vpc`, `assay-cloud`, `assay-vpc`) do `app` antigo (`gate-barato*`/`gate-rico*`)
para **`gate`** → o dispatcher passa a executar o job/imagem `rara-gate`. **Sem restart de worker**
(todos `on_demand`; o dispatcher lê `providers.app` fresh a cada passada e injeta env por wake).

## Tarefa A — migration `020_gate_app_flip.sql` (core, idempotente)

- `UPDATE providers SET app = 'gate' WHERE worker IN ('sift','assay');`
  (atinge os 4: sift-cloud/sift-vpc/assay-cloud/assay-vpc; re-rodar = no-op.)
- Valida na branch Neon do PR (`database-core`); aplica no merge.

## Tarefa B — seed (core)

- `seed.go`: trocar `App: "gate-barato"` / `"gate-barato-local"` / `"gate-rico"` / `"gate-rico-local"`
  → **`App: "gate"`** nos 4 providers do gate. (Os outros campos — Worker, Env com SIFT_GATE/
  SIFT_PROVIDER — ficam como estão.)
- Testes: asserir `App == "gate"` pros 4; `make test`/`make lint` verdes.

## Ordem do cutover (ATENÇÃO — ops antes do efeito no vpc)

Para `sift-cloud`/`assay-cloud` (cloud) o job `rara-gate` já existe (fase A) → flip imediato ok.
Para `sift-vpc`/`assay-vpc` (vpc via agent), a imagem é resolvida pela **allowlist** por `app`:

1. **ANTES de o flip valer no vpc:** adicionar `gate=<imagem rara-gate @sha256>` ao
   `RUNNER_ALLOWED_IMAGES` do agent na VPC e recarregar o agent. (Se `sift-vpc`/`assay-vpc` estiverem
   `enabled=false`, não há wake vpc — mas atualize a allowlist mesmo assim pra quando ligar.)
2. Merge do PR → migration flipa o `app`.
3. Verificar: próximo dispatch executa `rara-gate`; `providers.last_error` limpo nos 4; um item de
   cada gate flui.

## Limpeza (pós-verificação, ops — §2.1 do plano)
- Remover os jobs antigos `rara-gate-barato` e `rara-gate-rico` no Cloud Run.
- Remover a imagem antiga `rara-sift` do Artifact Registry.
- Remover as chaves antigas (`gate-barato*`/`gate-rico*`) do `RUNNER_ALLOWED_IMAGES`.
- Rollback (se preciso, §3): reverter o PR (app volta ao antigo) + reabilitar/uso dos jobs antigos.

## Aceite
`cd rara-core && make test && make lint` verdes. Migration 020 idempotente. Pós-merge, o gate roda via
`rara-gate` (cloud e, se enabled, vpc); jobs/imagem antigos removidos; allowlist limpa. Grep no repo:
`gate-barato`/`gate-rico` como **valor de app** = 0 (continuam só como `SIFT_GATE` capability e na
migration 020 `WHERE`/comentário).

---

## Ao terminar — ciclo de encerramento (Definition of Done)
1. **Commit** (convencional; corpo referencia o plano §4/P2b).
2. **PR base `main`**.
3. **CI** (`ci-core` + `database-core`). Falhou → corrige até verde.
4. **CodeRabbit** → corrigir tudo → limpo + verde.
5. **Avisar o Renato** + **os passos de ops** (allowlist antes; cleanup depois). **PAUSAR**. Não mergear.
6. Após o Renato fazer allowlist + aprovar + mergear, **acompanhar o deploy** e a verificação do
   roteamento via `rara-gate`. Falhou → corrige.
7. **Resumo final:** o que mudou, PR, CI/CodeRabbit, migration+deploy, checklist de ops (allowlist +
   cleanup GCP), e o follow-up **P2b-extract-A** (rara-glean→rara-extract, mesma fase A).
