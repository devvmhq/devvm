#!/bin/bash
# Run nilaway on devvm's own packages, not the tests or their helpers (stubtest, backendtest), and
# compare its findings with the triaged ones in testdata/nilaway-baseline.txt (mise run nilaway).
#   scripts/nilaway.sh   print every finding, then the ones the baseline hasn't and the baseline's
#                        lines that match none; exit 3 when there's a new finding, and 1 when
#                        nilaway can't run, when its output can't be read, or when a baseline
#                        line is stale or malformed
# A finding's key is its file and its trace, one step after another, without line or column
# numbers, so the baseline holds when code above a finding moves. A baseline line is a group, one
# word for why it's a false positive, then the key, and the baseline has a line for each finding:
# two findings with the same key are two lines, so a third with that key is new.
# NILAWAY_BASELINE names another baseline, for scripts/nilaway_test.go.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
baseline=${NILAWAY_BASELINE:-$root/testdata/nilaway-baseline.txt}
groups="checked bounds mapkey nilsafe"
mod=github.com/devvmhq/devvm
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

status=0
(cd "$root" && nilaway -include-pkgs="$mod" -exclude-pkgs="$mod/internal/stubtest,$mod/internal/backend/backendtest" \
  -test=false -pretty-print=false ./...) > "$tmp/out.txt" 2>&1 || status=$?
cat "$tmp/out.txt"
# 3: findings; anything else but 0: nilaway didn't run.
if (( status != 0 && status != 3 )); then
  echo "nilaway: failed with status $status" >&2
  exit 1
fi

# found: each finding's key, a line each. A finding is its header line, PATH:LINE:COL: Potential
# nil panic..., then its trace's lines, each starting with a tab and "- "; a "(Same nil source..."
# line and blank lines end it.
header=' Potential nil panic detected\.'
ROOT="$root/" awk -v header="$header" '
  function flush() { if (key != "") print key; key = "" }
  function strip(s) { gsub(/\.go:[0-9]+(:[0-9]+)?/, ".go", s); return s }
  $0 ~ header {
    flush()
    f = $0
    if (index(f, ENVIRON["ROOT"]) == 1) f = substr(f, length(ENVIRON["ROOT"]) + 1)
    sub(/:[0-9]+:[0-9]+: .*/, "", f)
    key = f ":"
    next
  }
  key != "" && /^\t+- / { s = $0; sub(/^\t+/, "", s); key = key " " strip(s); next }
  { flush() }
  END { flush() }
' "$tmp/out.txt" | LC_ALL=C sort > "$tmp/found.txt"
# Every finding nilaway printed has a key, and findings mean it exited 3: else its output has
# changed shape, and the comparison would be empty.
headers=$(grep -c -- "$header" "$tmp/out.txt" || true)
found=$(wc -l < "$tmp/found.txt" | tr -d ' ')
if (( found != headers )) || { (( status == 3 )) && (( found == 0 )); } || { (( status == 0 )) && (( found > 0 )); }; then
  echo "nilaway: can't read its output: $headers findings, $found keys read, status $status" >&2
  exit 1
fi

bad=$(grep -nvE "^(#|$)|^($(tr ' ' '|' <<<"$groups")) [^ ]" "$baseline" || true)
if [[ -n $bad ]]; then
  echo "nilaway: baseline lines that aren't a group ($groups) and a key:" >&2
  echo "$bad" >&2
  exit 1
fi
grep -vE '^(#|$)' "$baseline" | cut -d' ' -f2- | LC_ALL=C sort > "$tmp/known.txt" || true
# Multisets: comm pairs equal lines one to one, so a key found more often than the baseline has
# it is new, and one the baseline has more often than it's found is stale.
LC_ALL=C comm -23 "$tmp/found.txt" "$tmp/known.txt" > "$tmp/new.txt"
LC_ALL=C comm -13 "$tmp/found.txt" "$tmp/known.txt" > "$tmp/gone.txt"

echo
echo "nilaway: $found findings, $(wc -l < "$tmp/new.txt" | tr -d ' ') not in testdata/nilaway-baseline.txt"
result=0
if [[ -s $tmp/new.txt ]]; then
  echo "nilaway: new findings (fix a real one, with a test; add a false positive to the baseline with its group):"
  sed 's/^/new: /' "$tmp/new.txt"
  result=3
fi
if [[ -s $tmp/gone.txt ]]; then
  echo "nilaway: baseline lines that match no finding (remove them):"
  sed 's/^/stale: /' "$tmp/gone.txt"
  result=1
fi
exit "$result"
