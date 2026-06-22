CONSOLE-WORKERS-#P2b-deploy-firstcreate-fix — corrige first-create dos deploys gated (extract+transcribe)

- **Nome da sessão:** nomeie esta sessão do Claude Code como `CONSOLE-WORKERS-#P2b-deploy-firstcreate-fix`.
- **Branch dedicada:** do **`main` atualizado**, crie `fix/deploy-firstcreate`. NÃO empilhar.
- **SUBSTITUI** o `P2b-extract-A-fix` (que só removia a SA) — este cobre os dois bugs. Se a sessão do
  fix de SA ainda está aberta, redirecione-a pra este prompt (ou cancele e rode este).
- **URGENTE:** `extract` e `echo`/transcribe estão **travados em prod** (apps flipados, mas os jobs
  `rara-extract`/`rara-transcribe` não existem porque os deploys falham).

## Causa raiz (dois bugs nos deploys gated de first-create)
1. **Grep não reconhece a mensagem do gcloud.** O `update` de um job inexistente retorna
   `Job [X] could not be found.`, mas o guard `grep -qi "not found\|does not exist\|cannot be found"`
   **não casa "could not be found"** → o workflow dá `exit 1` sem nunca tentar o `create`. (O `gate`
   escapou porque NÃO tem esse guard — faz `update || { create }` direto.)
2. **(só extract) runtime-SA inexistente.** O `deploy-extract.yml` exige
   `GLEAN_RUNTIME_SERVICE_ACCOUNT` (var vazia) — resíduo do glean; gate/distill/transcribe não usam.

## Tarefa A — `deploy-extract.yml`
- Remover as 3 referências a `GLEAN_RUNTIME_SERVICE_ACCOUNT` (bloco `env:` do step, a linha de guard
  `:?...`, e `--service-account "$GLEAN_RUNTIME_SERVICE_ACCOUNT"` do `COMMON_ARGS`) — alinhar a
  gate/distill (SA padrão).
- Corrigir o grep de not-found para reconhecer "could not be found" (ver Tarefa C).

## Tarefa B — `deploy-transcribe.yml`
- Corrigir o grep de not-found (Tarefa C). Sem mudança de SA (já está limpo).

## Tarefa C — o conserto do grep (nos dois, e tb em `deploy-distill.yml` por consistência)
Trocar o padrão para incluir a frase do gcloud:
```
grep -qiE "not found|does not exist|could not be found|cannot be found"
```
(o essencial é adicionar **`could not be found`**.) Alternativa mais robusta, se preferir: remover o
guard e fazer `update || { echo "creating…"; create; }` como o `gate` faz — escolha do implementador,
mas mantenha o comportamento de criar no primeiro deploy.

## Não fazer
- NÃO mexer em core/seed/migrations. NÃO flipar nada. É só workflow (deploy).
- NÃO renomear nada. (O `SCRIBE_PROVIDER=asr-direct-audio` hardcoded no deploy-transcribe é default
  sobrescrito pelo dispatcher — nome legado, fica pro sweep da P5; **não** é escopo deste fix.)

## Aceite
Diffs mínimos. Grep `could not be found` presente nos guards de extract/transcribe/distill (ou guards
removidos no estilo gate). `GLEAN_RUNTIME_SERVICE_ACCOUNT` = 0 no repo.

---

## Ao terminar — ciclo de encerramento (Definition of Done)
1. **Commit** (`fix(deploy): first-create of gated jobs — match gcloud 'could not be found' + drop glean runtime-SA`).
2. **PR base `main`**.
3. **CI verde** (workflow-only; sem testes Go). **CodeRabbit** → corrigir tudo → limpo.
4. **Avisar o Renato** e **PAUSAR**. Não mergear.
5. Após o Renato aprovar e mergear, **disparar os dois deploys e confirmar os jobs**:
   ```
   gh workflow run deploy-extract.yml --ref main
   gh workflow run deploy-transcribe.yml --ref main
   # aguardar os runs:
   gh run list --workflow=deploy-extract.yml --limit 1
   gh run list --workflow=deploy-transcribe.yml --limit 1
   gcloud run jobs describe rara-extract    --region=us-central1 --project=oute-rara
   gcloud run jobs describe rara-transcribe --region=us-central1 --project=oute-rara
   ```
   Ambos devem existir. Aí extract e echo **destravam** (filas drenam, `last_error` limpa).
6. **Resumo final:** o que mudou, PR, CI/CodeRabbit, os dois deploys verdes, jobs `rara-extract` +
   `rara-transcribe` criados, e confirmação de que extract/echo voltaram a processar. Aí **a P2b fecha**.
