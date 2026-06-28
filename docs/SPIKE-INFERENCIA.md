# Spike — Inferência: mapa do caminho LITELLM_* (fase 0)

> Resultado do spike `feat/console-infer-0-spike`. Sem mudança de código de produção.
> Companion: [CONSOLE-INFERENCIA.pt-BR.md](../CONSOLE-INFERENCIA.pt-BR.md)

---

## 1. Caminho completo `LITELLM_BASE_URL` / `LITELLM_MODEL` → container

### Dois vetores de injeção (importante: são diferentes)

**`LITELLM_BASE_URL`** — fixada em deploy time, nunca no banco:
- Cloud Run Jobs (distill, gate, hone): `--set-env-vars "LITELLM_BASE_URL=$LITELLM_BASE_URL"` nos workflows
  `.github/workflows/deploy-distill.yml:123` / `deploy-gate.yml:120` / `deploy-hone.yml:106`
- VPC runner (workers ativados por rara-runner): carregada em `RUNNER_WORKER_ENV_FILE` (`~/.rara-runner/worker.env`)
  e lida em `rara-runner/main.go:78` como `baseEnv`. A `baseEnv` é injetada em **todos** os containers
  via `mergeEnv(baseEnv, req.Env)` em `rara-runner/agent.go:185`.

**`LITELLM_MODEL`** — vem do banco (`providers.env`), mas com exceções hardcoded no deploy:

| Worker | Fonte do model |
|--------|----------------|
| distill (Cloud Run) | hardcoded `groq-llama` em `deploy-distill.yml:131` (com nota: `vars.LITELLM_MODEL=gemini-flash` spend-capped) |
| reason (Cloud Run) | `vars.LITELLM_MODEL` (GitHub Actions var) em `deploy-reason.yml:112` |
| gate (Cloud Run) | hardcoded `groq-fast` em `deploy-gate.yml:134` |
| hone (Cloud Run) | hardcoded `groq-llama` em `deploy-hone.yml:114` |
| workers via rara-runner | `providers.env` JSONB → dispatch → `req.Env` → `docker run -e` |

### Cadeia banco → container (path do rara-runner)

```
seed.go:130-145      DISTILL_MODEL / GATE_MODEL env vars  
  → providers.env JSONB: {"LITELLM_MODEL":"groq-llama", ...}  (migrations/010_provider_env.sql:18)
                         │
rara-core/dispatch.go:159-178  buildRunRequest() copia provider.Env → RunRequest.Env
                         │
rara-runner/main.go:78   baseEnv = loadEnvFile(RUNNER_WORKER_ENV_FILE)  ← secrets + LITELLM_BASE_URL
rara-runner/agent.go:185 merged = mergeEnv(baseEnv, req.Env)           ← model override wins
rara-runner/agent.go:247 dockerRunner.Run() → docker run -e KEY=VAL    ← cada par vira flag -e
                         │
rara-distill/main.go:1225  os.Getenv("LITELLM_MODEL")   ← lê do env do processo
rara-reason/reasoner_litellm.go:37-50  os.Getenv("LITELLM_BASE_URL") + os.Getenv("LITELLM_MODEL")
rara-gate/main.go:434-447  os.Getenv("LITELLM_BASE_URL") + envOr("LITELLM_MODEL","claude-sonnet-4-6")
rara-hone/runners.go:81-92  os.Getenv("LITELLM_BASE_URL") + envOr("LITELLM_MODEL","...")
```

### Divergência vs design doc

**`CONSOLE-INFERENCIA.pt-BR.md` §1** afirma que a escolha de model "vive como string no `providers.env`
(`LITELLM_MODEL`) — editada na mão." Isso é **parcialmente correto**: vale para workers ativados via
rara-runner (path VPC). Mas os Cloud Run Jobs **hardcodam o model no workflow de deploy** — `distill`
usa `groq-llama` e `gate` usa `groq-fast` independente do banco. O design doc deveria mencionar ambos
os caminhos. A fase 8 (dropdown de model no form de Worker) precisará endereçar também o deploy dos
Cloud Run Jobs (atualizar a Cloud Run env var, não só o banco) ou migrar esses workers para o runner.

---

## 2. Gateway litellm

**Como sobe:** `deploy-litellm.yml:50` faz `docker build rara-distill/litellm` — o
`Dockerfile:1-6` parte de `ghcr.io/berriai/litellm:main-latest` (tag **não pinada**) e copia
`config.yaml` para `/app/config.yaml`. O Cloud Run Service recebe `--args="--config,/app/config.yaml,--port,8080"`.

**Fonte de config:** `config.yaml` é a **única** fonte de model_list hoje. Estrutura:

```yaml
model_list:
  - model_name: groq-llama          # alias → LITELLM_MODEL
    litellm_params:
      model: groq/llama-3.3-70b-versatile
      api_key: os.environ/GROQ_API_KEY

  - model_name: groq-fast
  - model_name: gemini-flash        # litellm_params.model: gemini/gemini-2.5-flash-lite
  - model_name: deepseek-chat       # alias aposentado em 24/07/2026; upstream deepseek-v4-flash

general_settings:
  master_key: os.environ/LITELLM_MASTER_KEY
```

**`store_model_in_db` — NÃO está ativo.** Nenhuma linha `store_model_in_db`, `database_url` ou
`LITELLM_SALT_KEY` em `config.yaml` ou `deploy-litellm.yml`. O gateway é stateless, sem DB próprio.

**Para ativar o DB backend (fase 4 — reconciler):**
1. Provisionar um Postgres para o litellm (pode ser branch Neon ou schema separado).
2. Adicionar `store_model_in_db: true` e `database_url: os.environ/LITELLM_DATABASE_URL` em
   `general_settings` no `config.yaml`.
3. Adicionar `LITELLM_SALT_KEY` (para cifragem das keys no DB do litellm) via Secret Manager.
4. A partir daí o reconciler pode usar a Admin API para gerenciar models sem rebuild de imagem.

**Risco ativo:** imagem `main-latest` não pinada — uma versão nova do litellm pode quebrar a Admin
API sem aviso. Recomendado pinar antes da fase 4.

---

## 3. Admin API litellm

**Versão:** `rara-distill/litellm/Dockerfile:1` usa `ghcr.io/berriai/litellm:main-latest` — sem pin.
A Admin API abaixo é válida para litellm ≥ 1.x (API estável desde 1.0).

**Autenticação:** `Authorization: Bearer <LITELLM_MASTER_KEY>` em todos os endpoints admin.
- Secret Manager: `litellm-api-key:latest` → env `LITELLM_MASTER_KEY` no Cloud Run Service.
- Workers autenticam o mesmo jeito: `rara-distill/main.go:969` envia o Bearer se `apiKey != ""`.

**Endpoints confirmados pela documentação oficial (v1.x):**

| Endpoint | Método | Função |
|---|---|---|
| `/v1/models` | GET | Lista models ativos (OpenAI-compat) |
| `/model/info` | GET | Lista models com params completos |
| `/model/new` | POST | Adiciona model ao runtime |
| `/model/update` | POST | Atualiza params de um model |
| `/model/delete` | POST | Remove model do runtime |
| `/health/liveliness` | GET | Healthcheck (sem auth) |
| `/health/readiness` | GET | Readiness (sem auth) |
| `/spend/logs` | GET | Spend/token logs (requer `store_model_in_db`) |
| `/v1/chat/completions` | POST | Inference (OpenAI-compat, usa bearer) |

**Nota:** `/model/new` e `/model/delete` só persistem entre restarts se `store_model_in_db: true`.
Sem DB ativo, as mudanças via Admin API são **in-memory only** — reiniciar o container reverte para
`config.yaml`. Portanto o reconciler (fase 4) **depende** de ativar o DB backend.

---

## 4. Padrão CRUD do rara-core (âncoras para fases 2–4)

O padrão é **Core + HTTP adapter** com seam `Database` interface:

```
main.go:610      type Database interface { ... }   ← seam de I/O
main_test.go:132 type MockDatabase struct { ... }   ← in-memory, zero I/O real
store_reads.go   pgxDatabase (impl real)
surface.go:111   type Core struct { db Database }   ← ops transport-agnostic
surface.go:~1090 type HTTP struct { core *Core }    ← adapter REST
surface.go:1100+ mux.HandleFunc(...)                ← rotas Go 1.22 method+path
```

**Arquivos-âncora:**

| Arquivo | O que contém |
|---|---|
| `rara-core/main.go:610` | `Database` interface — adicionar métodos aqui para `llm_providers`/`llm_models` |
| `rara-core/main_test.go:132` | `MockDatabase` — implementar os novos métodos com dados em memória |
| `rara-core/surface.go:111` | `Core` struct — adicionar ops (`ListLLMProviders`, `CreateLLMProvider`, …) |
| `rara-core/surface.go:1100` | `mux.HandleFunc` — registrar rotas `/v1/llm-providers`, `/v1/llm-models` |
| `rara-core/store_reads.go` | queries de leitura do pgxDatabase |
| `rara-core/store_writes.go` | queries de escrita (upsert/delete) |
| `rara-core/surface_test.go` | testes HTTP com httptest + MockDatabase — modelo de teste |

**Padrão de rota CRUD** (`/v1/sources` como referência):
- `GET /v1/sources` → `h.listSources` → `c.db.ListSources(ctx)`
- `GET /v1/sources/{id}` → `h.getSource` → `c.db.GetSource(ctx, id)`
- `POST /v1/sources/{kind}` → `h.createSource` → `c.db.UpsertSource(ctx, …)` com `ON CONFLICT`
- `PATCH /v1/sources/{id}` → `h.patchSource` → update parcial
- `POST /v1/sources/bulk` → `h.bulkSources` → batch upsert

**Auth:** middleware bearer-token único, falha fechado. `surface.go:18-20`.

---

## 5. `owner_id` / single-tenant

**Confirmado: sistema é single-tenant hoje.** Busca grep em todo o repo retornou **zero ocorrências**
de `owner_id` ou `tenant` em qualquer migration ou código Go.

Tabelas do `rara-core` não têm hierarquia de usuário:
- `items`, `providers`, `flows`, `flow_steps`, `interest_profile` — nenhuma coluna de ownership
- `providers.name` é único globalmente (UNIQUE(name, capability)) — sem partição por tenant

A coluna `owner_id NULLABLE` proposta em `CONSOLE-INFERENCIA.pt-BR.md §3` é **net-new** e não quebra
o single-tenant atual (NULL = global). Quando inserida em `llm_providers` e `llm_models`, as queries
de leitura devem filtrar `WHERE owner_id IS NULL OR owner_id = $1` para suportar os dois modos sem
migração destrutiva.

---

## Divergências vs `CONSOLE-INFERENCIA.pt-BR.md`

| # | Seção | Divergência |
|---|---|---|
| 1 | §1 | "model vive em `providers.env`" — verdade só para workers via rara-runner. Cloud Run Jobs têm `LITELLM_MODEL` hardcoded no workflow de deploy. A fase 8 precisa endereçar ambos. |
| 2 | §2 | Design pressupõe que o reconciler pode gerenciar models via Admin API hoje — **falso**: sem `store_model_in_db` ativo, as mudanças são in-memory only. A fase 4 deve ativar o DB backend **antes** de implementar o reconciler. |
| 3 | §2 | `LITELLM_SALT_KEY` citado no design doc não está presente em `deploy-litellm.yml`. É necessário para cifragem das keys no DB do litellm (não confundir com `RARA_SECRETS_KEY` do nosso AES-256-GCM). |
| 4 | §3 | `Dockerfile` usa `main-latest` (não pinado). Risco de breaking change na Admin API sem aviso. Recomendado pinar a imagem antes da fase 4. |
