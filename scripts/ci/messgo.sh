#!/usr/bin/env bash
set -euo pipefail

# messgo scans the complete production Go tree. Tests are excluded by design,
# but no PR diff base narrows the source set: a clean required check means the
# repository as checked out satisfies the rulesets, not only the latest patch.

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd -- "$script_dir/../.." && pwd)"
cd "$repo_root"

# Keep the tool's Go subprocesses on the repository's pure-Go build path.
# shellcheck disable=SC1091
source ./.buildflags

production_files="$({
  git ls-files -- '*.go'
  git ls-files --others --exclude-standard -- '*.go'
} | sort -u)"

paths=()
while IFS= read -r path; do
  [[ -n "$path" ]] || continue
  [[ "$path" != *_test.go ]] || continue
  [[ -f "$path" ]] || continue
  paths+=("$path")
done <<<"$production_files"

if ((${#paths[@]} == 0)); then
  echo 'messgo: no production Go files in scope; skipping'
  exit 0
fi

path_list=""
for path in "${paths[@]}"; do
  path_list+="${path_list:+,}$path"
done

if [[ -n "${MESSGO_BIN:-}" ]]; then
  messgo=("$MESSGO_BIN")
elif command -v messgo >/dev/null 2>&1; then
  messgo=("$(command -v messgo)")
else
  messgo=(go run github.com/quality-gates/messgo/cmd/messgo@v0.2.2)
fi

printf 'messgo: scanning %d production Go file(s)\n' "${#paths[@]}"
"${messgo[@]}" "$path_list" github 'unusedcode,design,codesize' --ignore-tests
