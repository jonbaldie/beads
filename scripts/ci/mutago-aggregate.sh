#!/usr/bin/env bash
set -euo pipefail

# Combine per-shard Mutago summaries using the same covered-MSI definition as
# Mutago itself: (killed + errored + skipped) / (total - not-covered). The
# numerator and denominator are summed before dividing so a small shard cannot
# fail or pass the gate merely because its local denominator is tiny.

summary_dir="${1:-mutago-summaries}"
shopt -s nullglob
summaries=("$summary_dir"/mutago-summary-*.json)
if ((${#summaries[@]} == 0)); then
  printf 'mutago: no shard summaries found in %s\n' "$summary_dir" >&2
  exit 1
fi
expected_shards="${MUTAGO_EXPECTED_SHARDS:-}"
if [[ -n "$expected_shards" ]]; then
  if [[ ! "$expected_shards" =~ ^[1-9][0-9]*$ ]]; then
    printf 'mutago: MUTAGO_EXPECTED_SHARDS must be a positive integer (got %q)\n' "$expected_shards" >&2
    exit 1
  fi
  if ((${#summaries[@]} != expected_shards)); then
    printf 'mutago: expected %d shard summaries, found %d\n' "$expected_shards" "${#summaries[@]}" >&2
    exit 1
  fi
  for ((shard = 0; shard < expected_shards; shard++)); do
    if [[ ! -f "$summary_dir/mutago-summary-$shard.json" ]]; then
      printf 'mutago: missing summary for shard %d\n' "$shard" >&2
      exit 1
    fi
  done
fi

total=0
killed=0
escaped=0
errored=0
skipped=0
not_covered=0

for summary in "${summaries[@]}"; do
  values="$(jq -er '[.totalMutantsCount,.killedCount,.escapedCount,.errorCount,.skippedCount,.notCoveredCount] | @tsv' "$summary")" || {
    printf 'mutago: invalid summary JSON: %s\n' "$summary" >&2
    exit 1
  }
  IFS=$'\t' read -r shard_total shard_killed shard_escaped shard_errored shard_skipped shard_not_covered <<<"$values"
  for value in "$shard_total" "$shard_killed" "$shard_escaped" "$shard_errored" "$shard_skipped" "$shard_not_covered"; do
    if [[ ! "$value" =~ ^[0-9]+$ ]]; then
      printf 'mutago: summary contains a non-negative integer violation: %s\n' "$summary" >&2
      exit 1
    fi
  done
  if ((shard_total != shard_killed + shard_escaped + shard_errored + shard_skipped + shard_not_covered)); then
    printf 'mutago: summary counts do not add up: %s\n' "$summary" >&2
    exit 1
  fi
  total=$((total + shard_total))
  killed=$((killed + shard_killed))
  escaped=$((escaped + shard_escaped))
  errored=$((errored + shard_errored))
  skipped=$((skipped + shard_skipped))
  not_covered=$((not_covered + shard_not_covered))
done

if ((total == 0)); then
  echo 'mutago: no mutations were generated across the changed production Go files; covered-MSI gate passes'
  exit 0
fi

covered=$((total - not_covered))
if ((covered <= 0)); then
  score='0.00'
  passes=1
else
  score="$(awk -v killed="$((killed + errored + skipped))" -v covered="$covered" 'BEGIN { printf "%.2f", 100 * killed / covered }')"
  if awk -v killed="$((killed + errored + skipped))" -v covered="$covered" 'BEGIN { exit !(killed / covered >= 0.80) }'; then
    passes=0
  else
    passes=1
  fi
fi

printf 'mutago: aggregate covered-MSI %.2f%% (killed=%d, errored=%d, skipped=%d, escaped=%d, not-covered=%d, covered=%d, total=%d)\n' \
  "$score" "$killed" "$errored" "$skipped" "$escaped" "$not_covered" "$covered" "$total"
if ((passes)); then
  printf 'mutago: aggregate covered-MSI is below the required 80.00%%\n' >&2
  exit 1
fi
