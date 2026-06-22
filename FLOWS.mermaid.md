# rara 2.0 — Fluxos das funcionalidades (Mermaid)

Todos os fluxos do 2.0: o pipeline universal, a máquina de estados do item, o loop do reconciler,
o despacho (pull + ativação), os portões em cascata, o loop de aprendizado, as raias de fonte e o
roteamento de provider. Companheiro do [ARCHITECTURE-2.0.pt-BR.md](./ARCHITECTURE-2.0.pt-BR.md).

---

## 1. Pipeline universal

Toda raia colapsa neste template; os toggles do flow podem pular passos.

```mermaid
flowchart LR
  C["coletar_X"] --> G1{"gate_barato"}
  G1 -->|keep| T["transcrever | extrair"]
  G1 -->|drop| F1["filtered"]
  G1 -->|defer| Q1["quarentena"]
  T --> G2{"gate_rico"}
  G2 -->|keep| D["destilar"]
  G2 -->|drop| F2["filtered"]
  G2 -->|defer| Q2["quarentena"]
  D --> O["saída → distillations"]
```

---

## 2. Ciclo de vida do item (máquina de estados)

```mermaid
stateDiagram-v2
  [*] --> discovered
  discovered --> gate1
  gate1 --> to_text: keep
  gate1 --> filtered: drop
  gate1 --> quarantine: defer
  to_text --> gate2
  gate2 --> distilled: keep
  gate2 --> filtered: drop
  gate2 --> quarantine: defer
  distilled --> done
  done --> [*]
  filtered --> [*]
  quarantine --> gate1: revisão (keep)
```

---

## 3. Loop do reconciler (control plane)

Level-triggered: observa estado desejado vs atual e age. Roda always-on na VPC.

```mermaid
flowchart TD
  A["observa items não-terminais"] --> B["próximo passo<br/>(pela flow_version do item)"]
  B --> C{"status do passo?"}
  C -->|pending| D["router seleciona provider<br/>(policy + constraints + saúde)"]
  D --> E["grava atribuição em item_steps"]
  E --> F{"activation do provider?"}
  F -->|on_demand| G["dispara Cloud Run Jobs run"]
  F -->|resident| H["worker já está em polling"]
  C -->|assigned + heartbeat velho > T| I["re-dispara ativação<br/>ou fallback p/ próximo provider"]
  C -->|done/failed| J["avança ou termina o item"]
  G --> A
  H --> A
  I --> A
  J --> A
```

---

## 4. Despacho — pull pro trabalho, ativação configurável

Entrega de trabalho é sempre **pull**; só a forma de **acordar** o worker muda.

```mermaid
sequenceDiagram
  participant R as Reconciler (VPC)
  participant N as Neon (item_steps)
  participant W as Worker resident (Mac/VPC)
  participant CR as Cloud Run Job (on_demand)
  R->>N: atribui(item, step, provider)
  alt provider resident
    W->>N: claim (SELECT ... FOR UPDATE SKIP LOCKED)
  else provider on_demand
    R->>CR: run (ativação)
    CR->>N: claim (SELECT ... FOR UPDATE SKIP LOCKED)
  end
  Note over W,CR: executa a capability
  W-->>N: grava resultado + status + heartbeat
  CR-->>N: grava resultado + status + heartbeat
  R->>N: observa e avança (próximo ciclo)
```

---

## 5. Portões de curadoria — cascata barato→caro

A camada cara (LLM) só roda no que ficou em cima do muro.

```mermaid
flowchart TD
  IN["item (metadata) ou texto completo"] --> R{"regras<br/>allow/deny"}
  R -->|decidiu| OUT1["keep / drop"]
  R -->|indeciso| P{"match de perfil<br/>interest_profile"}
  P -->|decidiu| OUT2["keep / drop"]
  P -->|em cima do muro| L["LLM-judge<br/>(caro)"]
  L --> OUT3["keep / drop / defer"]
  OUT1 --> GD["grava em gate_decisions"]
  OUT2 --> GD
  OUT3 --> GD
  GD --> NEXT{"decisão"}
  NEXT -->|keep| ADV["avança o item"]
  NEXT -->|drop| FIL["filtered"]
  NEXT -->|defer| QUA["quarentena"]
```

---

## 6. Loop de aprendizado (feedback → perfil → próximas decisões)

Loop fechado por um artefato legível por humanos; sem infra de treino.

```mermaid
flowchart LR
  T["thumbs nas distillations"] --> FB["feedback"]
  K["uso no KURA (implícito)"] --> FB
  QR["revisão da quarentena (defer)"] --> FB
  GD["gate_decisions"] --> REV["revisão periódica"]
  FB --> REV
  REV --> IP["interest_profile<br/>(vivo, versionado)"]
  IP --> USE["camada de perfil + contexto do LLM-judge"]
  USE --> GD
```

---

## 7. Raias de fonte → template comum

Cada raia muda só o coletor e o passo de virar texto; o resto é compartilhado.

```mermaid
flowchart LR
  Y1["YouTube canais<br/>harvest · API key"] --> G1
  Y2["YouTube playlists<br/>shelf · OAuth"] --> G1
  PC["Podcast<br/>coletor RSS"] --> G1
  EM["Email<br/>Gmail API · OAuth"] --> G1
  LI["LinkedIn<br/>stash → Bright Data"] --> G1
  NW["News<br/>feed · RSS/HN/HTML"] --> G1
  subgraph TPL["Template comum"]
    direction LR
    G1{"gate_barato"} --> TX["transcrever | extrair"] --> G2{"gate_rico"} --> DS["destilar"]
  end
  DS --> OUT["distillations"]
```

---

## 8. Roteamento de provider (uma capability, vários runtimes)

O router ordena por custo ou qualidade e respeita constraints duras.

```mermaid
flowchart TD
  CAP["capability: transcrever"] --> ROUTER{"router<br/>custo ⇄ qualidade<br/>+ constraints + fallback"}
  ROUTER -->|exige residencial| P1["caption<br/>Local Mac · grátis · alta q"]
  ROUTER -->|qualquer runtime| P2["echo<br/>Cloud Run · $ · rápido"]
  ROUTER -->|self-host| P3["echo-vpc<br/>VPC · $$ · isolado"]
  P1 -.->|claim| N["Neon (item_steps)"]
  P2 -.->|claim| N
  P3 -.->|claim| N
```
