CONSOLE-WORKERS-#FIX-198 — deploys multi-arch pushar a tag :latest (agent puxa :latest)

- **Nome da sessão:** `CONSOLE-WORKERS-#FIX-198`.
- **Branch dedicada:** do `main`, `fix/198-push-latest-tag`. Seguir `CLAUDE.md`.
- **Contexto:** issue #198. O agent roda `docker run --pull=always <imagem-bare>` → o Docker resolve
  `:latest`. Mas o buildx dos deploys só pusha `tags: ${{ env.IMAGE }}` (= `:<sha>`); **`:latest`
  nunca é pushado** → o agent puxa uma `:latest` ausente/stale (hoje só funciona com tag manual).

## Tarefa — pushar `:latest` junto do `:<sha>`
Nos deploys multi-arch que o agent puxa (`deploy-distill.yml`, `deploy-gate.yml`, `deploy-extract.yml`,
`deploy-transcribe.yml`; e qualquer coletor que venha a ter placement vpc/mac), no step
"Build and push manifest list" (`docker/build-push-action`), incluir **as duas tags**:
```
tags: |
  ${{ env.IMAGE }}
  <REGION>-docker.pkg.dev/<PROJECT>/rara/rara-<app>:latest
```
(usar as mesmas vars `GCP_REGION`/`GCP_PROJECT_ID` que compõem o `IMAGE`; só trocar o sufixo `:<sha>`
por `:latest` na segunda tag). Assim o manifest list multi-arch é pushado também como `:latest`, e o
`--pull=always` do agent sempre pega o build mais novo.

## REGRAS
- Não mexer no deploy do Cloud Run Job em si (ele referencia `$IMAGE` por sha — ok).
- Manter o `:<sha>` (rastreável); `:latest` é adição.

## Aceite
Os 4 deploys pusham `:<sha>` **e** `:latest` (manifest list multi-arch). Após um deploy, o agent
(vpc/mac) com `--pull=always` pega a imagem nova sem tag manual.

## Encerramento (DoD)
1. Commit (`fix(deploy): push :latest tag for agent-pulled multi-arch images (#198)`).
2. PR base `main` → CI verde → CodeRabbit limpo → avisar Renato → PAUSAR.
3. Após aprovar + mergear, disparar um deploy (ex.: `gh workflow run deploy-distill.yml --ref main`) e
   confirmar a tag `:latest` no Artifact Registry; remover o workaround de tag manual. Fecha #198.
4. Resumo: 4 deploys com :latest, verificação no AR. **Encerra os 3 issues da P3.**
