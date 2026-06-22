CONSOLE-WORKERS-#P2b-extract-A — rename rara-glean→rara-extract + job consolidado (aditivo)

- **Nome da sessão:** nomeie esta sessão do Claude Code como `CONSOLE-WORKERS-#P2b-extract-A`.
- **Branch dedicada:** do **`main` atualizado**, crie `feat/console-workers-p2b-extract-a`. NÃO empilhar.
- **Doc de referência:** `CONSOLE-WORKERS-PLANO-E5b.pt-BR.md` §1.2/§1.6/§4 (P2b). Modelos: o que a
  **P2b-gate-A** fez (rara-sift→rara-gate) + `deploy-sift.yml`/`ci-sift.yml` + `DOCKER-MULTIMODULE.md`.
  Seguir o `CLAUDE.md`.

Fase **A (aditiva)** do app `extract`. Renomeia `rara-glean`→`rara-extract`, builda a imagem nova
multi-arch e cria o **job consolidado novo `rara-extract`**. **NÃO flipa `providers.app`** (continua
`extrair-email`/`extrair-news`/`extrair-linkedin`) → prod segue nos jobs antigos. **Zero gap.** O flip
+ limpeza é a fase B. (glean era single-arch — a migração multi-arch acontece aqui, junto do rename.)

## Tarefa A — rename do módulo
- Renomear `rara-glean/` → `rara-extract/`; atualizar `module` no `go.mod`; manter
  `replace rara-addon => ../rara-addon`; ajustar o `Dockerfile` (multi-módulo/multi-arch, padrão do
  rara-gate/rara-distill).
- README do app: `rara-extract` serve **três workers** por lane — `glean`=news, `winnow`=email,
  `scrub`=linkedin (a escolha é por `item.Lane`, identidade por `GLEAN_PROVIDER`/env). (Sweep amplo de
  docs = P5.)
- `cd rara-extract && make test && make lint` verdes.

## Tarefa B — workflows
- Renomear `ci-glean.yml`→`ci-extract.yml`, `deploy-glean.yml`→`deploy-extract.yml` (e
  `database-glean.yml` se existir). Path filters → `rara-extract/**`.
- `ci-extract.yml`: validação multi-arch (amd64+arm64), padrão do ci-gate.
- `deploy-extract.yml`: buildx multi-arch da imagem `rara-extract` + deploy de **um job Cloud Run
  novo `rara-extract`** (consolidado). **Manter** os jobs antigos (`rara-extrair-email`/`-news`/
  `-linkedin` — confira os nomes reais; NÃO deletar aqui).

## Não fazer aqui (fase B)
- NÃO mexer no `providers.app` (continua `extrair-email`/`extrair-news`/`extrair-linkedin`).
- NÃO atualizar allowlist nem deletar jobs/imagens antigos.

## Aceite
`ci-extract` valida o manifest multi-arch da imagem `rara-extract`. Após deploy, o job `rara-extract`
existe no GCP **além** dos antigos. Prod inalterado (app ainda aponta pros jobs antigos). Código/
workflows/Dockerfile já em `rara-extract`; `rara-glean` só pode sobrar em docs (sweep = P5).

---

## Ao terminar — ciclo de encerramento (Definition of Done)
1. **Commit** (convencional; corpo referencia o plano §4/P2b).
2. **PR base `main`**.
3. **CI** (`ci-extract`). Falhou → corrige até verde.
4. **CodeRabbit** → corrigir tudo → limpo + verde.
5. **Avisar o Renato** e **PAUSAR**. Não mergear.
6. Após aprovar e mergear, **acompanhar o deploy** (imagem + job `rara-extract` criados, antigos
   intactos). Falhou → corrige.
7. **Resumo final:** o que mudou, PR, CI/CodeRabbit, deploy (job `rara-extract`), e o follow-up
   **P2b-extract-B** (flipar `providers.app`→`extract` p/ winnow/glean/scrub + allowlist + remover
   jobs/imagem antigos).
