#!/usr/bin/env bash
# scripts/k3d-setup.sh
#
# Creates the SETI k3d development cluster with a local registry.
# Run once. Idempotent — safe to re-run if the cluster already exists.
#
# Prerequisites: k3d, kubectl, helm

set -euo pipefail

CLUSTER_NAME="seti"
REGISTRY_NAME="seti-registry"
REGISTRY_PORT="5000"
AGENTS=2

# Bail out cleanly if the cluster already exists.
if k3d cluster list | grep -q "^${CLUSTER_NAME} "; then
  echo "Cluster '${CLUSTER_NAME}' already exists. Nothing to do."
  echo "To rebuild: k3d cluster delete ${CLUSTER_NAME} && ./scripts/k3d-setup.sh"
  exit 0
fi

echo "Creating k3d cluster: ${CLUSTER_NAME}"
echo "  Agents:   ${AGENTS}"
echo "  Registry: ${REGISTRY_NAME}:${REGISTRY_PORT}"
echo ""

k3d cluster create "${CLUSTER_NAME}" \
  --registry-create "${REGISTRY_NAME}:0.0.0.0:${REGISTRY_PORT}" \
  --agents "${AGENTS}" \
  --k3s-arg "--disable=traefik@server:0" \
  -p "4000:30400@loadbalancer"

# Merge the new cluster into the active kubeconfig.
k3d kubeconfig merge "${CLUSTER_NAME}" --kubeconfig-merge-default

echo ""
echo "Cluster ready. kubectl context: k3d-${CLUSTER_NAME}"
echo "Registry:   localhost:${REGISTRY_PORT}"
echo ""
echo "Next steps:"
echo "  1. Build and push Phase 1 images:"
echo "       ./scripts/build-push.sh"
echo ""
echo "  2. Install the chart:"
echo "       helm install seti ./charts/seti -f charts/seti/values/dev.yaml --create-namespace"
echo ""
echo "  3. Watch startup:"
echo "       kubectl get pods -n seti -w"
