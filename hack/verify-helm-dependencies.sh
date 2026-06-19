#!/usr/bin/env bash

set -euo pipefail

HELM="${HELM:-helm}"

if ! command -v "${HELM}" >/dev/null 2>&1; then
	echo "Helm executable not found: ${HELM}" >&2
	exit 1
fi

failed=0
temp_dirs=()
sync_helm_dependencies_hint_printed=0

cleanup() {
	for dir in "${temp_dirs[@]}"; do
		rm -rf "${dir}"
	done
}
trap cleanup EXIT

print_sync_helm_dependencies_hint() {
	if [[ "${sync_helm_dependencies_hint_printed}" -eq 1 ]]; then
		return
	fi

	echo "Run 'make sync-helm-dependencies' and commit the regenerated Chart.lock and charts/*.tgz files." >&2
	sync_helm_dependencies_hint_printed=1
}

resolve_file_repository() {
	local chart_dir="$1"
	local repository="$2"
	local source_ref="${repository#file://}"

	if [[ "${source_ref}" = /* ]]; then
		realpath -m "${source_ref}"
	else
		realpath -m "${chart_dir}/${source_ref}"
	fi
}

is_tracked() {
	local path="$1"

	! git rev-parse --is-inside-work-tree >/dev/null 2>&1 ||
		git ls-files --error-unmatch "${path}" >/dev/null 2>&1
}

verify_file_dependency() {
	local chart_dir="$1"
	local name="$2"
	local version="$3"
	local repository="$4"
	local archive="${chart_dir}/charts/${name}-${version}.tgz"
	local source_dir

	source_dir="$(resolve_file_repository "${chart_dir}" "${repository}")"

	if [[ ! -d "${source_dir}" ]]; then
		echo "${chart_dir}: local dependency source does not exist: ${repository}" >&2
		return 1
	fi

	if [[ ! -f "${archive}" ]]; then
		echo "${chart_dir}: dependency archive is missing: ${archive}" >&2
		return 1
	fi

	if ! is_tracked "${archive}"; then
		echo "${chart_dir}: dependency archive is not tracked by git: ${archive}" >&2
		return 1
	fi

	local extract_dir
	extract_dir="$(mktemp -d)"
	temp_dirs+=("${extract_dir}")
	tar -xzf "${archive}" -C "${extract_dir}"

	if [[ ! -d "${extract_dir}/${name}" ]]; then
		echo "${chart_dir}: dependency archive does not contain ${name}/" >&2
		return 1
	fi

	if ! diff -u <("${HELM}" show chart "${source_dir}") <("${HELM}" show chart "${archive}"); then
		echo "${chart_dir}: dependency archive ${archive} has stale Chart.yaml content from ${repository}" >&2
		return 1
	fi

	if ! diff -ru -x Chart.yaml "${source_dir}" "${extract_dir}/${name}"; then
		echo "${chart_dir}: dependency archive ${archive} is out of sync with ${repository}" >&2
		return 1
	fi
}

verify_dependency_list() {
	local chart_dir="$1"
	local output
	local chart_failed=0
	local has_dependencies=0

	if ! output="$("${HELM}" dependency list "${chart_dir}" 2>&1)"; then
		echo "${chart_dir}: failed to list Helm dependencies" >&2
		echo "${output}" >&2
		return 1
	fi

	while IFS= read -r line; do
		[[ -z "${line}" ]] && continue
		[[ "${line}" == NAME* ]] && continue
		[[ "${line}" == WARNING:* ]] && continue

		local name=""
		local version=""
		local repository=""
		local status=""
		read -r name version repository status <<<"${line}"

		[[ -z "${name}" || -z "${status}" ]] && continue
		has_dependencies=1

		case "${status}" in
		ok)
			;;
		*)
			echo "${chart_dir}: dependency ${name} is ${status}" >&2
			echo "${output}" >&2
			chart_failed=1
			continue
			;;
		esac

		if [[ -d "${chart_dir}/charts/${name}" ]]; then
			echo "${chart_dir}: dependency ${name} is committed as an unpacked chart; commit helm dependency build output instead" >&2
			chart_failed=1
		fi

		if [[ "${repository}" == file://* ]]; then
			if ! verify_file_dependency "${chart_dir}" "${name}" "${version}" "${repository}"; then
				chart_failed=1
			fi
		fi
	done <<<"${output}"

	if [[ "${has_dependencies}" -eq 1 ]]; then
		local lock_file="${chart_dir}/Chart.lock"
		if [[ ! -f "${lock_file}" ]]; then
			echo "${chart_dir}: Chart.lock is missing" >&2
			chart_failed=1
		elif ! is_tracked "${lock_file}"; then
			echo "${chart_dir}: Chart.lock is not tracked by git: ${lock_file}" >&2
			chart_failed=1
		fi
	fi

	if [[ "${chart_failed}" -eq 1 ]]; then
		print_sync_helm_dependencies_hint
	fi

	return "${chart_failed}"
}

mapfile -t chart_dirs < <(
	find . -name Chart.yaml -not -path './.git/*' -print |
		sed 's#^\./##; s#/Chart.yaml$##' |
		sort
)

for chart_dir in "${chart_dirs[@]}"; do
	echo "Verifying Helm chart: ${chart_dir}"

	if ! verify_dependency_list "${chart_dir}"; then
		failed=1
	fi

	if ! "${HELM}" lint "${chart_dir}"; then
		failed=1
	fi

	chart_type="$("${HELM}" show chart "${chart_dir}" | awk -F': *' '$1 == "type" { print $2; exit }')"
	if [[ "${chart_type}" != "library" ]]; then
		if ! "${HELM}" template "verify-$(basename "${chart_dir}")" "${chart_dir}" --namespace default >/dev/null; then
			echo "${chart_dir}: Helm template rendering failed" >&2
			failed=1
		fi
	fi
done

exit "${failed}"
