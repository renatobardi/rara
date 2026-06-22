CONSOLE-WORKERS-#P5c-2 — sweep de legado no CÓDIGO (comentários/logs/fixtures) + nits

- **Nome da sessão:** nomeie esta sessão do Claude Code como `CONSOLE-WORKERS-#P5c-2`.
- **Branch dedicada:** do **`main` atualizado**, crie `chore/console-workers-p5c2-code-sweep`. NÃO empilhar.
- **Doc de referência:** `CONSOLE-WORKERS-PLANO-E5b.pt-BR.md` §1 (de→para), §1.2 (módulos), §4 (P5/P6).
  Seguir o `CLAUDE.md`. **Só código/config** (docs foram a P5c-1).

Limpa os nomes antigos que sobraram em **comentários, logs, strings e fixtures de teste** no código.
A maioria é cosmética; o cuidado é **não tocar no que é legítimo nem na lógica de teste**.

## REGRAS (crítico)
- **Capabilities `gate_barato`/`gate_rico` (underscore) FICAM.** Lanes (`youtube`/`podcast`/`email`/
  `news`/`linkedin`) ficam. Não confundir com os providers antigos (hífen).
- **NÃO** tocar em `rara-core/migrations/**` (histórico, `WHERE` clauses) nem nos `CONSOLE-WORKERS*`/
  `prompts/` nem em `.superpowers/sdd/*` (changelog histórico).
- Em **fixtures de teste**: renomear os nomes de provider antigos (string literais) para os novos
  (mapping abaixo), **preservando a lógica/asserts** do teste. Rodar `make test` em cada agente tocado
  — verde obrigatório (não pode quebrar teste).
- Substring "tran**scribe**" / "tran**scribe-job**" é ok (não é legado).

## Mapping (de → para)
Providers: `gate-barato`→`sift-cloud`, `gate-rico`→`assay-cloud`, `asr-youtube`→`caption-mac`,
`asr-direct-audio`→`echo-cloud`, `extrair-email`→`winnow-cloud`, `extrair-news`→`glean-cloud`,
`extrair-linkedin`→`scrub-cloud`, `distill-local`→`distill-vpc`, `manual-inbox`→`stash`. Módulos/jobs:
`rara-sift`→`rara-gate`, `rara-glean`→`rara-extract`, `rara-scribe`→`rara-transcribe`,
`rara-gate-barato`/`rara-gate-rico`→`rara-gate`, `rara-asr-direct-audio`→`rara-transcribe`,
`rara-extrair-*`→`rara-extract`.

## Alvos (use o grep do aceite pra achar todos)
- `rara-transcribe/`: `main.go:1400,1425` (logs `rara-scribe worker` → `rara-transcribe`), `.gitignore`
  (`scribe-job`/`rara-scribe` → `transcribe-job`), `.env.example` (comentário "rara-scribe").
- `rara-core/`: comentários/logs em `linkedin.go`, `runners.go`, `reconciler.go`, `main.go`, `seed.go`;
  fixtures em `*_test.go` (email/news/podcast/sensitivity/worker/main/linkedin tests).
- `rara-runner/`: `dispatch_test.go`, `runner_test.go` (fixtures genéricos com nomes antigos).
- `rara-console/`: `main_test.go`, `step_hosts_test.go` (fixtures).
- `rara-addon/`: `addon_test.go` (comentário/exemplo).
- READMEs já foram (P5c-1); aqui só `.go`/`.sh`/`.gitignore`/`.env.example`/comentários em `.yml`.
- **Nit de consistência:** `linkedin.go` — `stash` está com `App: "manual-inbox"`; como stash é
  surface (não tem job/imagem), trocar pra `App: "stash"` (consistência; não é dispatchado, sem risco).

## Aceite
`make test && make lint` verdes em **todos** os agentes tocados. Grep de legado no código = 0 (fora
migrations/planos/.superpowers):
```
grep -rnE 'gate-barato|gate-rico|asr-youtube|asr-direct-audio|extrair-(email|news|linkedin)|distill-local|manual-inbox|rara-sift|rara-glean|rara-scribe|rara-gate-barato|rara-gate-rico|rara-asr-direct-audio|rara-extrair' --include=*.go --include=*.sh --include=*.yml --include=*.env.example . | grep -vE 'migrations/|CONSOLE-WORKERS|/prompts/|\.superpowers/'
```
(capabilities `gate_barato`/`gate_rico` underscore continuam presentes — corretas.)

---

## Ao terminar — ciclo de encerramento (Definition of Done)
1. **Commit** (`chore: sweep legacy names in code/comments/fixtures (P5c-2)`; corpo ref. §1/§4).
2. **PR base `main`**.
3. **CI verde** (todos os `ci-*` dos agentes tocados). Falhou → corrige até verde.
4. **CodeRabbit** → corrigir tudo → limpo + verde.
5. **Avisar o Renato** e **PAUSAR**. Não mergear.
6. Após aprovar + mergear, acompanhar os deploys dos agentes tocados (a maioria é comentário/teste, mas
   o `seed.go`/`linkedin.go` redeploya o core).
7. **Resumo final:** o que mudou, PR, CI/CodeRabbit, deploys. **Fecha a P5.** Próximo: **P6** (gate de
   zero-legado: grep = 0 no repo + confirmação de órfãos GCP + smoke).
