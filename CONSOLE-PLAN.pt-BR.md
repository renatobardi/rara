# rara Console — Plano de Frontend & Operação Visual

Plano completo para a interface visual do `rara`: um **console de operador** publicado na VPC
Oracle, backend em Go, dados no Neon, com o **design system herdado do Kura**. Pensado para
encaixar em todo o ecossistema (rara → KURA, sob a marca *oute*).

> Companheiro de [ARCHITECTURE-2.0.pt-BR.md](./ARCHITECTURE-2.0.pt-BR.md). Referência visual:
> `renatobardi/kura-macos` (app SwiftUI nativo — referência visual).

---

## 0. Decisão de visual — Opção B (estilo ChatGPT) ✅

Escolhido o visual **neutro inspirado no ChatGPT**: monocromático, muito branco, cantos bem
arredondados, botão primário preto, borda hairline no lugar de sombra, verde só como sinal de
status. **Clean (claro) é o padrão; Dark opcional.** Trade-off consciente: abre mão da identidade
compartilhada com o Kura (índigo + hanko) em troca de uma ferramenta de operador **neutra e
familiar, que some e deixa o conteúdo falar**. A seção 2 abaixo mantém os tokens do Kura apenas
como referência/herança, caso um dia se queira reaproximar as duas marcas.

**Tokens canônicos (Opção B):**

| Token | Clean (padrão) | Dark |
|---|---|---|
| `bg` | `#FFFFFF` | `#212121` |
| `sidebar` | `#F9F9F9` | `#171717` |
| `surface` | `#FFFFFF` | `#2A2A2A` |
| `surface-2` | `#F7F7F8` | `#262626` |
| `border` | `#E5E5E5` | `#3A3A3A` |
| `hover` | `#ECECEC` | `#2E2E2E` |
| `text` | `#0D0D0D` | `#ECECEC` |
| `muted` | `#6E6E80` | `#9B9B9B` |
| `primary` / `primary-fg` | `#0D0D0D` / `#FFFFFF` | `#FFFFFF` / `#0D0D0D` |

Sinais (pontuais, ponto colorido + texto neutro): `green #19C37D` (done/online) · `blue #2563EB`
(running) · `violet #7C5CFF` (to_text) · `amber #E0A23B` (quarentena) · `red #EF4444` (drop).
Tipografia: `ui-sans-serif, system-ui` (sem Hiragino). Raio: **14px** em cards, **999px** (pill)
em botões e badges. Sidebar **268px**. Ícones thin/line. Status nunca em pílula colorida cheia —
sempre ponto + texto neutro (restrição monocromática). Mockup de referência:
`console-mockup-b-chatgpt.html`.

---

## 1. O que é (e o que não é)

São duas telas diferentes para dois usos diferentes — não confundir:

- **Console do rara (este plano)** — o painel de *operador/curador*: ver o pipeline, revisar a
  quarentena, dar thumbs, editar o perfil de interesse e as regras, ver saúde dos providers e
  custo. É onde você **opera e treina** o rara.
- **Leitor do KURA (fora deste plano)** — onde você *lê* o conhecimento destilado (o segundo
  cérebro). É outro projeto, com seu próprio cliente (o `kura-macos` e/ou um web). O rara
  **produz**; o KURA é onde se **consome**.

Este plano cobre só o console do rara. Mas o design system é compartilhado com o KURA — então a
primeira decisão de arquitetura é extrair os tokens num pacote comum (seção 4).

---

## 2. Design system herdado do Kura

Extraído do `kura-macos/CLAUDE.md`. A identidade é **dark, minimalista, japonesa**: índigo (藍)
como cor principal, um único vermelho quente (hanko 蔵 — o "selo") como acento de calor/perigo,
tipografia Hiragino Sans, cantos suaves de 10px, sidebar de 260px.

### Tokens de cor

| Token | Hex | Papel no console |
|---|---|---|
| `accent` | `#3730A3` | índigo — ações primárias, links, seleção |
| `accent-hover` | `#4338CA` | hover do accent |
| `background` | `#212121` | fundo da aplicação |
| `sidebar` | `#171717` | fundo da navegação lateral |
| `surface` | `#2A2A2A` | cards, tabelas, painéis |
| `hanko` | `#9B1C1C` | o único vermelho — `drop`/perigo, selo da marca, badge de alerta |
| `text` | `#ECECEC` | texto primário |
| `text-muted` | `#6B7280` | texto secundário, labels |
| `divider` | `#2D2D2D` | separadores, bordas |

> Estados que o Kura não define mas o console precisa (derive na mesma família, dessaturados para
> não competir com o índigo/hanko): `success` ~ `#1D9E75` (done), `warning` ~ `#C98A1A`
> (quarentena/defer), `info` ~ `#3B82F6` (running). Use com parcimônia — a regra Kura é "um único
> momento quente".

### Tipografia

Hiragino Sans como primária (no Mac do operador renderiza nativo; fallback `system-ui`). Escala:
`title` 18 / `headline` 15 / `body` 14 / `caption` 12 / `micro` 11. Pesos W3 (regular), W6
(medium), W8 (bold).

### Espaçamento & layout

Spacing: `xs:4 · sm:8 · md:12 · lg:16 · xl:24 · xxl:32`. Raio de canto: **10px**. Sidebar:
**260px**. Ícones: traço fino (no web, ícones *thin/line* — ex. Lucide com stroke 1.5).

### Regras de estilo (do Kura)

- **Glass só na camada de navegação** (topbar, sheets, botões flutuantes) — nunca em listas,
  conteúdo ou texto. No web, aproxime com `backdrop-filter: blur()` + translucidez sutil só no
  topbar/sheets; o resto é flat.
- **Respeitar `prefers-reduced-motion`** e `prefers-reduced-transparency`.
- Layout **sidebar + conteúdo** (equivalente web do `NavigationSplitView`).
- Calma e silêncio visual: muito espaço negativo, hierarquia por peso/cor, não por bordas.

---

## 3. Stack de frontend — recomendação

**Recomendado: SvelteKit (adapter-static) + Go servindo via `embed.FS`.**

O build estático do SvelteKit é embutido no binário Go com `embed.FS` — então em produção
continua sendo **um binário só na VPC** (sem Node em runtime, sem deploy separado), mas você ganha
uma SPA polida, componível, que faz jus a um design system. Svelte é leve, sem vendor lock-in, e
as telas do console (boards vivos, editores de config, sliders custo⇄qualidade, gráficos de saúde,
⌘K) ficam muito melhores num framework de componentes do que em HTML puro.

```
Build (CI):  SvelteKit  ──(adapter-static)──▶  dist/  ──embed.FS──▶  binário Go
Runtime:     um binário Go serve a SPA + faz proxy/BFF para a API do rara-core
```

**Alternativa lean: Go + `templ` + `htmx` + Tailwind** (zero Node, server-rendered). Mais simples
ainda e 100% Go, mas as interações ricas (kanban vivo, command palette, editores) dão mais
trabalho. É a escolha se você quiser Go-puro acima de tudo. É a **única decisão reversível** deste
plano — dá pra trocar depois sem mexer no backend, porque o contrato é a API.

**Por que não SPA com deploy separado (Vercel etc.):** quebraria o "um binário, uma VPC,
anti-lock-in" que rege todo o projeto. `embed.FS` te dá a SPA sem abrir mão disso.

Bibliotecas (Svelte): Tailwind (preset = tokens Kura), Lucide (ícones thin), um lib de charts leve
(uPlot ou Chart.js) para custo/qualidade/saúde, e nada além disso.

---

## 4. Arquitetura

O console é um **serviço Go próprio** (`rara-console`), separado do `rara-core`, que **consome a
API HTTP do rara-core** (a mesma superfície da Fase 5) e serve a SPA embutida. Ele **não fala com
o Neon direto** — toda leitura/escrita passa pela API do rara-core (fonte única da verdade, menor
superfície de ataque, sem credencial de banco no console).

```
  Seus dispositivos
        │  HTTPS (Caddy auto-TLS)  ou  Tailscale (rede privada)
        ▼
  ┌────────────────────────┐     HTTP (JSON, BFF)     ┌──────────────────┐
  │  rara-console (Go)      │ ───────────────────────▶ │  rara-core API   │
  │  • serve SPA (embed.FS) │                          │  (Fase 5)        │
  │  • BFF: auth + agrega   │ ◀─────────────────────── │                  │
  └────────────────────────┘                          └────────┬─────────┘
                                                               ▼
                                                             Neon
```

Por que serviço separado (e não dentro do rara-core): mantém o **control plane enxuto e estável**
(o reconciler é crítico; a UI itera rápido). O console pode subir/reiniciar sem tocar no motor. O
acoplamento é a API — o mesmo princípio "o contrato é a tabela/superfície, nunca a chamada direta"
que rege o projeto.

**Pacote de design compartilhado:** extraia os tokens (cor/tipo/spacing/raio) num pacote
`oute-design-tokens` (um preset Tailwind + CSS vars) versionado, consumido pelo `rara-console` e,
no futuro, por qualquer superfície web do KURA. Assim a identidade *oute* fica única e DRY.

---

## 5. Telas (Information Architecture)

Layout: sidebar 260px (`#171717`) + conteúdo (`#212121`), topbar translúcida. Navegação:

1. **Visão geral** — KPIs do dia (items por lane, em voo, na quarentena, distillations
   produzidas, estimativa de gasto LLM), saúde do pipeline, atividade recente. Selo hanko como
   marca no topo da sidebar.
2. **Pipeline (board de items)** — items por status (`discovered → … → done/filtered/quarantine`),
   filtro por lane. Clique → **detalhe do item**: timeline dos `item_steps`, as `gate_decisions`
   (qual camada decidiu e por quê), `output_ref` (link pra transcript/distillation).
3. **Quarentena** — a fila de revisão: items `defer` com metadata, botões **keep/drop** (=
   `ReviewQuarantine`, thumbs up resgata / down confirma o drop). É o coração do combate ao
   cold-start.
4. **Distillations** — feed do que foi produzido (título, fonte, pattern) com 👍/👎 (feedback
   explícito). A leitura profunda é do KURA; aqui é o feed + o sinal.
5. **Curadoria** — editor do `interest_profile` (temas/autores/anti-temas/pesos/keep_threshold) +
   CRUD das `gate_rules` (allow/deny) + amostra das decisões recentes (o que passou/caiu e por
   qual camada). É onde você **treina** o gosto.
6. **Fontes & Flows** — lista de flows, os `flow_steps` como pipeline visual com toggles por passo,
   ligar/desligar lanes, e o **formulário manual-inbox do LinkedIn** (cola URL+texto).
7. **Providers & Roteamento** — registry de providers (capability, runtime, activation,
   custo/qualidade, constraints, heartbeat/saúde) + editor de `routing_policies` (slider
   custo⇄qualidade, ordem de fallback) + board de saúde.
8. **Atividade / Auditoria** — log de `gate_decisions`, eventos do reconciler, runs de worker.
9. **Configurações** — auth/tokens, config do LiteLLM, **tema (Clean é o padrão; Dark opcional; respeitar o sistema)**, idioma.

> **Idioma:** PT-only no MVP (ferramenta pessoal, um único usuário), mas com as strings
> **externalizadas** (sem texto hardcoded; mensagens num arquivo, ex. paraglide/inlang no
> SvelteKit) — adicionar EN depois é um flip, não retrabalho.
>
> **Tema:** dois temas na mesma família Kura — **Clean (claro, padrão)** e Dark. Ambos usam o
> mesmo índigo `#3730A3` e o vermelho hanko `#9B1C1C`; o Clean troca o fundo por off-white quente
> (`#F6F6F3`), surfaces brancas com sombra hairline; o Dark é o token-set original do Kura.

Extras de polish (estilo Kura): **⌘K command palette**, transições suaves (com guard de
reduced-motion), o selo **hanko** como momento de marca, microinterações nos thumbs.

---

## 6. Inventário de componentes (design system na web)

Sidebar nav · topbar (glass) · KPI card · **status pill** (color-coded: discovered/running info,
done success, filtered muted, quarantine warning, drop hanko) · tabela de dados densa · timeline
de steps · botões thumbs · slider custo⇄qualidade · toggle · editor de JSON/lista (perfil, regras)
· modal/sheet (glass) · toast · badge · empty-state · command palette. Tudo a partir dos tokens —
nunca cor hardcoded.

---

## 7. Backend do console (Go)

`rara-console` é um Go service fino:

- **Serve a SPA** (`embed.FS` com o build SvelteKit) + assets.
- **BFF / proxy** para a API do rara-core: repassa chamadas autenticadas e faz *agregações* que a
  UI precisa (ex.: montar os KPIs da visão geral a partir de várias chamadas) para a SPA fazer
  uma request só.
- **Auth** (seção 8) — sessão/ível na borda; o console guarda o token de serviço do rara-core
  server-side (a SPA nunca o vê).
- Sem acesso a Neon. Stateless (sessão em cookie assinado ou num KV simples).

Convenções do projeto: Go, `net/http` ou `chi`, single binary, Dockerfile, CI por serviço, testes
de handler com a API do rara-core mockada.

---

## 8. Auth & exposição

Por ser ferramenta **pessoal**, a recomendação prioriza superfície de ataque mínima:

**Recomendado: Tailscale (rede privada).** O `rara-console` escuta só na interface do tailnet; só
os seus dispositivos alcançam. Zero exposição pública, zero gestão de TLS público, zero tela de
login. É o caminho mais seguro e simples para um console de operador pessoal.

**Alternativa (se quiser URL pública, ex. `console.oute.me`):** Caddy na frente (auto-TLS
Let's Encrypt) + login single-user. Para o login, duas opções coerentes com o ecossistema:
**Sign in with Apple** (igual ao Kura — mesma identidade, mas mais peça pra montar no web) ou um
OAuth Google simples. Sessão em cookie assinado, sem senha local.

> Em qualquer caminho: o token do rara-core fica **server-side** no console; a SPA só tem a sessão.

---

## 9. Deploy na VPC Oracle

A VPC Oracle (provavelmente Ampere ARM always-free) já hospeda o reconciler always-on do
rara-core. O console é mais um serviço residente na mesma VM.

- **Build:** CI compila o SvelteKit (`adapter-static`) → embute no binário Go (`go:embed`) →
  imagem/binário ARM64. Um artefato.
- **Runtime:** `systemd` service `rara-console` (igual ao reconciler). Restart on failure.
- **Borda:** Tailscale (recomendado) ou Caddy (TLS + domínio). Caddy também serve de reverse proxy
  único caso outras superfícies subam depois.
- **Observabilidade:** logs no journald; um `/healthz`; opcional: as métricas já visíveis no
  próprio console (saúde de provider, fila), então não precisa de stack de monitoração separada no
  MVP.
- **Rede pra o Neon:** quem fala com o Neon é o rara-core (já resolvido); o console não precisa de
  egress pro banco.

---

## 10. Fases de build do console

Cada fatia entregável; depende da **Fase 5 do rara-core** (a API). C0 pode começar já (shell +
deploy), e as telas preenchem conforme os endpoints da Fase 5 entram.

| Fatia | Entrega | Depende de |
|---|---|---|
| **C0 — Fundação** | scaffold `rara-console` (Go + embed + SvelteKit), tokens Kura no Tailwind, shell sidebar+topbar, auth (Tailscale), deploy systemd+borda na VPC, `/healthz` lendo o rara-core | nada (pode começar já) |
| **C1 — Ver** | Visão geral (KPIs) + Pipeline (board + detalhe do item), read-only | endpoints de leitura de estado (Fase 5) |
| **C2 — Agir** | Quarentena (keep/drop) + Distillations (thumbs) — o human-in-the-loop | endpoints feedback/quarentena (Fase 5; funções já existem da Fase 3) |
| **C3 — Treinar** | Curadoria (perfil + regras) + Fontes/Flows (toggles) + LinkedIn manual-inbox | endpoints de config (Fase 5) |
| **C4 — Afinar** | Providers & Roteamento (saúde + policy) + Auditoria + ⌘K + polish (toasts, transições, selo hanko) | — |

---

## 11. Como encaixa no projeto inteiro

- O console é a **face humana da Fase 5** (a superfície MCP-sobre-HTTP). A Fase 5 expõe a API; o
  console a consome. Então o caminho crítico é: **terminar a Fase 5 do rara-core** → preencher as
  telas do console. O C0 (shell + deploy) pode rodar em paralelo agora.
- A **Fase 6** do rara (loop de aprendizado real) ganha uma casa visual no console: a revisão do
  perfil deixa de ser stub quando há uma tela pra editar/ver o efeito do feedback.
- **rara × KURA × oute:** os tokens viram o pacote `oute-design-tokens`, compartilhado entre o
  `rara-console` e qualquer superfície web do KURA. Uma identidade só. O rara-console e o
  kura-macos são duas janelas (operar vs ler) do mesmo universo visual.

---

## 12. Decisões em aberto

- **Frontend:** SvelteKit+embed (recomendado) vs templ+htmx (lean, Go-puro). Reversível — é a
  única escolha que vale confirmar antes do C0.
- **Auth/exposição:** Tailscale (recomendado, privado) vs Caddy+OAuth (URL pública). Define se o
  console é só seu-na-rede ou acessível de qualquer lugar.
- **Console dentro do rara-core vs serviço separado:** recomendo separado (`rara-console`); a
  alternativa (embutir no rara-core) economiza um serviço ao custo de acoplar UI ao control plane.
- **Leitura: via API vs Neon direto:** recomendo só-API (sem credencial de banco no console). Se a
  agregação ficar pesada, reavaliar um read-replica/endpoint dedicado.
