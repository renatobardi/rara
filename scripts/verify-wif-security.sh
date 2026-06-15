#!/usr/bin/env bash
#
# verify-wif-security.sh — valida que o Workload Identity Federation do rara
# só permite que o repo esperado (renatobardi/rara) impersone a service account
# de deploy, e que essa SA não tem roles excessivas.
#
# Corre no GCP Cloud Shell (gcloud já autenticado):
#   bash verify-wif-security.sh                       # usa o projeto activo do gcloud
#   PROJECT_ID=meu-projeto bash verify-wif-security.sh
#   PROJECT_ID=meu-projeto EXPECTED_REPO=org/repo SA_NAME=rara-deployer bash verify-wif-security.sh
#
# Só faz leituras (describe / get-iam-policy / list). Não altera nada.

set -uo pipefail

# ── Config (override por env var) ─────────────────────────────────────────────
PROJECT_ID="${PROJECT_ID:-$(gcloud config get-value project 2>/dev/null)}"
EXPECTED_REPO="${EXPECTED_REPO:-renatobardi/rara}"
SA_NAME="${SA_NAME:-rara-deployer}"
EXPECTED_OWNER="${EXPECTED_REPO%%/*}"   # renatobardi

if [[ -z "${PROJECT_ID}" || "${PROJECT_ID}" == "(unset)" ]]; then
  echo "ERRO: PROJECT_ID não definido e nenhum projeto activo no gcloud." >&2
  echo "      Corre:  PROJECT_ID=<o-teu-projeto> bash $0" >&2
  exit 1
fi

SA_EMAIL="${SA_NAME}@${PROJECT_ID}.iam.gserviceaccount.com"

# ── Cores / helpers ───────────────────────────────────────────────────────────
if [[ -t 1 ]]; then G=$'\e[32m'; Y=$'\e[33m'; R=$'\e[31m'; B=$'\e[1m'; N=$'\e[0m'; else G=; Y=; R=; B=; N=; fi
pass() { echo "${G}✅ PASS${N} $*"; }
warn() { echo "${Y}⚠️  WARN${N} $*"; WARNINGS=$((WARNINGS+1)); }
fail() { echo "${R}❌ FAIL${N} $*"; FAILURES=$((FAILURES+1)); }
hr()   { echo "────────────────────────────────────────────────────────────────"; }
WARNINGS=0; FAILURES=0

echo "${B}WIF security check${N}"
echo "  Project:        ${PROJECT_ID}"
echo "  Service account: ${SA_EMAIL}"
echo "  Repo esperado:  ${EXPECTED_REPO}"
hr

# ── 1. Provider attribute conditions ──────────────────────────────────────────
echo "${B}1) Workload Identity providers — attribute condition${N}"
echo "   (deve restringir assertion.repository/repository_owner ao repo esperado)"
echo

mapfile -t POOLS < <(gcloud iam workload-identity-pools list \
  --location=global --project="${PROJECT_ID}" \
  --format="value(name.basename())" 2>/dev/null)

if [[ ${#POOLS[@]} -eq 0 ]]; then
  warn "Nenhum workload identity pool encontrado em 'global'. Confirma a location/projeto."
fi

for POOL in "${POOLS[@]}"; do
  echo "  • pool: ${POOL}"
  mapfile -t PROVIDERS < <(gcloud iam workload-identity-pools providers list \
    --location=global --workload-identity-pool="${POOL}" --project="${PROJECT_ID}" \
    --format="value(name.basename())" 2>/dev/null)

  for PROV in "${PROVIDERS[@]}"; do
    COND=$(gcloud iam workload-identity-pools providers describe "${PROV}" \
      --location=global --workload-identity-pool="${POOL}" --project="${PROJECT_ID}" \
      --format="value(attributeCondition)" 2>/dev/null)
    MAP=$(gcloud iam workload-identity-pools providers describe "${PROV}" \
      --location=global --workload-identity-pool="${POOL}" --project="${PROJECT_ID}" \
      --format="value(attributeMapping)" 2>/dev/null)

    echo "    provider: ${PROV}"
    echo "      attributeMapping:   ${MAP:-<none>}"
    echo "      attributeCondition: ${COND:-<none>}"

    if [[ -z "${COND}" ]]; then
      fail "      provider sem attributeCondition → QUALQUER repo GitHub pode obter um token deste provider."
    elif [[ "${COND}" == *"${EXPECTED_REPO}"* ]]; then
      pass "      condition restringe ao repo '${EXPECTED_REPO}'."
    elif [[ "${COND}" == *"${EXPECTED_OWNER}"* ]]; then
      warn "      condition restringe ao owner '${EXPECTED_OWNER}' (todos os repos do owner), não a um único repo."
    else
      warn "      condition não menciona '${EXPECTED_REPO}' nem '${EXPECTED_OWNER}' — revê manualmente acima."
    fi
    echo
  done
done
hr

# ── 2. Quem pode impersonar a SA (binding workloadIdentityUser) ───────────────
echo "${B}2) Binding 'workloadIdentityUser' na service account${N}"
echo "   (o principalSet deve ser específico do repo, não o pool inteiro nem '*')"
echo

if ! gcloud iam service-accounts describe "${SA_EMAIL}" --project="${PROJECT_ID}" >/dev/null 2>&1; then
  fail "Service account ${SA_EMAIL} não existe (confere SA_NAME/PROJECT_ID)."
else
  mapfile -t WI_MEMBERS < <(gcloud iam service-accounts get-iam-policy "${SA_EMAIL}" \
    --project="${PROJECT_ID}" --format="json" 2>/dev/null \
    | python3 -c "import sys,json
try:
    p=json.load(sys.stdin)
except Exception:
    sys.exit(0)
for b in p.get('bindings',[]):
    if b.get('role')=='roles/iam.workloadIdentityUser':
        for m in b.get('members',[]):
            print(m)")

  if [[ ${#WI_MEMBERS[@]} -eq 0 ]]; then
    warn "Nenhum binding roles/iam.workloadIdentityUser na SA — o deploy via WIF pode não funcionar, ou usa outra SA."
  fi
  for M in "${WI_MEMBERS[@]}"; do
    echo "  member: ${M}"
    if [[ "${M}" == *"attribute.repository/${EXPECTED_REPO}" ]]; then
      pass "    scoped ao repo exacto '${EXPECTED_REPO}'."
    elif [[ "${M}" == *"/*" || "${M}" == *"workloadIdentityPools/"*"/*" ]]; then
      fail "    principalSet abrange o POOL inteiro/curinga → qualquer identidade do pool pode impersonar a SA."
    elif [[ "${M}" == *"attribute.repository_owner/${EXPECTED_OWNER}" ]]; then
      warn "    scoped ao owner '${EXPECTED_OWNER}' (todos os repos do owner)."
    elif [[ "${M}" == *"attribute.repository/"* ]]; then
      warn "    scoped a um repo, mas diferente do esperado — revê acima."
    else
      warn "    member não reconhecido como scope-por-repo — revê manualmente."
    fi
    echo
  done
fi
hr

# ── 3. Roles da SA a nível de projeto (least privilege) ───────────────────────
echo "${B}3) Roles da service account a nível de projeto${N}"
echo "   (não deve ter roles/owner nem roles/editor)"
echo
mapfile -t SA_ROLES < <(gcloud projects get-iam-policy "${PROJECT_ID}" \
  --flatten="bindings[].members" \
  --filter="bindings.members:serviceAccount:${SA_EMAIL}" \
  --format="value(bindings.role)" 2>/dev/null)

if [[ ${#SA_ROLES[@]} -eq 0 ]]; then
  warn "Nenhuma role a nível de projeto para ${SA_EMAIL} (pode ter permissões a nível de recurso)."
fi
for ROLE in "${SA_ROLES[@]}"; do
  if [[ "${ROLE}" == "roles/owner" || "${ROLE}" == "roles/editor" ]]; then
    fail "  ${ROLE} — demasiado amplo para uma SA de deploy."
  else
    echo "  • ${ROLE}"
  fi
done
hr

# ── Veredicto ─────────────────────────────────────────────────────────────────
echo "${B}Resumo:${N} ${FAILURES} falha(s), ${WARNINGS} aviso(s)."
if [[ ${FAILURES} -gt 0 ]]; then
  echo "${R}Há configuração que permite abuso da SA. Corrige antes de confiar no WIF.${N}"
  exit 1
elif [[ ${WARNINGS} -gt 0 ]]; then
  echo "${Y}Sem falhas críticas, mas revê os avisos acima.${N}"
  exit 0
else
  echo "${G}WIF restrito ao repo esperado e SA com privilégios contidos.${N}"
  exit 0
fi
