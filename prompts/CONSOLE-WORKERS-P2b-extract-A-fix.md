CONSOLE-WORKERS-#P2b-extract-A-fix — deploy-extract: remover exigência de runtime SA (destrava prod)

- **Nome da sessão:** nomeie esta sessão do Claude Code como `CONSOLE-WORKERS-#P2b-extract-A-fix`.
- **Branch dedicada:** do **`main` atualizado**, crie `fix/deploy-extract-runtime-sa`. NÃO empilhar.
- **Doc de referência:** `CONSOLE-WORKERS-PLANO-E5b.pt-BR.md` §4 (P2b). Seguir o `CLAUDE.md`.
- **URGENTE:** o extract está **travado em prod** (extract-B já flipou `app=extract`, mas o job
  `rara-extract` não foi criado porque o `deploy-extract.yml` falha).

## Causa
O `deploy-extract.yml` (herdado do `deploy-glean.yml`) exige a *repository variable*
`GLEAN_RUNTIME_SERVICE_ACCOUNT` (guard `:?... must be set`), que está **vazia** → o step "Deploy
Cloud Run Job" morre antes de criar o job. Os deploys irmãos (`deploy-gate.yml`, `deploy-distill.yml`,
`deploy-transcribe.yml`) **não** usam runtime SA própria — deployam o Cloud Run Job sem
`--service-account` (SA padrão) e funcionam. Logo, a exigência no extract é resíduo do glean.

## Tarefa — alinhar o deploy-extract ao padrão gate/distill
No `.github/workflows/deploy-extract.yml`, no step **"Deploy Cloud Run Job"**, remover as 3 referências
à runtime SA:
1. O bloco `env:` do step com `GLEAN_RUNTIME_SERVICE_ACCOUNT: ${{ vars.GLEAN_RUNTIME_SERVICE_ACCOUNT }}`.
2. A linha de guard `: "${GLEAN_RUNTIME_SERVICE_ACCOUNT:?...}"`.
3. A linha `--service-account "$GLEAN_RUNTIME_SERVICE_ACCOUNT"` dentro do `COMMON_ARGS`.

Conferir que o resto do step fica idêntico ao `deploy-gate.yml`/`deploy-distill.yml` (mesmo
`gcloud run jobs update || create`, `--set-secrets DATABASE_URL=...`, `--set-env-vars`, etc.).
Grep `GLEAN_RUNTIME_SERVICE_ACCOUNT` no repo = **0** depois (era só do glean).

## Aceite
Diff mínimo (só as 3 remoções). `deploy-extract.yml` consistente com gate/distill. (Sem testes Go —
é workflow; a validação real é re-rodar o deploy no encerramento.)

---

## Ao terminar — ciclo de encerramento (Definition of Done)
1. **Commit** (`fix(deploy): drop leftover glean runtime-SA requirement from deploy-extract`; corpo
   referencia o plano §4/P2b e que destrava o extract).
2. **Abrir o PR com base no `main`**.
3. **Acompanhar o CI**. Se falhar, corrigir e re-push até **verde**.
4. **Aguardar o CodeRabbit** → corrigir tudo → limpo + verde.
5. **Avisar o Renato** e **PAUSAR**. Não mergear.
6. Após o Renato aprovar e mergear:
   - `gh workflow run deploy-extract.yml --ref main` e acompanhar (`gh run watch <id>`).
   - Confirmar: `gcloud run jobs describe rara-extract --region=us-central1 --project=oute-rara` →
     **existe**.
   - Verificar que winnow/glean/scrub destravaram (`last_error` limpa no próximo dispatch; fila drena).
7. **Resumo final:** o que mudou, PR, CI/CodeRabbit, deploy do `rara-extract`, e confirmação de que o
   extract voltou a processar. (Follow-up: o `deploy-transcribe` precisa ser disparado também, e a
   transcribe-B só pode valer com o job `rara-transcribe` existindo.)
