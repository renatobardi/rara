# Deploy — rara-shelf (Cloud Run via GitHub Actions)

O [`deploy-shelf.yml`](../.github/workflows/deploy-shelf.yml) builda a imagem com
**Cloud Build** e faz deploy de um **Cloud Run Job** `rara-shelf`, autenticando no GCP
via **Workload Identity Federation** (sem chaves de service account).

Reaproveita toda a infra do rara-harvest (projeto `${PROJECT_ID}`, Artifact Registry `rara`,
WIF, service account `rara-deployer`, bucket `${PROJECT_ID}_cloudbuild`, base Neon). A única
coisa nova é **autenticação OAuth** para ler as tuas playlists — 3 secrets no Secret Manager.

> Define `export PROJECT_ID=<o-teu-projeto>` antes de correr os comandos abaixo. O valor real
> vive na GitHub Variable `GCP_PROJECT_ID`, não neste ficheiro.

Diferente do harvest (API key pública), o shelf precisa de OAuth porque lê as **tuas**
playlists (incl. privadas), o que exige login Google. **Watch Later / History não são
acessíveis pela Data API** e são ignorados pelo app.

---

## 1. Criar credenciais OAuth (uma vez)

No console GCP do projeto `${PROJECT_ID}`:

### 1.1 OAuth consent screen
APIs & Services → **OAuth consent screen**:
- User type: **External**
- Preenche app name / email de suporte
- Em **Test users**, adiciona o teu próprio email Google (o dono das playlists)
- Scope necessário: `https://www.googleapis.com/auth/youtube.readonly`

### 1.2 OAuth Client
APIs & Services → **Credentials** → **Create credentials** → **OAuth client ID**:
- Application type: **Desktop app**
- Copia o **Client ID** e o **Client secret**

### 1.3 Ativar a API
Garante que a **YouTube Data API v3** está ativada:
```bash
gcloud services enable youtube.googleapis.com --project "${PROJECT_ID}"
```

---

## 2. Obter o refresh token (uma vez)

Via **OAuth 2.0 Playground** (sem código):
1. Abre https://developers.google.com/oauthplayground
2. Engrenagem (⚙) → marca **Use your own OAuth credentials** → cola Client ID + Secret
3. No painel esquerdo, em "Input your own scopes", escreve:
   `https://www.googleapis.com/auth/youtube.readonly`
4. **Authorize APIs** → faz login com a tua conta → consente
5. **Exchange authorization code for tokens** → copia o **Refresh token**

> O refresh token é de longa duração; o app troca-o por um access token a cada execução.

---

## 3. Guardar os secrets (Cloud Shell)

```bash
printf '%s' 'SEU_CLIENT_ID'     | gcloud secrets create shelf-oauth-client-id     --replication-policy=automatic --data-file=- --project "${PROJECT_ID}"
printf '%s' 'SEU_CLIENT_SECRET' | gcloud secrets create shelf-oauth-client-secret --replication-policy=automatic --data-file=- --project "${PROJECT_ID}"
printf '%s' 'SEU_REFRESH_TOKEN' | gcloud secrets create shelf-oauth-refresh-token --replication-policy=automatic --data-file=- --project "${PROJECT_ID}"
```

`database-url` já existe (reutilizado do harvest). A service account `rara-deployer` e a
SA de runtime (compute default) já têm `secretmanager.secretAccessor` a nível de projeto,
portanto os 3 secrets novos ficam acessíveis **sem alterações de IAM**.

Rotação futura: `gcloud secrets versions add shelf-oauth-refresh-token --data-file=-`.

---

## 4. Deploy

- **Automático**: merge de qualquer coisa em `rara-shelf/**` para `main` dispara o
  `deploy-shelf.yml`.
- **Manual**: Actions → **Deploy rara-shelf to Cloud Run** → *Run workflow*.

O workflow builda a imagem, cria/atualiza o Cloud Run Job `rara-shelf` (montando os 4
secrets) e executa uma vez.

---

## 5. Verificar

```sql
SELECT p.title, p.privacy_status, COUNT(pv.id) AS videos
FROM playlists p
LEFT JOIN playlist_videos pv ON pv.playlist_id = p.id
GROUP BY p.id
ORDER BY videos DESC;
```

## 6. Agendamento (opcional)

```bash
gcloud scheduler jobs create http rara-shelf-daily \
  --location=us-central1 --schedule="0 3 * * *" \
  --uri="https://us-central1-run.googleapis.com/apis/run.googleapis.com/v1/namespaces/${PROJECT_ID}/jobs/rara-shelf:run" \
  --http-method=POST \
  --oauth-service-account-email="rara-deployer@${PROJECT_ID}.iam.gserviceaccount.com"
```
