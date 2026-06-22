CONSOLE-WORKERS-#P2b-transcribe-B — flip providers.app→transcribe + Mac (caption) + limpeza

- **Nome da sessão:** nomeie esta sessão do Claude Code como `CONSOLE-WORKERS-#P2b-transcribe-B`.
- **Branch dedicada:** do **`main` atualizado**, crie `feat/console-workers-p2b-transcribe-b`. NÃO empilhar.
- **Doc de referência:** `CONSOLE-WORKERS-PLANO-E5b.pt-BR.md` §1.6/§3/§4. Espelho da `gate-B`/`extract-B`
  + passo manual no Mac. Seguir o `CLAUDE.md`. Pré-requisito: transcribe-A mergeada (job + imagem
  `rara-transcribe` existem). **Fecha a P2b.**

Fase **B (cutover + limpeza)** do app `transcribe`. Flipa o `app` dos placements `caption` e `echo`
→ **`transcribe`**. `echo`=cloud (on_demand) passa a usar o job `rara-transcribe` automaticamente.
`caption`=resident no Mac → o flip do `app` é cosmético pra ele (resident não é job-target); o que
importa é o **rebuild do binário no Mac** (passo manual).

## Tarefa A — migration `022_transcribe_app_flip.sql` (core, idempotente)
- `UPDATE providers SET app = 'transcribe' WHERE worker IN ('caption','echo') AND app != 'transcribe';`
  Valida na branch Neon do PR; aplica no merge.

## Tarefa B — seed (core)
- `seed.go`: trocar `App: "asr-youtube"` (caption) e `App: "asr-direct-audio"` (echo) → **`App: "transcribe"`**.
- Testes: asserir `App == "transcribe"` pros 2; `make test`/`make lint` verdes.

## Ordem do cutover
1. Merge do PR → migration flipa `app`. `echo` (cloud): próximo dispatch executa `rara-transcribe`
   (job já existe da fase A) → verificar `last_error` limpo + um podcast flui.
2. **Mac (caption) — manual, Renato:** `cd rara-transcribe && make build` + reinstalar o binário no
   Mac + restart do launchd do caption-mac. (Não é prod-break: o binário antigo instalado segue
   claimando `caption-mac` com lane-branch da P1c até o rebuild; o rebuild é pra consistência + builds
   futuros.) O env do Mac (`SCRIBE_PROVIDER=caption-mac`) já está correto desde a P1c.
- (Allowlist: só relevante se um placement **vpc** de echo/caption for adicionado — aí incluir
  `transcribe=<imagem rara-transcribe>`. Não é necessário agora.)

## Limpeza (pós-verificação, ops — §2.1 do plano)
- Remover o job antigo `rara-asr-direct-audio` no Cloud Run.
- Remover a imagem antiga `rara-scribe` do Artifact Registry.

## Aceite
`cd rara-core && make test && make lint` verdes. Migration 022 idempotente. `echo` roda via
`rara-transcribe`; `caption-mac` no binário novo após o rebuild. Job/imagem antigos removidos. Grep
`asr-youtube`/`asr-direct-audio` como **valor de app** = 0 (continuam só como lane/identidade e na
migration `WHERE`). **P2b completa** (gate ✓ extract ✓ transcribe ✓).

---

## Ao terminar — ciclo de encerramento (Definition of Done)
1. **Commit** (convencional; corpo referencia o plano §4/P2b — fecha a P2b).
2. **PR base `main`**.
3. **CI** (`ci-core` + `database-core`). Falhou → corrige até verde.
4. **CodeRabbit** → corrigir tudo → limpo + verde.
5. **Avisar o Renato** + **os passos manuais** (rebuild Mac do caption; cleanup GCP). **PAUSAR**. Não mergear.
6. Após aprovar + mergear + Mac rebuild, **acompanhar** o roteamento via `rara-transcribe` (echo) e o
   caption-mac. Falhou → corrige.
7. **Resumo final:** o que mudou, PR, CI/CodeRabbit, migration+deploy, Mac rebuild, cleanup GCP.
   **P2b encerrada** → próximo bloco: **P3** (ops/runners: agent no Mac + operador adiciona placements
   vpc/mac) e **P4** (console).
