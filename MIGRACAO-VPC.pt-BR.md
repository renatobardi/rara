# Migração GCP Cloud Run → VPC Oracle — CONCLUÍDO ✅

> **Status:** cutover realizado em 23/jun/2026. Workers rodam VPC-first via `rara-runner agent`
> (docker run); Cloud Run permanece como fallback ordenado. Caption (YouTube) continua no Mac.

Objetivo original: tirar o compute dos workers do Cloud Run e rodar no VPC Oracle (Ampere/arm64),
onde o `rara-runner agent` já rodava. Cloud Run Job só cobra em execução → mover pro VPC (VM já
paga) zerando o gasto de compute. O multi-runtime já existia (E5b/P3: cada worker tem placements
nos 3 runtimes); esta migração **ativou** o VPC-first.

---

## O que mudou em produção

- **Roteamento VPC-first:** cada worker tem um placement `<worker>-vpc` (distill, sift, assay,
  winnow, glean, scrub, echo, harvest, shelf, dial, feed, courier, clip), `enabled` e ordenado
  **à frente** do `<worker>-cloud` no `routing_policies.fallback`. Habilitados quando
  `RUNNER_LOCAL_URL` está setado no `/etc/rara-core/env`.
- **Agent no VPC** (host `kura-oute-server`, endpoint tailnet `http://100.66.254.24:9000`):
  executa via `docker run --pull=always <imagem>:latest`. Roda como usuário `ubuntu` (autenticado
  no Artifact Registry). Serviços systemd: `rara-runner-agent` e `rara-runner-dispatch`.
- **Engine/modelo via body env (seed):** `distill-vpc` carrega `CURATE_ENGINE=litellm` +
  `LITELLM_MODEL=groq-llama`; `sift-vpc`/`assay-vpc` carregam `LITELLM_MODEL=groq-fast`.
- **LiteLLM** roda como container Docker no VPC (gateway groq), acessível em `172.17.0.1:4010`
  (bridge docker0 → host).

---

## Runbook — cutover (passos na ordem)

### Pré-requisitos: `:latest` no Artifact Registry

Todos os apps precisam ter `:latest` materializado no AR antes do cutover. Para qualquer app
que ainda não teve um deploy pós-#198:

```bash
# Disparar deploy manual (workflow_dispatch):
gh workflow run deploy-<app>.yml --ref main
# Verificar que o digest mudou antes de prosseguir:
gcloud artifacts docker images list \
  REGION-docker.pkg.dev/PROJECT/rara/rara-<app> \
  --filter="tags:latest" --format="value(digest,updateTime)"
```

Apps consolidados: `transcribe` (echo + caption), `gate` (sift + assay), `extract` (winnow + glean + scrub).

### 1. Configurar `RUNNER_ALLOWED_IMAGES` no VPC

```bash
# No host VPC (ubuntu):
sudo nano /etc/rara-runner/agent.env
# Adicionar/atualizar RUNNER_ALLOWED_IMAGES com todos os apps:
# harvest=REGION-docker.pkg.dev/PROJECT/rara/rara-harvest,
# shelf=…/rara-shelf, dial=…/rara-dial, feed=…/rara-feed,
# courier=…/rara-courier, clip=…/rara-clip,
# transcribe=…/rara-transcribe, extract=…/rara-extract,
# distill=…/rara-distill, gate=…/rara-gate
# (chave = providers.app bare — sem prefixo "rara-")

sudo chmod 600 /etc/rara-runner/agent.env
```

### 2. Configurar `/etc/rara-runner/worker.env`

```bash
sudo touch /etc/rara-runner/worker.env
sudo chown ubuntu:ubuntu /etc/rara-runner/worker.env
sudo chmod 600 /etc/rara-runner/worker.env
nano /etc/rara-runner/worker.env
# DATABASE_URL=postgresql://...
# LITELLM_BASE_URL=http://172.17.0.1:4010
# LITELLM_API_KEY=...
```

### 3. Setar `RUNNER_LOCAL_URL` e variáveis de modelo no `/etc/rara-core/env`

```bash
sudo nano /etc/rara-core/env
# Adicionar/confirmar:
# RUNNER_LOCAL_URL=http://100.66.254.24:9000   (tailnet IP do agent)
# DISTILL_MODEL=groq-llama
# GATE_MODEL=groq-fast
```

### 4. Reiniciar serviços e re-seed

```bash
sudo systemctl restart rara-runner-agent rara-runner-dispatch
# Re-seed: habilita os placements vpc (lê RUNNER_LOCAL_URL no startup)
core-job seed
```

### 5. Verificação (4 sinais)

```bash
# 1. Providers vpc enabled, last_error vazio:
psql "$DATABASE_URL" -c "SELECT app, name, enabled, last_error FROM providers WHERE name LIKE '%-vpc' ORDER BY app;"

# 2. Item steps roteando para vpc:
psql "$DATABASE_URL" -c "SELECT assigned_provider, count(*) FROM item_steps WHERE status='pending' GROUP BY 1 ORDER BY 2 DESC LIMIT 10;"

# 3. Prova do custo — execuções Cloud Run param de crescer:
gcloud run jobs executions list --project PROJECT --region REGION \
  --filter="metadata.labels.job-name:rara-distill OR metadata.labels.job-name:rara-gate" \
  --format="table(metadata.name, metadata.creationTimestamp)" | head -20

# 4. Smoke — last_error limpo após 1 execução por worker vpc:
psql "$DATABASE_URL" -c "SELECT name, last_error, heartbeat_at FROM providers WHERE name LIKE '%-vpc';"
```

### Rollback

Reverter é apenas desabilitar o VPC — sem operações destrutivas:

```bash
# Opção A: tirar RUNNER_LOCAL_URL do /etc/rara-core/env + re-seed
# (desabilita todos os placements vpc; dispatcher volta ao *-cloud)
sudo nano /etc/rara-core/env   # remover RUNNER_LOCAL_URL
core-job seed

# Opção B: disable granular via SQL (sem re-seed):
psql "$DATABASE_URL" -c "UPDATE providers SET enabled=false WHERE name LIKE '%-vpc';"
```

---

## Gotchas — armadilhas que custaram tempo

### Deploys gated: `workflow_dispatch` só

`deploy-runner.yml` e os `deploy-<worker>.yml` são `workflow_dispatch`. **Mexeu em código?
Rode o deploy manual** — não sobe no merge automático. O processo em execução no VPC precisa
de `systemctl restart` para pegar o binário novo (o dispatch ficou stale 3 dias sem restart).

```bash
gh workflow run deploy-runner.yml
sudo systemctl restart rara-runner-dispatch rara-runner-agent
```

### Verificar o digest do `:latest` antes do cutover

Um deploy disparado logo após o merge pode buildar antes da `main` propagar no AR. Sempre confirmar
que o digest mudou (ver comando no passo "Pré-requisitos" acima). Aconteceu no PR #4 do épico:
1º deploy não materializou; precisou re-disparar.

### `sudo docker` não tem auth no Artifact Registry

O cred helper do Docker está configurado no usuário `ubuntu`, não no `root`. Rodar docker
**sem sudo** no VPC. O `rara-runner-agent.service` usa `SupplementaryGroups=docker` para isso.

### Chave da allowlist = `providers.app` bare (sem prefixo "rara-")

O dispatcher envia `req.App = providers.app`. A chave na `RUNNER_ALLOWED_IMAGES` deve ser o
nome bare: `gate` (não `rara-gate`), `distill` (não `rara-distill`). Apps consolidados:

| providers.app | workers cobertos |
|---|---|
| `transcribe` | caption (Mac), echo (VPC/cloud) |
| `gate` | sift (gate_barato), assay (gate_rico) |
| `extract` | winnow, glean, scrub |

Chave errada → `providers.last_error = "app not in allowlist"` + HTTP 403 (visível na console).

### groq 429 = quota esgotada

Drenar backlog grande em lotes — rajada satura a quota Groq. Aguardar recarregar (~1 min) e
continuar.

### `assigned_provider` é sticky

Backlog atribuído ao cloud antes da migração não re-roteia sozinho para o VPC. Re-rotear
manualmente se necessário:

```sql
UPDATE item_steps
SET assigned_provider = 'distill-vpc'
WHERE status = 'pending'
  AND assigned_provider = 'distill-cloud';
-- Repetir para sift→sift-vpc, assay→assay-vpc, etc.
```

Candidato a melhoria: reconciler auto-re-rotear pending preso em provider offline.

### Dívida técnica (não corrigida neste épico)

`deploy-distill.yml` e `deploy-gate.yml` não setam `CURATE_ENGINE` no `ENV_VARS` do Cloud Run
Job. O path cloud depende de estado manual no Secret Manager/console GCP. Corrigir ao retornar
ao Cloud Run por alguma razão.

---

## Fatias do épico (histórico)

| Fatia | PR | Status |
|---|---|---|
| #1 seed VPC-first (core/seed.go) | #205 | ✅ merged |
| #2 collectors latest + dispatch fix | #206 | ✅ merged |
| #3 engine+model no body env | #207 | ✅ merged |
| #4 distill structured save robustness | fix/distill-structured-save-robustness | ✅ merged |
| #5 docs/READMEs VPC-first sweep | docs/migracao-vpc | ✅ este PR |
