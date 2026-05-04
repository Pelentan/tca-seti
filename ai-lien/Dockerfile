# Build context: project root (.)
# Zero external Python dependencies — stdlib only.
FROM python:3.12-slim-bookworm
RUN apt-get update && apt-get install -y --no-install-recommends openssl && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY ai-lien/main.py .
COPY ai-lien/certforge.py .
COPY ai-lien/shutdown.py .
COPY results/healthcheck.py /healthcheck.py

EXPOSE 4252
CMD ["python3", "main.py"]
