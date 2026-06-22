CONSOLE-WORKERS-#P4 — console: polish do worker/placement (description, RO, runner_url, constraints)

- **Nome da sessão:** nomeie esta sessão do Claude Code como `CONSOLE-WORKERS-#P4`.
- **Branch dedicada:** do **`main` atualizado**, crie `feat/console-workers-p4-console-polish`. NÃO empilhar.
- **Doc de referência:** `CONSOLE-WORKERS-PLANO-E5b.pt-BR.md` §1.1/§1.4/§4 (P4). Seguir o `CLAUDE.md`.

Só `rara-console/web/`. Fecha os detalhes da tela Workers que faltaram. **Já está pronto (não mexer,
só não regredir):** badge de `last_error`, toggle enable/disable, reorder de fallback,
worker/capability lockados no *add-placement*. Faltam 4 itens:

> Verificação do front: `npm run check` + `npm run build` + smoke. Sem harness de teste no `web/`.

## Tarefa A — mostrar o `description` (nome claro)
- Adicionar `description?: string` ao type `Provider` no front (vem do `/api/workers` → placements).
- Exibir o nome claro junto do **codinome do worker** no cabeçalho de cada grupo (ex.: `distill —
  Destilador (LLM)`). Como é por-worker, ler de qualquer placement do grupo (todos iguais).

## Tarefa B — `capability` e `runtime` read-only no EDIT (`lib/WorkerForm.svelte`)
- Hoje: `capabilityReadonly = !!lockedCapability` (só no add-placement) e `runtime` é sempre editável.
- Ajustar: `capabilityReadonly = !!lockedCapability || isEdit` e criar `runtimeReadonly = isEdit`.
  No modo edit, `capability` e `runtime` aparecem **read-only** (são identidade do placement). Na
  criação seguem editáveis. (Pular a auto-sugestão de nome quando readonly, como já é no edit.)

## Tarefa C — `runner_url` só para vpc/mac (`lib/WorkerForm.svelte`)
- Mostrar o campo `runner_url` **apenas quando `runtime` ∈ {vpc, local}** (cloud é sempre vazio).
  Quando `runtime === 'cloudrun'`, esconder o campo e **não** enviar/validar `runner_url` no payload.

## Tarefa D — constraints travadas (impossível por constraint)
- Quando o worker tem constraint dura que restringe runtime (ex.: `requires: residential` →
  `caption`), no *add-placement* o select de `runtime` deve **oferecer só os runtimes válidos**
  (residential → `local`/mac apenas; esconder/disable cloud e vpc). Derivar do `constraints.requires`
  do worker (já disponível nos placements). Se não houver constraint, todos os runtimes ficam livres.
- (Se for muito custoso ler a constraint no form agora, no mínimo **não** deixar criar um placement de
  runtime que a constraint proíbe — barrar no submit com mensagem clara.)

## Não fazer
- Nenhuma chamada de deploy pela UI (confirmar que continua assim). Não mexer em core/BFF.

## Strings
Novas labels (nome claro, mensagens de constraint) em `lib/strings.ts`.

## Aceite
`cd rara-console/web && npm run check && npm run build` limpos. O `description` aparece por worker; no
**edit** de um placement, `capability` e `runtime` são read-only; `runner_url` só aparece em vpc/mac;
no add-placement de um worker com constraint residential (`caption`) o runtime fica restrito a
mac/local. last_error/enable/ordem seguem funcionando. Tema claro/escuro ok. Smoke contra o core ao vivo.

---

## Ao terminar — ciclo de encerramento (Definition of Done)
1. **Commit** (convencional; corpo referencia o plano §1.1/§1.4/§4 / P4).
2. **PR base `main`**.
3. **Acompanhar o CI.** Falhou → corrige até verde.
4. **Aguardar o CodeRabbit** → corrigir tudo → limpo + verde.
5. **Avisar o Renato** e **PAUSAR**. Não mergear.
6. Após aprovar + mergear, **acompanhar o deploy** e dar um smoke na tela. Falhou → corrige.
7. **Resumo final:** o que mudou, PR, CI/CodeRabbit, deploy. Próximo: **P3** (agent no Mac + adicionar
   placements vpc/mac) e depois **P5** (sweep de legado: install-local.sh, SCRIBE_PROVIDER default,
   distill app, etc.) + **P6** (gate de zero-legado).
