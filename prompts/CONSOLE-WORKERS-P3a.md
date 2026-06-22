CONSOLE-WORKERS-#P3a — rara-runner: install do agent no Mac (launchd) + runbook de placements

- **Nome da sessão:** nomeie esta sessão do Claude Code como `CONSOLE-WORKERS-#P3a`.
- **Branch dedicada:** do **`main` atualizado**, crie `feat/console-workers-p3a-mac-agent`. NÃO empilhar.
- **Doc de referência:** `CONSOLE-WORKERS-PLANO-E5b.pt-BR.md` §1.3 (provisionamento). Modelos vivos:
  `rara-runner/deploy/rara-runner-agent.service` (systemd VPC), `rara-runner/deploy/agent.env.example`,
  e o padrão launchd do `rara-transcribe/install-local.sh`. Seguir o `CLAUDE.md`.

Habilita o **agent no Mac** (o listener `POST /run` no tailnet que faz `docker run` de imagens
allowlisted) — o que falta pra rodar placements `*-mac`. (O agent VPC já existe via systemd;
placements vpc já funcionam.) É código + runbook; a execução no Mac é ops do Renato (DoD).

## Tarefa A — launchd plist + script de install (Mac)
- `rara-runner/deploy/com.rara.runner-agent.plist` (template launchd): roda `rara-runner agent`,
  `KeepAlive`/`RunAtLoad` true (daemon resident), logs em `~/Library/Logs/rara-runner-agent`,
  carrega o env de `~/.rara-runner/agent.env`. Espelhar a robustez do `rara-runner-agent.service`
  (mesmas envs/flags), adaptado pra macOS.
- `rara-runner/install-agent-mac.sh`: guard de diretório (`rara-runner/`); checa Docker presente;
  builda o binário (`rara-runner`), instala em `~/.rara-runner/`; cria `~/.rara-runner/agent.env` a
  partir de `deploy/agent.env.example` se não existir e aplica `chmod 0600 ~/.rara-runner/agent.env`
  (contém `RUNNER_TOKEN` — apenas owner pode ler); instala + carrega o launchd
  (`launchctl unload` se já existir, depois `load`). Espelhar o `install-local.sh` do transcribe.

## Tarefa B — README/runbook
- Em `rara-runner/README.md` (ou um `deploy/RUNBOOK-mac.md`): passos pra provisionar o agent no Mac
  (Docker Desktop, Tailscale up, `RUNNER_ADDR`=IP tailnet, `RUNNER_TOKEN`=mesmo da dispatch,
  `RUNNER_ALLOWED_IMAGES`=imagens dos workers que vão rodar no Mac — **digest multi-arch**, arm64).
- **Runbook "adicionar um placement (vpc/mac) pela console":** abrir o worker → "adicionar placement"
  → `runtime` = vpc ou local(mac) → `runner_url` = URL tailnet do agent daquele host → enable + ordem.
  Pré-req: a imagem do app na allowlist do agent daquele host. Constraint: `caption` só Mac. Falha por
  artefato faltando aparece em `last_error` (§1.3).

## Aceite
`cd rara-runner && make test && make lint` verdes (se mexeu em código; o install é script). O plist +
script existem e são coerentes com o agent systemd. Runbook claro o suficiente pra o Renato
provisionar o Mac e adicionar placements. (Sem deploy automático — o agent no Mac é ops manual.)

---

## Ao terminar — ciclo de encerramento (Definition of Done)
1. **Commit** (`feat(runner): macOS launchd install for the agent + placement runbook (P3a)`).
2. **PR base `main`**.
3. **CI verde.** **CodeRabbit** → corrigir tudo → limpo.
4. **Avisar o Renato** + **os passos de ops no Mac** (rodar `install-agent-mac.sh`, preencher
   `~/.rara-runner/agent.env`, garantir Docker + Tailscale, validar que a dispatch alcança o agent).
   **PAUSAR**.
5. Após aprovar + mergear + provisionar o Mac:
   - **ops (Renato):** subir o agent; adicionar 1 placement de teste (ex.: `distill-mac`, runtime
     local, runner_url do Mac) **enabled**; garantir a imagem `rara-distill` na allowlist do Mac.
   - **verificar:** o dispatch acorda o `distill-mac` no próximo item de destilar; heartbeat fresco +
     `last_error` limpo na console.
6. **Resumo final:** o que mudou, PR, CI/CodeRabbit, e o estado do agent no Mac + primeiro placement
   mac no ar. (Com isso, "cada worker nos 3 runtimes" fica plenamente operável.)
