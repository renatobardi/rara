FROM ghcr.io/berriai/litellm:main-latest

COPY config.yaml /app/config.yaml

CMD ["--config", "/app/config.yaml", "--port", "8080", "--detailed_debug"]
