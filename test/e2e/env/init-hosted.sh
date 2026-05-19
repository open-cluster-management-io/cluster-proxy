#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
WORK_DIR="${WORK_DIR:-${ROOT_DIR}/_output/e2e-hosted}"
if [[ "${WORK_DIR}" != /* ]]; then
	WORK_DIR="${ROOT_DIR}/${WORK_DIR}"
fi
HUB_CLUSTER_NAME="${HUB_CLUSTER_NAME:-cluster-proxy-hosted-hub}"
HOSTING_CLUSTER_NAME="${HOSTING_CLUSTER_NAME:-cluster-proxy-hosted-hosting}"
MANAGED_CLUSTER_NAME="${MANAGED_CLUSTER_NAME:-cluster-proxy-hosted-managed}"
IMAGE_REGISTRY_NAME="${IMAGE_REGISTRY_NAME:-quay.io/open-cluster-management}"
IMAGE_NAME="${IMAGE_NAME:-cluster-proxy}"
IMAGE_TAG="${IMAGE_TAG:-latest}"

HUB_NAMESPACE="open-cluster-management-addon"
ADDON_NAMESPACE="open-cluster-management-cluster-proxy"
PLACEMENT_NAME="cluster-proxy-hosted-placement"
DEPLOY_CONFIG_NAME="hosted-relay"
EXTERNAL_KUBECONFIG_SECRET="external-managed-kubeconfig"
HTTPS_CA_CONFIGMAP="hello-world-https-ca"
PROXY_ENTRYPOINT_NODE_PORT="30091"
PROXY_SERVER_NODE_PORT="30090"

HUB_KUBECONFIG="${WORK_DIR}/hub.kubeconfig"
HOSTING_KUBECONFIG="${WORK_DIR}/hosting.kubeconfig"
MANAGED_KUBECONFIG="${WORK_DIR}/managed.kubeconfig"
HUB_CONTAINER_KUBECONFIG="${WORK_DIR}/hub-container.kubeconfig"
MANAGED_CONTAINER_KUBECONFIG="${WORK_DIR}/managed-container.kubeconfig"

log() {
	echo "[$(date '+%H:%M:%S')] $*"
}

require_cmd() {
	if ! command -v "$1" >/dev/null 2>&1; then
		echo "Required command '$1' not found" >&2
		exit 1
	fi
}

create_kind_config() {
	local cluster_name="$1"
	local config_file="$2"

	cat >"${config_file}" <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
  kubeadmConfigPatches:
  - |
    kind: ClusterConfiguration
    apiServer:
      certSANs:
      - 127.0.0.1
      - localhost
      - ${cluster_name}-control-plane
EOF
}

create_kind_cluster() {
	local cluster_name="$1"
	local config_file="${WORK_DIR}/${cluster_name}.kind.yaml"

	log "Creating kind cluster ${cluster_name}"
	create_kind_config "${cluster_name}" "${config_file}"
	kind create cluster --name "${cluster_name}" --config "${config_file}"
}

kubeconfig_server() {
	kubectl --kubeconfig "$1" config view --minify -o jsonpath='{.clusters[0].cluster.server}'
}

rewrite_kubeconfig_server() {
	local source_file="$1"
	local target_file="$2"
	local server="$3"
	local cluster

	cp "${source_file}" "${target_file}"
	cluster="$(kubectl --kubeconfig "${target_file}" config view --minify -o jsonpath='{.contexts[0].context.cluster}')"
	kubectl --kubeconfig "${target_file}" config set-cluster "${cluster}" --server="${server}" >/dev/null
}

rewrite_cluster_data_server() {
	local kubeconfig="$1"
	local old_server="$2"
	local new_server="$3"

	if [[ "${old_server}" == "${new_server}" ]]; then
		return
	fi

	log "Rewriting kubeconfig server ${old_server} -> ${new_server}"
	kubectl --kubeconfig "${kubeconfig}" get secrets -A -o json | \
		jq -r --arg old "${old_server}" --arg new "${new_server}" '
			.items[] as $item
			| (($item.data // {}) | to_entries[])
			| (try (.value | @base64d) catch "") as $decoded
			| select($decoded | contains($old))
			| [
				$item.metadata.namespace,
				$item.metadata.name,
				.key,
				($decoded | split($old) | join($new) | @base64)
			  ] | @tsv
		' | while IFS=$'\t' read -r namespace name key value; do
			local patch
			patch="$(jq -cn --arg key "${key}" --arg value "${value}" '[{
				"op": "replace",
				"path": ("/data/" + ($key | gsub("~"; "~0") | gsub("/"; "~1"))),
				"value": $value
			}]')"
			kubectl --kubeconfig "${kubeconfig}" -n "${namespace}" patch secret "${name}" --type=json -p "${patch}" >/dev/null
		done

	kubectl --kubeconfig "${kubeconfig}" get configmaps -A -o json | \
		jq -r --arg old "${old_server}" --arg new "${new_server}" '
			.items[] as $item
			| (($item.data // {}) | to_entries[])
			| select(.value | contains($old))
			| [
				$item.metadata.namespace,
				$item.metadata.name,
				.key,
				(.value | split($old) | join($new) | @base64)
			  ] | @tsv
		' | while IFS=$'\t' read -r namespace name key encoded_value; do
			local decoded_value patch
			decoded_value="$(printf '%s' "${encoded_value}" | base64 -d)"
			patch="$(jq -cn --arg key "${key}" --arg value "${decoded_value}" '[{
				"op": "replace",
				"path": ("/data/" + ($key | gsub("~"; "~0") | gsub("/"; "~1"))),
				"value": $value
			}]')"
			kubectl --kubeconfig "${kubeconfig}" -n "${namespace}" patch configmap "${name}" --type=json -p "${patch}" >/dev/null
		done
}

restart_ocm_deployments() {
	local kubeconfig="$1"

	kubectl --kubeconfig "${kubeconfig}" get deployments -A -o json | \
		jq -r '.items[]
			| select(.metadata.namespace | startswith("open-cluster-management"))
			| [.metadata.namespace, .metadata.name] | @tsv' | \
		while IFS=$'\t' read -r namespace name; do
			kubectl --kubeconfig "${kubeconfig}" -n "${namespace}" rollout restart "deployment/${name}" >/dev/null || true
		done
}

wait_managed_cluster_available() {
	local cluster_name="$1"

	log "Waiting for ManagedCluster ${cluster_name} to become available"
	kubectl --kubeconfig "${HUB_KUBECONFIG}" wait \
		--for=condition=ManagedClusterConditionAvailable \
		"managedcluster/${cluster_name}" \
		--timeout=600s
}

wait_deployment() {
	local kubeconfig="$1"
	local namespace="$2"
	local name="$3"
	local timeout="${4:-600s}"

	kubectl --kubeconfig "${kubeconfig}" -n "${namespace}" wait \
		--for=condition=available \
		"deployment/${name}" \
		--timeout="${timeout}"
}

wait_resource() {
	local kubeconfig="$1"
	local resource="$2"
	local timeout="${3:-300}"

	for _ in $(seq 1 "${timeout}"); do
		if kubectl --kubeconfig "${kubeconfig}" get ${resource} >/dev/null 2>&1; then
			return
		fi
		sleep 1
	done

	echo "Timed out waiting for ${resource}" >&2
	exit 1
}

wait_container_health() {
	local kubeconfig="$1"
	local namespace="$2"
	local workload="$3"
	local container="$4"
	local port="$5"
	local timeout="${6:-180}"

	log "Waiting for ${workload}/${container} healthz on port ${port}"
	for _ in $(seq 1 "${timeout}"); do
		if kubectl --kubeconfig "${kubeconfig}" -n "${namespace}" exec "${workload}" -c "${container}" -- \
			wget -qO- "http://127.0.0.1:${port}/healthz" 2>/dev/null | grep -q ok; then
			return
		fi
		sleep 1
	done

	kubectl --kubeconfig "${kubeconfig}" -n "${namespace}" logs "${workload}" -c "${container}" --tail=120 || true
	echo "Timed out waiting for ${workload}/${container} healthz" >&2
	exit 1
}

apply_hosted_addon_config() {
	log "Applying hosted AddOnDeploymentConfig and ManagedClusterAddOn"
	kubectl --kubeconfig "${HUB_KUBECONFIG}" apply -f - <<EOF
apiVersion: addon.open-cluster-management.io/v1alpha1
kind: AddOnDeploymentConfig
metadata:
  name: ${DEPLOY_CONFIG_NAME}
  namespace: ${MANAGED_CLUSTER_NAME}
spec:
  agentInstallNamespace: ${ADDON_NAMESPACE}
  customizedVariables:
  - name: hostedServiceProxyMode
    value: Relay
  - name: externalManagedKubeConfigSecretNamespace
    value: ${MANAGED_CLUSTER_NAME}
  - name: externalManagedKubeConfigSecretName
    value: ${EXTERNAL_KUBECONFIG_SECRET}
  - name: managedKubeConfigSyncInterval
    value: 10s
  - name: managedKubeConfigTokenExpiration
    value: 1h
  - name: managedKubeConfigRefreshBefore
    value: 5m
  - name: additionalServiceCAConfigMap
    value: ${HTTPS_CA_CONFIGMAP}
---
apiVersion: addon.open-cluster-management.io/v1alpha1
kind: ManagedClusterAddOn
metadata:
  name: cluster-proxy
  namespace: ${MANAGED_CLUSTER_NAME}
  annotations:
    addon.open-cluster-management.io/hosting-cluster-name: ${HOSTING_CLUSTER_NAME}
spec:
  installNamespace: ${ADDON_NAMESPACE}
  configs:
  - group: addon.open-cluster-management.io
    resource: addondeploymentconfigs
    namespace: ${MANAGED_CLUSTER_NAME}
    name: ${DEPLOY_CONFIG_NAME}
EOF
}

apply_placement() {
	log "Applying placement for hosted managed cluster"
	kubectl --kubeconfig "${HUB_KUBECONFIG}" create namespace "${HUB_NAMESPACE}" --dry-run=client -o yaml | \
		kubectl --kubeconfig "${HUB_KUBECONFIG}" apply -f -
	kubectl --kubeconfig "${HUB_KUBECONFIG}" label \
		"managedcluster/${MANAGED_CLUSTER_NAME}" \
		cluster-proxy-e2e=hosted-managed \
		--overwrite
	kubectl --kubeconfig "${HUB_KUBECONFIG}" apply -f - <<EOF
apiVersion: cluster.open-cluster-management.io/v1beta2
kind: ManagedClusterSetBinding
metadata:
  name: global
  namespace: ${HUB_NAMESPACE}
spec:
  clusterSet: global
---
apiVersion: cluster.open-cluster-management.io/v1beta1
kind: Placement
metadata:
  name: ${PLACEMENT_NAME}
  namespace: ${HUB_NAMESPACE}
spec:
  clusterSets:
  - global
  predicates:
  - requiredClusterSelector:
      labelSelector:
        matchLabels:
          cluster-proxy-e2e: hosted-managed
EOF
}

install_cluster_proxy() {
	local image="${IMAGE_REGISTRY_NAME}/${IMAGE_NAME}:${IMAGE_TAG}"

	log "Installing cluster-proxy chart with image ${image}"
	helm --kubeconfig "${HUB_KUBECONFIG}" upgrade --install \
		-n "${HUB_NAMESPACE}" --create-namespace \
		cluster-proxy "${ROOT_DIR}/charts/cluster-proxy" \
		--set "registry=${IMAGE_REGISTRY_NAME}" \
		--set "image=${IMAGE_NAME}" \
		--set "tag=${IMAGE_TAG}" \
		--set "proxyServerImage=${IMAGE_REGISTRY_NAME}/${IMAGE_NAME}" \
		--set "proxyAgentImage=${IMAGE_REGISTRY_NAME}/${IMAGE_NAME}" \
		--set "proxyServer.entrypointAddress=${HUB_CLUSTER_NAME}-control-plane" \
		--set "proxyServer.port=${PROXY_ENTRYPOINT_NODE_PORT}" \
		--set "proxyServer.imagePullPolicy=IfNotPresent" \
		--set "enableServiceProxy=true" \
		--set "enableKubeApiProxy=true" \
		--set "userServer.enabled=true" \
		--set "installByPlacement.placementName=${PLACEMENT_NAME}" \
		--set "installByPlacement.placementNamespace=${HUB_NAMESPACE}"

	wait_resource "${HUB_KUBECONFIG}" "service/proxy-entrypoint -n ${HUB_NAMESPACE}" 300
	log "Exposing proxy-entrypoint agent port through a hub NodePort"
	kubectl --kubeconfig "${HUB_KUBECONFIG}" -n "${HUB_NAMESPACE}" patch service proxy-entrypoint --type=json -p "[
		{\"op\":\"replace\",\"path\":\"/spec/type\",\"value\":\"NodePort\"},
		{\"op\":\"add\",\"path\":\"/spec/ports/0/nodePort\",\"value\":${PROXY_SERVER_NODE_PORT}},
		{\"op\":\"add\",\"path\":\"/spec/ports/1/nodePort\",\"value\":${PROXY_ENTRYPOINT_NODE_PORT}}
	]" >/dev/null
}

prepare_test_services() {
	log "Deploying test services on managed cluster"
	kubectl --kubeconfig "${MANAGED_KUBECONFIG}" apply -f "${ROOT_DIR}/test/e2e/env/hello-world.yaml"
	kubectl --kubeconfig "${MANAGED_KUBECONFIG}" apply -f "${ROOT_DIR}/test/e2e/env/hello-world-https.yaml"
	kubectl --kubeconfig "${MANAGED_KUBECONFIG}" wait --for=condition=ready pod/hello-world -n default --timeout=120s
	kubectl --kubeconfig "${MANAGED_KUBECONFIG}" wait --for=condition=ready pod/hello-world-https -n default --timeout=180s

	log "Deploying optional BestEffort test service on hosting cluster"
	kubectl --kubeconfig "${HOSTING_KUBECONFIG}" apply -f "${ROOT_DIR}/test/e2e/env/hello-world.yaml"
	kubectl --kubeconfig "${HOSTING_KUBECONFIG}" wait --for=condition=ready pod/hello-world -n default --timeout=120s

	log "Creating HTTPS service CA ConfigMaps"
	kubectl --kubeconfig "${MANAGED_KUBECONFIG}" exec -n default pod/hello-world-https -- \
		cat /certs/server.crt >"${WORK_DIR}/hello-world-https-ca.crt"
	for kubeconfig in "${MANAGED_KUBECONFIG}" "${HOSTING_KUBECONFIG}"; do
		kubectl --kubeconfig "${kubeconfig}" create namespace "${ADDON_NAMESPACE}" --dry-run=client -o yaml | \
			kubectl --kubeconfig "${kubeconfig}" apply -f -
		kubectl --kubeconfig "${kubeconfig}" -n "${ADDON_NAMESPACE}" create configmap "${HTTPS_CA_CONFIGMAP}" \
			--from-file=service-ca.crt="${WORK_DIR}/hello-world-https-ca.crt" \
			--dry-run=client -o yaml | kubectl --kubeconfig "${kubeconfig}" apply -f -
	done
}

prepare_external_managed_kubeconfig() {
	log "Creating external managed kubeconfig Secret on hosting cluster"
	kubectl --kubeconfig "${HOSTING_KUBECONFIG}" create namespace "${MANAGED_CLUSTER_NAME}" --dry-run=client -o yaml | \
		kubectl --kubeconfig "${HOSTING_KUBECONFIG}" apply -f -
	kubectl --kubeconfig "${HOSTING_KUBECONFIG}" -n "${MANAGED_CLUSTER_NAME}" create secret generic "${EXTERNAL_KUBECONFIG_SECRET}" \
		--from-file=kubeconfig="${MANAGED_CONTAINER_KUBECONFIG}" \
		--dry-run=client -o yaml | kubectl --kubeconfig "${HOSTING_KUBECONFIG}" apply -f -
}

wait_hosted_addon_ready() {
	log "Waiting for hosted cluster-proxy resources"
	wait_deployment "${HUB_KUBECONFIG}" "${HUB_NAMESPACE}" cluster-proxy-addon-manager 600s
	wait_deployment "${HUB_KUBECONFIG}" "${HUB_NAMESPACE}" cluster-proxy-addon-user 600s
	wait_deployment "${HUB_KUBECONFIG}" "${HUB_NAMESPACE}" cluster-proxy 600s

	wait_resource "${HOSTING_KUBECONFIG}" "deployment/cluster-proxy-managed-kubeconfig-provisioner -n ${ADDON_NAMESPACE}" 300
	wait_resource "${HOSTING_KUBECONFIG}" "deployment/cluster-proxy-proxy-agent -n ${ADDON_NAMESPACE}" 300
	wait_resource "${MANAGED_KUBECONFIG}" "deployment/cluster-proxy-service-relay -n ${ADDON_NAMESPACE}" 300

	wait_deployment "${HOSTING_KUBECONFIG}" "${ADDON_NAMESPACE}" cluster-proxy-managed-kubeconfig-provisioner 600s
	wait_deployment "${HOSTING_KUBECONFIG}" "${ADDON_NAMESPACE}" cluster-proxy-proxy-agent 600s
	wait_deployment "${MANAGED_KUBECONFIG}" "${ADDON_NAMESPACE}" cluster-proxy-service-relay 600s
	wait_container_health "${HOSTING_KUBECONFIG}" "${ADDON_NAMESPACE}" deployment/cluster-proxy-proxy-agent proxy-agent 8093
	wait_container_health "${HOSTING_KUBECONFIG}" "${ADDON_NAMESPACE}" deployment/cluster-proxy-proxy-agent service-proxy 8000
	wait_container_health "${HOSTING_KUBECONFIG}" "${ADDON_NAMESPACE}" deployment/cluster-proxy-proxy-agent managed-apiserver-proxy 8001
	wait_container_health "${MANAGED_KUBECONFIG}" "${ADDON_NAMESPACE}" deployment/cluster-proxy-service-relay service-relay 8000

	log "Waiting for generated managed kubeconfig and addon availability"
	wait_resource "${HOSTING_KUBECONFIG}" "secret/cluster-proxy-managed-kubeconfig -n ${ADDON_NAMESPACE}" 300
	for _ in $(seq 1 120); do
		if kubectl --kubeconfig "${HUB_KUBECONFIG}" -n "${MANAGED_CLUSTER_NAME}" get managedclusteraddon cluster-proxy \
			-o jsonpath='{.status.conditions[?(@.type=="Available")].status}' | grep -q True; then
			return
		fi
		sleep 5
	done

	kubectl --kubeconfig "${HUB_KUBECONFIG}" -n "${MANAGED_CLUSTER_NAME}" get managedclusteraddon cluster-proxy -o yaml
	echo "Timed out waiting for hosted ManagedClusterAddOn availability" >&2
	exit 1
}

write_env_file() {
	cat >"${WORK_DIR}/env" <<EOF
export E2E_HUB_KUBECONFIG="${HUB_KUBECONFIG}"
export E2E_HOSTING_KUBECONFIG="${HOSTING_KUBECONFIG}"
export E2E_MANAGED_KUBECONFIG="${MANAGED_KUBECONFIG}"
export MANAGED_CLUSTER_NAME="${MANAGED_CLUSTER_NAME}"
export E2E_HOSTED_HOSTING_CLUSTER_NAME="${HOSTING_CLUSTER_NAME}"
export E2E_HOSTED_DEPLOY_CONFIG_NAME="${DEPLOY_CONFIG_NAME}"
export E2E_HOSTED_EXTERNAL_KUBECONFIG_SECRET="${EXTERNAL_KUBECONFIG_SECRET}"
export E2E_HOSTED_HTTPS_CA_CONFIGMAP="${HTTPS_CA_CONFIGMAP}"
export E2E_HOSTED_ADDON_NAMESPACE="${ADDON_NAMESPACE}"
export E2E_HOSTED_PLACEMENT_LABEL_KEY="cluster-proxy-e2e"
export E2E_HOSTED_PLACEMENT_LABEL_VALUE="hosted-managed"
EOF
}

main() {
	require_cmd kind
	require_cmd kubectl
	require_cmd clusteradm
	require_cmd helm
	require_cmd jq

	mkdir -p "${WORK_DIR}"

	create_kind_cluster "${HUB_CLUSTER_NAME}"
	create_kind_cluster "${HOSTING_CLUSTER_NAME}"
	create_kind_cluster "${MANAGED_CLUSTER_NAME}"

	kind get kubeconfig --name "${HUB_CLUSTER_NAME}" >"${HUB_KUBECONFIG}"
	kind get kubeconfig --name "${HOSTING_CLUSTER_NAME}" >"${HOSTING_KUBECONFIG}"
	kind get kubeconfig --name "${MANAGED_CLUSTER_NAME}" >"${MANAGED_KUBECONFIG}"

	local hub_host_server managed_host_server hub_container_server managed_container_server
	hub_host_server="$(kubeconfig_server "${HUB_KUBECONFIG}")"
	managed_host_server="$(kubeconfig_server "${MANAGED_KUBECONFIG}")"
	hub_container_server="https://${HUB_CLUSTER_NAME}-control-plane:6443"
	managed_container_server="https://${MANAGED_CLUSTER_NAME}-control-plane:6443"
	rewrite_kubeconfig_server "${HUB_KUBECONFIG}" "${HUB_CONTAINER_KUBECONFIG}" "${hub_container_server}"
	rewrite_kubeconfig_server "${MANAGED_KUBECONFIG}" "${MANAGED_CONTAINER_KUBECONFIG}" "${managed_container_server}"

	local image="${IMAGE_REGISTRY_NAME}/${IMAGE_NAME}:${IMAGE_TAG}"
	for cluster in "${HUB_CLUSTER_NAME}" "${HOSTING_CLUSTER_NAME}" "${MANAGED_CLUSTER_NAME}"; do
		log "Loading ${image} into ${cluster}"
		kind load docker-image "${image}" --name "${cluster}"
	done

	log "Initializing OCM hub"
	KUBECONFIG="${HUB_KUBECONFIG}" clusteradm init \
		--output-join-command-file "${WORK_DIR}/join.sh" \
		--wait

	local join_cmd
	join_cmd="$(sed -e 's/ --wait//g' -e 's/ --cluster-name \$1/ --cluster-name/g' "${WORK_DIR}/join.sh")"

	log "Joining hosting cluster ${HOSTING_CLUSTER_NAME}"
	KUBECONFIG="${HOSTING_KUBECONFIG}" sh -c "${join_cmd} ${HOSTING_CLUSTER_NAME}"
	rewrite_cluster_data_server "${HOSTING_KUBECONFIG}" "${hub_host_server}" "${hub_container_server}"
	restart_ocm_deployments "${HOSTING_KUBECONFIG}"
	KUBECONFIG="${HUB_KUBECONFIG}" clusteradm accept --clusters "${HOSTING_CLUSTER_NAME}" --wait
	rewrite_cluster_data_server "${HOSTING_KUBECONFIG}" "${hub_host_server}" "${hub_container_server}"
	restart_ocm_deployments "${HOSTING_KUBECONFIG}"
	wait_managed_cluster_available "${HOSTING_CLUSTER_NAME}"

	log "Joining managed cluster ${MANAGED_CLUSTER_NAME} in hosted mode"
	KUBECONFIG="${HOSTING_KUBECONFIG}" sh -c "${join_cmd} ${MANAGED_CLUSTER_NAME} --mode hosted --managed-cluster-kubeconfig ${MANAGED_KUBECONFIG}"
	rewrite_cluster_data_server "${HOSTING_KUBECONFIG}" "${hub_host_server}" "${hub_container_server}"
	rewrite_cluster_data_server "${HOSTING_KUBECONFIG}" "${managed_host_server}" "${managed_container_server}"
	restart_ocm_deployments "${HOSTING_KUBECONFIG}"
	KUBECONFIG="${HUB_KUBECONFIG}" clusteradm accept --clusters "${MANAGED_CLUSTER_NAME}" --wait
	rewrite_cluster_data_server "${HOSTING_KUBECONFIG}" "${hub_host_server}" "${hub_container_server}"
	rewrite_cluster_data_server "${HOSTING_KUBECONFIG}" "${managed_host_server}" "${managed_container_server}"
	restart_ocm_deployments "${HOSTING_KUBECONFIG}"
	wait_managed_cluster_available "${MANAGED_CLUSTER_NAME}"

	prepare_test_services
	prepare_external_managed_kubeconfig
	apply_placement
	apply_hosted_addon_config
	install_cluster_proxy
	wait_hosted_addon_ready
	write_env_file

	log "Hosted E2E environment is ready"
	log "Environment file: ${WORK_DIR}/env"
}

main "$@"
