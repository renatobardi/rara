# rara 2.0 — Modelo de dados (Mermaid)

Estrutura de dados do 2.0 em dois níveis: **macro** (famílias de tabelas e como se ligam) e
**detalhado** (todas as colunas das tabelas do `rara-core`). Companheiro do
[ARCHITECTURE-2.0.pt-BR.md](./ARCHITECTURE-2.0.pt-BR.md).

Convenção: linhas **sólidas** = relação dentro do mesmo agente (com FK); linhas **tracejadas** =
link lógico cruzando fronteira de agente (sem FK), como no 1.0.

---

## Nível macro — duas famílias de tabelas

O `rara-core` é dono das tabelas de **controle**; os workers continuam donos das tabelas de
**domínio**. O control plane referencia o domínio só por chave lógica (`source_ref`, `output_ref`).

```mermaid
flowchart LR
  subgraph CORE["rara-core — control plane"]
    direction TB
    flows --> flow_steps
    capabilities --> providers
    capabilities --> flow_steps
    routing_policies
    items --> item_steps
    items --> gate_decisions
    feedback --> interest_profile
    interest_profile -.->|contexto| gate_decisions
  end
  subgraph DOM["Workers — tabelas de domínio (1.0, mantidas)"]
    direction TB
    channel_videos
    playlist_videos
    transcripts
    distillations
    news_items
  end
  item_steps -. output_ref .-> transcripts
  item_steps -. output_ref .-> distillations
  items -. source_ref .-> channel_videos
  items -. source_ref .-> playlist_videos
  items -. source_ref .-> news_items
```

---

## Nível macro — relacionamentos (ER simplificado)

```mermaid
erDiagram
  flows            ||--o{ flow_steps      : "tem"
  capabilities     ||--o{ flow_steps      : "referenciada por"
  capabilities     ||--o{ providers       : "implementada por"
  capabilities     ||--o{ routing_policies: "regida por"
  flows            ||--o{ items           : "instancia"
  items            ||--o{ item_steps      : "tem"
  items            ||--o{ gate_decisions  : "registra"
  providers        ||--o{ item_steps      : "executa"
  items            ||--o{ feedback        : "alvo de"
  feedback         }o--|| interest_profile: "revisa"
  item_steps       }o..o{ transcripts     : "output_ref (lógico)"
  item_steps       }o..o{ distillations   : "output_ref (lógico)"
  items            }o..o{ channel_videos  : "source_ref (lógico)"
```

---

## Nível detalhado — tabelas do rara-core

```mermaid
erDiagram
  flows {
    int          id            PK
    varchar      name          UK "NOT NULL"
    varchar      source_type      "youtube|podcast|email|linkedin|news"
    boolean      enabled          "DEFAULT true"
    timestamptz  created_at
    timestamptz  updated_at
  }

  flow_steps {
    int          id            PK
    int          flow_id       FK "NOT NULL"
    int          seq              "NOT NULL"
    varchar      capability       "NOT NULL  -> capabilities.name"
    jsonb        options          "DEFAULT {}  ex: {gate:skip}"
    boolean      enabled          "DEFAULT true"
  }

  capabilities {
    int          id            PK
    varchar      name          UK "coletar|transcrever|extrair|gate_barato|gate_rico|destilar"
    jsonb        io_contract      "schema de entrada/saída"
    text         description
  }

  providers {
    int          id            PK
    varchar      name          UK "NOT NULL"
    varchar      capability       "NOT NULL  -> capabilities.name"
    varchar      runtime          "local|cloudrun|vpc"
    varchar      activation       "resident|on_demand"
    numeric      cost
    numeric      quality
    int          latency_ms
    jsonb        constraints      "ex: {requires:residential} | {sensitivity:private}"
    boolean      enabled          "DEFAULT true"
    timestamptz  heartbeat_at     "saúde do provider"
  }

  routing_policies {
    int          id            PK
    varchar      scope            "global|capability"
    varchar      capability       "NULL se global"
    numeric      cost_weight
    numeric      quality_weight
    jsonb        fallback         "ordem de fallback de providers"
    boolean      enabled          "DEFAULT true"
  }

  items {
    bigint       id            PK
    varchar      lane             "NOT NULL"
    text         source_ref       "NOT NULL  id externo (video/url/msg)"
    int          flow_id       FK "NOT NULL"
    int          flow_version     "NOT NULL  carimbado na entrada"
    varchar      status           "discovered|running|done|filtered|quarantine"
    timestamptz  created_at
    timestamptz  updated_at
  }

  item_steps {
    bigint       id            PK
    bigint       item_id       FK "NOT NULL"
    int          seq              "NOT NULL"
    varchar      capability       "NOT NULL"
    varchar      status           "pending|assigned|running|done|failed|skipped"
    varchar      assigned_provider
    int          attempt          "DEFAULT 0"
    timestamptz  heartbeat_at
    text         output_ref       "ponteiro lógico p/ linha de domínio"
    text         error
    timestamptz  started_at
    timestamptz  finished_at
  }

  gate_decisions {
    bigint       id            PK
    bigint       item_id       FK "NOT NULL"
    varchar      gate             "gate_barato|gate_rico"
    varchar      decision         "keep|drop|defer"
    numeric      score
    varchar      decided_by       "rules|profile|llm"
    text         reason
    timestamptz  created_at
  }

  feedback {
    bigint       id            PK
    varchar      target_type      "item|distillation"
    text         target_ref       "NOT NULL"
    varchar      signal           "up|down|força"
    varchar      source           "user_explicit|kura_implicit|quarantine_review"
    timestamptz  created_at
  }

  interest_profile {
    int          id            PK
    int          version          "NOT NULL"
    jsonb        topics
    jsonb        authors
    jsonb        anti_topics
    jsonb        weights
    timestamptz  created_at
  }

  flows            ||--o{ flow_steps      : ""
  capabilities     ||--o{ providers       : ""
  capabilities     ||--o{ flow_steps      : ""
  flows            ||--o{ items           : ""
  items            ||--o{ item_steps      : ""
  items            ||--o{ gate_decisions  : ""
  providers        ||--o{ item_steps      : "assigned_provider"
  capabilities     ||--o{ routing_policies: ""
```

---

## Nível detalhado — tabelas de domínio (1.0, referência)

Mantidas como estão; o detalhe completo vive em [DATABASE_SCHEMA.md](./DATABASE_SCHEMA.md).
Aqui só os pontos de ancoragem (`output_ref` / `source_ref`).

```mermaid
erDiagram
  channel_videos {
    varchar youtube_video_id UK
    text    title
    text    url
  }
  transcripts {
    varchar youtube_video_id UK
    varchar source_type         "youtube|podcast|url|local"
    text    transcript
    varchar status
  }
  distillations {
    text    source_key
    varchar pattern
    text    content
    jsonb   structured
  }
  news_items {
    text    url   UK
    text    title
    text    body
  }
```
