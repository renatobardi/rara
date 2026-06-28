FROM --platform=linux/amd64 ghcr.io/berriai/litellm:v1.90.0

COPY config.yaml /app/config.yaml

USER nonroot

CMD ["--config", "/app/config.yaml", "--port", "8080"]
