# rara-shelf

Segundo agente do ecossistema **rara**. Enquanto o `rara-harvest` colhe vídeos de canais
públicos externos, o **rara-shelf** faz o inverso: cataloga **as tuas próprias playlists**
do YouTube (públicas, privadas e não-listadas) e os vídeos de cada uma, registando **a que
playlist cada vídeo pertence**.

Totalmente isolado do harvest (código, tabelas, Cloud Run Job e workflows próprios),
reaproveitando a mesma infraestrutura GCP + Neon.

## Como funciona

1. Troca um **refresh token** OAuth por um access token (`oauth2.googleapis.com/token`).
2. Descobre todas as tuas playlists via `playlists.list?mine=true` (paginado).
3. Para cada playlist, lista os vídeos via `playlistItems.list` (paginado) e grava no Neon.

```
OAuth refresh token → access token
   → playlists.list?mine=true        → tabela `playlists`
   → playlistItems.list (por playlist) → tabela `playlist_videos` (com playlist_id)
```

## Diferenças-chave face ao rara-harvest

| | rara-harvest | rara-shelf |
|---|---|---|
| Auth | API key (pública) | **OAuth** (refresh token) |
| Fonte | seed table de canais | **descoberta** `mine=true` |
| Unicidade do vídeo | global (`youtube_video_id`) | **composta** `(playlist_id, youtube_video_id)` |
| Endpoint | `/playlistItems` (uploads) | `/playlists` + `/playlistItems` |

A unicidade composta significa que **o mesmo vídeo pode estar em várias playlists** —
guardado uma vez por playlist.

## Modelo de dados

- **`playlists`**: `youtube_playlist_id` (unique), `title`, `description`,
  `privacy_status`, `item_count`, `active`, timestamps.
- **`playlist_videos`**: `playlist_id` (FK), `youtube_video_id`, `title`, `url`,
  `published_at` (nullable), `position`, `collected_at`, `UNIQUE(playlist_id, youtube_video_id)`.

## Variáveis de ambiente

| Var | Descrição |
|-----|-----------|
| `DATABASE_URL` | Connection string do Neon (reutilizada do harvest) |
| `GOOGLE_OAUTH_CLIENT_ID` | OAuth client id |
| `GOOGLE_OAUTH_CLIENT_SECRET` | OAuth client secret |
| `GOOGLE_OAUTH_REFRESH_TOKEN` | Refresh token (scope `youtube.readonly`) |

## Limitações

- **Watch Later / History**: não acessíveis pela YouTube Data API desde 2016. O app
  regista em log e ignora.

## Desenvolvimento

```bash
make test          # testes (harness TDD)
make test-race     # com race detector
make fmt lint      # formatação + vet
make build         # binário local
```

## Deploy

Ver [DEPLOY.md](DEPLOY.md) — setup OAuth + Cloud Run via GitHub Actions.
