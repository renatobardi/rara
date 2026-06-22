CONSOLE-WORKERS-#P5b — normalizar distill-vpc.app (distill-local → distill) + allowlist

- **Nome da sessão:** nomeie esta sessão do Claude Code como `CONSOLE-WORKERS-#P5b`.
- **Branch dedicada:** do **`main` atualizado**, crie `feat/console-workers-p5b-distill-app`. NÃO empilhar.
- **Doc de referência:** `CONSOLE-WORKERS-PLANO-E5b.pt-BR.md` §1.6/§3/§4 (P5). Espelho da `gate-B`.
  Seguir o `CLAUDE.md`.

Segunda fatia da P5. Normaliza o `app` do `distill-vpc` (hoje `distill-local`, resíduo do backfill
`app=name` da P1a) para **`distill`**, deixando os dois placements do distill com `app=distill`
(targeting consistente → job/imagem `rara-distill`). **Mini-cutover** (distill-vpc é on_demand e está
**enabled** em prod): a allowlist precisa ter a chave `distill` ANTES do flip valer.

## Tarefa A — migration `023_distill_app_normalize.sql` (core, idempotente)
- `UPDATE providers SET app = 'distill' WHERE worker = 'distill' AND app != 'distill';`
  (atinge só o distill-vpc; distill-cloud já é `distill`; re-rodar = no-op.) Valida na branch Neon.

## Tarefa B — seed (core)
- `seed.go`: no row `provDistillLocal` (distill-vpc), trocar `App: "distill-local"` → **`App: "distill"`**.
- Testes: asserir `App == "distill"` nos dois placements do distill; `make test`/`make lint` verdes.

## Ordem do cutover (ATENÇÃO — distill-vpc está enabled)
- `distill-cloud` (cloud): já usa job `rara-distill` (`app=distill`) — sem mudança.
- `distill-vpc` (vpc via agent): a imagem é resolvida por `allowlist[app]`. Hoje a chave é
  `distill-local`; com o flip vira `distill`.
  1. **ANTES do flip:** na VPC, trocar a chave do `RUNNER_ALLOWED_IMAGES` de
     `distill-local=...rara-distill` → `distill=...rara-distill` e **recarregar o agent**
     (`systemctl restart rara-runner-agent.service`).
  2. Merge do PR → migration flipa o `app`.
  3. Verificar: próximo dispatch de `destilar` no VPC executa `rara-distill` via chave `distill`;
     `last_error` limpo no `distill-vpc`.

## Aceite
`cd rara-core && make test && make lint` verdes. Migration 023 idempotente. Os dois placements do
distill com `app=distill`. Grep `distill-local` como **valor de app** = 0 (continua só como nome de
placement `distill-vpc`... ou seja, `distill-local` NÃO deve mais aparecer como `app`; o NOME do
placement já é `distill-vpc` desde a P1b). Allowlist da VPC com a chave `distill`.

---

## Ao terminar — ciclo de encerramento (Definition of Done)
1. **Commit** (`refactor(core): normalize distill-vpc app to 'distill' (P5b)`; corpo ref. §1.6/§4).
2. **PR base `main`**.
3. **CI** (`ci-core` + `database-core`). Falhou → corrige até verde.
4. **CodeRabbit** → corrigir tudo → limpo + verde.
5. **Avisar o Renato** + **o passo da allowlist** (trocar `distill-local`→`distill` na VPC + restart
   do agent, ANTES de mergear/flipar). **PAUSAR**. Não mergear.
6. Após o Renato fazer a allowlist + aprovar + mergear, **acompanhar** o roteamento do `distill-vpc`
   via `rara-distill` (last_error limpo). Falhou → corrige.
7. **Resumo final:** o que mudou, PR, CI/CodeRabbit, migration+deploy, passo de allowlist, e o
   follow-up **P5c** (sweep de docs/comentários/fixtures — inclui os restos `rara-scribe` no
   `rara-transcribe/main.go:1400,1425`, `.gitignore`, `.env.example`).
