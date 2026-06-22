CONSOLE-WORKERS-#FIX-199 — allowlist do agent = providers.app (bare), não rara-<app>/nome antigo

- **Nome da sessão:** `CONSOLE-WORKERS-#FIX-199`.
- **Branch dedicada:** do `main`, `fix/199-allowlist-convention`. Seguir `CLAUDE.md`.
- **Contexto:** issue #199. O agent casa a imagem por `allowed[req.App]`, e `req.App = providers.app`
  (bare: `distill`/`gate`/`extract`/`transcribe`, pós-P5). Mas o `agent.env.example` mostra chave
  `rara-distill=` (com prefixo) e o agent VPC tem `distill-local` (stale pós-P5b) → **mismatch** →
  wake do `distill-vpc` falha. Convenção correta: **chave da allowlist == `providers.app`**.

## Tarefa do Claude Code (doc/exemplo)
- `rara-runner/deploy/agent.env.example`: trocar as chaves de exemplo para **bare = providers.app**:
  `RUNNER_ALLOWED_IMAGES=distill=us-central1-docker.pkg.dev/oute-rara/rara/rara-distill,gate=.../rara-gate,extract=.../rara-extract,transcribe=.../rara-transcribe`
  e adicionar nota explícita: **"a chave DEVE ser igual ao `providers.app` do placement"** (o
  dispatcher manda `req.App = providers.app`). Atualizar o `README.md` do runner no mesmo sentido.

## Fix (ops, Renato) — alinhar os agents reais
- **VPC** (`/etc/rara-runner/agent.env`): chaves = app bare dos placements vpc habilitados (hoje
  `distill` e `gate`; adicionar `extract`/`transcribe` se criar placements vpc deles). Trocar a chave
  stale `distill-local` → `distill`. `systemctl restart rara-runner-agent`.
- **Mac** (`~/.rara-runner/agent.env`): idem, chaves bare (`distill`, etc.) pros placements `*-mac`.
  Reload do launchd.

## Aceite
`agent.env.example`/README com convenção bare + nota. Allowlists reais (VPC/Mac) com chaves = app.
Wake de `distill-vpc` (e demais placements vpc/mac) resolve a imagem sem mismatch (`last_error` limpo).

## Encerramento (DoD)
1. Commit (`fix(runner): allowlist key = providers.app (bare); fix example/README (#199)`).
2. PR base `main` → CI verde → CodeRabbit limpo → avisar Renato + **as edições de agent.env (VPC/Mac)**
   → PAUSAR.
3. Após o Renato alinhar as allowlists + mergear, verificar wake dos placements vpc/mac. Fecha #199.
4. Resumo: convenção documentada, allowlists alinhadas, verificação. Próximo: #198 (CI :latest).
