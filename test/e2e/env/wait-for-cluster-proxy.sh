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

condition_status() {
    local resource="$1"
    local name="$2"
    local namespace="$3"
    local condition="$4"
    local namespace_args=()

    if [ -n "$namespace" ]; then
        namespace_args=(-n "$namespace")
    fi

    kubectl get "$resource" "$name" "${namespace_args[@]}" \
        -o "jsonpath={.status.conditions[?(@.type==\"$condition\")].status}"
}

require_condition_true() {
    local resource="$1"
    local name="$2"
    local namespace="$3"
    local condition="$4"
    local status

    status="$(condition_status "$resource" "$name" "$namespace" "$condition" 2>/dev/null || true)"
    if [ "$status" != "True" ]; then
        if [ -n "$namespace" ]; then
            return_with_error "$resource/$name in namespace $namespace condition $condition is $status"
        else
            return_with_error "$resource/$name condition $condition is $status"
        fi
    fi
}

return_with_error() {
    echo "$1"
    return 1
}

require_deployment_ready() {
    local namespace="$1"
    local name="$2"
    local fields
    local -a deployment_fields
    local generation observed replicas ready updated available unavailable spec_replicas

    fields="$(kubectl get deployment "$name" -n "$namespace" -o jsonpath='{.metadata.generation}{"\n"}{.status.observedGeneration}{"\n"}{.status.replicas}{"\n"}{.status.readyReplicas}{"\n"}{.status.updatedReplicas}{"\n"}{.status.availableReplicas}{"\n"}{.status.unavailableReplicas}{"\n"}{.spec.replicas}{"\n"}' 2>/dev/null || true)"
    if [ -z "$fields" ]; then
        return_with_error "deployment/$name in namespace $namespace does not exist"
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

    if [ -z "$observed" ]; then observed=0; fi
    if [ -z "$replicas" ]; then replicas=0; fi
    if [ -z "$ready" ]; then ready=0; fi
    if [ -z "$updated" ]; then updated=0; fi
    if [ -z "$available" ]; then available=0; fi
    if [ -z "$unavailable" ]; then unavailable=0; fi
    if [ -z "$spec_replicas" ]; then spec_replicas=1; fi

    if [ "$observed" -lt "$generation" ]; then
        return_with_error "deployment/$name in namespace $namespace has not observed generation $generation"
    fi

    if [ "$replicas" -ne "$spec_replicas" ] ||
        [ "$ready" -ne "$spec_replicas" ] ||
        [ "$updated" -ne "$spec_replicas" ] ||
        [ "$available" -ne "$spec_replicas" ] ||
        [ "$unavailable" -ne 0 ]; then
        return_with_error "deployment/$name in namespace $namespace is not ready: replicas=$replicas ready=$ready updated=$updated available=$available unavailable=$unavailable expected=$spec_replicas"
    fi
}

require_service() {
    local namespace="$1"
    local name="$2"

    kubectl get service "$name" -n "$namespace" >/dev/null 2>&1 ||
        return_with_error "service/$name in namespace $namespace does not exist"
}

require_cluster_proxy_configuration() {
    local condition

    kubectl get managedproxyconfiguration cluster-proxy >/dev/null 2>&1 ||
        return_with_error "managedproxyconfiguration/cluster-proxy does not exist"

    for condition in ProxyServerDeployed ProxyServerSecretSigned AgentServerSecretSigned UserServerSecretSigned; do
        require_condition_true managedproxyconfiguration cluster-proxy "" "$condition"
    done
}

require_placement_decision() {
    local clusters

    kubectl get placement "$PLACEMENT_NAME" -n "$HUB_NAMESPACE" >/dev/null 2>&1 ||
        return_with_error "placement/$PLACEMENT_NAME in namespace $HUB_NAMESPACE does not exist"

    clusters="$(kubectl get placementdecision -n "$HUB_NAMESPACE" \
        -l "cluster.open-cluster-management.io/placement=$PLACEMENT_NAME" \
        -o 'jsonpath={range .items[*].status.decisions[*]}{.clusterName}{"\n"}{end}' 2>/dev/null || true)"
    if [ -z "$clusters" ]; then
        clusters="$(kubectl get placementdecision -n "$HUB_NAMESPACE" \
            -o 'jsonpath={range .items[*].status.decisions[*]}{.clusterName}{"\n"}{end}' 2>/dev/null || true)"
    fi

    echo "$clusters" | grep -Fxq "$MANAGED_CLUSTER_NAME" ||
        return_with_error "placement/$PLACEMENT_NAME has not selected managed cluster $MANAGED_CLUSTER_NAME"
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
        { kubectl get clustermanagementaddon cluster-proxy >/dev/null 2>&1 ||
            return_with_error "clustermanagementaddon/cluster-proxy does not exist"; } &&
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
