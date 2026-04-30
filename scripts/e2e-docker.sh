#!/usr/bin/env bash
set -euo pipefail

DEBUX_BIN="${DEBUX_BIN:-./bin/debux}"
DEBUX_IMAGE="${DEBUX_IMAGE:-ghcr.io/clement-tourriere/debux:latest}"
DEBUX_PULL_POLICY="${DEBUX_PULL_POLICY:-IfNotPresent}"
TARGET_NAME="${DEBUX_TARGET_NAME:-debux-e2e-docker-$$}"

if [[ ! -x "$DEBUX_BIN" ]]; then
  echo "Building debux test binary at $DEBUX_BIN"
  CGO_ENABLED=0 go build -o "$DEBUX_BIN" ./cmd/debux
fi

cleanup() {
  docker rm -f "$TARGET_NAME" "debux-$TARGET_NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM
cleanup

echo "Starting Docker target $TARGET_NAME"
docker run -d --name "$TARGET_NAME" nginx:alpine >/dev/null

# Wait for nginx inside the target to be healthy before debugging it.
for _ in {1..60}; do
  if docker exec "$TARGET_NAME" wget -qO- http://127.0.0.1 >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

echo "Running one-shot debux command against $TARGET_NAME"
"$DEBUX_BIN" "docker://$TARGET_NAME" \
  --image "$DEBUX_IMAGE" \
  --fresh \
  --pull-policy "$DEBUX_PULL_POLICY" \
  -- sh -lc 'test -d "$DEBUX_TARGET_ROOT" && curl -fsS http://127.0.0.1 >/dev/null && echo debux-e2e-docker-ok'

echo "Checking session cleanup command"
"$DEBUX_BIN" kill "docker://$TARGET_NAME"

echo "Docker e2e passed"
