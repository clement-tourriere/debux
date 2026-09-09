#!/usr/bin/env bash
# Validate the exact multiarch OCI archive later copied to the registry. APK
# repositories can advance between builds; a prior cache hit is not evidence
# that the publisher's bytes are the same as the native validation candidate.
set -euo pipefail
archive="${1:?Usage: validate-image-archive.sh OCI_ARCHIVE}"
for arch in amd64 arm64; do
  DEBUX_SECURITY_REPORT_DIR="dist/security/publish-$arch" \
    scripts/scan-image.sh "oci-archive:$archive" "linux/$arch"
  local_image="debux:publish-check-$arch"
  skopeo copy --override-os linux --override-arch "$arch" \
    "oci-archive:$archive" "docker-daemon:$local_image"
  scripts/test-image.sh "$local_image"
done
