#!/usr/bin/env bash

set -euo pipefail

HELM="${HELM:-helm}"

if ! command -v "${HELM}" >/dev/null 2>&1; then
	echo "Helm executable not found: ${HELM}" >&2
	exit 1
fi

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

sync_chart_dependencies() {
	local chart_dir="$1"
	local output
	local dependency_names=()

	output="$("${HELM}" dependency list "${chart_dir}" 2>&1)"

	while IFS= read -r line; do
		[[ -z "${line}" ]] && continue
		[[ "${line}" == NAME* ]] && continue
		[[ "${line}" == WARNING:* ]] && continue

		local name=""
		local version=""
		local repository=""
		local status=""
		read -r name version repository status <<<"${line}"

		[[ -z "${name}" || -z "${repository}" ]] && continue
		dependency_names+=("${name}")

		if [[ "${repository}" == file://* ]]; then
			local source_dir
			source_dir="$(resolve_file_repository "${chart_dir}" "${repository}")"
			if [[ ! -d "${source_dir}" ]]; then
				echo "${chart_dir}: local dependency source does not exist: ${repository}" >&2
				return 1
			fi
		fi
	done <<<"${output}"

	if [[ "${#dependency_names[@]}" -eq 0 ]]; then
		return 0
	fi

	for name in "${dependency_names[@]}"; do
		rm -rf "${chart_dir}/charts/${name}"
	done

	echo "Building Helm dependencies for ${chart_dir}"
	"${HELM}" dependency build --skip-refresh "${chart_dir}"

	for name in "${dependency_names[@]}"; do
		rm -rf "${chart_dir}/charts/${name}"
	done
}

mapfile -t chart_dirs < <(
	find . -name Chart.yaml -not -path './.git/*' -print |
		sed 's#^\./##; s#/Chart.yaml$##' |
		sort
)

for chart_dir in "${chart_dirs[@]}"; do
	[[ -d "${chart_dir}" ]] || continue
	sync_chart_dependencies "${chart_dir}"
done
