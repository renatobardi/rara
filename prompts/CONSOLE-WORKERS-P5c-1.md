CONSOLE-WORKERS-#P5c-1 — sweep de legado nos DOCS (md/mermaid/READMEs) → taxonomia nova

- **Nome da sessão:** nomeie esta sessão do Claude Code como `CONSOLE-WORKERS-#P5c-1`.
- **Branch dedicada:** do **`main` atualizado**, crie `docs/console-workers-p5c1-docs-sweep`. NÃO empilhar.
- **Doc de referência:** `CONSOLE-WORKERS-PLANO-E5b.pt-BR.md` §1 (de→para), §1.2 (módulos), §4 (P5/P6).
  Seguir o `CLAUDE.md`. **Só documentação** — nenhum `.go`/`.yml`/`.sh` (isso é a P5c-2).

Atualiza a documentação pra refletir a taxonomia nova (workers 1:1, módulos renomeados). Bulk, sem
risco funcional.

## Mapeamento (de → para) — aplicar nos textos
Providers/workers: `gate-barato`→`sift`(+`-cloud/-vpc`), `gate-rico`→`assay`, `asr-youtube`→`caption`,
`asr-direct-audio`→`echo`, `extrair-email`→`winnow`, `extrair-news`→`glean`, `extrair-linkedin`→`scrub`,
`distill-local`→`distill-vpc`, `manual-inbox`→`stash`. Módulos/jobs/imagens:
`rara-sift`→`rara-gate`, `rara-glean`→`rara-extract`, `rara-scribe`→`rara-transcribe`,
`rara-gate-barato`/`rara-gate-rico`→`rara-gate`, `rara-asr-direct-audio`→`rara-transcribe`,
`rara-extrair-*`→`rara-extract`. (Use os nomes claros do §1 quando fizer sentido no texto.)

## REGRAS (o que NÃO mexer)
- **Capabilities `gate_barato`/`gate_rico` (underscore) FICAM** — são nomes de capability, não de
  provider. Idem lanes (`youtube`/`podcast`/`email`/`news`/`linkedin`).
- **NÃO** tocar nos docs de plano `CONSOLE-WORKERS*.pt-BR.md` nem em `prompts/` (são o registro do
  rename, contêm de→para de propósito).
- Narrativa **histórica** (ex.: "antes era X") pode citar o nome antigo se for claramente histórico;
  o objetivo é que a descrição do **estado atual** use os nomes novos.

## Arquivos (ajustar onde houver nome antigo do estado atual)
Raiz: `ARCHITECTURE-2.0.pt-BR.md`, `ARCHITECTURE.md`, `ADDON-CONTRACT.pt-BR.md`, `DEPLOY-MATRIX.pt-BR.md`,
`DEPLOY-PHASES.pt-BR.md`, `DOCKER-MULTIMODULE.md`, `INFRASTRUCTURE.md`, `README.md`,
`RUNBOOK-OPS-W1.pt-BR.md`, `CONSOLE-PLAN.pt-BR.md`, `ATIVACAO-UNIFICADA.pt-BR.md`, `CLAUDE.md`,
`PIECES.mermaid.md`, `FLOWS.mermaid.md`. Por agente: READMEs de `rara-gate`, `rara-extract`,
`rara-transcribe`, `rara-distill`, `rara-clip`, `rara-dial`, `rara-harvest`, `rara-core` (+
`rara-core/deploy/RUNBOOK.md`), `rara-transcribe/DEPLOY.md`. Workflows-docs: `.github/workflows/README.md`,
e `.superpowers/sdd/task-b-report.md` (se for doc de estado). Use o grep abaixo pra achar todos.

## Aceite
Grep nos docs (fora de `CONSOLE-WORKERS*`/`prompts/`/migrations) por nome antigo de **provider/módulo**
= 0 (capabilities underscore e narrativa histórica explícita podem ficar):
```
grep -rnE 'gate-barato|gate-rico|asr-youtube|asr-direct-audio|extrair-(email|news|linkedin)|distill-local|manual-inbox|rara-sift|rara-glean|rara-scribe|rara-gate-barato|rara-gate-rico|rara-asr-direct-audio|rara-extrair' --include=*.md . | grep -vE 'CONSOLE-WORKERS|/prompts/'
```

---

## Ao terminar — ciclo de encerramento (Definition of Done)
1. **Commit** (`docs: sweep legacy worker/module names → new taxonomy (P5c-1)`; corpo ref. §1/§4).
2. **PR base `main`**.
3. **CI verde.** **CodeRabbit** → corrigir tudo → limpo.
4. **Avisar o Renato** e **PAUSAR**. Não mergear.
5. Após aprovar + mergear (docs, sem deploy de runtime). 
6. **Resumo final:** arquivos atualizados, PR, CI/CodeRabbit. Próximo: **P5c-2** (sweep no código:
   `.go` comentários/logs/fixtures + `.gitignore`/`.env.example` + nits de `app`) → depois **P6** (gate).
