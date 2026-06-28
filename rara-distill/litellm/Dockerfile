# Official litellm image with Prisma + query engine baked in for DB-mode
# (store_model_in_db). Debian/glibc base, so no npm/musl gymnastics — the
# query engine binary just runs. prod_entrypoint.sh handles prisma setup;
# DISABLE_SCHEMA_UPDATE=True (set on the Cloud Run deploy) skips migrate deploy
# since migrations are already applied to the Neon `litellm` schema.
FROM --platform=linux/amd64 ghcr.io/berriai/litellm-database:v1.90.0

COPY config.yaml /app/config.yaml

CMD ["--config", "/app/config.yaml", "--port", "8080"]
