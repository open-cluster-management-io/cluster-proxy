#!/usr/bin/env bash

# wait-for-cluster-proxy.sh - Wait for the e2e cluster-proxy installation to converge.
# Usage: wait-for-cluster-proxy.sh [timeout-seconds]

set -euo pipefail

TIMEOUT="${1:-600}"
INTERVAL=5
MANAGED_CLUSTER_NAME="${MANAGED_CLUSTER_NAME:-loopback}"
HUB_NAMESPACE="${HUB_NAMESPACE:-open-cluster-management-addon}"
AGENT_NAMESPACE="${AGENT_NAMESPACE:-open-cluster-management-cluster-proxy}"
PLACEMENT_NAME="${PLACEMENT_NAME:-cluster-proxy-placement}"

# Guards below echo the error and `return 1` inline on purpose: the require_*
# functions only run inside an `if` condition (set -e suspended), where a
# `return` in a shared error helper would exit the helper alone and let the
# caller fall through past the guard.
require_condition_true() {
    local resource="$1"
    local name="$2"
    local namespace="$3"
    local condition="$4"
    local namespace_args=()
    local status

    if [ -n "$namespace" ]; then
        namespace_args=(-n "$namespace")
    fi

    status="$(kubectl get "$resource" "$name" "${namespace_args[@]}" \
        -o "jsonpath={.status.conditions[?(@.type==\"$condition\")].status}" 2>/dev/null || true)"
    if [ "$status" != "True" ]; then
        if [ -n "$namespace" ]; then
            echo "$resource/$name in namespace $namespace condition $condition is $status"
        else
            echo "$resource/$name condition $condition is $status"
        fi
        return 1
    fi
}

require_deployment_ready() {
    local namespace="$1"
    local name="$2"
    local fields
    local -a deployment_fields
    local generation observed replicas ready updated available unavailable spec_replicas

    fields="$(kubectl get deployment "$name" -n "$namespace" -o jsonpath='{.metadata.generation}{"\n"}{.status.observedGeneration}{"\n"}{.status.replicas}{"\n"}{.status.readyReplicas}{"\n"}{.status.updatedReplicas}{"\n"}{.status.availableReplicas}{"\n"}{.status.unavailableReplicas}{"\n"}{.spec.replicas}{"\n"}' 2>/dev/null || true)"
    if [ -z "$fields" ]; then
        echo "deployment/$name in namespace $namespace does not exist"
        return 1
    fi

    mapfile -t deployment_fields <<<"$fields"
    generation="${deployment_fields[0]:-0}"
    observed="${deployment_fields[1]:-0}"
    replicas="${deployment_fields[2]:-0}"
    ready="${deployment_fields[3]:-0}"
    updated="${deployment_fields[4]:-0}"
    available="${deployment_fields[5]:-0}"
    unavailable="${deployment_fields[6]:-0}"
    spec_replicas="${deployment_fields[7]:-1}"

    if [ "$observed" -lt "$generation" ]; then
        echo "deployment/$name in namespace $namespace has not observed generation $generation"
        return 1
    fi

    if [ "$replicas" -ne "$spec_replicas" ] ||
        [ "$ready" -ne "$spec_replicas" ] ||
        [ "$updated" -ne "$spec_replicas" ] ||
        [ "$available" -ne "$spec_replicas" ] ||
        [ "$unavailable" -ne 0 ]; then
        echo "deployment/$name in namespace $namespace is not ready: replicas=$replicas ready=$ready updated=$updated available=$available unavailable=$unavailable expected=$spec_replicas"
        return 1
    fi
}

require_service() {
    local namespace="$1"
    local name="$2"

    if ! kubectl get service "$name" -n "$namespace" >/dev/null 2>&1; then
        echo "service/$name in namespace $namespace does not exist"
        return 1
    fi
}

require_clustermanagementaddon() {
    if ! kubectl get clustermanagementaddon cluster-proxy >/dev/null 2>&1; then
        echo "clustermanagementaddon/cluster-proxy does not exist"
        return 1
    fi
}

require_cluster_proxy_configuration() {
    local condition

    if ! kubectl get managedproxyconfiguration cluster-proxy >/dev/null 2>&1; then
        echo "managedproxyconfiguration/cluster-proxy does not exist"
        return 1
    fi

    for condition in ProxyServerDeployed ProxyServerSecretSigned AgentServerSecretSigned UserServerSecretSigned; do
        require_condition_true managedproxyconfiguration cluster-proxy "" "$condition" || return 1
    done
}

require_placement_decision() {
    local clusters

    if ! kubectl get placement "$PLACEMENT_NAME" -n "$HUB_NAMESPACE" >/dev/null 2>&1; then
        echo "placement/$PLACEMENT_NAME in namespace $HUB_NAMESPACE does not exist"
        return 1
    fi

    clusters="$(kubectl get placementdecision -n "$HUB_NAMESPACE" \
        -l "cluster.open-cluster-management.io/placement=$PLACEMENT_NAME" \
        -o 'jsonpath={range .items[*].status.decisions[*]}{.clusterName}{"\n"}{end}' 2>/dev/null || true)"

    if ! echo "$clusters" | grep -Fxq "$MANAGED_CLUSTER_NAME"; then
        echo "placement/$PLACEMENT_NAME has not selected managed cluster $MANAGED_CLUSTER_NAME"
        return 1
    fi
}

require_cluster_proxy_ready() {
    # Short-circuit on the first failing check: this function runs inside an
    # `if` (set -e suspended), so without &&-chaining every check would run on
    # each poll and the function would report the last check's status instead
    # of failing fast.
    require_deployment_ready "$HUB_NAMESPACE" cluster-proxy-addon-manager &&
        require_deployment_ready "$HUB_NAMESPACE" cluster-proxy-addon-user &&
        require_deployment_ready "$HUB_NAMESPACE" cluster-proxy &&
        require_service "$HUB_NAMESPACE" cluster-proxy-addon-user &&
        require_condition_true managedcluster "$MANAGED_CLUSTER_NAME" "" ManagedClusterConditionAvailable &&
        require_clustermanagementaddon &&
        require_placement_decision &&
        require_cluster_proxy_configuration &&
        require_condition_true managedclusteraddon cluster-proxy "$MANAGED_CLUSTER_NAME" Available &&
        require_deployment_ready "$AGENT_NAMESPACE" cluster-proxy-proxy-agent
}

echo "Waiting for cluster-proxy e2e installation to be ready..."
echo "Managed cluster: $MANAGED_CLUSTER_NAME"
echo "Hub namespace: $HUB_NAMESPACE"
echo "Agent namespace: $AGENT_NAMESPACE"
echo "Timeout: ${TIMEOUT}s"
echo ""

start_time="$(date +%s)"
deadline=$((start_time + TIMEOUT))
output=""
iteration=0
while [ "$(date +%s)" -lt "$deadline" ]; do
    iteration=$((iteration + 1))
    if output="$(require_cluster_proxy_ready 2>&1)"; then
        echo "cluster-proxy e2e installation is ready"
        exit 0
    fi

    if [ $((iteration % 6)) -eq 0 ]; then
        elapsed=$(($(date +%s) - start_time))
        echo "[$(date '+%H:%M:%S')] Still waiting for cluster-proxy readiness (${elapsed}s elapsed)"
        echo "$output"
    fi

    sleep "$INTERVAL"
done

echo ""
echo "cluster-proxy e2e installation was not ready after ${TIMEOUT}s"
if [ -n "$output" ]; then
    echo "Last readiness error:"
    echo "$output"
fi
exit 1
