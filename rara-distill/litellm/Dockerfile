FROM --platform=linux/amd64 ghcr.io/berriai/litellm:main-v1.89.3

COPY config.yaml /app/config.yaml

USER nonroot

CMD ["--config", "/app/config.yaml", "--port", "8080"]
