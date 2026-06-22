CONSOLE-WORKERS-#FIX-200 — SA do dispatcher precisa de run.jobs.runWithOverrides

- **Nome da sessão:** `CONSOLE-WORKERS-#FIX-200`.
- **Branch dedicada (só pro doc):** do `main`, `docs/fix-200-runwithoverrides`. Seguir `CLAUDE.md`.
- **Contexto:** issue #200. Os jobs consolidados (`rara-gate`/`rara-extract`/`rara-transcribe`) servem
  N workers via **env override por execução** (dispatcher manda `SIFT_GATE`/`GLEAN_PROVIDER`/
  `SCRIBE_PROVIDER`; default do job = `PLACEHOLDER_PROVIDER`). Isso usa `run.jobs.runWithOverrides`.
  **Se a SA do dispatcher não tem essa permissão, esses jobs rodam com PLACEHOLDER → não claimam →
  itens de gate/extract/transcribe travam no cloud.**

## Verificar PRIMEIRO (Renato)
- Um item recente de gate/extract/transcribe está processando no cloud, ou travado? (`last_error` nos
  placements `sift-cloud`/`assay-cloud`/`glean-cloud`/`winnow-cloud`/`scrub-cloud`/`echo-cloud`).
- Ver erro de permissão nos logs do dispatcher (rara-runner) ou do Cloud Run execute:
  `journalctl -u rara-runner-dispatch -n 200 | grep -iE 'permission|override|PERMISSION_DENIED|run.jobs'`

## Fix (ops, Renato) — conceder o papel à SA do dispatcher
1. Identificar a SA que o **dispatcher** usa pra chamar o Cloud Run (a credencial GCP na VM/VPC do
   `rara-runner dispatch` — SA key / ADC). 
2. Conceder um papel que inclua `run.jobs.run` **e** `run.jobs.runWithOverrides` (ex.: `roles/run.developer`),
   no projeto `oute-rara`:
   ```
   gcloud projects add-iam-policy-binding oute-rara \
     --member="serviceAccount:<SA_DO_DISPATCHER>@oute-rara.iam.gserviceaccount.com" \
     --role="roles/run.developer"
   ```
   (Se preferir mínimo privilégio: role custom com `run.jobs.run` + `run.jobs.runWithOverrides` +
   `run.jobs.get`.)
3. Verificar: próximo dispatch executa o job consolidado COM override; um item de gate/extract/
   transcribe processa; `last_error` limpa.

## Tarefa do Claude Code (doc)
- Documentar em `INFRASTRUCTURE.md` (seção CI/CD ou Runner) que a SA do dispatcher **requer**
  `run.jobs.runWithOverrides` (além de `run.jobs.run`), porque os jobs consolidados dependem de env
  override por execução. Linkar à issue #200.

## Aceite
SA com a permissão; jobs consolidados processando no cloud (override aplicado); doc atualizado.

## Encerramento (DoD)
1. Commit do doc (`docs(infra): dispatcher SA requires run.jobs.runWithOverrides (#200)`).
2. PR base `main` → CI verde → CodeRabbit limpo → avisar Renato + **a ação de IAM** → PAUSAR.
3. Após o Renato conceder o papel + mergear, **verificar o dreno** do cloud. Fecha #200.
4. Resumo: permissão concedida, confirmação de processamento, doc. Próximo: #199 (allowlist).
