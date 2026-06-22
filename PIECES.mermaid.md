# rara 2.0 — As peças (Mermaid)

Visão **macro das peças** do ecossistema, como estão hoje: os apps, a capability que cada um serve,
e onde rodam. Tela para repensar a arquitetura de peças. Companheiro do
[ARCHITECTURE-2.0.pt-BR.md](./ARCHITECTURE-2.0.pt-BR.md); fluxos detalhados em
[FLOWS.mermaid.md](./FLOWS.mermaid.md); dados em [DATA-MODEL.mermaid.md](./DATA-MODEL.mermaid.md).

Regra de ouro: **o contrato é a tabela no Neon, nunca a chamada direta.** O `core` decide; as peças
executam; ninguém chama ninguém.

---

## 1. As peças por papel + o pipeline

```mermaid
flowchart TB
  subgraph CP["CONTROL PLANE — VPC, sempre-on (não executa domínio)"]
    direction LR
    CORE["rara-core<br/><i>orquestrador · reconcile + surface REST/MCP</i>"]
    CONS["rara-console<br/><i>BFF + SPA · operar/curar</i>"]
    SDK["rara-addon<br/><i>SDK · claim · heartbeat · poke (o contrato)</i>"]
  end

  subgraph PIPE["PIPELINE DE CAPABILITIES — uma raia colapsa neste template"]
    direction LR
    COL["coletar<br/><i>harvest · shelf · dial<br/>courier · clip · feed</i>"]
    G1{"gate_barato<br/>(sift)"}
    TX["transcrever | extrair<br/><i>transcribe · extract</i>"]
    G2{"gate_rico<br/>(sift)"}
    DS["destilar<br/><i>distill</i>"]
    OUT["✦ distillations"]
    COL --> G1
    G1 -->|keep| TX
    G1 -->|drop| FIL1["filtered"]
    G1 -->|defer| QQ1["quarentena"]
    TX --> G2
    G2 -->|keep| DS
    G2 -->|drop| FIL2["filtered"]
    G2 -->|defer| QQ2["quarentena"]
    DS --> OUT
  end

  subgraph LEARN["APRENDIZADO — fecha o loop do gosto"]
    direction LR
    HONE["hone<br/><i>revise · reescreve interest_profile</i>"]
    FB["feedback<br/><i>thumbs · revisão de quarentena</i>"]
    FB --> HONE
  end

  CORE -->|atribui · ativa · roteia| PIPE
  HONE -. perfil ativo (aprovação humana) .-> G1
  HONE -. perfil ativo .-> G2
  OUT -. read-only .-> KURA["KURA (adiado)"]
```

---

## 2. Onde cada peça roda (instalação atual)

Regra de host: **always-on → VPC (systemd); on_demand → Cloud Run Jobs; áudio YouTube → Mac**
(IP residencial). Um app serve vários providers por config (codebases ≪ providers).

```mermaid
flowchart TB
  subgraph VPC["VPC Oracle ARM — systemd, sempre-on, tailnet"]
    CORE["rara-core<br/>reconcile + surface"]
    CONS["rara-console"]
  end
  subgraph MAC["Mac — launchd, residencial"]
    SCY["transcribe · caption<br/><i>daily 02:00</i>"]
  end
  subgraph CR["GCP Cloud Run — on_demand + 7 Cloud Schedulers"]
    direction TB
    COLs["coletores: harvest · shelf · dial · courier · clip · feed"]
    WRK["transcribe·echo · extract · gate (gate_barato/rico) · distill · distill-news · hone"]
    LL["LiteLLM (Service) — groq · gemini · deepseek"]
  end
  NEON[("Neon — estado (control) + domínio<br/>único ponto de acoplamento")]

  CORE -->|ativa: jobs run API| CR
  CORE -->|poke tailnet + pull| MAC
  VPC --- NEON
  MAC --- NEON
  CR --- NEON
  WRK -->|modelos| LL
```

---

## 3. Mapa app → capability → providers → host

```mermaid
flowchart LR
  subgraph PROD["Coletores (produtores — sem SDK)"]
    harvest["harvest → coletar · YT canais · CloudRun"]
    shelf["shelf → coletar · YT playlists · CloudRun"]
    dial["dial → coletar · podcast · CloudRun"]
    courier["courier → coletar · email · CloudRun"]
    clip["clip → coletar · linkedin · CloudRun"]
    feed["feed → coletar · news · CloudRun"]
  end
  subgraph WORK["Workers (SDK rara-addon — claim-workers)"]
    transcribe["transcribe → transcrever · caption (Mac) + echo (CloudRun)"]
    glean["extract → extrair · winnow/email + scrub/linkedin + glean/news · CloudRun"]
    sift["gate → gate_barato + gate_rico · sift/3rd (CloudRun) + *-local (Mac)"]
    distill["distill → destilar · distill/3rd (CloudRun) + distill-vpc (Mac)"]
  end
  subgraph PERIOD["Periódico"]
    hone["hone → revise · cron (CloudRun + Scheduler)"]
  end
  subgraph ORQ["Orquestra (não executa)"]
    core["core → reconcile/surface/seed/ingest/feedback/quarantine"]
    console["console → operar/curar"]
  end
```
