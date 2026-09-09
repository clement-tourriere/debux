#!/usr/bin/env bash
# Scan and smoke the exact archive on a native worker, then bind the successful
# result to its whole-index digest and report hashes. No registry writes here.
set -euo pipefail
archive="${1:?Usage: validate-image-archive.sh OCI_ARCHIVE EXPECTED_DIGEST [ARCH]}"
expected="${2:?Expected build digest is required}"
[[ "$expected" =~ ^sha256:[a-f0-9]{64}$ ]] || { echo 'Invalid image digest' >&2; exit 1; }
case "${3:-}" in
  "") arches=(amd64 arm64);;
  amd64|arm64) arches=("$3");;
  *) echo 'Expected amd64 or arm64' >&2; exit 2;;
esac
# Invalidate old success records even if digest verification or scanning fails.
for arch in "${arches[@]}"; do rm -f "dist/security/publish-$arch/validated.json"; done
actual="sha256:$(skopeo inspect --raw "oci-archive:$archive" | sha256sum | cut -d ' ' -f 1)"
[[ "$actual" == "$expected" ]] || { echo 'OCI archive differs from the build digest' >&2; exit 1; }
for arch in "${arches[@]}"; do
  report_dir="dist/security/publish-$arch"
  DEBUX_SECURITY_REPORT_DIR="$report_dir" \
    scripts/scan-image.sh "oci-archive:$archive" "linux/$arch"
  local_image="debux:publish-check-$arch"
  skopeo copy --override-os linux --override-arch "$arch" \
    "oci-archive:$archive" "docker-daemon:$local_image"
  scripts/test-image.sh "$local_image"
  uv run --script scripts/image_security.py record "$report_dir" "$expected" "$arch"
done
