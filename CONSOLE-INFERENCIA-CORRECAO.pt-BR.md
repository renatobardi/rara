# Correção conceitual — Provider + Model direto (sem alias) + gráficos de gasto

> Companheiro de [CONSOLE-INFERENCIA.pt-BR.md](./CONSOLE-INFERENCIA.pt-BR.md). Corrige o erro de modelagem
> da série CONSOLE-INFER e roda ANTES do #10 (Agents). Código/comentários em inglês; doc em pt-BR.

## 1. O erro vs o certo

**Erro:** `llm_models` é uma tabela de **aliases que o operador cadastra** (`groq-llama` → `groq/llama-3.3-70b`).
Introduziu uma indireção desnecessária.

**Certo (modelo do Bardi, nativo do litellm):**
- **Provider** = vendor (qualquer um do litellm) + API key. Adicionar um provider **habilita TODOS os models dele**.
- **Model** = não é cadastrado; vem do **catálogo do litellm** filtrado pelos providers adicionados.
- **Binding** (worker/agent) = escolhe **Provider + Model** direto → grava `LITELLM_MODEL = groq/llama-3.3-70b-versatile`
  (string completa, sem alias). O gateway roteia via wildcard `groq/*` + a key do provider.

## 2. Confirmado no gateway real (SPIKE CORR-#0 — ver `docs/SPIKE-CORR-INFER.md`)

**Provider+model direto roteia** (`model=groq/llama-3.3-70b-versatile` resolve sozinho). **MAS** o
wildcard `model_name: "groq/*"` via Admin API (`POST /model/new`) é **só em memória**: não persiste no
DB, não é idempotente, é invisível em `/model/info` e **não dá pra deletar** → **inseguro como primitivo
do reconciler** (ver §5 corrigida). O caminho seguro é **model concreto** com `model_name = string
completa do upstream` (persiste, lista, deleta — validado no spike). **Model discovery**
(`/v1/models?check_provider_endpoint=true`) lista os models reais por provider, mas é **opcional**: o
dropdown usa o **catálogo estático** (#286). Custo: o litellm já calcula pelo cost-map (alimenta o
`LiteLLM_SpendLogs`), então **não precisamos guardar custo por model**.

## 3. Decisões travadas (Bardi)
1. **All-or-nothing por provider** — adicionar um provider libera todos os models dele. Sem curadoria fina por model.
2. **Aba Models removida** do `/inferencia` (não vira browse; some).
3. **Gráficos de gasto** na `/inferencia`: **geral** (no tempo) + **por provider**.

## 4. Data model

- `llm_providers`: **MANTÉM**. `kind` deixa de ser enum fixo de 6 → aceita **qualquer provider do litellm**
  (validar contra o catálogo; `openai_compatible` continua como caso BYO com `base_url`). Key cifrada igual.
- `llm_models`: **REMOVIDA** (migração dropa a tabela — ver §6 CORR-#5).
- **Binding**: `providers.env.LITELLM_MODEL` passa a guardar `kind/model` (ex.: `groq/llama-3.3-70b-versatile`)
  em vez do alias.

## 5. Reconciler (rework do #4b) — **corrigido pelo SPIKE CORR-#0**
~~Empurra 1 wildcard `{kind}/*` por provider.~~ Wildcard via Admin API não persiste e não é deletável
(`docs/SPIKE-CORR-INFER.md`), então o reconciler registra **models concretos** com
`model_name == litellm_params.model == "{kind}/{model}"` e a key decifrada (secretbox). É o **mesmo**
reconciler de hoje (`ListModels`/`AddModel`/`DeleteModel` + fingerprint) — só muda a fonte do
`desired set` (upstreams ligados pelos bindings) e o `model_name` (= string do upstream, não apelido).
Full-sync: upstream ligado → model concreto no gateway; desligado/removido → deleta por id. Persiste no
DB, sobrevive a restart, deletável. (Discovery do dropdown = catálogo estático #286, não
`check_provider_endpoint`.)

## 6. Tela `/inferencia` (depois da correção)
- **Providers** (gestão principal): "Novo provider" escolhe da **lista completa do litellm** (catálogo) + key;
  enable/disable; health. (kebab ⋮ → Editar, como hoje.)
- **Gastos** (nova seção, no lugar da aba Models): **gráfico de gasto geral no tempo** + **gasto por provider**.
  Reusa o backend de spend do #9 (`LiteLLM_SpendLogs`), agregando por dia e por provider (prefixo do model
  antes do `/`).

## 7. Reaproveitado vs reworkado
- ♻️ Reaproveita: `llm_providers`+cifragem (#1/#2), gateway DB-mode (#4a), **CATALOG (#286)** (vira a fonte do
  provider-list e do model-dropdown por provider), backend de spend (#9).
- 🔧 Reworka: `llm_models` (#3) sai; reconciler (#4b) vira wildcard; dropdown do worker (#8) vira provider+model;
  display de spend (#9) vira gráficos.

## 8. Fatias (CORR-#n) — ANTES do #10

| # | Arquivo | Branch | Entrega |
|---|---|---|---|
| 0 | CORR-INFER-0.md | `feat/corr-infer-0-spike` | Confirma wildcard + discovery no gateway v1.90.0; plano de migração dos 4 bindings. Sem código. |
| 1 | CORR-INFER-1.md | `feat/corr-infer-1-reconciler-concrete` | Reconciler registra models **concretos** `kind/model` por upstream ligado (coexiste com os aliases antigos). _Era "wildcard" — corrigido pelo SPIKE CORR-#0._ |
| 2 | CORR-INFER-2.md | `feat/corr-infer-2-providers-ui` | Providers: kind = lista litellm; remove a aba Models. |
| 3 | CORR-INFER-3.md | `feat/corr-infer-3-worker-picker` | Worker form: Provider + Model picker → grava `kind/model` no env. |
| 4 | CORR-INFER-4.md | `feat/corr-infer-4-spend-charts` | Gráficos de gasto (geral + por provider) na `/inferencia`. |
| 5 | CORR-INFER-5.md | `feat/corr-infer-5-migrate-drop-models` | Migra env dos workers (alias→`kind/model`), dropa `llm_models`, ajusta spend. |

**Sequência de migração segura (não quebrar prod) — validada no SPIKE CORR-#0:**
1. CORR-#1: gateway passa a ter os models **concretos** `groq/llama-3.3-70b-versatile` **junto** dos aliases
   antigos (`groq-llama`) → ambos roteiam (coexistência confirmada).
2. CORR-#3: workers novos passam a gravar `kind/model`.
3. CORR-#5: migra o `env` dos workers existentes (alias→`kind/model`) **em `providers.env` E nos
   `deploy-*.yml`** (dois vetores) **antes** de dropar `llm_models` e remover os aliases do gateway. Só
   depois que nenhum worker manda mais alias é que a tabela/aliases somem.

## 9. Impacto no #9 (spend)
Continua: agrega `LiteLLM_SpendLogs` por `model_group` (que vira `groq/llama-...`). Per-provider = group by o
prefixo antes do `/`. O endpoint `/v1/llm-spend` ganha variantes p/ time-series e por-provider (CORR-#4).
