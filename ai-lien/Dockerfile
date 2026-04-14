# Build context: project root (.)
FROM python:3.12-slim-bookworm AS builder
WORKDIR /app
COPY ai-lien/requirements.txt .
RUN pip install --no-cache-dir --target=/app/deps -r requirements.txt
FROM python:3.12-slim-bookworm
RUN apt-get update && apt-get install -y --no-install-recommends openssl && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=builder /app/deps /app/deps
COPY ai-lien/main.py .
COPY ai-lien/certforge.py .
COPY results/healthcheck.py /healthcheck.py
ENV PYTHONPATH=/app/deps
EXPOSE 4252
CMD ["python3", "main.py"]
