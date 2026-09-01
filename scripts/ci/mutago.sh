#!/usr/bin/env bash
set -euo pipefail

# mutago measures only changed production lines. This keeps the required
# covered-MSI gate focused on the code a change actually owns while still
# running the package's full test suite for those lines.

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd -- "$script_dir/../.." && pwd)"
cd "$repo_root"

# shellcheck disable=SC1091
source ./.buildflags

diff_base="${MUTAGO_DIFF_BASE:-}"
if [[ -z "$diff_base" && -n "${GITHUB_BASE_REF:-}" ]]; then
  diff_base="origin/$GITHUB_BASE_REF"
fi
if [[ -z "$diff_base" ]] && git rev-parse --verify --quiet "origin/main^{commit}" >/dev/null; then
  diff_base=origin/main
fi
if [[ -z "$diff_base" ]]; then
  diff_base="$(git merge-base HEAD origin/main 2>/dev/null || true)"
fi
if [[ -z "$diff_base" ]] || ! git rev-parse --verify --quiet "$diff_base^{commit}" >/dev/null; then
  printf 'mutago: unable to resolve a merge-base (set MUTAGO_DIFF_BASE)\n' >&2
  exit 1
fi

changed_files="$(git diff --name-only "$diff_base"...HEAD -- '*.go')"
active_files="$(go list -f '{{range .GoFiles}}{{printf "%s/%s\n" $.Dir .}}{{end}}{{range .CgoFiles}}{{printf "%s/%s\n" $.Dir .}}{{end}}' ./...)"
declare -A active_paths=()
while IFS= read -r path; do
  [[ "$path" == "$repo_root/"* ]] || continue
  active_paths["${path#"$repo_root/"}"]=1
done <<<"$active_files"

paths=()
while IFS= read -r path; do
  [[ -n "$path" ]] || continue
  [[ "$path" != *_test.go ]] || continue
  [[ -f "$path" ]] || continue
  [[ -n "${active_paths[$path]+present}" ]] || continue
  paths+=("$path")
done <<<"$changed_files"

shard_count="${MUTAGO_SHARDS:-1}"
shard_index="${MUTAGO_SHARD_INDEX:-0}"
if [[ ! "$shard_count" =~ ^[1-9][0-9]*$ ]]; then
  printf 'mutago: MUTAGO_SHARDS must be a positive integer (got %q)\n' "$shard_count" >&2
  exit 1
fi
if [[ ! "$shard_index" =~ ^[0-9]+$ ]] || ((shard_index >= shard_count)); then
  printf 'mutago: MUTAGO_SHARD_INDEX must be an integer in [0,%d) (got %q)\n' "$shard_count" "$shard_index" >&2
  exit 1
fi

all_paths=("${paths[@]}")
paths=()
for i in "${!all_paths[@]}"; do
  if ((i % shard_count == shard_index)); then
    paths+=("${all_paths[$i]}")
  fi
done

if ((${#paths[@]} == 0)); then
  printf 'mutago: shard %d/%d has no changed production Go files; writing an empty summary\n' "$shard_index" "$shard_count"
  cat > mutago-summary.json <<'EOF'
{"totalMutantsCount":0,"killedCount":0,"escapedCount":0,"errorCount":0,"skippedCount":0,"notCoveredCount":0,"msi":0,"coveredCodeMsi":0}
EOF
  exit 0
fi

if [[ -n "${MUTAGO_BIN:-}" ]]; then
  mutago=("$MUTAGO_BIN")
elif command -v mutago >/dev/null 2>&1; then
  mutago=("$(command -v mutago)")
else
  mutago=(go run github.com/quality-gates/mutago/v2/cmd/mutago@v2.9.1)
fi

printf 'mutago: scanning %d changed production Go file(s) in shard %d/%d against %s\n' "${#paths[@]}" "$shard_index" "$shard_count" "$diff_base"
mutago_args=(
  --coverage \
  --git-diff-lines \
  --git-diff-base="$diff_base" \
  --min-covered-msi="${MUTAGO_MIN_COVERED_MSI:-80}" \
  --ignore-msi-with-no-mutations \
  --quiet \
  --no-diffs \
  --logger-summary-json \
  --workers="${MUTAGO_WORKERS:-2}" \
  --test-flags=-tags=gms_pure_go \
)
if [[ "${MUTAGO_PER_TEST:-0}" == 1 ]]; then
  mutago_args+=(--per-test)
fi
"${mutago[@]}" "${mutago_args[@]}" "${paths[@]}"
