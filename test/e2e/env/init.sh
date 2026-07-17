#!/bin/bash

set -e  # Exit on error

echo "=============================================="
echo "Starting E2E Test Environment Setup"
echo "=============================================="
echo ""

echo "[1] Creating namespace open-cluster-management-addon..."
kubectl create namespace open-cluster-management-addon --dry-run=client -o yaml | kubectl apply -f -
echo "✓ Namespace created"
echo ""

# Note: cert-manager is no longer required for user server certificates.
# The controller now automatically generates and rotates user server certificates
# when userServer.enabled=true is set in the helm values.

echo "[2] Initializing Open Cluster Management (OCM)..."
echo "Running: clusteradm init"
clusteradm init --output-join-command-file join.sh --wait
echo "✓ OCM hub initialized"
echo ""

echo "[3] Registering loopback managed cluster..."
echo "Running: clusteradm join"
sh -c "$(cat join.sh) loopback --force-internal-endpoint-lookup"
echo "Accepting cluster registration..."
clusteradm accept --clusters loopback --wait 30
echo "Waiting for managed cluster to be available..."
kubectl wait --for=condition=ManagedClusterConditionAvailable managedcluster/loopback
echo "✓ Loopback managed cluster registered and available"
echo ""

echo "[4] Deploying hello-world test services..."
kubectl apply -f test/e2e/env/hello-world.yaml
echo "Waiting for hello-world pod to be ready..."
kubectl wait --for=condition=ready pod/hello-world -n default --timeout=60s
echo "✓ hello-world HTTP test service deployed and ready"
echo ""

echo "[5] Deploying hello-world-https test service..."
kubectl apply -f test/e2e/env/hello-world-https.yaml
echo "Waiting for hello-world-https pod to be ready..."
kubectl wait --for=condition=ready pod/hello-world-https -n default --timeout=120s
echo "✓ hello-world-https HTTPS test service deployed and ready"
echo ""

echo "[6] Deploying Dex OIDC identity provider..."
kubectl create namespace dex --dry-run=client -o yaml | kubectl apply -f -
DEX_CERT_DIR=$(mktemp -d)
# Note: the CN/SANs must cover the issuer host in test/e2e/env/dex.yaml
openssl req -x509 -newkey rsa:2048 -nodes -days 30 \
  -keyout "${DEX_CERT_DIR}/tls.key" -out "${DEX_CERT_DIR}/tls.crt" \
  -subj "/CN=dex.dex.svc.cluster.local" \
  -addext "subjectAltName=DNS:dex.dex.svc.cluster.local,DNS:dex.dex.svc"
# the certificate is self-signed, so it doubles as the CA bundle (ca.crt)
kubectl -n dex create secret generic dex-tls \
  --from-file=tls.crt="${DEX_CERT_DIR}/tls.crt" \
  --from-file=tls.key="${DEX_CERT_DIR}/tls.key" \
  --from-file=ca.crt="${DEX_CERT_DIR}/tls.crt" \
  --dry-run=client -o yaml | kubectl apply -f -
rm -rf "${DEX_CERT_DIR}"
helm upgrade --install dex dex \
  --repo https://charts.dexidp.io \
  --version 0.21.1 \
  --namespace dex \
  --values test/e2e/env/dex.yaml \
  --wait \
  --timeout 180s
echo "✓ Dex OIDC identity provider deployed and ready"
echo ""

echo "=============================================="
echo "E2E Test Environment Setup Complete!"
echo "=============================================="
