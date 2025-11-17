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

echo "[2] Installing cert-manager..."
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.17.0/cert-manager.yaml
echo "Waiting for cert-manager to be ready..."
kubectl wait --for=condition=ready pod -l app.kubernetes.io/instance=cert-manager -n cert-manager --timeout=300s
echo "✓ cert-manager installed and ready"
echo ""

echo "[3] Installing cert-issuer..."
kubectl apply -f test/e2e/env/cert-issuer.yaml
echo "✓ cert-issuer installed"
echo ""

echo "[4] Initializing Open Cluster Management (OCM)..."
echo "Running: clusteradm init"
clusteradm init --output-join-command-file join.sh --wait
echo "✓ OCM hub initialized"
echo ""

echo "[5] Registering loopback managed cluster..."
echo "Running: clusteradm join"
sh -c "$(cat join.sh) loopback --force-internal-endpoint-lookup"
echo "Accepting cluster registration..."
clusteradm accept --clusters loopback --wait 30
echo "Waiting for managed cluster to be available..."
kubectl wait --for=condition=ManagedClusterConditionAvailable managedcluster/loopback
echo "✓ Loopback managed cluster registered and available"
echo ""

echo "[6] Deploying hello-world test service..."
kubectl apply -f test/e2e/env/hello-world.yaml
echo "Waiting for hello-world pod to be ready..."
kubectl wait --for=condition=ready pod/hello-world -n default --timeout=60s
echo "✓ hello-world test service deployed and ready"
echo ""

echo "=============================================="
echo "E2E Test Environment Setup Complete!"
echo "=============================================="
