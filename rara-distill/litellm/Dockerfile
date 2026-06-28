FROM --platform=linux/amd64 ghcr.io/berriai/litellm:v1.90.0

USER root
# Wolfi runtime image has nodejs but not npm; Prisma Python (store_model_in_db)
# needs npm to install the Prisma CLI. Install it so the system nodeenv has npm.
RUN apk add --no-cache npm

# Pre-install the Prisma CLI as nonroot so ensure_cached() is a no-op at startup.
# With npm now available, nodeenv uses system musl Node (not a downloaded glibc
# binary) and caches the Prisma CLI — nothing downloads at container start.
USER nonroot
RUN /app/.venv/bin/python -c \
      "from prisma.cli.prisma import ensure_cached; ensure_cached()" 2>&1 | tail -3

COPY config.yaml /app/config.yaml

CMD ["--config", "/app/config.yaml", "--port", "8080"]
