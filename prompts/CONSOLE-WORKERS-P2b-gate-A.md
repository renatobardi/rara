CONSOLE-WORKERS-#P2b-gate-A — rename rara-sift→rara-gate + job consolidado (aditivo, sem flipar app)

- **Nome da sessão:** nomeie esta sessão do Claude Code como `CONSOLE-WORKERS-#P2b-gate-A`.
- **Branch dedicada:** partindo do **`main` atualizado** (`git fetch origin` + branch de
  `origin/main`), crie `feat/console-workers-p2b-gate-a`. NÃO empilhar.
- **Doc de referência:** `CONSOLE-WORKERS-PLANO-E5b.pt-BR.md` §1.2/§1.6/§4 (P2b). Modelos:
  `deploy-sift.yml`/`ci-sift.yml` (já multi-arch) + `DOCKER-MULTIMODULE.md`. Seguir o `CLAUDE.md`.

Fase **A (aditiva)** do app `gate`. Renomeia o módulo `rara-sift`→`rara-gate`, builda a imagem nova
multi-arch e cria o **job consolidado novo `rara-gate`**. **NÃO flipa `providers.app`** (continua
gate-barato/gate-rico) → prod segue nos jobs antigos `rara-gate-barato`/`rara-gate-rico`. **Zero gap.**
O flip + limpeza é a fase B.

## Tarefa A — rename do módulo

- Renomear o diretório `rara-sift/` → `rara-gate/`; atualizar o `module` path no `go.mod`; manter o
  `replace rara-addon => ../rara-addon`. Ajustar o `Dockerfile` (paths/contexto) pro nome novo.
- README do app: descrever que `rara-gate` serve **dois workers** (`sift`=gate_barato,
  `assay`=gate_rico) por env (`SIFT_GATE`). (Sweep amplo de docs é a P5 — aqui só o README do app.)
- Confirmar `cd rara-gate && make test && make lint` verdes (build/test do binário sob o nome novo).

## Tarefa B — workflows

- Renomear `ci-sift.yml`→`ci-gate.yml` e `deploy-sift.yml`→`deploy-gate.yml` (e `database-sift.yml`
  se existir — gate provavelmente não tem migrations). Ajustar **path filters** pra `rara-gate/**`.
- `ci-gate.yml`: validação multi-arch (amd64+arm64), como o ci-sift.
- `deploy-gate.yml`: buildx multi-arch da imagem `rara-gate` + deploy de **um job Cloud Run novo
  `rara-gate`** (consolidado). O dispatcher executa job com overrides de env por execução (SIFT_GATE +
  SIFT_PROVIDER) — isso passa a ser usado só na fase B. **Manter** os jobs `rara-gate-barato`/
  `rara-gate-rico` (NÃO deletar aqui).

## Não fazer aqui (fase B)
- NÃO mexer no `providers.app` (core/seed) — continua `gate-barato`/`gate-rico` etc.
- NÃO atualizar allowlist nem deletar jobs/imagens antigos.

## Aceite
`ci-gate` valida o manifest multi-arch da imagem `rara-gate`. Após deploy, o job `rara-gate` existe no
GCP **além** dos antigos. Prod inalterado (app ainda aponta pros jobs antigos → dispatcher usa eles).
Grep `rara-sift` no repo só pode sobrar em docs (sweep é P5) — código/workflows/Dockerfile já em
`rara-gate`.

## Persistência do plano
Incluir no commit o `CONSOLE-WORKERS-PLANO-E5b.pt-BR.md` (estava untracked e foi perdido uma vez) pra
ele passar a viver no git.

---

## Ao terminar — ciclo de encerramento (Definition of Done)
1. **Commit** (convencional; corpo referencia o plano §4/P2b; incluir o doc do plano). 
2. **Abrir o PR com base no `main`**.
3. **Acompanhar o CI** (`ci-gate`). Se falhar, corrigir e re-push até **verde**.
4. **Aguardar o CodeRabbit**. 
5. **Corrigir TODOS os apontamentos** via Claude Code (security primeiro). Re-rodar até limpo + verde.
6. **Avisar o Renato** e **PAUSAR**. Não mergear.
7. Após aprovar e mergear, **acompanhar o deploy** (`deploy-gate` cria a imagem + job `rara-gate`).
   Confirmar que o job novo existe e os antigos seguem intactos. Se falhar, corrigir e re-push.
8. **Resumo final:** o que mudou, PR, CI/CodeRabbit, deploy (job `rara-gate` criado), e o follow-up
   **P2b-gate-B** (flipar `providers.app`→`gate` + allowlist + remover jobs/imagem antigos).
