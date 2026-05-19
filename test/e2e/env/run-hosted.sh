#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
WORK_DIR="${WORK_DIR:-${ROOT_DIR}/_output/e2e-hosted}"
if [[ "${WORK_DIR}" != /* ]]; then
	WORK_DIR="${ROOT_DIR}/${WORK_DIR}"
fi
ENV_FILE="${WORK_DIR}/env"
PROXY_ENTRYPOINT_LOCAL_PORT="${PROXY_ENTRYPOINT_LOCAL_PORT:-18090}"
USER_SERVER_LOCAL_PORT="${USER_SERVER_LOCAL_PORT:-19092}"
HOSTED_LABEL_FILTER="${HOSTED_LABEL_FILTER:-hosted}"
HUB_NAMESPACE="open-cluster-management-addon"

if [[ ! -f "${ENV_FILE}" ]]; then
	echo "Hosted E2E env file not found: ${ENV_FILE}" >&2
	echo "Run make setup-env-for-e2e-hosted first." >&2
	exit 1
fi

# shellcheck disable=SC1090
source "${ENV_FILE}"
for kubeconfig_env in E2E_HUB_KUBECONFIG E2E_HOSTING_KUBECONFIG E2E_MANAGED_KUBECONFIG; do
	kubeconfig_path="${!kubeconfig_env}"
	if [[ "${kubeconfig_path}" != /* ]]; then
		export "${kubeconfig_env}=${ROOT_DIR}/${kubeconfig_path}"
	fi
done

PIDS=()

cleanup() {
	for pid in "${PIDS[@]}"; do
		if kill -0 "${pid}" >/dev/null 2>&1; then
			kill "${pid}" >/dev/null 2>&1 || true
			wait "${pid}" >/dev/null 2>&1 || true
		fi
	done
}
trap cleanup EXIT

wait_for_port() {
	local port="$1"
	for _ in $(seq 1 120); do
		if (echo >/dev/tcp/127.0.0.1/"${port}") >/dev/null 2>&1; then
			return
		fi
		sleep 1
	done

	echo "Timed out waiting for localhost:${port}" >&2
	exit 1
}

start_port_forward() {
	local name="$1"
	local local_port="$2"
	local remote_port="$3"
	local log_file="${WORK_DIR}/${name}.port-forward.log"

	echo "Starting port-forward ${name}: 127.0.0.1:${local_port} -> ${remote_port}"
	kubectl --kubeconfig "${E2E_HUB_KUBECONFIG}" -n "${HUB_NAMESPACE}" port-forward \
		--address 127.0.0.1 \
		"svc/${name}" \
		"${local_port}:${remote_port}" >"${log_file}" 2>&1 &
	PIDS+=("$!")
	wait_for_port "${local_port}"
}

start_port_forward proxy-entrypoint "${PROXY_ENTRYPOINT_LOCAL_PORT}" 8090
start_port_forward cluster-proxy-addon-user "${USER_SERVER_LOCAL_PORT}" 9092

export PROXY_ENTRYPOINT_ADDRESS="127.0.0.1:${PROXY_ENTRYPOINT_LOCAL_PORT}"
export CLUSTER_PROXY_USER_SERVER_ADDRESS="127.0.0.1:${USER_SERVER_LOCAL_PORT}"

cd "${ROOT_DIR}"
go test ./test/e2e -count=1 -v -ginkgo.v -ginkgo.label-filter="${HOSTED_LABEL_FILTER}"
