#!/usr/bin/env bash
set -euo pipefail

ALLOWLIST="${GOVULNCHECK_ALLOWLIST:-scripts/govulncheck-allowlist.txt}"
OUT="$(mktemp)"
ERR="$(mktemp)"
trap 'rm -f "$OUT" "$ERR"' EXIT

patterns=("$@")
if [[ ${#patterns[@]} -eq 0 ]]; then
  patterns=("./...")
fi

set +e
go run golang.org/x/vuln/cmd/govulncheck@v1.3.0 -format json "${patterns[@]}" >"$OUT" 2>"$ERR"
status=$?
set -e

# govulncheck may use a non-zero exit status when vulnerabilities are found.
# Parse the machine-readable output first so accepted findings can be filtered.
python3 - "$OUT" "$ALLOWLIST" <<'PY'
import json
import sys
from pathlib import Path

out_path = Path(sys.argv[1])
allow_path = Path(sys.argv[2])

allowed = set()
if allow_path.exists():
    for line in allow_path.read_text().splitlines():
        line = line.split('#', 1)[0].strip()
        if line:
            allowed.add(line)

text = out_path.read_text()
decoder = json.JSONDecoder()
idx = 0
findings = []
while idx < len(text):
    while idx < len(text) and text[idx].isspace():
        idx += 1
    if idx >= len(text):
        break
    obj, end = decoder.raw_decode(text, idx)
    idx = end
    finding = obj.get('finding')
    if finding:
        findings.append(finding)

by_id = {}
for finding in findings:
    by_id.setdefault(finding.get('osv', '<unknown>'), 0)
    by_id[finding.get('osv', '<unknown>')] += 1

unaccepted = sorted(vuln_id for vuln_id in by_id if vuln_id not in allowed)
accepted = sorted(vuln_id for vuln_id in by_id if vuln_id in allowed)

if accepted:
    print('Accepted govulncheck findings:')
    for vuln_id in accepted:
        print(f'  {vuln_id} ({by_id[vuln_id]} finding(s))')

if unaccepted:
    print('Unaccepted govulncheck findings:', file=sys.stderr)
    for vuln_id in unaccepted:
        print(f'  {vuln_id} ({by_id[vuln_id]} finding(s))', file=sys.stderr)
    print('', file=sys.stderr)
    print('Run scripts/govulncheck.sh locally for details. If a finding is a documented false positive, add it to scripts/govulncheck-allowlist.txt with a justification.', file=sys.stderr)
    sys.exit(1)

if findings:
    print('govulncheck passed with only accepted findings.')
else:
    print('govulncheck passed with no findings.')
PY
parse_status=$?

if [[ $parse_status -ne 0 ]]; then
  cat "$ERR" >&2
  exit "$parse_status"
fi

# If govulncheck failed for reasons other than accepted findings (compile errors,
# network errors, invalid patterns), preserve that failure.
if [[ $status -ne 0 ]]; then
  if [[ -s "$ERR" ]]; then
    cat "$ERR" >&2
  fi
  # JSON mode usually exits 0 for findings. If it exits non-zero but all findings
  # were accepted and there was no stderr, keep CI green.
  if [[ -s "$ERR" ]]; then
    exit "$status"
  fi
fi
