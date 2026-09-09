#!/usr/bin/env bash
set -euo pipefail
image="${1:-docker:debux:security}"
report_dir="${DEBUX_SECURITY_REPORT_DIR:-dist/security}"
mkdir -p "$report_dir"
# Never leave a previous successful report beside a failed new assessment.
rm -f "$report_dir/grype.json" "$report_dir/grype-config.json"
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# Inventory once and fail closed if OS package metadata is missing. Wolfi's
# distro feed understands APK revision fixes; no package/CVE exclusions are used.
# Positional args also support macOS Bash 3.2 under nounset (empty arrays don't).
if [[ -n "${2:-}" ]]; then set -- --platform "$2"; else set --; fi
go run github.com/anchore/syft/cmd/syft@v1.51.1 "$image" "$@" \
  -o syft-json > "$report_dir/sbom.json"
uv run --script "$repo_dir/scripts/image_security.py" sbom "$report_dir/sbom.json"
printf '%s\n' '{"ignore": [], "only-fixed": false}' > "$report_dir/grype-config.json"
# Keep the complete report even on failure; never hide unfixed findings.
go run github.com/anchore/grype/cmd/grype@v0.118.0 "sbom:$report_dir/sbom.json" \
  --config "$report_dir/grype-config.json" --fail-on high -o json > "$report_dir/grype.json"
uv run --script "$repo_dir/scripts/image_security.py" report "$report_dir/grype.json"
echo "Image security report: $report_dir/grype.json"
