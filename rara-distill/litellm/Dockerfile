FROM ghcr.io/berriai/litellm:main-latest

COPY config.yaml /app/config.yaml

USER nonroot

CMD exec litellm --config /app/config.yaml --port "${PORT:-8080}"
