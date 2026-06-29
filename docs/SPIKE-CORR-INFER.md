# Spike — CORR-INFER #0: provider+model direto (wildcard) + plano de migração

> Resultado do spike `feat/corr-infer-0-spike`. **Sem mudança de código de produção.**
> Companion: [CONSOLE-INFERENCIA-CORRECAO.pt-BR.md](../CONSOLE-INFERENCIA-CORRECAO.pt-BR.md)
> Gateway testado: **litellm-database `v1.90.0`**, DB-mode (`store_model_in_db: true`),
> Cloud Run Service `litellm` (`https://litellm-…-uc.a.run.app`), DB Neon schema `litellm`.

---

## TL;DR

O wildcard **roteia** (`groq/*` → `groq/llama-3.3-70b-versatile` resolve), **mas** wildcard
registrado pela Admin API (`POST /model/new`) é **só em memória**: não persiste no
`LiteLLM_ProxyModelTable`, não é idempotente (cada `POST` cria uma duplicata), não aparece em
`/model/info`, e **não dá pra deletar** (`/model/delete` só apaga linha de DB). Isso o torna
**inseguro como primitivo de um reconciler level-triggered** (não dá pra fazer diff nem remover ao
desabilitar um provider; some no restart do gateway).

**Correção do plano:** o reconciler deve registrar **models concretos com `model_name = string
completa do upstream`** (ex.: `model_name = "groq/llama-3.3-70b-versatile"`), **não** wildcards.
Isso entrega a mesma UX "provider+model direto" (worker grava `LITELLM_MODEL=groq/llama-3.3-70b-versatile`)
e **reaproveita o reconciler/client atuais inteiros** (persiste no DB, aparece em `/model/info`,
deletável por id, fingerprint de drift funciona). O wildcard fica fora do registry; serve no máximo
para *discovery* transiente. Ver §Q1 e §Migração.

---

## Q1 — Wildcard via Admin API roteia? (sim, com ressalvas graves)

**Roteamento: SIM.** Registrado `groq/*` (`POST /model/new`, `litellm_params.model = "groq/*"`),
um `POST /v1/chat/completions` com `model=groq/llama-3.3-70b-versatile` roteou (`HTTP 200`), e um
segundo model **diferente** (`groq/llama-3.1-8b-instant`) roteou pelo **mesmo** wildcard — confirma
"1 wildcard cobre todos os models do provider".

**Ressalva 1 — chave não resolve `os.environ/`.** Com `api_key: "os.environ/GROQ_API_KEY"` o gateway
**armazenou a string literal** (cifrada) e devolveu `401 Invalid API Key` da Groq. A resolução de
`os.environ/` é feita no load do `config.yaml`, **não** no `POST /model/new`. Tem que mandar a **key
real decifrada** (que é exatamente o que o reconciler já faz para os aliases). Com a key real → `200`.

**Ressalva 2 (a crítica) — wildcard NÃO persiste.** Após o add (que retorna `model_id` e roteia),
consulta direta ao DB:

```sql
SELECT model_name FROM litellm."LiteLLM_ProxyModelTable";
-- → só os 4 aliases. groq/* AUSENTE.
```

`model_info.db_model = False` no add. O wildcard entra **só no router em memória**. Consequências:
some no próximo restart/redeploy do Cloud Run (config.yaml tem `model_list: []`), e o reconciler na
VPC re-empurraria a cada pass.

**Ressalva 3 — não idempotente.** Dois `POST /model/new` com o mesmo `groq/*` criaram **dois**
ids distintos (duplicata no router). Um reconciler que re-empurra a cada pass **acumula duplicatas**
dentro do mesmo processo.

**Ressalva 4 — invisível e não deletável.** `groq/*` **não** aparece em `/model/info` nem em
`/v1/models`. `/model/delete` por id retorna `not found in db` (só deleta linha de
`LiteLLM_ProxyModelTable`). **Não há como remover um wildcard** sem reiniciar o gateway. Ao
desabilitar/remover um provider, o wildcard dele continuaria roteando (com key possivelmente
inválida/revogada) até o próximo restart — falha de correção/segurança.

> **Validação do caminho recomendado (model concreto):** registrado `model_name =
> "groq/llama-3.3-70b-versatile"` (== upstream) → **persiste** no `LiteLLM_ProxyModelTable`,
> **aparece** em `/model/info`, **roteia** (`200`), e **deleta** por id (`deleted successfully`).
> Tudo que o wildcard não faz. É o mesmo padrão dos 4 aliases de hoje, só que `model_name` deixa de
> ser apelido e passa a ser a string completa do upstream.

## Q2 — Model discovery (`check_provider_endpoint`)? (sim, opcional)

`GET /v1/models?check_provider_endpoint=true` consultou o endpoint real da Groq e devolveu **19**
entradas — os 4 aliases + `groq/*` + os models concretos da Groq (`groq/whisper-large-v3-turbo`,
`groq/qwen/qwen3-32b`, `groq/openai/gpt-oss-120b`, …). Funciona, **mas exige a key do provider já
configurada no gateway** (via algum model registrado daquele provider). Sem `check_provider_endpoint`,
`/v1/models` devolve só os models registrados (e `return_wildcard_routes=true` **não** expandiu o
wildcard nesta versão).

**Recomendação:** o dropdown "Model" da UI **não precisa** de `check_provider_endpoint`. O **catálogo
estático** já existe (`model_prices_and_context_window.json` pinado em `v1.90.0`, PR #286), lista
todos os models por provider e custa zero chamada. Use o catálogo para o dropdown; deixe
`check_provider_endpoint` como *nice-to-have* de precisão "ao vivo" (e que depende da chicken/egg da
key estar no gateway).

## Q3 — Coexistência alias antigo + novo? (sim)

Com `groq-llama` (alias DB antigo) **e** `groq/llama-3.3-70b-versatile` (novo) ativos ao mesmo
tempo, **ambos rotearam** (`200` nos dois). É a base da migração segura: dá pra introduzir o formato
novo sem remover o antigo, e só remover o antigo quando nenhum worker mandar mais o alias.

## Q4 — Catálogo → lista de providers (119 / 80; `openai_compatible` à parte)

Do `model_prices_and_context_window.json` (v1.90.0, 2874 entradas):

- **119** `litellm_provider` distintos (todos os modes); **80** com ≥1 model `mode:chat`.
- Os 4 nossos presentes: `groq`, `gemini`, `deepseek`, `openai`.
- `openai_compatible` **NÃO** está no catálogo — confirma que é caso à parte (BYO + `base_url`),
  tratado fora da lista derivada do catálogo.
- ⚠️ Alguns `litellm_provider` são **sub-categorias** (`vertex_ai-language-models`,
  `vertex_ai-anthropic_models`, `bedrock_converse`, …), não providers "de topo". A lista do dropdown
  "Novo provider" precisa de **curadoria leve** (mapear/agrupar esses sufixos), não é o `distinct` cru.

Top por contagem: `fireworks_ai (295)`, `bedrock (266)`, `openai (213)`, `azure (199)`,
`bedrock_converse (131)`, `vercel_ai_gateway (101)`, `openrouter (95)`, `gemini (68)`, `mistral (54)`…

## Q5 — Spend per-provider (sim, pelo prefixo)

Após os requests do spike, `litellm."LiteLLM_SpendLogs"`:

| `model_group` | `model` (upstream) | via |
|---|---|---|
| `groq/llama-3.3-70b-versatile` | `groq/llama-3.3-70b-versatile` | string completa (wildcard/concreto) |
| `groq/llama-3.1-8b-instant` | `groq/llama-3.1-8b-instant` | string completa |
| `groq-llama` | `groq/llama-3.3-70b-versatile` | alias antigo |

Quando o cliente manda a **string completa**, `model_group` guarda a string completa →
**agregação por provider = `split_part(model_group,'/',1)`** (prefixo antes do `/`). Vale tanto para
o caminho wildcard quanto para o concreto (em ambos `model_group` = o que o cliente mandou). Confirma
§9 da correção. O backend de spend do #9 continua agregando por `model_group`.

## Q6 — Mapa dos 4 bindings atuais (confirmado em 3 fontes)

`/model/info` do gateway == `public.llm_models` (Neon) == hipótese do prompt:

| alias (hoje) | upstream / novo `LITELLM_MODEL` | kind |
|---|---|---|
| `groq-llama` | `groq/llama-3.3-70b-versatile` | groq |
| `groq-fast`  | `groq/llama-3.1-8b-instant`    | groq |
| `gemini-flash` | `gemini/gemini-2.5-flash-lite` | gemini |
| `deepseek-chat` | `deepseek/deepseek-v4-flash`  | deepseek |

**Bindings em uso** (`public.providers.env->>'LITELLM_MODEL'`):

| app | capability | runtime | hoje | depois |
|---|---|---|---|---|
| distill | destilar | local/vpc/cloudrun | `groq-llama` | `groq/llama-3.3-70b-versatile` |
| gate | gate_barato | vpc/cloudrun | `groq-fast` | `groq/llama-3.1-8b-instant` |
| gate | gate_rico | vpc/cloudrun | `groq-fast` | `groq/llama-3.1-8b-instant` |

`gemini-flash` e `deepseek-chat` existem no registry mas **não** têm binding em `providers.env`.
⚠️ **Segundo vetor:** Cloud Run Jobs (`distill`, `gate`, `hone`) também hardcodam `LITELLM_MODEL` no
`--set-env-vars` do `deploy-*.yml` (achado do SPIKE #0). A migração (CORR-#5) tem que cobrir **os dois
vetores**: `providers.env` (banco) **e** os `deploy-*.yml`.

---

## Migração validada (corrige §5 da CORR — sem wildcard no registry)

O primitivo é **model concreto**, não wildcard. `desired set` = strings de upstream usadas pelos
bindings ativos (workers + agents); a key vem de `llm_providers` (habilitado + decifrada server-side).
O reconciler atual (`ListModels`/`AddModel`/`DeleteModel` + fingerprint) **não muda de forma** — só
muda a fonte do `desired set` e o `model_name` passa a ser a string do upstream.

1. **CORR-#1** — reconciler passa a registrar, para cada upstream ligado, um **model concreto**
   `model_name == litellm_params.model == "{kind}/{model}"` com a key real do provider. Coexiste com
   os aliases antigos (Q3). *Não usa `POST /model/new` com wildcard.*
2. **CORR-#3** — worker form grava `LITELLM_MODEL = "{kind}/{model}"` (dropdown do catálogo, Q4).
3. **CORR-#5** — migra `providers.env` **e** os `deploy-*.yml` (alias→`kind/model`); só depois remove
   os 4 aliases antigos do registry e dropa `llm_models`. Como nenhum worker manda mais o alias, a
   remoção é segura.

**Discovery (dropdown):** usar o **catálogo estático** (#286) filtrado por provider (Q2/Q4) — sem
depender de `check_provider_endpoint`.

> Se mesmo assim quiser **wildcard de verdade**, o único jeito seguro nesta versão é colocá-lo no
> `config.yaml` (`model_list`) com `api_key: os.environ/...` — o que **reintroduz deploy por provider
> e keys em env** (perde o no-deploy e o ownership das keys no banco). O caminho de **model concreto**
> é estritamente melhor para os objetivos travados.

---

## Limpeza / pendência para o Renato

- Registry persistente (`LiteLLM_ProxyModelTable`) **voltou aos 4 aliases originais** — todo model
  concreto de teste foi deletado.
- ⚠️ Sobraram **2–3 `groq/*` fantasmas só em memória** (não deletáveis por API, ausentes do
  `/model/info`, não persistidos no DB). São **inócuos**: roteiam idêntico ao `groq-llama` com a key
  real, e nada em prod manda `model=groq/*`. **Somem no próximo redeploy/restart** do Service
  `litellm`. Não forcei restart (fora do escopo do spike).
- `LiteLLM_SpendLogs` tem ~6 linhas dos requests de teste do spike (custo de tokens desprezível).
