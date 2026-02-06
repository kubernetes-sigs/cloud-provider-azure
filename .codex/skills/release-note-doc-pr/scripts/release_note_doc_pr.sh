#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Create/update docs release note and open a PR.

Usage:
  release_note_doc_pr.sh --tag vX.Y.Z [--no-generate] [--dry-run] [--force-push] \
    [--base-remote <name>] [--push-remote <name>] [--base-branch <name>] \
    [--target-repo <owner/repo>] [--head-owner <owner>] [--op-item-id <id>]

Flags:
  --tag <tag>        Release tag like v1.35.7 (required)
  --no-generate      Skip running hack/generate-release-note.sh
  --dry-run          Print commands without executing them
  --force-push       Push with --force-with-lease
  --base-remote      Remote that hosts the base branch (default: upstream if it has base branch, else origin)
  --push-remote      Remote to push head branch to (default: origin)
  --base-branch      Base branch for the docs PR (default: documentation)
  --target-repo      PR target repo as owner/repo (default: derived from base remote URL)
  --head-owner       PR head owner (default: derived from push remote URL)
  --op-item-id       1Password item id for token fallback (default: 66wix7nx4jz56rn6h45mfsp7ja)
  -h, --help         Show help
EOF
}

tag=""
generate=true
dry_run=false
force_push=false
base_remote=""
push_remote="origin"
base_branch="documentation"
target_repo=""
head_owner=""
op_item_id="66wix7nx4jz56rn6h45mfsp7ja"

require_value() {
  local flag="$1"
  local value="${2:-}"
  if [[ -z "${value}" || "${value}" == --* ]]; then
    echo "[ERROR] ${flag} requires a value." >&2
    usage >&2
    exit 2
  fi
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --tag)
      require_value "$1" "${2:-}"
      tag="${2:-}"
      shift 2
      ;;
    --no-generate)
      generate=false
      shift 1
      ;;
    --dry-run)
      dry_run=true
      shift 1
      ;;
    --force-push)
      force_push=true
      shift 1
      ;;
    --base-remote)
      require_value "$1" "${2:-}"
      base_remote="${2:-}"
      shift 2
      ;;
    --push-remote)
      require_value "$1" "${2:-}"
      push_remote="${2:-}"
      shift 2
      ;;
    --base-branch)
      require_value "$1" "${2:-}"
      base_branch="${2:-}"
      shift 2
      ;;
    --target-repo)
      require_value "$1" "${2:-}"
      target_repo="${2:-}"
      shift 2
      ;;
    --head-owner)
      require_value "$1" "${2:-}"
      head_owner="${2:-}"
      shift 2
      ;;
    --op-item-id)
      require_value "$1" "${2:-}"
      op_item_id="${2:-}"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "[ERROR] Unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if [[ -z "${tag}" ]]; then
  echo "[ERROR] --tag is required" >&2
  usage >&2
  exit 2
fi

if [[ -n "$(git status --porcelain)" ]]; then
  echo "[ERROR] Working tree is not clean. Commit or stash changes first." >&2
  exit 1
fi

if ! command -v gh >/dev/null 2>&1; then
  echo "[ERROR] gh CLI is required (https://cli.github.com/)." >&2
  exit 1
fi

head_branch="doc/release-note-${tag}"
title="Update release notes for ${tag}"
body="This PR updates the release notes for version ${tag}."
label="kind/documentation"
site_file="content/en/blog/releases/${tag}.md"

run() {
  if [[ "${dry_run}" == "true" ]]; then
    printf '+'
    for a in "$@"; do printf ' %q' "${a}"; done
    printf '\n'
    return 0
  fi
  "$@"
}

remote_exists() {
  git remote get-url "$1" >/dev/null 2>&1
}

remote_has_branch() {
  git ls-remote --exit-code --heads "$1" "$2" >/dev/null 2>&1
}

parse_remote_owner_repo() {
  local remote="$1"
  local remote_url normalized
  remote_url="$(git remote get-url "${remote}" 2>/dev/null || true)"
  normalized="${remote_url%.git}"

  if [[ "${normalized}" =~ ^git@[^:]+:([^/]+)/([^/]+)$ ]]; then
    printf '%s/%s\n' "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}"
    return 0
  fi
  if [[ "${normalized}" =~ ^https?://[^/]+/([^/]+)/([^/]+)$ ]]; then
    printf '%s/%s\n' "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}"
    return 0
  fi
  if [[ "${normalized}" =~ ^ssh://git@[^/]+/([^/]+)/([^/]+)$ ]]; then
    printf '%s/%s\n' "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}"
    return 0
  fi

  return 1
}

resolve_base_remote() {
  if [[ -n "${base_remote}" ]]; then
    if ! remote_exists "${base_remote}"; then
      echo "[ERROR] Base remote '${base_remote}' does not exist." >&2
      exit 1
    fi
  else
    if remote_exists upstream && remote_has_branch upstream "${base_branch}"; then
      base_remote="upstream"
    else
      base_remote="origin"
    fi
  fi

  if ! remote_exists "${base_remote}"; then
    echo "[ERROR] Base remote '${base_remote}' does not exist." >&2
    exit 1
  fi
  if ! remote_has_branch "${base_remote}" "${base_branch}"; then
    echo "[ERROR] Base branch '${base_branch}' was not found on remote '${base_remote}'." >&2
    exit 1
  fi
}

resolve_push_remote() {
  if ! remote_exists "${push_remote}"; then
    echo "[ERROR] Push remote '${push_remote}' does not exist." >&2
    exit 1
  fi
}

resolve_target_repo() {
  if [[ -n "${target_repo}" ]]; then
    return 0
  fi

  target_repo="$(parse_remote_owner_repo "${base_remote}" || true)"
  if [[ -z "${target_repo}" ]]; then
    echo "[ERROR] Failed to derive --target-repo from remote '${base_remote}'." >&2
    echo "        Provide --target-repo <owner/repo> explicitly." >&2
    exit 1
  fi
}

resolve_head_owner() {
  local push_repo
  if [[ -n "${head_owner}" ]]; then
    return 0
  fi

  push_repo="$(parse_remote_owner_repo "${push_remote}" || true)"
  if [[ -z "${push_repo}" ]]; then
    echo "[ERROR] Failed to derive --head-owner from remote '${push_remote}'." >&2
    echo "        Provide --head-owner <owner> explicitly." >&2
    exit 1
  fi
  head_owner="${push_repo%%/*}"
}

load_github_token() {
  local token

  if [[ -n "${GITHUB_TOKEN:-}" ]]; then
    return 0
  fi

  if [[ -n "${GH_TOKEN:-}" ]]; then
    export GITHUB_TOKEN="${GH_TOKEN}"
    return 0
  fi

  token="$(gh auth token 2>/dev/null || true)"
  if [[ -n "${token}" ]]; then
    export GITHUB_TOKEN="${token}"
    return 0
  fi

  if command -v op >/dev/null 2>&1; then
    token="$(op item get "${op_item_id}" --reveal --fields token 2>/dev/null || true)"
    if [[ -n "${token}" ]]; then
      export GITHUB_TOKEN="${token}"
      return 0
    fi
  fi

  echo "[ERROR] Could not resolve GitHub token." >&2
  echo "        Provide GITHUB_TOKEN, GH_TOKEN, a valid 'gh auth login', or a readable 1Password token item (--op-item-id)." >&2
  exit 1
}

validate_generated_site_file() {
  if [[ ! -s "${site_file}" ]]; then
    echo "[ERROR] Expected docs release note file is missing or empty: ${site_file}" >&2
    echo "        Run ./hack/generate-release-note.sh ${tag} release-notes.md true (or omit --no-generate)." >&2
    exit 1
  fi

  if ! awk '
    BEGIN {in_frontmatter=0;seen_heading=0}
    NR == 1 && $0 == "---" {in_frontmatter=1; next}
    in_frontmatter && $0 == "---" {in_frontmatter=0; next}
    in_frontmatter {next}
    $0 ~ /^Full Changelog:/ {next}
    $0 ~ /^## / {seen_heading=1}
    END {exit seen_heading ? 0 : 1}
  ' "${site_file}"; then
    echo "[ERROR] Generated file lacks release note sections ('## ...'): ${site_file}" >&2
    exit 1
  fi
}

resolve_base_remote
resolve_push_remote
resolve_target_repo
resolve_head_owner
load_github_token

existing_pr="$(
  gh pr list \
    --repo "${target_repo}" \
    --head "${head_owner}:${head_branch}" \
    --base "${base_branch}" \
    --state open \
    --json number,url \
    --jq 'if length > 0 then .[0] | "\(.number) \(.url)" else "" end' 2>/dev/null || true
)"
if [[ -n "${existing_pr}" ]]; then
  echo "[OK] PR already exists for ${head_owner}:${head_branch} -> ${target_repo}:${base_branch}: ${existing_pr}"
  exit 0
fi

run git fetch "${base_remote}" "${base_branch}"
run git checkout -B "${head_branch}" "${base_remote}/${base_branch}"

if [[ "${generate}" == "true" ]]; then
  run ./hack/generate-release-note.sh "${tag}" release-notes.md true
fi

if [[ "${dry_run}" == "false" ]]; then
  validate_generated_site_file
else
  echo "[OK] Dry run: skipping generated content validation."
fi

run git add "${site_file}"

if [[ "${dry_run}" == "false" ]]; then
  if git diff --cached --quiet; then
    echo "[OK] No changes to commit for ${site_file}"
  else
    run git commit -m "${title}"
  fi
fi

push_args=(git push -u "${push_remote}" "${head_branch}")
if [[ "${force_push}" == "true" ]]; then
  push_args+=(--force-with-lease)
fi
run "${push_args[@]}"

run gh pr create \
  --repo "${target_repo}" \
  --head "${head_owner}:${head_branch}" \
  --base "${base_branch}" \
  --title "${title}" \
  --body "${body}" \
  --label "${label}"
