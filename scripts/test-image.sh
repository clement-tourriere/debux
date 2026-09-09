#!/usr/bin/env bash
# Only the named, disposable volumes/containers created here are touched.
set -euo pipefail
image="${1:?Usage: scripts/test-image.sh IMAGE}"
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
size="$(docker image inspect "$image" --format '{{.Size}}')"
[[ "$size" =~ ^[0-9]{1,12}$ ]] || { echo 'Invalid image size' >&2; exit 1; }
# Includes build prerequisites for source-only tools, not just binary downloads.
if (( 10#$size > 1280 * 1024 * 1024 )); then
  echo "Toolbox exceeds the 1280 MiB uncompressed image budget: $size bytes" >&2
  exit 1
fi
platform="$(docker image inspect "$image" --format '{{.Os}}/{{.Architecture}}')"
volumes=()
cleanup() {
  for volume in "${volumes[@]}"; do docker volume rm "$volume" >/dev/null || true; done
}
trap cleanup EXIT
for uid in 0 65534; do
  volume="debux-image-smoke-$$-$RANDOM-$uid"
  docker volume create "$volume" >/dev/null
  volumes+=("$volume")
  security=(--security-opt no-new-privileges:true)
  if [[ "$uid" = 65534 ]]; then security+=(--cap-drop=ALL); fi
  # A read-only system image with a private writable tool volume is sufficient,
  # even for unprivileged installs. Recreate the container to prove persistence.
  for phase in initial resume; do
    network=default
    # Reuse must not depend on a network, even in a brand-new container/HOME.
    if [[ "$phase" = resume ]]; then network=none; fi
    docker run --rm --platform "$platform" --user "$uid" "${security[@]}" --network "$network" --read-only \
      --cpus 4 --memory 3g \
      --tmpfs /tmp:rw,nosuid,nodev,mode=1777 \
      --env "HOME=/tmp/debux-smoke-$uid" \
      --volume "$volume:/var/lib/debux" \
      --volume "$repo_dir/scripts/test-image-container.sh:/debux-smoke.sh:ro" \
      --entrypoint /bin/bash "$image" /debux-smoke.sh "$phase"
  done
done
