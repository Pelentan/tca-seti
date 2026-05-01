#!/usr/bin/env bash
# scripts/build-push.sh
#
# Builds Docker images and pushes them to the k3d local registry.
# All images are prefixed with "seti-" to avoid collisions with other
# constellations sharing the same registry.
# Run from the project root.
#
# Usage:
#   ./scripts/build-push.sh          # tag: dev
#   ./scripts/build-push.sh v1.2.3   # tag: v1.2.3

set -euo pipefail

REGISTRY="localhost:5000"
PREFIX="seti-"
TAG="${1:-dev}"

# All services. Format: "service-name:dockerfile-path:build-context"
# Images are pushed as seti-<service-name>:<tag>
SERVICES=(
  "cert-forge:cert-forge/Dockerfile:."
  "seti-observability:seti-observability/Dockerfile:."
  "lore:lore/Dockerfile:."
  "augur-canis:augur-canis/Dockerfile:."
  "signal-clearance:signal-clearance/Dockerfile:signal-clearance"
  "policy:policy/Dockerfile:."
  "contract-test:contract-test/Dockerfile:."
  "results:results/Dockerfile:."
  "signal-aggregator:signal-aggregator/Dockerfile:."
  "plot-store:plot-store/Dockerfile:."
  "plot-test:plot-test/Dockerfile:."
  "interactions:interactions/Dockerfile:."
  "feed-wrangler:feed-wrangler/Dockerfile:feed-wrangler"
  "integration:integration/Dockerfile:."
  "ai-lien:ai-lien/Dockerfile:."
  "notifier:notifier/Dockerfile:."
  "connie-agent:connie-agent/Dockerfile:."
  "ui:ui/Dockerfile:."
  "gateway:gateway/Dockerfile:."
)

echo "Building and pushing to ${REGISTRY} (prefix: ${PREFIX}, tag: ${TAG})"
echo ""

for entry in "${SERVICES[@]}"; do
  IFS=':' read -r name dockerfile context <<< "${entry}"
  image="${REGISTRY}/${PREFIX}${name}:${TAG}"

  echo "--- ${PREFIX}${name} ---"
  docker build -t "${image}" -f "${dockerfile}" "${context}"
  docker push "${image}"
  echo ""
done

echo "Done. Images available in k3d cluster."
echo ""
echo "To upgrade the running chart:"
echo "  helm upgrade seti ./charts/seti -n seti -f charts/seti/values/dev.yaml \\"
echo "    --set postgresCredentials.password=... \\"
echo "    --set jwtSecret=..."