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

echo "[4] Deploying hello-world test service..."
kubectl apply -f test/e2e/env/hello-world.yaml
echo "Waiting for hello-world pod to be ready..."
kubectl wait --for=condition=ready pod/hello-world -n default --timeout=60s
echo "✓ hello-world test service deployed and ready"
echo ""

echo "=============================================="
echo "E2E Test Environment Setup Complete!"
echo "=============================================="
