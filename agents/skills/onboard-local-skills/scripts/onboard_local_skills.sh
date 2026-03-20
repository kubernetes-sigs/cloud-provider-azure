#!/usr/bin/env bash
# Copyright 2026 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
SKILL_DIR="$(cd "${SCRIPT_DIR}/.." && pwd -P)"
SKILLS_DIR="$(cd "${SKILL_DIR}/.." && pwd -P)"

TARGET_DIR=""
DRY_RUN=false
FORCE=false
LIST_ONLY=false
LINK_ALL=false
declare -a REQUESTED_SKILLS=()

usage() {
	cat <<'EOF'
Usage:
  onboard_local_skills.sh --list
  onboard_local_skills.sh --target <dir> --all [--dry-run] [--force]
  onboard_local_skills.sh --target <dir> --skill <name> [--skill <name> ...] [--dry-run] [--force]

Options:
  --target <dir>   Local agent skills directory to populate with symlinks
  --all            Link all shared skills found under agents/skills/
  --skill <name>   Link a specific shared skill. Repeat for multiple skills
  --list           Print available shared skills and exit
  --dry-run        Print planned changes without writing symlinks
  --force          Replace conflicting paths
  --help           Show this help text
EOF
}

die() {
	echo "Error: $*" >&2
	exit 1
}

list_skills() {
	find "${SKILLS_DIR}" -mindepth 1 -maxdepth 1 -type d \
		-exec test -f "{}/SKILL.md" \; -print \
		| sed "s|${SKILLS_DIR}/||" \
		| sort
}

skill_exists() {
	local skill_name="$1"
	[[ -f "${SKILLS_DIR}/${skill_name}/SKILL.md" ]]
}

relative_path() {
	local source_path="$1"
	local base_path="$2"
	python3 -c 'import os, sys; print(os.path.relpath(sys.argv[1], sys.argv[2]))' \
		"${source_path}" "${base_path}"
}

link_skill() {
	local skill_name="$1"
	local source_path="${SKILLS_DIR}/${skill_name}"
	local destination_path="${TARGET_DIR%/}/${skill_name}"
	local relative_source

	relative_source="$(relative_path "${source_path}" "${TARGET_DIR}")"

	if [[ -L "${destination_path}" ]]; then
		local current_target=""
		current_target="$(readlink "${destination_path}")"
		if [[ "${current_target}" == "${relative_source}" ]]; then
			echo "Unchanged ${destination_path}"
			return 0
		fi
		if [[ "${FORCE}" != true ]]; then
			die "${destination_path} already exists as a different symlink; rerun with --force"
		fi
		if [[ "${DRY_RUN}" == true ]]; then
			echo "Would replace symlink ${destination_path} -> ${relative_source}"
			return 0
		fi
		rm -f "${destination_path}"
		ln -s "${relative_source}" "${destination_path}"
		echo "Replaced symlink ${destination_path} -> ${relative_source}"
		return 0
	fi

	if [[ -e "${destination_path}" ]]; then
		if [[ "${FORCE}" != true ]]; then
			die "${destination_path} already exists; rerun with --force"
		fi
		if [[ "${DRY_RUN}" == true ]]; then
			echo "Would replace existing path ${destination_path} -> ${relative_source}"
			return 0
		fi
		rm -rf "${destination_path}"
	fi

	if [[ "${DRY_RUN}" == true ]]; then
		echo "Would link ${destination_path} -> ${relative_source}"
		return 0
	fi

	ln -s "${relative_source}" "${destination_path}"
	echo "Linked ${destination_path} -> ${relative_source}"
}

while [[ $# -gt 0 ]]; do
	case "$1" in
		--target)
			[[ $# -ge 2 ]] || die "--target requires a value"
			TARGET_DIR="$2"
			shift 2
			;;
		--all)
			LINK_ALL=true
			shift
			;;
		--skill)
			[[ $# -ge 2 ]] || die "--skill requires a value"
			REQUESTED_SKILLS+=("$2")
			shift 2
			;;
		--list)
			LIST_ONLY=true
			shift
			;;
		--dry-run)
			DRY_RUN=true
			shift
			;;
		--force)
			FORCE=true
			shift
			;;
		--help)
			usage
			exit 0
			;;
		*)
			die "unknown argument: $1"
			;;
	esac
done

if [[ "${LIST_ONLY}" == true ]]; then
	list_skills
	exit 0
fi

[[ -n "${TARGET_DIR}" ]] || die "--target is required unless --list is used"
[[ "${LINK_ALL}" == true || ${#REQUESTED_SKILLS[@]} -gt 0 ]] || die "choose --all or at least one --skill"
[[ ! ("${LINK_ALL}" == true && ${#REQUESTED_SKILLS[@]} -gt 0) ]] || die "use either --all or --skill, not both"

mkdir -p "${TARGET_DIR}"
TARGET_DIR="$(cd "${TARGET_DIR}" && pwd -P)"

declare -a SKILLS_TO_LINK=()

if [[ "${LINK_ALL}" == true ]]; then
	while IFS= read -r skill_name; do
		[[ -n "${skill_name}" ]] || continue
		SKILLS_TO_LINK+=("${skill_name}")
	done < <(list_skills)
else
	for skill_name in "${REQUESTED_SKILLS[@]}"; do
		local_seen=false
		skill_exists "${skill_name}" || die "shared skill not found: ${skill_name}"
		if [[ ${#SKILLS_TO_LINK[@]} -gt 0 ]]; then
			for seen_skill in "${SKILLS_TO_LINK[@]}"; do
				if [[ "${seen_skill}" == "${skill_name}" ]]; then
					local_seen=true
					break
				fi
			done
		fi
		if [[ "${local_seen}" == false ]]; then
			SKILLS_TO_LINK+=("${skill_name}")
		fi
	done
fi

[[ ${#SKILLS_TO_LINK[@]} -gt 0 ]] || die "no shared skills found"

for skill_name in "${SKILLS_TO_LINK[@]}"; do
	link_skill "${skill_name}"
done
