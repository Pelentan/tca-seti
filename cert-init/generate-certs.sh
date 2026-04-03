#!/bin/bash
set -e

CERTS_DIR="/certs"
echo "[cert-init] Starting certificate generation..."

# Backdate service certs 5 minutes to absorb container clock skew.
# The CA uses the standard req -x509 path (no date override needed —
# it's generated first and its NotBefore will predate all service certs).
PAST=$(date -u -d "5 minutes ago" +%Y%m%d%H%M%SZ 2>/dev/null \
  || python3 -c "import datetime; print((datetime.datetime.utcnow()-datetime.timedelta(minutes=5)).strftime('%Y%m%d%H%M%SZ'))")

echo "[cert-init] Service cert NotBefore: ${PAST}"

# CA — standard self-signed, no date override required
openssl genrsa -out "${CERTS_DIR}/ca.key" 4096 2>/dev/null
openssl req -new -x509 -days 3650 \
  -key "${CERTS_DIR}/ca.key" \
  -out "${CERTS_DIR}/ca.crt" \
  -subj "/CN=seti-ca/O=SETI/C=US" 2>/dev/null
echo "[cert-init] CA generated."

gen_cert() {
  SERVICE=$1
  openssl genrsa -out "${CERTS_DIR}/${SERVICE}.key" 2048 2>/dev/null
  openssl req -new \
    -key "${CERTS_DIR}/${SERVICE}.key" \
    -out "${CERTS_DIR}/${SERVICE}.csr" \
    -subj "/CN=${SERVICE}/O=SETI/C=US" 2>/dev/null
  openssl x509 -req -days 3650 \
    -in "${CERTS_DIR}/${SERVICE}.csr" \
    -CA "${CERTS_DIR}/ca.crt" \
    -CAkey "${CERTS_DIR}/ca.key" \
    -CAcreateserial \
    -out "${CERTS_DIR}/${SERVICE}.crt" \
    -startdate "${PAST}" \
    -extfile <(printf "subjectAltName=DNS:%s,DNS:localhost" "${SERVICE}") \
    2>/dev/null
  rm "${CERTS_DIR}/${SERVICE}.csr"
  echo "[cert-init] ${SERVICE} certificate generated."
}

# Phase 1
gen_cert "gateway"
gen_cert "seti-observability"
gen_cert "signal-clearance"
gen_cert "ui"
gen_cert "augur-canis"

# Phase 2
gen_cert "policy"
gen_cert "contract-test"
gen_cert "results"

# Phase 3
gen_cert "signal-aggregator"
gen_cert "plot-store"
gen_cert "plot-test"
gen_cert "interactions"
gen_cert "feed-wrangler"

gen_cert "integration"
gen_cert "ai-lien"

# Federation identity
gen_cert "star-gazer"

chmod 644 "${CERTS_DIR}"/*.crt "${CERTS_DIR}"/*.key "${CERTS_DIR}/ca.crt"

echo "[cert-init] All certificates generated successfully."
ls -la "${CERTS_DIR}"
echo "[cert-init] Exiting cleanly."
