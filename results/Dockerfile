# Build context: project root (.)
# Zero external Python dependencies — stdlib only.
FROM python:3.12-slim-bookworm
RUN apt-get update && apt-get install -y --no-install-recommends openssl && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY results/main.py .
COPY results/certforge.py .
COPY results/shutdown.py .
COPY results/healthcheck.py /healthcheck.py

EXPOSE 4008
CMD ["python3", "main.py"]
