#!/usr/bin/env bash
# Called only by the trusted main/release workflows, after native validation.
set -euo pipefail
archive="${1:?Usage: publish-image-archive.sh OCI_ARCHIVE EXPECTED_DIGEST}"
expected="${2:?Expected build digest is required}"
: "${IMAGE_TAGS:?Image tags are required}"
[[ "$expected" =~ ^sha256:[a-f0-9]{64}$ ]] || { echo 'Invalid image digest' >&2; exit 1; }
tags=()
while IFS= read -r tag; do
  [[ -z "$tag" ]] && continue
  [[ "$tag" =~ ^ghcr.io/clement-tourriere/debux:[A-Za-z0-9_][A-Za-z0-9_.-]*$ ]] || {
    echo "Unexpected publication tag: $tag" >&2; exit 1;
  }
  tags+=("$tag")
done <<< "$IMAGE_TAGS"
[[ ${#tags[@]} -gt 0 ]] || exit 1
actual="sha256:$(skopeo inspect --raw "oci-archive:$archive" | sha256sum | cut -d ' ' -f 1)"
[[ "$actual" == "$expected" ]] || { echo 'OCI archive differs from the build digest' >&2; exit 1; }
scripts/validate-image-archive.sh "$archive"
# Copy the scanned bytes, including both architectures and BuildKit SBOM /
# provenance attestations. Never rebuild or promote an unscanned registry tag.
for tag in "${tags[@]}"; do
  skopeo copy --all --preserve-digests --dest-authfile "$HOME/.docker/config.json" \
    "oci-archive:$archive" "docker://$tag"
done
