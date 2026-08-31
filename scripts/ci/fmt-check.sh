#!/usr/bin/env bash
# Shared Go formatting check for Make and PR lint wrappers.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

cd "$REPO_ROOT"

printf 'Checking Go formatting...\n'
# Bash 3.2 (the macOS system shell) does not provide mapfile.
GO_FILES=()
while IFS= read -r -d '' GO_FILE; do
    GO_FILES[${#GO_FILES[@]}]="$GO_FILE"
done < <(git ls-files -z --cached --others --exclude-standard -- '*.go')
if ((${#GO_FILES[@]} == 0)); then
    UNFORMATTED=""
elif UNFORMATTED="$(gofmt -l "${GO_FILES[@]}")"; then
    :
else
    status=$?
    printf 'gofmt failed while checking formatting\n' >&2
    exit "$status"
fi

if [[ -n "$UNFORMATTED" ]]; then
    printf 'The following files are not properly formatted:\n'
    printf '%s\n' "$UNFORMATTED"
    printf '\n'
    printf "Run 'make fmt' to fix formatting\n"
    exit 1
fi

printf 'All Go files are properly formatted\n'
