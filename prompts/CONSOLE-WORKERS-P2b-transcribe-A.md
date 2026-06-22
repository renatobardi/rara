CONSOLE-WORKERS-#P2b-transcribe-A — rename rara-scribe→rara-transcribe + job consolidado (aditivo)

- **Nome da sessão:** nomeie esta sessão do Claude Code como `CONSOLE-WORKERS-#P2b-transcribe-A`.
- **Branch dedicada:** do **`main` atualizado**, crie `feat/console-workers-p2b-transcribe-a`. NÃO empilhar.
- **Doc de referência:** `CONSOLE-WORKERS-PLANO-E5b.pt-BR.md` §1.2/§1.6/§4. Espelho da
  **P2b-gate-A**/`extract-A`. Seguir o `CLAUDE.md`.

Fase **A (aditiva)** do app `transcribe` — o último da P2b. Renomeia `rara-scribe`→`rara-transcribe`,
builda a imagem nova multi-arch e cria o **job consolidado novo `rara-transcribe`** (para o
`echo`=cloud). **NÃO flipa `providers.app`** (continua `asr-youtube`/`asr-direct-audio`) → prod segue
no antigo. **Zero gap.** (O `caption`=resident no Mac roda o binário pelo launchd — o rebuild no Mac é
manual, na fase B; aqui não afeta o Mac.)

## Tarefa A — rename do módulo
- Renomear `rara-scribe/` → `rara-transcribe/`; atualizar `module` no `go.mod`; manter
  `replace rara-addon => ../rara-addon`; ajustar `Dockerfile` (multi-arch, padrão rara-gate). Renomear
  o binário se tiver nome próprio (ex.: `scribe-job` → `transcribe-job`).
- README: `rara-transcribe` serve **dois workers** — `caption`=YouTube (Mac), `echo`=áudio direto/
  podcast (cloud) — escolha por `item.Lane` (P1c), identidade por `SCRIBE_PROVIDER`/env.
- `cd rara-transcribe && make test && make lint` verdes.

## Tarefa B — workflows
- Renomear `ci-scribe.yml`→`ci-transcribe.yml`, `deploy-scribe.yml`→`deploy-transcribe.yml` (e
  `database-scribe.yml` se existir). Path filters → `rara-transcribe/**`.
- `ci-transcribe.yml`: validação multi-arch (amd64+arm64).
- `deploy-transcribe.yml`: buildx multi-arch da imagem `rara-transcribe` + deploy de **um job Cloud
  Run novo `rara-transcribe`** (para o echo). **Manter** o job antigo `rara-asr-direct-audio` (NÃO
  deletar aqui).

## Não fazer aqui (fase B)
- NÃO mexer no `providers.app` (continua `asr-youtube`/`asr-direct-audio`).
- NÃO atualizar allowlist, NÃO mexer no Mac (caption), NÃO deletar jobs/imagens antigos.

## Aceite
`ci-transcribe` valida o manifest multi-arch da imagem `rara-transcribe`. Após deploy, o job
`rara-transcribe` existe no GCP **além** do antigo. Prod inalterado. Código/workflows/Dockerfile já em
`rara-transcribe`; `rara-scribe` só pode sobrar em docs (sweep = P5).

---

## Ao terminar — ciclo de encerramento (Definition of Done)
1. **Commit** (convencional; corpo referencia o plano §4/P2b).
2. **PR base `main`**.
3. **CI** (`ci-transcribe`). Falhou → corrige até verde.
4. **CodeRabbit** → corrigir tudo → limpo + verde.
5. **Avisar o Renato** e **PAUSAR**. Não mergear.
6. Após aprovar + mergear, **acompanhar o deploy** (imagem + job `rara-transcribe` criados, antigo
   intacto). Falhou → corrige.
7. **Resumo final:** o que mudou, PR, CI/CodeRabbit, deploy (job `rara-transcribe`), e o follow-up
   **P2b-transcribe-B** (flip `app`→`transcribe` p/ caption/echo + **passo manual no Mac**: rebuild do
   binário `rara-transcribe` + restart do launchd do caption-mac + cleanup do job/imagem antigos).
