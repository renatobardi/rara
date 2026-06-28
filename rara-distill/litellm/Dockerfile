FROM --platform=linux/amd64 ghcr.io/berriai/litellm:v1.90.0

USER root
# Prisma Python downloads a glibc Node.js binary at runtime (store_model_in_db),
# which fails on Wolfi musl. Pre-create the nodeenv pointing at the system
# musl-compatible Node.js so no download happens at container start.
RUN /app/.venv/bin/python -m nodeenv --node=system \
      /home/nonroot/.cache/prisma-python/nodeenv 2>/dev/null || true && \
    chown -R nonroot:nonroot /home/nonroot/.cache 2>/dev/null || true

USER nonroot

COPY config.yaml /app/config.yaml

CMD ["--config", "/app/config.yaml", "--port", "8080"]
