#!/usr/bin/env python3
"""
harvest-status.py — snapshot do que rara-harvest coletou no NeonDB.

Uso:
    DATABASE_URL=postgresql://... python3 scripts/harvest-status.py
    # ou coloque DATABASE_URL no arquivo rara-harvest/.env
"""

import os
import sys
import subprocess
import importlib.util

# ── auto-install psycopg2-binary se necessário ───────────────────────────────
if importlib.util.find_spec("psycopg2") is None:
    print("⚙  instalando psycopg2-binary...")
    subprocess.check_call([sys.executable, "-m", "pip", "install", "-q", "psycopg2-binary"])

import psycopg2
from psycopg2.extras import RealDictCursor
from datetime import timezone

# ── carrega DATABASE_URL ──────────────────────────────────────────────────────
DATABASE_URL = os.environ.get("DATABASE_URL")

if not DATABASE_URL:
    env_file = os.path.join(os.path.dirname(__file__), "..", "rara-harvest", ".env")
    if os.path.exists(env_file):
        for line in open(env_file):
            line = line.strip()
            if line.startswith("DATABASE_URL="):
                DATABASE_URL = line.split("=", 1)[1].strip().strip('"').strip("'")
                break

if not DATABASE_URL:
    print("ERRO: DATABASE_URL não encontrado.")
    print("      Defina a variável de ambiente ou crie rara-harvest/.env")
    sys.exit(1)

# ── helpers ───────────────────────────────────────────────────────────────────
SEP  = "─" * 64
SEP2 = "═" * 64

def fmt_ts(ts):
    if ts is None:
        return "—"
    if hasattr(ts, "tzinfo") and ts.tzinfo is None:
        ts = ts.replace(tzinfo=timezone.utc)
    return ts.strftime("%Y-%m-%d %H:%M UTC")

def bar(n, total, width=20):
    if total == 0:
        return "░" * width
    filled = round(n / total * width)
    return "█" * filled + "░" * (width - filled)

# ── queries ───────────────────────────────────────────────────────────────────
CHANNELS_Q = """
SELECT
    tc.id,
    tc.channel_name,
    tc.youtube_channel_id,
    tc.active,
    tc.created_at,
    COUNT(cv.id)                                AS video_count,
    MIN(cv.published_at)                        AS oldest_video,
    MAX(cv.published_at)                        AS newest_video,
    MAX(cv.collected_at)                        AS last_collected
FROM target_channels tc
LEFT JOIN channel_videos cv ON cv.channel_id = tc.id
GROUP BY tc.id
ORDER BY video_count DESC;
"""

TOTALS_Q = """
SELECT
    COUNT(*)                                    AS total_videos,
    COUNT(DISTINCT channel_id)                  AS channels_with_videos,
    MIN(published_at)                           AS oldest,
    MAX(published_at)                           AS newest,
    MAX(collected_at)                           AS last_run,
    COUNT(*) FILTER (
        WHERE collected_at >= NOW() - INTERVAL '24 hours'
    )                                           AS collected_last_24h,
    COUNT(*) FILTER (
        WHERE collected_at >= NOW() - INTERVAL '7 days'
    )                                           AS collected_last_7d
FROM channel_videos;
"""

PIPELINE_Q = """
SELECT
    cv.youtube_video_id,
    cv.title,
    cv.published_at,
    cv.collected_at,
    t.status  AS transcript_status,
    d.status  AS distill_status
FROM channel_videos cv
LEFT JOIN transcripts t    ON t.youtube_video_id = cv.youtube_video_id
LEFT JOIN distillations d  ON d.youtube_video_id = cv.youtube_video_id
ORDER BY cv.collected_at DESC
LIMIT 10;
"""

PIPELINE_SUMMARY_Q = """
SELECT
    COUNT(cv.id)                                         AS harvest_total,
    COUNT(t.id)                                          AS transcribed,
    COUNT(d.id)                                          AS distilled,
    COUNT(cv.id) FILTER (WHERE t.id IS NULL)             AS pending_transcript,
    COUNT(cv.id) FILTER (WHERE t.id IS NOT NULL
                           AND d.id IS NULL)             AS pending_distill,
    COUNT(t.id) FILTER (WHERE t.status = 'error')        AS transcript_errors,
    COUNT(d.id) FILTER (WHERE d.status = 'error')        AS distill_errors
FROM channel_videos cv
LEFT JOIN transcripts t    ON t.youtube_video_id = cv.youtube_video_id
LEFT JOIN distillations d  ON d.youtube_video_id = cv.youtube_video_id;
"""

# ── main ──────────────────────────────────────────────────────────────────────
def main():
    conn = psycopg2.connect(DATABASE_URL)
    cur  = conn.cursor(cursor_factory=RealDictCursor)

    # ── totais gerais ─────────────────────────────────────────────────────────
    cur.execute(TOTALS_Q)
    tot = cur.fetchone()

    print()
    print(SEP2)
    print("  rara-harvest — status NeonDB")
    print(SEP2)
    print(f"  Vídeos coletados    : {tot['total_videos']:>6}")
    print(f"  Canais com vídeos   : {tot['channels_with_videos']:>6}")
    print(f"  Último run          : {fmt_ts(tot['last_run'])}")
    print(f"  Vídeos (24 h)       : {tot['collected_last_24h']:>6}")
    print(f"  Vídeos (7 dias)     : {tot['collected_last_7d']:>6}")
    print(f"  Range publicação    : {fmt_ts(tot['oldest'])} → {fmt_ts(tot['newest'])}")
    print()

    # ── por canal ─────────────────────────────────────────────────────────────
    cur.execute(CHANNELS_Q)
    channels = cur.fetchall()
    total_videos = tot['total_videos'] or 1

    print(SEP)
    print("  Canais monitorados")
    print(SEP)
    for ch in channels:
        status = "✓ ativo" if ch['active'] else "✗ inativo"
        pct    = ch['video_count'] / total_videos * 100
        print(f"\n  [{status}] {ch['channel_name']}")
        print(f"           ID        : {ch['youtube_channel_id']}")
        print(f"           Vídeos    : {ch['video_count']:>5}  {bar(ch['video_count'], total_videos)} {pct:.1f}%")
        print(f"           Range     : {fmt_ts(ch['oldest_video'])} → {fmt_ts(ch['newest_video'])}")
        print(f"           Coletado  : {fmt_ts(ch['last_collected'])}")
    print()

    # ── funil harvest → scribe → distill ─────────────────────────────────────
    cur.execute(PIPELINE_SUMMARY_Q)
    ps = cur.fetchone()

    print(SEP)
    print("  Funil harvest → scribe → distill")
    print(SEP)
    h = ps['harvest_total'] or 1
    print(f"  Harvest total       : {ps['harvest_total']:>6}")
    print(f"  Transcritos         : {ps['transcribed']:>6}  {bar(ps['transcribed'], h)}  {ps['transcribed']/h*100:.1f}%")
    print(f"  Destilados          : {ps['distilled']:>6}  {bar(ps['distilled'], h)}  {ps['distilled']/h*100:.1f}%")
    print(f"  Aguard. transcrição : {ps['pending_transcript']:>6}")
    print(f"  Aguard. destilação  : {ps['pending_distill']:>6}")
    if ps['transcript_errors'] or ps['distill_errors']:
        print(f"  ⚠  Erros transcrição : {ps['transcript_errors']}")
        print(f"  ⚠  Erros destilação  : {ps['distill_errors']}")
    print()

    # ── últimos 10 vídeos coletados ───────────────────────────────────────────
    cur.execute(PIPELINE_Q)
    recent = cur.fetchall()

    print(SEP)
    print("  Últimos 10 vídeos coletados")
    print(SEP)
    for v in recent:
        t_sym = {"done": "✓", "error": "✗", None: "·"}.get(v['transcript_status'], "?")
        d_sym = {"done": "✓", "error": "✗", None: "·"}.get(v['distill_status'],    "?")
        title = (v['title'] or "")[:50].ljust(50)
        pub   = fmt_ts(v['published_at'])[:10]
        print(f"  T{t_sym} D{d_sym}  {pub}  {title}  {v['youtube_video_id']}")
    print()
    print("  T=transcrição  D=destilação  ✓=done  ✗=error  ·=pendente")
    print()

    cur.close()
    conn.close()

if __name__ == "__main__":
    main()
