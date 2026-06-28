# Builder: installs npm (not in Wolfi runtime) and pre-caches the Prisma CLI.
# Only the cache is copied to the final image — npm never ships to production.
FROM --platform=linux/amd64 ghcr.io/berriai/litellm:v1.90.0 AS prisma-builder

USER root
RUN apk add --no-cache npm=11.17.0

USER nonroot
RUN set -e && \
    /app/.venv/bin/python -c \
      "from prisma.cli.prisma import ensure_cached; ensure_cached()"

# ---- runtime image ----
FROM --platform=linux/amd64 ghcr.io/berriai/litellm:v1.90.0

# Copy only the pre-built Prisma CLI cache; no npm in the final image.
COPY --from=prisma-builder /home/nonroot/.cache/prisma-python/ \
                            /home/nonroot/.cache/prisma-python/

USER nonroot

COPY config.yaml /app/config.yaml

CMD ["--config", "/app/config.yaml", "--port", "8080"]
