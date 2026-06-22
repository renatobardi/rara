CONSOLE-WORKERS-#P5a — limpeza de naming do transcribe (install-local + launchd + deploy default)

- **Nome da sessão:** nomeie esta sessão do Claude Code como `CONSOLE-WORKERS-#P5a`.
- **Branch dedicada:** do **`main` atualizado**, crie `feat/console-workers-p5a-transcribe-naming`. NÃO empilhar.
- **Doc de referência:** `CONSOLE-WORKERS-PLANO-E5b.pt-BR.md` §1.2/§4 (P5). Seguir o `CLAUDE.md`.

Primeira fatia da P5 (sweep de legado). **Sem cutover, sem DB.** Limpa os nomes "scribe" que sobraram
no app `rara-transcribe` e o default legado no deploy. Destrava o reinstall limpo do caption no Mac.

## Tarefa A — `rara-transcribe/install-local.sh`
Renomear todas as referências `scribe` → `transcribe` (mantendo o que já é "transcribe"):
- dir-guard: rodar de **`rara-transcribe/`** (não `rara-scribe/`).
- `INSTALL_DIR="$HOME/.rara-transcribe"`, `LOG_DIR="$HOME/Library/Logs/rara-transcribe"`.
- `LABEL="com.rara.transcribe"`, `PLIST_PATH=".../com.rara.transcribe.plist"`.
- binário: buildar/exec **`$INSTALL_DIR/transcribe-job`** (mesmo nome do `Makefile`, que gera
  `transcribe-job`).
- mensagens/echo e a dica "Update binary: cd rara-transcribe && make build && bash install-local.sh".
- A identidade continua via `~/.rara-transcribe/.env` (operador seta `SCRIBE_PROVIDER=caption-mac`);
  **não** hardcodar provider no script.

## Tarefa B — `rara-transcribe/DEPLOY.md` e `README.md`
Atualizar referências `rara-scribe`/`scribe-job`/`com.rara.scribe`/caminhos antigos → transcribe.

## Tarefa C — `deploy-transcribe.yml` (default de env → placeholder)
- Trocar `ENV_VARS="SCRIBE_PROVIDER=asr-direct-audio,TRANSCRIBE_ENGINE=groq"` por
  `ENV_VARS="SCRIBE_PROVIDER=PLACEHOLDER_PROVIDER,TRANSCRIBE_ENGINE=groq"` (mesmo padrão do
  `deploy-extract.yml`: o dispatcher SEMPRE sobrescreve o provider por execução; o placeholder é
  inválido de propósito). Atualizar o comentário (linha ~117) pra refletir isso.

## Não fazer
- NÃO mexer no `providers.app`, DB, allowlist (distill é a P5b). NÃO tocar em capabilities
  (`gate_barato`/`gate_rico` com underscore ficam). Só `rara-transcribe/` + `deploy-transcribe.yml`.

## Aceite
`cd rara-transcribe && make test && make lint` verdes (se o script/Makefile mudou, garantir build ok).
Grep no `rara-transcribe/` e em `deploy-transcribe.yml`: `rara-scribe`, `com.rara.scribe`, `scribe-job`,
`asr-direct-audio` (como default de env) = **0**. (A palavra dentro de "tran**scribe**" é ok.)

---

## Ao terminar — ciclo de encerramento (Definition of Done)
1. **Commit** (`chore(transcribe): rename scribe→transcribe in install/deploy (P5a)`; corpo ref. §4/P5).
2. **PR base `main`**.
3. **CI verde.** **CodeRabbit** → corrigir tudo → limpo.
4. **Avisar o Renato** + **passo manual no Mac** (após merge): rodar `cd rara-transcribe &&
   ./install-local.sh` (agora roda do dir certo) e **descarregar o serviço antigo**
   (`launchctl unload ~/Library/LaunchAgents/com.rara.scribe.plist` + remover o plist antigo). **PAUSAR**.
5. Após o Renato aprovar + mergear + reinstalar no Mac, **acompanhar o deploy** e confirmar o caption
   no novo serviço (`com.rara.transcribe`, binário `transcribe-job`, heartbeat fresco na console).
6. **Resumo final:** o que mudou, PR, CI/CodeRabbit, deploy, e o reinstall do caption no Mac. Próximo:
   **P5b** (normalizar `distill-vpc.app`) e **P5c** (sweep de docs/comentários/fixtures).
